package main

import (
	"context"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Abyssinia-Labs/latem/internal/config"
	"github.com/Abyssinia-Labs/latem/internal/source"
	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

const (
	changesBase = "0123456789abcdef0123456789abcdef01234567"
	changesMade = "abcdefabcdefabcdefabcdefabcdefabcdefabcd"
)

// fakeChangesRepo stands in for the agent's mirror (A91): it makes no commit,
// and says what it was asked to make.
type fakeChangesRepo struct {
	got  []source.Changes
	made source.Changed
	// onRef is whether the base is in the ref's history; tip, the ref's
	// commit here.
	onRef bool
	tip   string
	jobs  string // .latem/jobs.yaml at the base; empty for none
	asked []string
}

func (f *fakeChangesRepo) Commit(_ context.Context, c source.Changes) (source.Changed, error) {
	f.got = append(f.got, c)
	return f.made, nil
}

func (f *fakeChangesRepo) OnRef(_ context.Context, commit, ref string) (bool, string, error) {
	f.asked = append(f.asked, commit+" on "+ref)
	return f.onRef, f.tip, nil
}

func (f *fakeChangesRepo) JobsAt(context.Context, string) ([]byte, bool, error) {
	return []byte(f.jobs), f.jobs != "", nil
}

const changesJobs = `
version: 1
jobs:
  test:
    runs_on: [test]
    workspace: warm
    steps:
      - run: ./ci/test.sh
`

// withChangesRepo points changes runs at a fake mirror in which everything an
// agent's run needs holds, for a test to take one thing away.
func withChangesRepo(t *testing.T) *fakeChangesRepo {
	t.Helper()
	f := &fakeChangesRepo{
		made:  source.Changed{SHA: changesMade, Paths: 2},
		onRef: true, tip: changesBase, jobs: changesJobs,
	}
	was := openChangesRepo
	openChangesRepo = func(*config.Config, string) (changesRepo, error) { return f, nil }
	t.Cleanup(func() { openChangesRepo = was })
	return f
}

// allowAgentChanges writes the grant into the test's config.toml, as an
// operator would.
func allowAgentChanges(t *testing.T, dir string) {
	t.Helper()
	path := filepath.Join(dir, "config.toml")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	out := strings.Replace(string(b), `allowed_refs = ["main"]`, "allowed_refs = [\"main\"]\nagent_changes = true", 1)
	if out == string(b) {
		t.Fatal("the test config has no allowed_refs line to put the grant after")
	}
	if err := os.WriteFile(path, []byte(out), 0o600); err != nil {
		t.Fatal(err)
	}
}

func strPtr(s string) *string { return &s }

func TestChangeSetToSource(t *testing.T) {
	bin := base64.StdEncoding.EncodeToString([]byte{0, 1, 2, 0xff})
	cs := changeSet{
		Base: strings.ToUpper(changesBase),
		Files: []changeFile{
			{Path: "a.txt", Text: strPtr("hello\n")},
			{Path: "empty", Text: strPtr("")},
			{Path: "bin/tool", Base64: &bin, Mode: "755"},
		},
		Remove: []string{"old.txt"},
	}
	got, err := cs.toSource()
	if err != nil {
		t.Fatal(err)
	}
	if got.Base != changesBase || len(got.Writes) != 3 || len(got.Removes) != 1 {
		t.Fatalf("toSource = %+v", got)
	}
	if string(got.Writes[0].Data) != "hello\n" || got.Writes[0].Mode != "" {
		t.Errorf("a text file: %+v", got.Writes[0])
	}
	if got.Writes[1].Data == nil && len(got.Writes[1].Data) != 0 {
		t.Errorf("an empty file: %+v", got.Writes[1])
	}
	if string(got.Writes[2].Data) != "\x00\x01\x02\xff" || got.Writes[2].Mode != "100755" {
		t.Errorf("a binary, executable file: %+v", got.Writes[2])
	}

	big := strings.Repeat("x", maxChangeBytes/2+1)
	for name, tc := range map[string]struct {
		cs   changeSet
		want string
	}{
		"a short base":        {changeSet{Base: "0123abc", Files: cs.Files}, "not a full commit"},
		"nothing to change":   {changeSet{Base: changesBase}, "write no file and remove none"},
		"two contents":        {changeSet{Base: changesBase, Files: []changeFile{{Path: "a", Text: strPtr("x"), Base64: &bin}}}, "both text and base64"},
		"no content":          {changeSet{Base: changesBase, Files: []changeFile{{Path: "a"}}}, "neither text nor base64"},
		"bad base64":          {changeSet{Base: changesBase, Files: []changeFile{{Path: "a", Base64: strPtr("%%%")}}}, "not standard base64"},
		"a mode not a file's": {changeSet{Base: changesBase, Files: []changeFile{{Path: "a", Text: strPtr("x"), Mode: "777"}}}, "644 or 755"},
		"too many bytes": {changeSet{Base: changesBase, Files: []changeFile{
			{Path: "a", Text: &big}, {Path: "b", Text: &big}}}, "MiB together"},
		"too many paths": {changeSet{Base: changesBase, Remove: make([]string, maxChangePaths+1)}, "limit 5000"},
	} {
		if _, err := tc.cs.toSource(); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: toSource = %v, want an error mentioning %q", name, err, tc.want)
		}
	}
}

// `latem run --changes` builds the files on the commit as a run nobody pushed:
// the row carries the commit, its base, and the approval running the command
// is. A person needs no grant, and is asked nothing about refs.
func TestRunChanges(t *testing.T) {
	dir := newClientConfigDir(t)
	repo := withChangesRepo(t)
	repo.onRef = false // a person's run does not ask
	file := filepath.Join(dir, "change.json")
	body := `{"base": "` + changesBase + `",
  "files": [{"path": "src/lib.rs", "text": "fn main() {}\n"}, {"path": ".latem/jobs.yaml", "text": "version: 1\n"}],
  "remove": ["src/old.rs"]}`
	if err := os.WriteFile(file, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	stdout, stderr, err := execCommand(t, "run", "app", "test", "--changes", file, "--ref", "feature/x", "--input", "tier=2")
	if err != nil {
		t.Fatalf("run --changes: %v\n%s", err, stderr)
	}
	id := strings.TrimSpace(stdout)
	if protocol.ValidateID(id) != nil {
		t.Fatalf("stdout is %q, want the run id alone", stdout)
	}
	if !strings.Contains(stderr, "2 path(s) changed on 0123456789ab: commit abcdefabcdef, on no ref") {
		t.Errorf("stderr does not say what was built:\n%s", stderr)
	}
	if len(repo.got) != 1 || repo.got[0].Base != changesBase || len(repo.got[0].Writes) != 2 ||
		repo.got[0].Removes[0] != "src/old.rs" {
		t.Fatalf("the mirror was asked for %+v", repo.got)
	}
	if len(repo.asked) != 0 {
		t.Errorf("a person's changes were held to a ref: %v", repo.asked)
	}

	r, err := clientStore(t, dir).Get(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if r.State != protocol.StateQueued || r.Job != "test" || r.Ref != "refs/heads/feature/x" || r.Inputs["tier"] != "2" {
		t.Errorf("the row is %+v", r)
	}
	if !r.IsLocal() || r.ChangesBase() != changesBase || r.Workflow.SHA != changesMade || r.Workflow.Workflow != "" {
		t.Errorf("the row is not a changes run pinned to its commit: %+v", r.Workflow)
	}
	if r.ApprovedAt == nil || r.ApprovedSHA != changesMade {
		t.Errorf("the row is not approved for its own commit: %v %q", r.ApprovedAt, r.ApprovedSHA)
	}
}

func TestRunChangesRefusesWhatIsNotChanges(t *testing.T) {
	dir := newClientConfigDir(t)
	repo := withChangesRepo(t)
	file := filepath.Join(dir, "change.json")
	for name, tc := range map[string]struct{ body, want string }{
		"not JSON":        {"base: abc", "not the JSON a changes run takes"},
		"an unknown key":  {`{"base": "` + changesBase + `", "delete": ["a"]}`, `unknown field "delete"`},
		"a short base":    {`{"base": "0123abc", "remove": ["a"]}`, "not a full commit"},
		"nothing in them": {`{"base": "` + changesBase + `"}`, "write no file and remove none"},
	} {
		if err := os.WriteFile(file, []byte(tc.body), 0o600); err != nil {
			t.Fatal(err)
		}
		_, stderr, err := execCommand(t, "run", "app", "test", "--changes", file)
		if err == nil || !strings.Contains(err.Error()+stderr, tc.want) {
			t.Errorf("%s: %v %s; want an error mentioning %q", name, err, stderr, tc.want)
		}
	}
	if err := os.WriteFile(file, []byte(`{"base": "`+changesBase+`", "remove": ["a"]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := execCommand(t, "run", "nope", "test", "--changes", file); err == nil ||
		!strings.Contains(err.Error(), "builds no repository") {
		t.Errorf("an unknown repository: %v", err)
	}
	if len(repo.got) != 0 {
		t.Errorf("a refused run reached the mirror: %+v", repo.got)
	}
}

func changesArgs(extra map[string]any) map[string]any {
	args := map[string]any{"repo": "app", "job": "test", "changes": map[string]any{
		"base":   changesBase,
		"files":  []any{map[string]any{"path": "src/lib.rs", "text": "fn main() {}\n"}},
		"remove": []any{"src/old.rs"},
	}}
	for k, v := range extra {
		args[k] = v
	}
	return args
}

// An agent's changes run where the machine's config says they may, and the
// answer carries what it needs to follow the run and to name what it built.
func TestMCPStartRunWithChanges(t *testing.T) {
	dir := newClientConfigDir(t)
	allowAgentChanges(t, dir)
	repo := withChangesRepo(t)
	c := startMCP(t)

	v, errText := c.tool("startRun", changesArgs(map[string]any{"inputs": map[string]any{"tier": 2}}))
	if errText != "" {
		t.Fatalf("startRun with changes: %s", errText)
	}
	id, _ := v["runId"].(string)
	if v["sha"] != changesMade || v["base"] != changesBase || v["changedPaths"] != float64(2) || protocol.ValidateID(id) != nil {
		t.Fatalf("startRun answered %v", v)
	}
	if len(repo.asked) != 1 || repo.asked[0] != changesBase+" on refs/heads/main" {
		t.Errorf("the base was checked against %v, want the default branch", repo.asked)
	}

	r, err := clientStore(t, dir).Get(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(r.OriginActor, " via latem mcp") || r.ChangesBase() != changesBase ||
		r.ApprovedSHA != changesMade || r.Inputs["tier"] != "2" {
		t.Errorf("the row is %+v, workflow %+v", r, r.Workflow)
	}

	run, _ := c.tool("getRun", map[string]any{"runId": id})
	if ch, _ := run["changes"].(map[string]any); ch["base"] != changesBase || run["sha"] != changesMade || run["workflow"] != nil {
		t.Errorf("getRun does not show a changes run: %v", run)
	}
	listed, _ := c.tool("listRuns", map[string]any{"sha": changesMade})
	if runs := runsOf(t, listed); len(runs) != 1 || runs[0]["id"] != id {
		t.Errorf("the run is not found by the commit it builds: %v", listed)
	}
}

// Each thing an agent's changes are held to, taken away one at a time: the
// refusal names it, and nothing is queued.
func TestMCPStartRunWithChangesIsHeldToTheOperatorsRules(t *testing.T) {
	for name, tc := range map[string]struct {
		grant bool
		set   func(*fakeChangesRepo)
		args  map[string]any
		want  string
	}{
		"no grant": {false, nil, changesArgs(nil), "agent_changes = true"},
		"a ref outside allowed_refs": {true, nil, changesArgs(map[string]any{"ref": "feature/x"}),
			"not a ref app builds without a person's approval"},
		"a base that is not on the ref": {true, func(f *fakeChangesRepo) { f.onRef = false }, changesArgs(nil),
			"is not in the history of refs/heads/main"},
		"no jobs.yaml at the base": {true, func(f *fakeChangesRepo) { f.jobs = "" }, changesArgs(nil),
			"has no .latem/jobs.yaml at"},
		"a job the base does not declare": {true, nil, changesArgs(map[string]any{"job": "deploy"}),
			`declares no job "deploy"; it declares test`},
		"a change to jobs.yaml": {true, nil, map[string]any{"repo": "app", "job": "test", "changes": map[string]any{
			"base": changesBase, "files": []any{map[string]any{"path": ".latem/jobs.yaml", "text": "version: 1\n"}}}},
			"the changes touch .latem/jobs.yaml"},
		"a change to jobs.yaml in other capitals": {true, nil, map[string]any{"repo": "app", "job": "test", "changes": map[string]any{
			"base": changesBase, "files": []any{map[string]any{"path": ".LATEM/Jobs.YAML", "text": "version: 1\n"}}}},
			"the changes touch .latem/jobs.yaml"},
		"a removal of jobs.yaml": {true, nil, map[string]any{"repo": "app", "job": "test", "changes": map[string]any{
			"base": changesBase, "remove": []any{".latem/jobs.yaml"}}}, "the changes touch .latem/jobs.yaml"},
		"an unknown key in a file": {true, nil, map[string]any{"repo": "app", "job": "test", "changes": map[string]any{
			"base": changesBase, "files": []any{map[string]any{"path": "a", "content": "x"}}}}, `unknown field "content"`},
	} {
		t.Run(name, func(t *testing.T) {
			dir := newClientConfigDir(t)
			if tc.grant {
				allowAgentChanges(t, dir)
			}
			repo := withChangesRepo(t)
			if tc.set != nil {
				tc.set(repo)
			}
			c := startMCP(t)
			v, errText := c.tool("startRun", tc.args)
			if errText == "" || !strings.Contains(errText, tc.want) {
				t.Fatalf("startRun = %v, %q; want a refusal mentioning %q", v, errText, tc.want)
			}
			listed, _ := c.tool("listRuns", map[string]any{})
			if runs := runsOf(t, listed); len(runs) != 0 {
				t.Errorf("a refused run was queued: %v", runs)
			}
		})
	}
}
