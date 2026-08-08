package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/Abyssinia-Labs/latem/internal/config"
	"github.com/Abyssinia-Labs/latem/internal/logstream"
	"github.com/Abyssinia-Labs/latem/internal/store"
	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

// newClientConfigDir writes a config directory the client commands can use.
//
// It is short because a Unix socket path is capped at 103 bytes by the kernel
// and the default temporary directory on macOS is longer than that on its own;
// config.Load rejects the overrun, so a test using t.TempDir() would fail on
// its own scratch space rather than on the command under test.
func newClientConfigDir(t *testing.T) string {
	t.Helper()
	home, err := os.MkdirTemp("/tmp", "latcli")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(home) })

	dir := filepath.Join(home, ".latem")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	body := `schema_version = 1

[machine]
name = "cli-test"

[agent]
metrics_addr = ""

[broker.local]
enabled = true

[repos.app]
url          = "https://git.example.invalid/app.git"
allowed_refs = ["main"]

[[runner]]
name        = "test"
executor    = "native"
labels      = ["test"]
concurrency = 1

  [runner.native]
  min_free_gb = 0.001
`
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Setenv("HOME", home)
	t.Setenv("LATEM_CONFIG_DIR", dir)
	t.Cleanup(func() { global.configDir = "" })
	global.configDir = ""
	return dir
}

// clientStore opens the same sqlite the commands use, for arranging rows.
func clientStore(t *testing.T, dir string) *store.DB {
	t.Helper()
	db, err := store.Open(context.Background(), filepath.Join(dir, "state.db"), logstream.NewRedactor())
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func seedCLIRun(t *testing.T, db *store.DB, id string, state protocol.RunState) {
	t.Helper()
	started := time.Now().Add(-time.Minute)
	r := store.Run{
		ID:         id,
		Repo:       "app",
		Job:        "test",
		Ref:        "main",
		State:      state,
		OriginKind: "cli",
		CreatedAt:  time.Now().Add(-2 * time.Minute),
		StartedAt:  &started,
	}
	if err := db.Create(context.Background(), r); err != nil {
		t.Fatal(err)
	}
}

// seedParkedCLIRun is a run parked by gate 8: awaiting approval, with the
// commit it would build already recorded.
func seedParkedCLIRun(t *testing.T, db *store.DB, id, sha string) {
	t.Helper()
	r := store.Run{
		ID:            id,
		Repo:          "app",
		Job:           "test",
		Ref:           "feature/x",
		RefNormalized: "refs/heads/feature/x",
		SHA:           sha,
		State:         protocol.StateAwaitingApproval,
		OriginKind:    "cli",
		CreatedAt:     time.Now().Add(-2 * time.Minute),
	}
	if err := db.Create(context.Background(), r); err != nil {
		t.Fatal(err)
	}
}

// execCommand runs one subcommand and returns stdout, stderr, and the error.
func execCommand(t *testing.T, args ...string) (string, string, error) {
	t.Helper()
	root := newRootCommand()
	var out, errBuf bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&errBuf)
	root.SetArgs(args)
	err := root.ExecuteContext(context.Background())
	return out.String(), errBuf.String(), err
}

// `latem run` with no agent listening queues the run and exits 0 (§7.3). The
// run id goes to stdout alone so it can be captured; the "nothing will start
// it" notice goes to stderr, where it cannot corrupt `id=$(latem run …)`.
func TestRunQueuesWithNoAgentAndExitsZero(t *testing.T) {
	dir := newClientConfigDir(t)

	stdout, stderr, err := execCommand(t, "run", "app", "test")
	if err != nil {
		t.Fatalf("latem run with no agent = %v, want exit 0 (§7.3)", err)
	}
	id := strings.TrimSpace(stdout)
	if err := protocol.ValidateID(id); err != nil {
		t.Fatalf("stdout %q is not a bare run id: %v", stdout, err)
	}
	if !strings.Contains(stderr, "agent not running") {
		t.Errorf("stderr should say nothing will start it yet:\n%s", stderr)
	}

	db := clientStore(t, dir)
	r, err := db.Get(context.Background(), id)
	if err != nil {
		t.Fatalf("the run was not durably queued: %v", err)
	}
	if r.State != protocol.StateQueued {
		t.Errorf("state = %s, want queued", r.State)
	}
	if r.Ref != DefaultRef {
		t.Errorf("ref = %q, want the documented default %q", r.Ref, DefaultRef)
	}
	if r.ExpiresAt == nil {
		t.Error("a queued run needs a queue_ttl deadline (§11)")
	}
}

// The other half of §7.3: --wait and --follow against a down agent are an
// error with exit 3, not a hang and not a queue write. Queueing a run that
// begins hours later, unwatched, is worse than refusing.
func TestRunWaitWithNoAgentExitsThree(t *testing.T) {
	dir := newClientConfigDir(t)

	for _, flag := range []string{"--wait", "--follow"} {
		t.Run(flag, func(t *testing.T) {
			stdout, _, err := execCommand(t, "run", "app", "test", flag)
			if err == nil {
				t.Fatalf("%s against a down agent exited 0", flag)
			}
			if got := exitCode(err); got != ExitAgentDown {
				t.Errorf("exit code = %d, want %d (E_AGENT_DOWN)", got, ExitAgentDown)
			}
			if !strings.Contains(err.Error(), string(protocol.ErrAgentDown)) {
				t.Errorf("the error should name the code a script greps for: %v", err)
			}
			if stdout != "" {
				t.Errorf("nothing should reach stdout when nothing was queued: %q", stdout)
			}
		})
	}

	db := clientStore(t, dir)
	rows, err := db.List(context.Background(), store.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Errorf("%d runs were queued by a command that reported E_AGENT_DOWN", len(rows))
	}
}

// `latem list` defaults to the runs something might still happen to, and the
// output is tab separated so it composes with cut and awk.
func TestListFiltersAndFormat(t *testing.T) {
	dir := newClientConfigDir(t)
	db := clientStore(t, dir)

	seedCLIRun(t, db, "r_queued", protocol.StateQueued)
	seedCLIRun(t, db, "r_running", protocol.StateRunning)
	seedCLIRun(t, db, "r_done", protocol.StateSucceeded)

	stdout, _, err := execCommand(t, "list")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stdout, "r_done") {
		t.Errorf("the default listing should exclude finished runs:\n%s", stdout)
	}
	for _, want := range []string{"r_queued", "r_running"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("the default listing is missing %s:\n%s", want, stdout)
		}
	}

	header := strings.SplitN(stdout, "\n", 2)[0]
	if fields := strings.Split(header, "\t"); len(fields) != 7 {
		t.Errorf("header has %d tab-separated fields, want 7: %q", len(fields), header)
	}

	all, _, err := execCommand(t, "list", "--all")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(all, "r_done") {
		t.Errorf("--all should include finished runs:\n%s", all)
	}

	one, _, err := execCommand(t, "list", "--state", "running")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(one, "r_queued") || !strings.Contains(one, "r_running") {
		t.Errorf("--state running returned the wrong set:\n%s", one)
	}
}

// An empty result must not look like a working filter. The rows go to stdout
// and the "nothing matched" line to stderr, so a pipeline sees nothing at all.
func TestListSaysSoWhenNothingMatches(t *testing.T) {
	newClientConfigDir(t)
	stdout, stderr, err := execCommand(t, "list")
	if err != nil {
		t.Fatal(err)
	}
	if stdout != "" {
		t.Errorf("stdout should be empty when there are no runs: %q", stdout)
	}
	if !strings.Contains(stderr, "no runs match") {
		t.Errorf("stderr should explain the empty result:\n%s", stderr)
	}
}

// `latem logs` reads the file the pipeline wrote, and a run nobody has heard
// of exits 1 rather than printing nothing and exiting 0.
func TestLogsReadsTheFileAndTail(t *testing.T) {
	dir := newClientConfigDir(t)
	db := clientStore(t, dir)
	seedCLIRun(t, db, "r_logs", protocol.StateSucceeded)

	logs := filepath.Join(dir, "logs")
	if err := os.MkdirAll(logs, 0o700); err != nil {
		t.Fatal(err)
	}
	var body strings.Builder
	for i := 1; i <= 5; i++ {
		fmt.Fprintf(&body, "line %d\n", i)
	}
	if err := os.WriteFile(filepath.Join(logs, "r_logs.log"), []byte(body.String()), 0o600); err != nil {
		t.Fatal(err)
	}

	stdout, _, err := execCommand(t, "logs", "r_logs")
	if err != nil {
		t.Fatal(err)
	}
	if stdout != body.String() {
		t.Errorf("logs printed:\n%s\nwant:\n%s", stdout, body.String())
	}

	tailed, _, err := execCommand(t, "logs", "r_logs", "--tail", "2")
	if err != nil {
		t.Fatal(err)
	}
	if tailed != "line 4\nline 5\n" {
		t.Errorf("--tail 2 printed %q", tailed)
	}

	_, _, err = execCommand(t, "logs", "r_nosuchrun")
	if err == nil {
		t.Fatal("logs for an unknown run exited 0")
	}
	if got := exitCode(err); got != ExitFailure {
		t.Errorf("exit code = %d, want %d", got, ExitFailure)
	}
}

// A run that exists but has no log yet is exit 1 with an explanation, not an
// empty stdout and exit 0 — the two look identical to a script.
func TestLogsForARunWithNoLogYet(t *testing.T) {
	dir := newClientConfigDir(t)
	db := clientStore(t, dir)
	seedCLIRun(t, db, "r_nolog", protocol.StateQueued)

	_, _, err := execCommand(t, "logs", "r_nolog")
	if err == nil {
		t.Fatal("logs for a run with no output exited 0")
	}
	if !strings.Contains(err.Error(), "not started yet") {
		t.Errorf("the failure should say why there is nothing: %v", err)
	}
	if got := exitCode(err); got != ExitFailure {
		t.Errorf("exit code = %d, want %d", got, ExitFailure)
	}
}

// `latem logs -f` on a queued run must wait for the log to appear rather than
// giving up: on a busy machine that is the ordinary case, and it is exactly
// the run somebody wants to watch. With no agent listening it is exit 3.
func TestLogsFollowOnAQueuedRunNeedsAnAgent(t *testing.T) {
	dir := newClientConfigDir(t)
	db := clientStore(t, dir)
	seedCLIRun(t, db, "r_pending", protocol.StateQueued)

	_, _, err := execCommand(t, "logs", "r_pending", "-f")
	if err == nil {
		t.Fatal("-f against a down agent exited 0")
	}
	if got := exitCode(err); got != ExitAgentDown {
		t.Errorf("exit code = %d, want %d (E_AGENT_DOWN)", got, ExitAgentDown)
	}
	// The point of the fix: it must not be the "no log" failure, which would
	// mean -f gave up on a run that had simply not started.
	if strings.Contains(err.Error(), "not started yet") {
		t.Errorf("-f treated a missing log as fatal: %v", err)
	}
}

// A log gzipped by the daily sweep is the same log. A client that only knew
// about `.log` would report "no log" for every build older than a day, which
// is the answer it also gives for a run that never started.
func TestLogsReadsAGzippedLog(t *testing.T) {
	dir := newClientConfigDir(t)
	db := clientStore(t, dir)
	seedCLIRun(t, db, "r_gz", protocol.StateSucceeded)

	logs := filepath.Join(dir, "logs")
	if err := os.MkdirAll(logs, 0o700); err != nil {
		t.Fatal(err)
	}
	plain := filepath.Join(logs, "r_gz.log")
	if err := os.WriteFile(plain, []byte("compressed output\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Age it past the gzip threshold and let the real sweep compress it, so
	// this test cannot drift from the naming logstream actually uses.
	old := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(plain, old, old); err != nil {
		t.Fatal(err)
	}
	if _, err := logstream.Prune(logs, logstream.PruneOptions{Retention: 30 * 24 * time.Hour}, time.Now(), nil); err != nil {
		t.Fatal(err)
	}

	stdout, _, err := execCommand(t, "logs", "r_gz")
	if err != nil {
		t.Fatalf("reading a gzipped log: %v", err)
	}
	if stdout != "compressed output\n" {
		t.Errorf("got %q", stdout)
	}
}

// Cancelling a queued run works with no agent, because nothing but the queue
// is holding it. An unknown id is exit 1 — the CLI's caller is a person who
// mistyped, not a broker that must learn nothing.
func TestCancelAQueuedRunWithNoAgent(t *testing.T) {
	dir := newClientConfigDir(t)
	db := clientStore(t, dir)
	seedCLIRun(t, db, "r_cancel", protocol.StateQueued)

	if _, _, err := execCommand(t, "cancel", "r_cancel", "--reason", "wrong branch"); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	r, err := db.Get(context.Background(), "r_cancel")
	if err != nil {
		t.Fatal(err)
	}
	if r.State != protocol.StateCancelled {
		t.Errorf("state = %s, want cancelled", r.State)
	}
	if r.CancelReason == "" {
		t.Error("the reason should be recorded; it exists so a stopped build says why")
	}

	_, _, err = execCommand(t, "cancel", "r_nosuchrun")
	if err == nil || exitCode(err) != ExitFailure {
		t.Errorf("cancelling an unknown run = %v, want exit 1", err)
	}
}

// `latem approve` is the only local-only verb (INV-5, INV-2). It works with no
// agent running, because approval is a store transition rather than a message.
//
// What it approves is the commit the run was parked at, and it says so: an
// operator whose next question is "which commit did I just authorise?" should
// not have to run a second command to find out (amendment A27).
func TestApproveMovesAParkedRunToQueued(t *testing.T) {
	const sha = "0123456789abcdef0123456789abcdef01234567"

	dir := newClientConfigDir(t)
	db := clientStore(t, dir)
	seedParkedCLIRun(t, db, "r_parked", sha)

	_, stderr, err := execCommand(t, "approve", "r_parked")
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	if !strings.Contains(stderr, "queued") {
		t.Errorf("approve should say what it did:\n%s", stderr)
	}
	if !strings.Contains(stderr, sha[:12]) {
		t.Errorf("approve should name the commit it pinned:\n%s", stderr)
	}

	r, err := db.Get(context.Background(), "r_parked")
	if err != nil {
		t.Fatal(err)
	}
	if r.State != protocol.StateQueued {
		t.Errorf("state = %s, want queued", r.State)
	}
	if r.ApprovedSHA != sha {
		t.Errorf("approved_sha = %q, want %q", r.ApprovedSHA, sha)
	}

	seedCLIRun(t, db, "r_running", protocol.StateRunning)
	if _, _, err := execCommand(t, "approve", "r_running"); err == nil {
		t.Error("approving a run that is not parked should fail")
	}

	// Approving twice is something a person does. The second one succeeds and
	// repeats the commit that is pinned, rather than reading as an error or as
	// a fresh decision.
	_, stderr, err = execCommand(t, "approve", "r_parked")
	if err != nil {
		t.Fatalf("second approve: %v", err)
	}
	if !strings.Contains(stderr, sha[:12]) {
		t.Errorf("a second approve should still name the pinned commit:\n%s", stderr)
	}

	// An ordinary queued run was never waiting on anybody, and no commit was
	// approved. Printing "approved ... at " with nothing after it would
	// misrepresent the one fact this command exists to establish.
	seedCLIRun(t, db, "r_plain", protocol.StateQueued)
	_, stderr, err = execCommand(t, "approve", "r_plain")
	if err != nil {
		t.Fatalf("approving an already-queued run: %v", err)
	}
	if !strings.Contains(stderr, "already queued") {
		t.Errorf("approving a queued run should say so:\n%s", stderr)
	}

	// A run with no resolved commit has nothing to pin, and the message has to
	// say so rather than reading as a database error.
	seedCLIRun(t, db, "r_nocommit", protocol.StateAwaitingApproval)
	_, _, err = execCommand(t, "approve", "r_nocommit")
	if err == nil {
		t.Fatal("approving a run with no commit succeeded")
	}
	if !strings.Contains(err.Error(), "no resolved commit") {
		t.Errorf("error should explain what is missing: %v", err)
	}
	if _, _, err := execCommand(t, "approve", "r_nosuchrun"); exitCode(err) != ExitFailure {
		t.Errorf("approving an unknown run = %v, want exit 1", err)
	}
}

func TestListFilterStates(t *testing.T) {
	tests := []struct {
		name string
		f    listFilter
		want int
	}{
		{"default is the non-terminal set", listFilter{}, 6},
		{"--all is unfiltered", listFilter{all: true}, 0},
		{"an explicit state wins over --all", listFilter{state: protocol.StateFailed, all: true}, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := len(tt.f.states()); got != tt.want {
				t.Errorf("states() has %d entries, want %d", got, tt.want)
			}
		})
	}
}

func TestRefOfPrefersTheNormalizedName(t *testing.T) {
	// The normalized name is what policy matched and what the SHA came from
	// (INV-5); the raw one is all a run rejected before gate 7 ever had.
	if got := refOf(store.Run{Ref: "main", RefNormalized: "refs/heads/main"}); got != "refs/heads/main" {
		t.Errorf("refOf = %q", got)
	}
	if got := refOf(store.Run{Ref: "main"}); got != "main" {
		t.Errorf("refOf = %q", got)
	}
}

func TestLastLines(t *testing.T) {
	const body = "a\nb\nc\n"
	tests := []struct {
		n    int
		want string
	}{
		{0, body},
		{-1, body},
		{2, "b\nc\n"},
		{10, body},
	}
	for _, tt := range tests {
		if got := lastLines(body, tt.n); got != tt.want {
			t.Errorf("lastLines(%d) = %q, want %q", tt.n, got, tt.want)
		}
	}
}

func TestTerminalStatusExitCodes(t *testing.T) {
	cmd := &cobra.Command{}
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})

	started := time.Now().Add(-time.Minute)
	finished := time.Now()

	if err := terminalStatus(cmd, store.Run{
		ID: "r_ok", State: protocol.StateSucceeded, StartedAt: &started, FinishedAt: &finished,
	}); err != nil {
		t.Errorf("a succeeded run should exit 0: %v", err)
	}

	// Cancelled is not a crash and it is not a build either. A script that
	// treats "somebody pressed Stop" as success deploys something nobody built.
	for _, state := range []protocol.RunState{
		protocol.StateFailed, protocol.StateCancelled,
		protocol.StateRejected, protocol.StateTimedOut, protocol.StateInterrupted,
	} {
		err := terminalStatus(cmd, store.Run{ID: "r_x", State: state})
		if err == nil {
			t.Errorf("%s exited 0", state)
			continue
		}
		if got := exitCode(err); got != ExitFailure {
			t.Errorf("%s exit code = %d, want %d", state, got, ExitFailure)
		}
	}

	// A retryable code should say so: the machine failed, not the commit.
	err := terminalStatus(cmd, store.Run{
		ID: "r_i", State: protocol.StateInterrupted, ErrorCode: protocol.ErrInterrupted,
	})
	if err == nil || !strings.Contains(err.Error(), "retryable") {
		t.Errorf("an interrupted run should be marked retryable: %v", err)
	}
}

func TestCurrentActorIsNeverEmpty(t *testing.T) {
	if currentActor() == "" {
		t.Error("origin_actor is display and audit only, but it must be something")
	}
}

func TestAgentLoggerHonoursLatemLog(t *testing.T) {
	t.Setenv("LATEM_LOG", "debug")
	var buf bytes.Buffer
	agentLogger(&buf).Debug("visible at debug")
	if !strings.Contains(buf.String(), "visible at debug") {
		t.Errorf("LATEM_LOG=debug did not lower the level:\n%s", buf.String())
	}

	t.Setenv("LATEM_LOG", "error")
	buf.Reset()
	agentLogger(&buf).Info("should be filtered")
	if buf.Len() != 0 {
		t.Errorf("LATEM_LOG=error still logged Info:\n%s", buf.String())
	}
}

func TestAgentRejectsAMalformedDrainAfter(t *testing.T) {
	newClientConfigDir(t)
	_, _, err := execCommand(t, "agent", "--drain-after", "not-a-duration")
	if err == nil {
		t.Fatal("a malformed --drain-after was accepted")
	}
	if got := exitCode(err); got != ExitUsage {
		t.Errorf("exit code = %d, want %d", got, ExitUsage)
	}
}

var _ = config.DefaultQueueTTL // keep the config import honest for future edits
