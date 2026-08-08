package native

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

// fakeRunner answers the host probes without a host. The two conditions §13.3
// and §23 call the most common install failures cannot be produced on a working
// machine, so without this every branch that matters would be reachable only by
// hand on somebody's broken Mac.
type fakeRunner struct {
	out  map[string][]byte // keyed by argv[1], the subcommand
	err  map[string]error
	seen [][]string
}

func (f *fakeRunner) Run(_ context.Context, argv []string) ([]byte, error) {
	f.seen = append(f.seen, argv)
	key := ""
	if len(argv) > 1 {
		key = argv[1]
	}
	if err, ok := f.err[key]; ok {
		return nil, err
	}
	return f.out[key], nil
}

func managerRunner(name string) *fakeRunner {
	return &fakeRunner{out: map[string][]byte{"managername": []byte(name + "\n")}}
}

func TestDetectSession_ClassifiesEveryManager(t *testing.T) {
	tests := map[string]struct {
		manager string
		want    SessionKind
		signs   bool
	}{
		"gui login":      {"Aqua", SessionAqua, true},
		"no gui session": {"Background", SessionBackground, false},
		"ssh":            {"StandardIO", SessionStandardIO, false},
		"launch daemon":  {"System", SessionSystem, false},
		"unrecognised":   {"Nonesuch", SessionUnknown, false},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			got := DetectSession(t.Context(), managerRunner(tc.manager), 501)
			if got.Kind != tc.want {
				t.Errorf("Kind = %q, want %q", got.Kind, tc.want)
			}
			if got.CanSignCode() != tc.signs {
				t.Errorf("CanSignCode = %v, want %v", got.CanSignCode(), tc.signs)
			}
			if got.Explain() == "" {
				t.Error("Explain is empty; `latem doctor` has nothing to print")
			}
		})
	}
}

// Whatever launchd claims, a runner agent running as root is a LaunchDaemon.
// The check comes first because it needs no subprocess, and the daemon case is
// exactly where the machine is least likely to behave normally.
func TestDetectSession_RootIsAlwaysADaemon(t *testing.T) {
	r := managerRunner("Aqua")
	got := DetectSession(t.Context(), r, 0)

	if got.Kind != SessionSystem {
		t.Errorf("Kind = %q, want %q for uid 0", got.Kind, SessionSystem)
	}
	if len(r.seen) != 0 {
		t.Errorf("launchctl was run anyway: %v", r.seen)
	}
	if !strings.Contains(got.Explain(), "LaunchAgents") {
		t.Errorf("Explain = %q, want it to name the directory the plist belongs in", got.Explain())
	}
}

// A diagnostic that cannot run is not itself a reason to refuse work.
func TestDetectSession_ProbeFailureIsUnknownNotFatal(t *testing.T) {
	r := &fakeRunner{err: map[string]error{"managername": errors.New("launchctl: not found")}}
	got := DetectSession(t.Context(), r, 501)

	if got.Kind != SessionUnknown {
		t.Errorf("Kind = %q, want %q", got.Kind, SessionUnknown)
	}
	if got.Err == nil {
		t.Error("Err is nil; the reason the probe failed was thrown away")
	}
	if _, err := CheckSession(t.Context(), r, 501, false); err != nil {
		t.Errorf("CheckSession refused work because a diagnostic failed: %v", err)
	}
}

func TestCheckSession(t *testing.T) {
	tests := map[string]struct {
		manager    string
		uid        int
		requireGUI bool
		wantErr    bool
	}{
		"aqua passes":                   {"Aqua", 501, false, false},
		"aqua passes strict":            {"Aqua", 501, true, false},
		"daemon always fails":           {"System", 501, false, true},
		"root always fails":             {"Aqua", 0, false, true},
		"ssh is tolerated by default":   {"StandardIO", 501, false, false},
		"ssh fails a signing runner":    {"StandardIO", 501, true, true},
		"background fails a signer":     {"Background", 501, true, true},
		"unknown is tolerated":          {"Nonesuch", 501, false, false},
		"unknown fails a signer strict": {"Nonesuch", 501, true, true},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := CheckSession(t.Context(), managerRunner(tc.manager), tc.uid, tc.requireGUI)
			if tc.wantErr {
				if err == nil {
					t.Fatal("CheckSession accepted a context codesign cannot work in")
				}
				if got := CodeOf(err); got != protocol.ErrExecutor {
					t.Errorf("code = %q, want %q", got, protocol.ErrExecutor)
				}
				return
			}
			if err != nil {
				t.Fatalf("CheckSession: %v", err)
			}
		})
	}
}

func TestCheckKeychain(t *testing.T) {
	t.Run("unlocked", func(t *testing.T) {
		r := &fakeRunner{out: map[string][]byte{
			"show-keychain-info": []byte(`Keychain "login.keychain-db" no-timeout`),
		}}
		if err := CheckKeychain(t.Context(), r, ""); err != nil {
			t.Fatalf("CheckKeychain on an unlocked keychain: %v", err)
		}
		if len(r.seen) != 1 || r.seen[0][0] != "security" {
			t.Fatalf("argv = %v", r.seen)
		}
		if len(r.seen[0]) != 2 {
			t.Errorf("argv = %v; an empty keychain must mean the default one, with no path argument", r.seen[0])
		}
	})

	t.Run("named keychain becomes an argv element", func(t *testing.T) {
		r := &fakeRunner{}
		if err := CheckKeychain(t.Context(), r, "/Users/runner/Library/Keychains/build.keychain"); err != nil {
			t.Fatal(err)
		}
		want := []string{"security", "show-keychain-info", "/Users/runner/Library/Keychains/build.keychain"}
		if fmt.Sprint(r.seen[0]) != fmt.Sprint(want) {
			t.Errorf("argv = %v, want %v", r.seen[0], want)
		}
	})

	t.Run("locked", func(t *testing.T) {
		r := &fakeRunner{err: map[string]error{
			"show-keychain-info": &probeError{
				program: "security",
				stderr:  "SecKeychainCopySettings: User interaction is not allowed.",
				err:     errors.New("exit status 1"),
			},
		}}
		err := CheckKeychain(t.Context(), r, "")
		if err == nil {
			t.Fatal("a locked keychain was reported as fine")
		}
		if got := CodeOf(err); got != protocol.ErrExecutor {
			t.Errorf("code = %q, want %q", got, protocol.ErrExecutor)
		}
		if !strings.Contains(err.Error(), "unlock-keychain") {
			t.Errorf("error = %q, want it to name the command that fixes this", err)
		}
	})

	// The measured behaviour: `security show-keychain-info` on a locked
	// keychain raises a SecurityAgent dialog and blocks on it. "Did not answer"
	// is therefore the diagnosis, not an inconclusive probe.
	t.Run("no answer means a dialog nobody can reach", func(t *testing.T) {
		r := &fakeRunner{err: map[string]error{
			"show-keychain-info": fmt.Errorf("security: %w", context.DeadlineExceeded),
		}}
		err := CheckKeychain(t.Context(), r, "")
		if err == nil {
			t.Fatal("a probe that never answered was treated as success")
		}
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("error %v does not unwrap to DeadlineExceeded", err)
		}
		if !strings.Contains(err.Error(), "dialog") {
			t.Errorf("error = %q, want it to explain what the machine is waiting for", err)
		}
	})
}

func TestExecRunner_EmptyArgv(t *testing.T) {
	if _, err := (ExecRunner{}).Run(t.Context(), nil); err == nil {
		t.Error("an empty argv was accepted")
	}
}

func TestProbeError_Message(t *testing.T) {
	base := errors.New("exit status 1")
	withStderr := &probeError{program: "security", stderr: "User interaction is not allowed.", err: base}
	if !strings.Contains(withStderr.Error(), "User interaction") {
		t.Errorf("error = %q, want the tool's own diagnosis", withStderr)
	}
	if !errors.Is(withStderr, base) {
		t.Error("the underlying error was not preserved")
	}
	bare := &probeError{program: "launchctl", err: base}
	if strings.Contains(bare.Error(), ": : ") {
		t.Errorf("error = %q has an empty stderr field in it", bare)
	}
}

func TestFirstLine(t *testing.T) {
	cases := map[string]string{
		"":               "",
		"one":            "one",
		"one\ntwo":       "one",
		"  one\ntwo  \n": "one",
	}
	for in, want := range cases {
		if got := firstLine(in); got != want {
			t.Errorf("firstLine(%q) = %q, want %q", in, got, want)
		}
	}
}
