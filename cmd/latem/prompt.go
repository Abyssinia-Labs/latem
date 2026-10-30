package main

import (
	"bufio"
	"errors"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"
)

// Asking the person at the terminal. Every question has a default that is
// safe to take, and a command not on a terminal never asks: it takes the
// default, or a flag says otherwise.

// interactive reports whether cmd's stdin is a terminal. A seam: a test
// answers questions through a string and says it is one.
var interactive = func(cmd *cobra.Command) bool {
	f, ok := cmd.InOrStdin().(*os.File)
	return ok && isTerminal(f)
}

// prompter reads answers from one stdin for the whole of a command, so a
// question does not lose what was typed ahead for the next.
type prompter struct {
	in  *bufio.Reader
	out io.Writer
}

func newPrompter(cmd *cobra.Command) *prompter {
	return &prompter{in: bufio.NewReader(cmd.InOrStdin()), out: cmd.OutOrStdout()}
}

// ask prints question and returns the line typed, trimmed. End of input is an
// empty answer.
func (p *prompter) ask(question string) (string, error) {
	printf(p.out, "%s", question)
	line, err := p.in.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	if errors.Is(err, io.EOF) {
		printf(p.out, "\n")
	}
	return strings.TrimSpace(line), nil
}

// confirm asks a yes or no question. Enter, or end of input, takes def.
func (p *prompter) confirm(question string, def bool) (bool, error) {
	hint := " [y/N] "
	if def {
		hint = " [Y/n] "
	}
	for {
		a, err := p.ask(question + hint)
		if err != nil {
			return false, err
		}
		switch strings.ToLower(a) {
		case "":
			return def, nil
		case "y", "yes":
			return true, nil
		case "n", "no":
			return false, nil
		}
		printf(p.out, "  y or n, please\n")
	}
}
