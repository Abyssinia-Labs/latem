package main

import (
	"errors"
	"fmt"
	"io"
	neturl "net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/spf13/cobra"

	"github.com/Abyssinia-Labs/latem/internal/broker/remote"
	"github.com/Abyssinia-Labs/latem/internal/config"
)

func newRegisterCommand() *cobra.Command {
	var (
		url       string
		token     string
		tokenFile string
		name      string
		labels    []string
	)
	cmd := &cobra.Command{
		Use:   "register --url <wss://…> --token <token>",
		Short: "Register this machine with a control plane",
		Long: `Register this machine with a control plane.

Writes [broker.remote] into config.toml, stores the machine token in
~/.latem/agent.key at mode 0600, verifies connectivity, and records the
machine id the control plane assigns.

The token goes in its own file and never inline in config.toml, because
config.toml is the file that gets pasted into an issue when something breaks
(RUNNER_SPEC §8).

--token reads the value from the command line, where it lands in shell
history and in the process table of every other user on the machine. Prefer
--token-file, or pipe it in: --token-file - reads stdin.

A machine that never registers is a fully supported state: the local broker
works without a control plane (§7.3), which is what makes ` + "`latem run`" + ` useful
on a laptop.

Exit codes:
  0  registered and the connection verified
  1  the control plane refused, or the token or config could not be written
  2  usage`,
		Args: noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			switch {
			case url == "":
				return usageErrorf("--url is required")
			case token == "" && tokenFile == "":
				return usageErrorf("one of --token or --token-file is required")
			case token != "" && tokenFile != "":
				return usageErrorf("--token and --token-file are mutually exclusive")
			}
			normalized, err := normalizeAgentURL(url)
			if err != nil {
				return err
			}
			return runRegister(cmd, registerFlags{
				url:       normalized,
				token:     token,
				tokenFile: tokenFile,
				name:      name,
				labels:    labels,
			})
		},
	}
	cmd.Flags().StringVar(&url, "url", "", "control plane agent endpoint, e.g. wss://api.example.com/agent")
	cmd.Flags().StringVar(&token, "token", "", "machine token (visible in shell history; prefer --token-file)")
	cmd.Flags().StringVar(&tokenFile, "token-file", "", "read the machine token from this file, or - for stdin")
	cmd.Flags().StringVar(&name, "name", "", "display name for this machine (default: the hostname)")
	cmd.Flags().StringArrayVar(&labels, "label", nil, "machine label; repeatable")
	return cmd
}

type registerFlags struct {
	url       string
	token     string
	tokenFile string
	name      string
	labels    []string
}

// runRegister proves the control plane before it changes anything.
//
// The order is the whole design of this command, and the TODO it replaces said
// so: a config.toml naming a control plane that rejected us is worse than no
// config at all, because the agent would then spend its life reconnecting to
// somewhere it is not welcome and the operator would have no obvious thing to
// undo. So: read the token, prove it, then write the token, then write the
// config — each step only once the previous one has succeeded.
//
// Nothing in this function prints the token, and nothing puts it in an error.
func runRegister(cmd *cobra.Command, f registerFlags) error {
	out := cmd.OutOrStdout()

	tok, err := readRegistrationToken(cmd.InOrStdin(), f)
	if err != nil {
		return err
	}

	dir := configDir()
	paths := config.DerivePaths(dir)

	// The machine may already have an id from an earlier registration. Sending
	// it means a machine that is re-registering says who it was, rather than
	// arriving as a stranger and being issued a second identity for the same
	// hardware.
	existingID, existingName := "", ""
	if cfg, lerr := loadConfig(); lerr == nil {
		existingID, existingName = cfg.Machine.ID, cfg.Machine.Name
	}
	if f.name == "" && existingName == "" {
		f.name = defaultMachineName()
	}

	printf(out, "verifying %s …\n", f.url)
	reg, err := remote.Verify(cmd.Context(), remote.VerifyOptions{
		URL:       f.url,
		Token:     tok,
		MachineID: existingID,
	})
	if err != nil {
		if errors.Is(err, remote.ErrRevoked) {
			return fmt.Errorf("%w\n\nnothing was written. check the token, or ask for a new one", err)
		}
		return fmt.Errorf("%w\n\nnothing was written. check the url and that this machine can reach it", err)
	}
	printf(out, "  connection verified\n")
	if reg.Message != "" {
		// The control plane's own words, printed verbatim and interpolated into
		// nothing. It is display text from outside the trust boundary.
		printf(out, "  %s\n", reg.Message)
	}

	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("creating %s: %w", dir, err)
	}
	if err := remote.WriteToken(paths.TokenFile, tok); err != nil {
		return err
	}
	printf(out, "  machine token written to %s (mode 0600)\n", paths.TokenFile)

	edit := registration{
		URL:       f.url,
		TokenFile: paths.TokenFile,
		MachineID: reg.MachineID,
		Name:      f.name,
		Labels:    f.labels,
	}
	warning, err := writeRegistration(paths, edit)
	if err != nil {
		return err
	}
	printf(out, "  [broker.remote] written to %s\n", paths.File)

	printf(out, "\nregistered.\n")
	if warning != "" {
		printf(out, "\n%s\n", warning)
	}
	if reg.MachineID != "" {
		printf(out, "  machine id  %s\n", reg.MachineID)
	} else {
		// Saying so is better than leaving a blank line. A control plane that
		// assigns no id is conformant — nothing in pkg/protocol carries one —
		// and an operator who expected one should know it did not arrive.
		printf(out, "  machine id  (the control plane did not assign one)\n")
	}
	printf(out, "  url         %s\n", f.url)
	printf(out, "\nrestart the agent to connect:\n  latem service restart\nor, if it is not installed as a service:\n  latem agent\n")
	return nil
}

// readRegistrationToken resolves --token, --token-file, or stdin.
func readRegistrationToken(stdin io.Reader, f registerFlags) (string, error) {
	if f.token != "" {
		return strings.TrimSpace(f.token), nil
	}
	if f.tokenFile == "-" {
		data, err := io.ReadAll(io.LimitReader(stdin, remote.MaxTokenBytes+1))
		if err != nil {
			return "", fmt.Errorf("reading the machine token from stdin: %w", err)
		}
		tok := strings.TrimSpace(string(data))
		if tok == "" {
			return "", errors.New("no machine token arrived on stdin")
		}
		return tok, nil
	}
	// ReadToken rather than os.ReadFile: the same mode, size, and shape rules
	// the agent will apply on every dial, applied now, while there is a person
	// at a prompt who can fix it.
	return remote.ReadToken(f.tokenFile)
}

// normalizeAgentURL turns what a person pastes into what the config accepts.
//
// A control plane's dashboard shows an https:// address, so that is what gets
// copied, and `wss://` is the spelling of the same endpoint that
// internal/config will store. Rewriting it here rather than refusing means the
// obvious paste works; the scheme's *security* content is unchanged, because
// https and wss are the same transport and the config loader still rejects a
// plaintext scheme to a non-loopback host.
//
// Anything else is a usage error rather than a silent guess: `ssh://` or a bare
// hostname is a mistake worth stopping on, not one worth interpreting.
func normalizeAgentURL(raw string) (string, error) {
	u, err := neturl.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "", usageErrorf("--url %q is not a URL: %v", raw, err)
	}
	switch u.Scheme {
	case "wss", "ws":
	case "https":
		u.Scheme = "wss"
	case "http":
		u.Scheme = "ws"
	case "":
		return "", usageErrorf("--url %q has no scheme; it should look like wss://api.example.com/agent", raw)
	default:
		return "", usageErrorf("--url %q uses scheme %q; the remote broker is a WebSocket client and needs wss://", raw, u.Scheme)
	}
	if u.Host == "" {
		return "", usageErrorf("--url %q has no host; it should look like wss://api.example.com/agent", raw)
	}
	return u.String(), nil
}

// machineNamePattern is internal/config's rule for `[machine] name`, restated
// because the loader exports no way to ask.
//
// It is restated in order to *decline* rather than to validate: a hostname like
// "Sam's MacBook" is perfectly ordinary and is not a legal machine name, and
// writing it would make the config this command just proved refuse to load. An
// unnamed machine is a cosmetic loss; an unloadable config is not.
var machineNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9 ._-]{0,63}$`)

// defaultMachineName is the hostname, when the hostname is spellable.
func defaultMachineName() string {
	host, err := os.Hostname()
	if err != nil {
		return ""
	}
	// Bonjour appends .local to everything on macOS, and it is noise in a UI.
	host = strings.TrimSuffix(host, ".local")
	if !machineNamePattern.MatchString(host) {
		return ""
	}
	return host
}

// registration is the set of config keys `latem register` owns.
type registration struct {
	URL       string
	TokenFile string
	MachineID string
	Name      string
	Labels    []string
}

// writeRegistration edits config.toml in place, atomically.
//
// It returns a warning to show the operator, which is empty on the ordinary
// path. An error means nothing was written.
//
// # Why this edits text rather than re-encoding the file
//
// config.toml is a file people write by hand: it has their comments in it,
// their ordering, and their formatting. Decoding it into a struct and encoding
// it back would silently destroy all three, and the first anyone would know is
// the next time they went looking for the note they left themselves about why
// a runner has a strange label. So this replaces the one table it owns and
// leaves every byte outside it alone.
//
// # The rule the safety net enforces
//
// Not "the result must load" — "this command must never make a loadable config
// unloadable". Those differ in exactly the case that matters most, which is the
// first one anybody meets: a machine with no config.toml at all. What register
// can write there is necessarily incomplete, because §8 requires a [[runner]]
// block and this command has no way to know what this machine can build
// (`latem setup` runs `init` first for exactly this reason). Refusing to write anything would leave a new user with a
// verified token, a proven URL, and nothing on disk to show for it.
//
// So the gate is conditional on the original: if it loaded, the result must
// load too, and a failure leaves the file untouched and prints the block to
// paste in by hand. If it did not load — or did not exist — the edit cannot
// have broken anything, and it is installed with a warning saying what is still
// missing.
func writeRegistration(paths config.Paths, r registration) (warning string, err error) {
	original, err := os.ReadFile(paths.File)
	switch {
	case err == nil:
	case os.IsNotExist(err):
		original = nil
	default:
		return "", fmt.Errorf("reading %s: %w", paths.File, err)
	}

	updated, err := applyRegistration(string(original), r)
	if err != nil {
		return "", err
	}

	_, loadedBefore := config.Parse(original, config.Options{Dir: paths.Dir})
	_, loadsAfter := config.Parse([]byte(updated), config.Options{Dir: paths.Dir})

	switch {
	case loadsAfter == nil:
	case loadedBefore == nil:
		return "", fmt.Errorf(
			"the updated config would not load, so %s was left as it was: %w\n\nadd this by hand instead:\n\n%s",
			paths.File, loadsAfter, remoteTable(r))
	default:
		warning = fmt.Sprintf(
			"%s still does not load, and registering did not change that:\n  %v\n\n`latem doctor` names every fix.",
			paths.File, loadsAfter)
	}

	if err := writeFileAtomic(paths.File, []byte(updated), 0o600); err != nil {
		return "", err
	}
	return warning, nil
}

// applyRegistration returns config.toml with this registration in it.
func applyRegistration(original string, r registration) (string, error) {
	if strings.TrimSpace(original) == "" {
		return newConfigFile(r), nil
	}
	body := replaceTable(original, "broker.remote", remoteTable(r))
	body, err := applyMachineKeys(body, r)
	if err != nil {
		return "", err
	}
	return body, nil
}

// newConfigFile is what a machine with no config.toml gets.
//
// It is deliberately incomplete: there is no [[runner]] block, because this
// command has no way to know what this machine can build, and inventing one
// would produce an agent that accepts work it cannot do. `latem doctor` names
// the gap in one line.
func newConfigFile(r registration) string {
	var b strings.Builder
	fmt.Fprintf(&b, "schema_version = %d\n\n", config.SchemaVersion)
	b.WriteString(machineTable(r))
	b.WriteString("\n[broker.local]\nenabled = true\n\n")
	b.WriteString(remoteTable(r))
	b.WriteString("\n# This machine has no [[runner]] block yet, so it can accept nothing.\n" +
		"# `latem doctor` names what is missing; RUNNER_SPEC §8 has the schema.\n")
	return b.String()
}

func machineTable(r registration) string {
	var b strings.Builder
	b.WriteString("[machine]\n")
	if r.Name != "" {
		fmt.Fprintf(&b, "name   = %q\n", r.Name)
	}
	if r.MachineID != "" {
		fmt.Fprintf(&b, "id     = %q\n", r.MachineID)
	}
	if len(r.Labels) > 0 {
		fmt.Fprintf(&b, "labels = %s\n", tomlStrings(r.Labels))
	}
	return b.String()
}

func remoteTable(r registration) string {
	return fmt.Sprintf("[broker.remote]\nenabled    = true\nurl        = %q\ntoken_file = %q\n",
		r.URL, r.TokenFile)
}

// applyMachineKeys sets the three [machine] keys this command owns, creating
// the table if it is absent and leaving every other key in it alone.
func applyMachineKeys(body string, r registration) (string, error) {
	set := map[string]string{}
	if r.Name != "" {
		set["name"] = fmt.Sprintf("%q", r.Name)
	}
	if r.MachineID != "" {
		set["id"] = fmt.Sprintf("%q", r.MachineID)
	}
	if len(r.Labels) > 0 {
		set["labels"] = tomlStrings(r.Labels)
	}
	if len(set) == 0 {
		return body, nil
	}

	start, end, found := findTable(body, "machine")
	if !found {
		// No [machine] table. Append one rather than guessing where it belongs:
		// TOML is order-independent and a table appended at the end means the
		// same thing as one at the top.
		return strings.TrimRight(body, "\n") + "\n\n" + machineTable(r), nil
	}

	lines := strings.Split(body[start:end], "\n")
	remaining := map[string]bool{}
	for k := range set {
		remaining[k] = true
	}
	for i, line := range lines {
		key := tomlKeyOf(line)
		if key == "" || set[key] == "" {
			continue
		}
		lines[i] = fmt.Sprintf("%s = %s", key, set[key])
		delete(remaining, key)
	}

	var added []string
	for _, k := range []string{"name", "id", "labels"} {
		if remaining[k] {
			added = append(added, fmt.Sprintf("%s = %s", k, set[k]))
		}
	}
	section := strings.TrimRight(strings.Join(lines, "\n"), "\n")
	if len(added) > 0 {
		section += "\n" + strings.Join(added, "\n")
	}
	return body[:start] + section + "\n\n" + body[end:], nil
}

// replaceTable swaps the body of a named table, or appends it if absent.
func replaceTable(body, name, replacement string) string {
	start, end, found := findTable(body, name)
	if !found {
		return strings.TrimRight(body, "\n") + "\n\n" + replacement
	}
	return body[:start] + replacement + "\n" + body[end:]
}

// findTable locates `[name]` and the run of lines belonging to it.
//
// A table ends at the next header at the start of a line, which is TOML's own
// rule. Indented headers are ignored deliberately: §8's own examples indent
// `[runner.docker]` under its `[[runner]]`, and treating an indented header as
// a new top-level table would cut a runner's block in half.
func findTable(body, name string) (start, end int, found bool) {
	header := "[" + name + "]"
	lines := strings.Split(body, "\n")

	offset := 0
	startLine := -1
	for i, line := range lines {
		switch {
		case startLine < 0 && strings.TrimSpace(line) == header:
			startLine = i
			start = offset
		case startLine >= 0 && strings.HasPrefix(line, "["):
			return start, offset, true
		}
		offset += len(line) + 1
	}
	if startLine < 0 {
		return 0, 0, false
	}
	return start, len(body), true
}

// tomlKeyOf returns the key a line assigns, or "".
func tomlKeyOf(line string) string {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" || strings.HasPrefix(trimmed, "#") {
		return ""
	}
	eq := strings.Index(trimmed, "=")
	if eq <= 0 {
		return ""
	}
	key := strings.TrimSpace(trimmed[:eq])
	if strings.ContainsAny(key, " \t\"'[") {
		return ""
	}
	return key
}

func tomlStrings(values []string) string {
	quoted := make([]string, 0, len(values))
	for _, v := range values {
		quoted = append(quoted, fmt.Sprintf("%q", v))
	}
	return "[" + strings.Join(quoted, ", ") + "]"
}

// writeFileAtomic replaces a file through a temporary and a rename, at an
// explicit mode.
//
// config.toml holds no secret, but it is the file the agent refuses to start
// without, and a machine that lost power during `latem register` should come
// back with the config it had rather than half of a new one.
func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".config.toml-*")
	if err != nil {
		return fmt.Errorf("creating a temporary file in %s: %w", dir, err)
	}
	name := tmp.Name()
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(name) // a no-op once the rename has succeeded
	}()

	if err := tmp.Chmod(mode); err != nil {
		return fmt.Errorf("setting the mode on %s: %w", path, err)
	}
	if _, err := tmp.Write(data); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("flushing %s: %w", path, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("closing %s: %w", path, err)
	}
	if err := os.Rename(name, path); err != nil {
		return fmt.Errorf("installing %s: %w", path, err)
	}
	return nil
}
