package secrets

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"syscall"
	"time"
	"unicode"
)

// OpenVaultSocketName is the OpenVault service's local socket, in its
// directory: $OPENVAULT_HOME, or ~/.openvault.
const OpenVaultSocketName = "service.sock"

// openVaultMaxFrame is the service's own limit on a message (service.md,
// framing). A length above it is a peer this package does not understand, and
// allocating it would be the second mistake.
const openVaultMaxFrame = 16 << 20

// Local API message types (OpenVault service.md, v1).
const (
	ovCollections byte = 0x01
	ovGet         byte = 0x03
	ovList        byte = 0x06

	ovNames   byte = 0x81
	ovEntry   byte = 0x82
	ovListing byte = 0x84
	ovError   byte = 0x7f

	ovSecret byte = 0x01
	ovVar    byte = 0x02
)

// openVaultCollection is OpenVault's rule for a collection name, so a
// repository key it could never hold is turned away here rather than as an
// error from the service.
var openVaultCollection = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// openVaultMaxEntry is OpenVault's limit on an entry name, in bytes.
const openVaultMaxEntry = 512

// ErrNoCollection is a repository this machine holds no OpenVault collection
// for: none was created, or this machine was never scoped to it. It is
// [ErrNotFound] too, because the chain goes on to the next store, and it is
// separate so that doctor can say which machine lacks which collection.
var ErrNoCollection = fmt.Errorf("no collection on this machine: %w", ErrNotFound)

// OpenVaultSocket returns the OpenVault service's socket for environ, which is
// os.Environ's form: $OPENVAULT_HOME/service.sock, else
// $HOME/.openvault/service.sock, as OpenVault itself finds it.
func OpenVaultSocket(environ []string) (string, error) {
	var home, ovHome string
	for _, kv := range environ {
		switch k, v, _ := strings.Cut(kv, "="); k {
		case "HOME":
			home = v
		case "OPENVAULT_HOME":
			ovHome = v
		}
	}
	switch {
	case ovHome != "":
		return filepath.Join(ovHome, OpenVaultSocketName), nil
	case home != "":
		return filepath.Join(home, ".openvault", OpenVaultSocketName), nil
	}
	return "", errors.New("secrets: openvault: neither OPENVAULT_HOME nor HOME is set, so there is no socket to read")
}

// OpenVaultProvider resolves names from OpenVault (A86), on the machine that
// runs the job, at the moment it runs.
//
// OpenVault keeps one collection per repository and names an entry by its
// environment, so a secret NAME for repository R reads as:
//
//	R__E__NAME   collection R, entry E/NAME
//	R__NAME      collection R, entry NAME, at the collection's top
//	NAME         nothing: a machine-wide name has no repository to be in
//
// The scope arrives as its parts ([ScopedProvider]), not as the flattened
// name, because flattening is lossy: latem-cloud and latem_cloud both become
// latem_cloud, and the collection is named latem-cloud.
//
// It speaks the service's local API on its Unix socket, and never runs the
// `openvault` CLI. The CLI opens the vault itself when the service does not
// answer, and that can mean a passphrase prompt on /dev/tty — a job that
// waits for a person who is not there. The socket answers or it does not.
// It also gives a missing entry as an answer rather than as stderr to parse,
// needs no `openvault` on the service manager's PATH, and starts no process
// per lookup.
//
// A service that does not answer is not a miss. The operator said this
// machine's secrets are in OpenVault, so "not running" is a backend that is
// down, E_SECRET_BACKEND and retryable, as a locked Keychain is: falling
// through would give a workflow job an empty secret, which reads as unset,
// and a deploy that runs without its token.
type OpenVaultProvider struct {
	socket  string
	timeout time.Duration
}

var (
	_ Provider       = (*OpenVaultProvider)(nil)
	_ ScopedProvider = (*OpenVaultProvider)(nil)
)

// NewOpenVaultProvider reads from the service listening on socket. A zero
// timeout uses [DefaultTimeout].
func NewOpenVaultProvider(socket string, timeout time.Duration) *OpenVaultProvider {
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	return &OpenVaultProvider{socket: socket, timeout: timeout}
}

// Name identifies this provider in diagnostics.
func (p *OpenVaultProvider) Name() string { return "openvault" }

// Socket is the socket this provider reads, for diagnostics.
func (p *OpenVaultProvider) Socket() string { return p.socket }

// Resolve is the machine-wide name, which OpenVault does not hold. It asks the
// service nothing, so a job that reads only machine-wide names never waits
// for it.
func (p *OpenVaultProvider) Resolve(ctx context.Context, name string) (Value, error) {
	return p.ResolveIn(ctx, nil, name)
}

// ResolveIn returns name under scope: a repository, then its environment.
func (p *OpenVaultProvider) ResolveIn(ctx context.Context, scope []string, name string) (Value, error) {
	if err := validName(name); err != nil {
		return nil, fmt.Errorf("openvault: %w", err)
	}
	collection, entry, err := OpenVaultEntry(scope, name)
	if err != nil {
		return nil, fmt.Errorf("openvault: %w", err)
	}

	typ, body, err := p.request(ctx, ovGet, ovStr(collection), ovStr(entry))
	if err != nil {
		return nil, err
	}
	defer zero(body)

	switch typ {
	case ovEntry:
		r := ovReader{b: body}
		switch found := r.u8(); {
		case r.err == nil && found == 0:
			return nil, fmt.Errorf("openvault: %s has no %s: %w", collection, entry, ErrNotFound)
		case found != 1:
			return nil, p.protocolError("get", "an entry that does not parse")
		}
		kind := r.u8()
		value := r.bytes()
		if r.err != nil || !r.done() {
			return nil, p.protocolError("get", "an entry that does not parse")
		}
		if kind != ovSecret && kind != ovVar {
			return nil, fmt.Errorf("openvault: %s/%s is a kind of entry a secret cannot be read from", collection, entry)
		}
		// Copied out because body is zeroed on return: the caller owns this
		// buffer and will zero it with the run.
		return append(Value(nil), value...), nil
	case ovError:
		refused := p.serviceError("get", body)
		// The service says "no collection R" in prose. Asking which
		// collections exist tells that apart without parsing it: a machine
		// that was never scoped to R is a store without the name, not a
		// broken one.
		if names, cerr := p.Collections(ctx); cerr == nil && !slices.Contains(names, collection) {
			return nil, fmt.Errorf("openvault: %s: %w", collection, ErrNoCollection)
		}
		return nil, refused
	}
	return nil, p.protocolError("get", fmt.Sprintf("message type 0x%02x", typ))
}

// Where is the entry ResolveIn reads for scope and name, as OpenVault's CLI
// names it: collection/entry. It is a name, so it is safe to print.
func (p *OpenVaultProvider) Where(scope []string, name string) string {
	collection, entry, err := OpenVaultEntry(scope, name)
	if err != nil {
		return ""
	}
	return collection + "/" + entry
}

// Collections lists the collections this machine holds, by name.
func (p *OpenVaultProvider) Collections(ctx context.Context) ([]string, error) {
	typ, body, err := p.request(ctx, ovCollections)
	if err != nil {
		return nil, err
	}
	switch typ {
	case ovNames:
		r := ovReader{b: body}
		n := r.u32()
		var out []string
		for i := uint32(0); i < n && r.err == nil; i++ {
			out = append(out, r.str())
		}
		if r.err != nil || !r.done() {
			return nil, p.protocolError("collections", "a list of names that does not parse")
		}
		return out, nil
	case ovError:
		return nil, p.serviceError("collections", body)
	}
	return nil, p.protocolError("collections", fmt.Sprintf("message type 0x%02x", typ))
}

// Names lists what OpenVault holds for repos, names only, as the scoped names
// a run's lookup builds: entry E/NAME of collection R as R__E__NAME, entry
// NAME as R__NAME. Absent are the repositories this machine has no collection
// for, sorted. Nothing is decrypted: OpenVault lists from its index.
//
// An entry deeper than one folder, a file, or one whose last part is not a
// secret's name is left out, because no lookup could reach it.
func (p *OpenVaultProvider) Names(ctx context.Context, repos []string) (names, absent []string, err error) {
	have, err := p.Collections(ctx)
	if err != nil {
		return nil, nil, err
	}
	for _, repo := range slices.Sorted(slices.Values(repos)) {
		if !slices.Contains(have, repo) {
			absent = append(absent, repo)
			continue
		}
		entries, err := p.list(ctx, repo)
		if err != nil {
			return nil, nil, err
		}
		for _, e := range entries {
			if n, ok := scopedEntryName(repo, e); ok {
				names = append(names, n)
			}
		}
	}
	slices.Sort(names)
	return slices.Compact(names), absent, nil
}

func scopedEntryName(repo, entry string) (string, bool) {
	env, name, nested := strings.Cut(entry, "/")
	if !nested {
		env, name = "", entry
	}
	if strings.Contains(name, "/") || validName(name) != nil {
		return "", false
	}
	if env == "" {
		return ScopedName(name, repo), true
	}
	return ScopedName(name, repo, env), true
}

// list is the names of a collection's secrets and variables.
func (p *OpenVaultProvider) list(ctx context.Context, collection string) ([]string, error) {
	typ, body, err := p.request(ctx, ovList, ovStr(collection))
	if err != nil {
		return nil, err
	}
	switch typ {
	case ovListing:
		r := ovReader{b: body}
		n := r.u32()
		var out []string
		for i := uint32(0); i < n && r.err == nil; i++ {
			name, kind := r.str(), r.u8()
			if kind == ovSecret || kind == ovVar {
				out = append(out, name)
			}
		}
		if r.err != nil || !r.done() {
			return nil, p.protocolError("list", "a listing that does not parse")
		}
		return out, nil
	case ovError:
		return nil, p.serviceError("list", body)
	}
	return nil, p.protocolError("list", fmt.Sprintf("message type 0x%02x", typ))
}

// OpenVaultEntry is the collection and entry a scoped name reads as in
// OpenVault, or an [ErrNotFound] saying why it reads as none.
func OpenVaultEntry(scope []string, name string) (collection, entry string, err error) {
	switch len(scope) {
	case 0:
		return "", "", fmt.Errorf("a machine-wide name is in no collection: %w", ErrNotFound)
	case 1:
		entry = name
	case 2:
		env := scope[1]
		if env == "" || env == "." || env == ".." || strings.Contains(env, "/") ||
			strings.ContainsFunc(env, unicode.IsControl) {
			return "", "", fmt.Errorf("environment %q cannot be a folder of an entry: %w", env, ErrNotFound)
		}
		entry = env + "/" + name
	default:
		return "", "", fmt.Errorf("a scope of %d parts has no entry: %w", len(scope), ErrNotFound)
	}
	collection = scope[0]
	if !openVaultCollection.MatchString(collection) {
		return "", "", fmt.Errorf("repository %q cannot be a collection's name: %w", collection, ErrNotFound)
	}
	if len(entry) > openVaultMaxEntry {
		return "", "", fmt.Errorf("an entry name over %d bytes: %w", openVaultMaxEntry, ErrNotFound)
	}
	return collection, entry, nil
}

// request sends one request and reads its one response: a connection carries
// exactly that, and then closes (service.md, the socket). The body returned
// is the caller's to zero.
func (p *OpenVaultProvider) request(ctx context.Context, typ byte, fields ...[]byte) (byte, []byte, error) {
	op := ovOpName(typ)
	if err := checkOpenVaultSocket(p.socket); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return 0, nil, &OpenVaultError{Op: op, NotRunning: true, Socket: p.socket, Err: err}
		}
		return 0, nil, &OpenVaultError{Op: op, Socket: p.socket, Err: err}
	}

	ctx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", p.socket)
	if err != nil {
		// A socket with no service behind it is what a crashed service
		// leaves; the next one to start replaces it.
		notRunning := errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, fs.ErrNotExist)
		return 0, nil, &OpenVaultError{Op: op, NotRunning: notRunning, Socket: p.socket, Err: err}
	}
	defer func() { _ = conn.Close() }()
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}

	size := 1
	for _, f := range fields {
		size += len(f)
	}
	msg := make([]byte, 4, 4+size)
	binary.BigEndian.PutUint32(msg, uint32(size)) //nolint:gosec // size is a few short names
	msg = append(msg, typ)
	for _, f := range fields {
		msg = append(msg, f...)
	}
	if _, err := conn.Write(msg); err != nil {
		return 0, nil, p.ioError(ctx, op, err)
	}

	var head [4]byte
	if _, err := io.ReadFull(conn, head[:]); err != nil {
		return 0, nil, p.ioError(ctx, op, err)
	}
	n := binary.BigEndian.Uint32(head[:])
	if n == 0 || n > openVaultMaxFrame {
		return 0, nil, p.protocolError(op, fmt.Sprintf("a message of %d bytes", n))
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(conn, buf); err != nil {
		zero(buf)
		return 0, nil, p.ioError(ctx, op, err)
	}
	return buf[0], buf[1:], nil
}

func (p *OpenVaultProvider) ioError(ctx context.Context, op string, err error) error {
	switch {
	case ctx.Err() != nil:
		err = ctx.Err()
	case errors.Is(err, os.ErrDeadlineExceeded):
		// The connection's deadline is the context's, and on a loaded
		// machine it fires before the context's own timer says so.
		err = context.DeadlineExceeded
	}
	return &OpenVaultError{Op: op, Socket: p.socket, Err: err}
}

func (p *OpenVaultProvider) protocolError(op, what string) error {
	return &OpenVaultError{Op: op, Socket: p.socket, Err: fmt.Errorf("the service answered with %s", what)}
}

// serviceError is the service's error response. Its message is kept out of
// Error(), as a program's stderr is ([CommandError]).
func (p *OpenVaultProvider) serviceError(op string, body []byte) error {
	r := ovReader{b: body}
	code := r.u8()
	msg := r.str()
	if r.err != nil {
		msg = ""
	}
	return &OpenVaultError{Op: op, Refused: true, Code: code, Socket: p.socket, Message: msg}
}

func ovOpName(typ byte) string {
	switch typ {
	case ovCollections:
		return "collections"
	case ovGet:
		return "get"
	case ovList:
		return "list"
	}
	return fmt.Sprintf("0x%02x", typ)
}

// OpenVaultError is an OpenVault service that gave no answer: not running,
// refusing the request, or saying something this package cannot read. It is
// always E_SECRET_BACKEND.
type OpenVaultError struct {
	// Op is the request: "get", "collections" or "list".
	Op string
	// NotRunning is nothing listening on the socket.
	NotRunning bool
	// Refused is the service's own error response, with its Code.
	Refused bool
	Code    byte
	// Socket is the path asked. It is a local path, so it is in
	// [OpenVaultError.Diagnostic] and not in Error(), which can reach a
	// broker.
	Socket string
	// Message is the service's own words, for a local terminal only: it is
	// another program's text, which this package cannot vouch for (INV-4).
	Message string
	// Err is the underlying cause.
	Err error
}

func (e *OpenVaultError) Error() string {
	switch {
	case e.NotRunning:
		return "openvault: the service is not running; `openvault service install` keeps it running, or start `openvault service`"
	case e.Refused:
		return fmt.Sprintf("openvault: the service refused %s (error 0x%02x)", e.Op, e.Code)
	case errors.Is(e.Err, context.DeadlineExceeded):
		return fmt.Sprintf("openvault: the service did not answer %s within the timeout", e.Op)
	}
	return fmt.Sprintf("openvault: %s: %v", e.Op, withoutPath(e.Err))
}

// withoutPath is err without the path an *fs.PathError or a *net.OpError
// names: the socket is in [OpenVaultError.Diagnostic], for this terminal.
func withoutPath(err error) error {
	var pe *fs.PathError
	if errors.As(err, &pe) {
		return pe.Err
	}
	var oe *net.OpError
	if errors.As(err, &oe) {
		return oe.Err
	}
	return err
}

func (e *OpenVaultError) Unwrap() error { return e.Err }

// Diagnostic is what a local terminal may show: the socket, and the service's
// own message when it sent one.
func (e *OpenVaultError) Diagnostic() string {
	if e.Message != "" {
		return fmt.Sprintf("%s: %s", e.Socket, e.Message)
	}
	return e.Socket
}

func ovStr(s string) []byte {
	b := make([]byte, 2, 2+len(s))
	binary.BigEndian.PutUint16(b, uint16(len(s))) //nolint:gosec // names are bounded far below 64 KiB
	return append(b, s...)
}

// ovReader reads the service's fields, and stops at the first one that runs
// past the message.
type ovReader struct {
	b   []byte
	err error
}

func (r *ovReader) take(n int) []byte {
	if r.err != nil {
		return nil
	}
	if n < 0 || n > len(r.b) {
		r.err = io.ErrUnexpectedEOF
		return nil
	}
	out := r.b[:n]
	r.b = r.b[n:]
	return out
}

func (r *ovReader) u8() byte {
	if b := r.take(1); b != nil {
		return b[0]
	}
	return 0
}

func (r *ovReader) u32() uint32 {
	if b := r.take(4); b != nil {
		return binary.BigEndian.Uint32(b)
	}
	return 0
}

func (r *ovReader) str() string {
	b := r.take(2)
	if b == nil {
		return ""
	}
	return string(r.take(int(binary.BigEndian.Uint16(b))))
}

// bytes is a value field. The slice aliases the message, which the caller
// zeroes.
func (r *ovReader) bytes() []byte {
	b := r.take(4)
	if b == nil {
		return nil
	}
	return r.take(int(binary.BigEndian.Uint32(b)))
}

func (r *ovReader) done() bool { return r.err == nil && len(r.b) == 0 }
