package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
)

// A full screen on a terminal, through stty(1) and escape codes rather than a
// terminal library, for the reason readHidden and useColor give: what this
// needs is a dozen lines, and a dependency is a reviewed part of the spec (§3).

// keyCode is a key the screen acts on.
type keyCode int

const (
	keyRune keyCode = iota
	keyUp
	keyDown
	keyLeft
	keyRight
	keyEnter
	keyEsc
	keyInterrupt
)

// keyPress is one key: a code, and the character for keyRune.
type keyPress struct {
	code keyCode
	r    rune
}

// parseKeys splits what a terminal sent into keys. An escape code a terminal
// sends for a key arrives in one read; an Esc on its own is a key.
func parseKeys(b []byte) (keys []keyPress) {
	for len(b) > 0 {
		c := b[0]
		switch {
		case c == 0x1b && len(b) >= 3 && (b[1] == '[' || b[1] == 'O'):
			code, ok := map[byte]keyCode{'A': keyUp, 'B': keyDown, 'C': keyRight, 'D': keyLeft}[b[2]]
			if ok {
				keys = append(keys, keyPress{code: code})
			}
			b = b[3:]
			continue
		case c == 0x1b:
			keys = append(keys, keyPress{code: keyEsc})
		case c == '\r' || c == '\n':
			keys = append(keys, keyPress{code: keyEnter})
		case c == 0x03 || c == 0x04: // ^C, ^D: raw mode brings no signal
			keys = append(keys, keyPress{code: keyInterrupt})
		case c >= 0x20 && c < 0x7f:
			keys = append(keys, keyPress{r: rune(c)})
		}
		b = b[1:]
	}
	return keys
}

// terminal is the screen, while it is held.
type terminal struct {
	in, out *os.File
	saved   string
	held    bool
	width   int
	height  int
	pending []keyPress
	resized chan os.Signal
}

func newTerminal(in, out *os.File) *terminal {
	return &terminal{in: in, out: out, width: defaultWidth, height: 24, resized: make(chan os.Signal, 1)}
}

// enter takes the screen: no echo, keys as they are pressed, and a read that
// comes back after a tenth of a second with or without one. That timeout is
// why there is no goroutine reading the terminal: one would still be blocked
// in its read when a shell was handed the terminal, and steal its first key.
func (t *terminal) enter() error {
	saved, err := sttySettings(t.in)
	if err != nil {
		return err
	}
	t.saved = saved
	if err := stty(t.in, "-echo", "-icanon", "-isig", "-ixon", "-icrnl", "min", "0", "time", "1"); err != nil {
		_ = stty(t.in, saved)
		return err
	}
	signal.Notify(t.resized, syscall.SIGWINCH)
	t.measure()
	t.held = true
	_, err = io.WriteString(t.out, "\x1b[?1049h\x1b[?25l")
	return err
}

// leave gives the screen back as it was found. It is safe to call twice: a
// defer has to be sure, and the normal way out has things to print after.
func (t *terminal) leave() {
	if !t.held {
		return
	}
	t.held = false
	signal.Stop(t.resized)
	_, _ = io.WriteString(t.out, "\x1b[?25h\x1b[?1049l")
	_ = stty(t.in, t.saved)
}

// suspend gives the terminal back for f, which may run anything on it, and
// takes it again after.
func (t *terminal) suspend(f func() error) error {
	t.leave()
	err := f()
	if e := t.enter(); e != nil {
		return errors.Join(err, e)
	}
	return err
}

// measure reads the window's size. Two numbers are all a terminal library
// would be for here.
func (t *terminal) measure() {
	c := exec.Command("stty", "size")
	c.Stdin = t.in
	out, err := c.Output()
	if err != nil {
		return
	}
	if f := strings.Fields(string(out)); len(f) == 2 {
		if h, err := strconv.Atoi(f[0]); err == nil && h > 0 {
			t.height = h
		}
		if w, err := strconv.Atoi(f[1]); err == nil && w > 0 {
			t.width = w
		}
	}
}

// draw paints lines over what is there, each to the end of its line, which
// does not flash the way clearing first does.
func (t *terminal) draw(lines []string) error {
	var b strings.Builder
	b.WriteString("\x1b[H")
	for i, l := range lines {
		if i > 0 {
			b.WriteString("\r\n")
		}
		b.WriteString(l)
		b.WriteString("\x1b[K")
	}
	b.WriteString("\x1b[J")
	_, err := io.WriteString(t.out, b.String())
	return err
}

// next waits for a key. ok is false when the window changed and wants
// drawing, with no key.
func (t *terminal) next(ctx context.Context) (k keyPress, ok bool, err error) {
	buf := make([]byte, 64)
	for len(t.pending) == 0 {
		if err := ctx.Err(); err != nil {
			return k, false, err
		}
		select {
		case <-t.resized:
			t.measure()
			return k, false, nil
		default:
		}
		n, err := t.in.Read(buf)
		if err != nil && !errors.Is(err, io.EOF) {
			return k, false, err
		}
		t.pending = parseKeys(buf[:n])
	}
	k, t.pending = t.pending[0], t.pending[1:]
	return k, true, nil
}

// waitForEnter reads a line from the terminal in its normal mode.
func waitForEnter(in io.Reader, out io.Writer) {
	printf(out, "\npress Enter to go back\n")
	_, _ = bufio.NewReader(in).ReadString('\n')
}

// openShell gives a person a shell on the terminal, with what to type in it
// above the prompt, and comes back when they leave it. It is how a command
// latem will not run gets run: by the person, in their own shell.
func openShell(ctx context.Context, in, out *os.File, title string, lines []string) error {
	printf(out, "\n%s\n\n", title)
	for _, l := range lines {
		printf(out, "  %s\n", l)
	}
	printf(out, "\nType exit to go back to latem setup.\n\n")
	shell := os.Getenv("SHELL")
	if shell == "" {
		shell = "/bin/sh"
	}
	c := exec.CommandContext(ctx, shell) //nolint:gosec // the person's own shell, as they would open it
	c.Stdin, c.Stdout, c.Stderr = in, out, out
	if err := c.Run(); err != nil {
		var exit *exec.ExitError
		if !errors.As(err, &exit) { // how a shell ended is the last command's
			return fmt.Errorf("starting %s: %w", shell, err)
		}
	}
	return nil
}
