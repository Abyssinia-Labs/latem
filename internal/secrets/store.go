package secrets

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

// A Store is somewhere `latem secrets set` puts a value and the chain later
// finds it (A54). Two exist: [KeychainStore], the default on macOS, and
// [FileStore], secrets.env, everywhere else. Each writes exactly where its
// provider in [New]'s chain reads, so a value set here resolves on the next
// run with no reload.
//
// Names is the only way anything leaves a store, and it is names: listing
// secrets never reads a value, and neither does `latem workflows check`
// asking whether one is set.
type Store interface {
	Name() string
	Set(ctx context.Context, name string, value Value) error
	// Remove reports whether there was anything to remove.
	Remove(ctx context.Context, name string) (bool, error)
	Names(ctx context.Context) ([]string, error)
}

// InputRunner is a [Runner] that can also feed a child its stdin.
// [ExecRunner] is one.
type InputRunner interface {
	Runner
	RunInput(ctx context.Context, argv []string, stdin []byte) (stdout, stderr []byte, err error)
}

// FileStore writes secrets.env, the file [FileProvider] reads (§15).
type FileStore struct {
	provider *FileProvider
}

var _ Store = (*FileStore)(nil)

// NewFileStore writes path, conventionally ~/.latem/secrets.env.
func NewFileStore(path string) *FileStore { return &FileStore{provider: NewFileProvider(path)} }

// Name identifies the store in output.
func (s *FileStore) Name() string { return "file" }

// Path is the file this store writes.
func (s *FileStore) Path() string { return s.provider.Path() }

// Names lists the names the file declares. A missing file declares none.
func (s *FileStore) Names(context.Context) ([]string, error) {
	names, err := s.provider.Names()
	if errors.Is(err, ErrNotFound) {
		return nil, nil
	}
	return names, err
}

// Set writes name=value, replacing the line that held name or appending one.
//
// Every other line — comments, blank lines, other secrets — is kept as it was.
// The file is read through the same checks [FileProvider] applies (mode 0600,
// not a symlink, not over [MaxFileSize]), and one that does not parse is
// refused rather than rewritten: rewriting a file this code cannot read is how
// a credential on a malformed line gets lost.
func (s *FileStore) Set(_ context.Context, name string, value Value) error {
	if err := validName(name); err != nil {
		return err
	}
	lines, entries, err := s.load()
	if err != nil {
		return err
	}
	defer zeroLines(lines)
	line := encodeFileLine(name, value)
	defer zero(line)

	if e, ok := entries[name]; ok {
		zero(lines[e.line-1])
		lines[e.line-1] = line
		// Setting a name defined twice is how one says which value is meant.
		lines = deleteLines(lines, e.also)
	} else {
		if n := len(lines); n > 0 && len(lines[n-1]) == 0 {
			// The file's trailing newline produced an empty last element; the
			// new line goes before it so the file still ends in one newline.
			lines = slices.Insert(lines, n-1, line)
		} else {
			lines = append(lines, line, nil)
		}
	}
	return s.write(lines)
}

// Remove deletes name's line.
func (s *FileStore) Remove(_ context.Context, name string) (bool, error) {
	if err := validName(name); err != nil {
		return false, err
	}
	lines, entries, err := s.load()
	if err != nil {
		return false, err
	}
	defer zeroLines(lines)
	e, ok := entries[name]
	if !ok {
		return false, nil
	}
	lines = deleteLines(lines, append([]int{e.line}, e.also...))
	return true, s.write(lines)
}

// deleteLines zeroes and removes the 1-based line numbers nums, which are in
// ascending order.
func deleteLines(lines [][]byte, nums []int) [][]byte {
	for i := len(nums) - 1; i >= 0; i-- {
		n := nums[i]
		zero(lines[n-1])
		lines = slices.Delete(lines, n-1, n)
	}
	return lines
}

// load reads and parses the file. Every parsed value is zeroed before it
// returns: only the raw lines, which the caller zeroes, outlive the call.
func (s *FileStore) load() ([][]byte, map[string]fileEntry, error) {
	buf, err := s.provider.read()
	if errors.Is(err, ErrNotFound) {
		return nil, map[string]fileEntry{}, nil
	}
	if err != nil {
		return nil, nil, err
	}
	entries, perr := parseSecretsFile(buf)
	for _, e := range entries {
		zero(e.value)
	}
	if perr != nil {
		zero(buf)
		return nil, nil, fmt.Errorf("file: %s does not parse (%w), so it was left as it was; fix it first", s.Path(), perr)
	}
	// Copies, so zeroing a line zeroes nothing still in use.
	raw := bytes.Split(buf, []byte("\n"))
	lines := make([][]byte, len(raw))
	for i, l := range raw {
		lines[i] = append([]byte(nil), l...)
	}
	zero(buf)
	return lines, entries, nil
}

// write installs lines through a temporary file in the same directory, at
// 0600 from the moment it exists.
func (s *FileStore) write(lines [][]byte) error {
	path := s.Path()
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".secrets.env-*")
	if err != nil {
		return fmt.Errorf("file: %w", err)
	}
	name := tmp.Name()
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(name) // a no-op once renamed
	}()
	if err := tmp.Chmod(0o600); err != nil {
		return fmt.Errorf("file: %w", err)
	}
	body := bytes.Join(lines, []byte("\n"))
	defer zero(body)
	if _, err := tmp.Write(body); err != nil {
		return fmt.Errorf("file: writing %s: %w", path, err)
	}
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("file: %w", err)
	}
	if err := os.Rename(name, path); err != nil {
		return fmt.Errorf("file: installing %s: %w", path, err)
	}
	return nil
}

// encodeFileLine renders NAME="value" in the quoting [parseSecretsFile]
// reads back to exactly value: \n, \r, \t, \\ and \" escaped, every other byte
// as it is.
func encodeFileLine(name string, value Value) []byte {
	out := make([]byte, 0, len(name)+len(value)+3)
	out = append(out, name...)
	out = append(out, '=', '"')
	for _, b := range value {
		switch b {
		case '\n':
			out = append(out, '\\', 'n')
		case '\r':
			out = append(out, '\\', 'r')
		case '\t':
			out = append(out, '\\', 't')
		case '\\':
			out = append(out, '\\', '\\')
		case '"':
			out = append(out, '\\', '"')
		default:
			out = append(out, b)
		}
	}
	return append(out, '"')
}

func zeroLines(lines [][]byte) {
	for _, l := range lines {
		zero(l)
	}
}

// KeychainStore writes generic passwords filed under the service
// [KeychainProvider] reads (§15), in the default keychain.
type KeychainStore struct {
	service string
	runner  InputRunner
}

var _ Store = (*KeychainStore)(nil)

// NewKeychainStore writes under service, or [KeychainService] when empty.
func NewKeychainStore(service string, runner InputRunner) (*KeychainStore, error) {
	if service == "" {
		service = KeychainService
	}
	if !servicePattern.MatchString(service) {
		return nil, fmt.Errorf("secrets: keychain service %q must match %s", service, servicePattern)
	}
	if runner == nil {
		runner = ExecRunner{}
	}
	return &KeychainStore{service: service, runner: runner}, nil
}

// Name identifies the store in output.
func (s *KeychainStore) Name() string { return "keychain" }

// Set adds or replaces the item for name.
//
// `security add-generic-password -w <value>` would put the value in argv, where
// every user on the machine can read it from the process table. `security -i`
// reads its command from stdin instead, and -X takes the value as hex, so a
// value with a newline or a quote in it — a PEM — needs no quoting and arrives
// byte for byte. [KeychainProvider] reads it back through -g, which says when
// it printed hex.
//
// The item's access list is security(1)'s default: the program that created
// it, which is the same /usr/bin/security the agent reads it with, so a
// LaunchAgent resolves it without a dialog.
func (s *KeychainStore) Set(ctx context.Context, name string, value Value) error {
	if err := validName(name); err != nil {
		return fmt.Errorf("keychain: %w", err)
	}
	// Every word is a constant, the validated name, or hex: nothing needs
	// security's quoting.
	prefix := "add-generic-password -U -s " + s.service + " -a " + name + " -X "
	// `security -i` reads a command line of at most 4095 bytes and runs what
	// is left over as a command of its own. An add cut there stores a
	// truncated value over the good one (-U), and the remainder comes back on
	// stderr as `unknown command "<hex of the value>"`. So a value that would
	// not fit is refused before anything is sent.
	if limit := (keychainMaxLine - len(prefix) - 1) / 2; len(value) > limit {
		return fmt.Errorf("keychain: a value of %d bytes is longer than security(1) can take in one command "+
			"(%d bytes for this name); store it with --store file", len(value), limit)
	}
	encoded := make([]byte, hex.EncodedLen(len(value)))
	hex.Encode(encoded, value)
	defer zero(encoded)
	cmd := make([]byte, 0, len(prefix)+len(encoded)+1)
	cmd = append(cmd, prefix...)
	cmd = append(cmd, encoded...)
	cmd = append(cmd, '\n')
	defer zero(cmd)

	_, stderr, err := s.runner.RunInput(ctx, []string{"security", "-i"}, cmd)
	if err != nil {
		return keychainWriteError(err)
	}
	// `security -i` reports a failed command on stderr and, for some, still
	// exits zero; an add that succeeded prints nothing.
	if msg := strings.TrimSpace(string(stderr)); msg != "" {
		// Whatever security says, it says without the value: a run of hex
		// in its complaint can only be the value echoed back (INV-4).
		return fmt.Errorf("keychain: security did not add the item: %s",
			hexRun.ReplaceAllString(firstLine(msg), "<value elided>"))
	}
	return nil
}

// keychainMaxLine is the longest command line `security -i` runs whole, its
// newline included, with a margin under the 4095 bytes it reads.
const keychainMaxLine = 4000

// hexRun is sixteen or more hex digits: not something security prints in an
// error of its own.
var hexRun = regexp.MustCompile(`[0-9a-fA-F]{16,}`)

// Remove deletes the item for name.
func (s *KeychainStore) Remove(ctx context.Context, name string) (bool, error) {
	if err := validName(name); err != nil {
		return false, fmt.Errorf("keychain: %w", err)
	}
	_, _, err := s.runner.Run(ctx, []string{"security", "delete-generic-password", "-s", s.service, "-a", name})
	switch {
	case err == nil:
		return true, nil
	case exitCodeOf(err) == keychainNotFound:
		return false, nil
	default:
		return false, keychainWriteError(err)
	}
}

// Names lists the accounts filed under the service, from `security
// dump-keychain` — attributes only, without -d, so no value is decrypted and no
// access dialog is raised.
func (s *KeychainStore) Names(ctx context.Context) ([]string, error) {
	out, _, err := s.runner.Run(ctx, []string{"security", "dump-keychain"})
	if err != nil {
		if notInstalled(err) {
			return nil, nil
		}
		return nil, keychainWriteError(err)
	}
	return parseKeychainDump(out, s.service), nil
}

// parseKeychainDump reads the accounts of generic passwords under service.
// Items are separated by their `keychain:` line; an account printed as 0x… is
// not a name this package could have written, and is skipped.
func parseKeychainDump(out []byte, service string) []string {
	seen := map[string]bool{}
	var names []string
	acct, svce, genp := "", "", false
	flush := func() {
		if genp && svce == service && acct != "" && validName(acct) == nil && !seen[acct] {
			seen[acct] = true
			names = append(names, acct)
		}
		acct, svce, genp = "", "", false
	}
	for _, line := range strings.Split(string(out), "\n") {
		t := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(t, "keychain: "):
			flush()
		case t == `class: "genp"`:
			genp = true
		case strings.HasPrefix(t, `"acct"<blob>="`):
			acct = strings.TrimSuffix(strings.TrimPrefix(t, `"acct"<blob>="`), `"`)
		case strings.HasPrefix(t, `"svce"<blob>="`):
			svce = strings.TrimSuffix(strings.TrimPrefix(t, `"svce"<blob>="`), `"`)
		}
	}
	flush()
	slices.Sort(names)
	return names
}

// keychainWriteError names the likely cause of a security(1) that did not
// answer: a locked keychain puts an unlock dialog on the Mac's screen, which
// nobody at an ssh session will see.
func keychainWriteError(err error) error {
	var ce *CommandError
	if errors.As(err, &ce) && errors.Is(ce.Err, context.DeadlineExceeded) {
		return fmt.Errorf("keychain: security did not answer — a locked keychain asks to be unlocked "+
			"in a dialog on this Mac's screen; unlock it (security unlock-keychain) and try again: %w", err)
	}
	return fmt.Errorf("keychain: %w", err)
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	return line
}
