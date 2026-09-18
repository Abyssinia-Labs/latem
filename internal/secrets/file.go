package secrets

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"slices"
)

// FileName is the secrets file inside the config directory (§15).
const FileName = "secrets.env"

// MaxFileSize bounds the secrets file. A KEY=value file holding a handful of
// credentials, PEMs included, does not reach a megabyte; anything that does is
// pointed at the wrong path, and reading it into memory would be the second
// mistake rather than the diagnosis.
const MaxFileSize = 1 << 20

// FileProvider resolves names from ~/.latem/secrets.env.
//
// Mode 0600 is enforced rather than recommended: RUNNER_SPEC §8 calls a
// world-readable config directory "the whole threat model failing quietly",
// and a file provider that reads a 0644 secrets.env is exactly that failure,
// silently. A loose mode is refused with the chmod that fixes it.
//
// The file is read on each [FileProvider.Resolve] rather than cached. That
// costs one read per declared secret — at most a few dozen, of a file measured
// in kilobytes, once per run — and buys two things worth more than the syscall:
// the mode is re-checked every time rather than at some earlier moment the
// operator has since invalidated, and every other secret in the file exists in
// this process for microseconds instead of for the agent's lifetime. The
// buffer, and every parsed value except the one asked for, is zeroed before
// Resolve returns.
type FileProvider struct {
	path string
}

var _ Provider = (*FileProvider)(nil)

// NewFileProvider reads from path, conventionally ~/.latem/secrets.env.
func NewFileProvider(path string) *FileProvider { return &FileProvider{path: path} }

// Path returns the file this provider reads, for diagnostics.
func (p *FileProvider) Path() string { return p.path }

// Name identifies this provider in diagnostics.
func (p *FileProvider) Name() string { return "file" }

// Names returns the secret names the file declares, sorted, and nothing else.
//
// It exists so that a diagnostic enumerating "what is declared on this machine"
// does not have to write a second parser for this format. The one that was
// written split every line on the first "=" and reported the left side as a
// name, which is correct for `KEY=value` and wrong for the interior lines of a
// quoted multi-line value — a PEM's own base64, printed to the terminal as
// though it were the name of a secret. `doctor --secrets` promises "names only,
// never values", and that is the shape of the only way it has ever broken that
// promise.
//
// A file that does not parse yields an error and no names at all. Half-reading
// a credential file is how the fragments got out in the first place: the error
// from [parseSecretsFile] names a line number and never line content, which is
// exactly what a caller should print instead of guessing.
//
// Values are parsed, because deciding where one ends is the whole job, and then
// zeroed before returning. Nothing but the names outlives this call.
func (p *FileProvider) Names() ([]string, error) {
	buf, err := p.read()
	if err != nil {
		return nil, err
	}
	defer zero(buf)

	entries, parseErr := parseSecretsFile(buf)
	names := make([]string, 0, len(entries))
	for name, entry := range entries {
		zero(entry.value)
		names = append(names, name)
	}
	if parseErr != nil {
		return nil, parseErr
	}
	slices.Sort(names)
	return names, nil
}

// Resolve returns the value of name from the secrets file.
func (p *FileProvider) Resolve(_ context.Context, name string) (Value, error) {
	if err := validName(name); err != nil {
		return nil, fmt.Errorf("file: %w", err)
	}

	buf, err := p.read()
	if err != nil {
		return nil, err
	}
	entries, parseErr := parseSecretsFile(buf)
	// The raw file is no longer needed the moment it is parsed, and the parse
	// result owns its own copies.
	zero(buf)
	defer func() {
		for _, e := range entries {
			zero(e.value)
		}
	}()
	if parseErr != nil {
		return nil, fmt.Errorf("file: %s: %w", p.path, parseErr)
	}

	e, ok := entries[name]
	if !ok {
		return nil, fmt.Errorf("file: %s holds no %s: %w", p.path, name, ErrNotFound)
	}
	return append(Value(nil), e.value...), nil
}

// read returns the file's bytes after checking that it is safe to read at all.
func (p *FileProvider) read() ([]byte, error) {
	// Lstat first: the mode of a symlink is always 0777, so a permission check
	// that followed one would either be meaningless or spuriously fail. A
	// symlinked secrets.env is refused outright — there is no legitimate reason
	// for one, and honouring it means the mode guarantee belongs to a file this
	// code never inspected.
	info, err := os.Lstat(p.path)
	if errors.Is(err, fs.ErrNotExist) {
		// Not every machine keeps a secrets file. An absent one is a provider
		// with nothing to say, not a misconfiguration.
		return nil, fmt.Errorf("file: %s does not exist: %w", p.path, ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("file: %w", err)
	}
	if info.Mode()&fs.ModeSymlink != 0 {
		return nil, fmt.Errorf("file: %s is a symlink; point the config at the real file", p.path)
	}

	// #nosec G304 -- p.path is the operator's own configured secrets file, from
	// ~/.latem/config.toml. It never originates from a broker; INV-3's concern
	// is values that crossed the trust boundary, and no such value reaches here.
	f, err := os.Open(p.path)
	if err != nil {
		return nil, fmt.Errorf("file: %w", err)
	}
	defer func() { _ = f.Close() }() // read-only; a close error changes nothing

	// Stat the descriptor, not the path, so the mode checked is the mode of the
	// file actually opened.
	st, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("file: %w", err)
	}
	if !st.Mode().IsRegular() {
		return nil, fmt.Errorf("file: %s is not a regular file", p.path)
	}
	if perm := st.Mode().Perm(); perm&0o077 != 0 {
		return nil, fmt.Errorf(
			"file: %s is mode %04o and must not be readable by group or other; run: chmod 600 %s",
			p.path, perm, p.path)
	}
	// Bounded by the reader as well as by the stat, because the two can
	// disagree: st.Size() is a snapshot and a fifo or a file being appended to
	// would read past it.
	buf, err := io.ReadAll(io.LimitReader(f, MaxFileSize+1))
	if err != nil {
		zero(buf)
		return nil, fmt.Errorf("file: %w", err)
	}
	if len(buf) > MaxFileSize {
		zero(buf)
		return nil, fmt.Errorf("file: %s is larger than %d bytes", p.path, MaxFileSize)
	}
	return buf, nil
}

type fileEntry struct {
	value []byte
	line  int
}

// parseSecretsFile reads the KEY=value form of §15.
//
// Every error names a line NUMBER and never line CONTENT. That is not
// fastidiousness: a malformed line in this file is, by construction, a line
// someone was trying to put a credential on, and an error message is the least
// controlled string in the system — it reaches the log, the run row, and
// Update.ErrorMsg (INV-4).
//
// Quoting is supported because §15's own worked example declares
// APP_STORE_CONNECT_KEY_P8, which is a PEM block, and a PEM cannot be written
// as one KEY=value line without an escape:
//
//   - unquoted values are taken verbatim after trimming surrounding whitespace;
//   - '…' is literal from the first quote to the last on the line;
//   - "…" honours \n, \r, \t, \\ and \", and nothing else — an unknown escape
//     is an error rather than a literal backslash, because the two readings of
//     \n differ by an entire PEM.
func parseSecretsFile(buf []byte) (map[string]fileEntry, error) {
	out := make(map[string]fileEntry)

	for i, raw := range bytes.Split(buf, []byte("\n")) {
		line := bytes.TrimSpace(bytes.TrimRight(raw, "\r"))
		lineNo := i + 1
		if len(line) == 0 || line[0] == '#' {
			continue
		}

		rawKey, rawValue, found := bytes.Cut(line, []byte("="))
		if !found {
			return out, fmt.Errorf("line %d: expected KEY=value", lineNo)
		}
		key := string(bytes.TrimSpace(rawKey))
		if err := validName(key); err != nil {
			return out, fmt.Errorf("line %d: %w", lineNo, err)
		}
		if prev, dup := out[key]; dup {
			// Last-wins is the dotenv convention and it is the wrong one here:
			// a shadowed credential is a build that signs with the key nobody
			// meant, and the failure appears at the App Store, not at load.
			return out, fmt.Errorf("line %d: %s is already defined on line %d", lineNo, key, prev.line)
		}

		value, err := parseSecretValue(bytes.TrimLeft(rawValue, " \t"), lineNo)
		if err != nil {
			return out, err
		}
		out[key] = fileEntry{value: value, line: lineNo}
	}

	return out, nil
}

func parseSecretValue(v []byte, lineNo int) ([]byte, error) {
	if len(v) == 0 {
		return nil, nil
	}
	switch v[0] {
	case '\'':
		end := bytes.LastIndexByte(v, '\'')
		if end == 0 {
			return nil, fmt.Errorf("line %d: unterminated single quote", lineNo)
		}
		if end != len(v)-1 {
			return nil, fmt.Errorf("line %d: trailing characters after the closing quote", lineNo)
		}
		return append([]byte(nil), v[1:end]...), nil
	case '"':
		return unquoteDouble(v, lineNo)
	}
	return append([]byte(nil), v...), nil
}

func unquoteDouble(v []byte, lineNo int) ([]byte, error) {
	out := make([]byte, 0, len(v))
	for i := 1; i < len(v); i++ {
		switch v[i] {
		case '"':
			if i != len(v)-1 {
				return nil, fmt.Errorf("line %d: trailing characters after the closing quote", lineNo)
			}
			return out, nil
		case '\\':
			i++
			if i >= len(v) {
				return nil, fmt.Errorf("line %d: unterminated double quote", lineNo)
			}
			switch v[i] {
			case 'n':
				out = append(out, '\n')
			case 'r':
				out = append(out, '\r')
			case 't':
				out = append(out, '\t')
			case '\\':
				out = append(out, '\\')
			case '"':
				out = append(out, '"')
			default:
				// Named without quoting the character: one byte of a value is
				// still a byte of a value.
				return nil, fmt.Errorf("line %d: unknown escape sequence in a double-quoted value", lineNo)
			}
		default:
			out = append(out, v[i])
		}
	}
	return nil, fmt.Errorf("line %d: unterminated double quote", lineNo)
}

// zero overwrites a buffer in place. It is [Value.Zero] under a name that
// reads correctly for buffers that are not yet anybody's secret.
func zero(b []byte) { Value(b).Zero() }
