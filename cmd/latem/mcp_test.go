package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/Abyssinia-Labs/latem/internal/config"
	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

// mcpClient drives `latem mcp`'s server over pipes, as an agent's client does
// over ssh: one JSON-RPC message per line each way.
type mcpClient struct {
	t    *testing.T
	in   *io.PipeWriter
	out  *bufio.Scanner
	next int
}

func startMCP(t *testing.T) *mcpClient {
	t.Helper()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	done := make(chan error, 1)
	go func() {
		done <- newMCPServer(mcpTools()).serve(context.Background(), inR, outW)
		_ = outW.Close()
	}()
	sc := bufio.NewScanner(outR)
	sc.Buffer(make([]byte, 0, 64<<10), 4<<20)
	t.Cleanup(func() {
		_ = inW.Close()
		go func() { _, _ = io.Copy(io.Discard, outR) }()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("serve: %v", err)
			}
		case <-time.After(10 * time.Second):
			t.Error("the server did not stop when stdin closed")
		}
	})
	return &mcpClient{t: t, in: inW, out: sc}
}

func (c *mcpClient) send(line string) {
	c.t.Helper()
	if _, err := io.WriteString(c.in, line+"\n"); err != nil {
		c.t.Fatal(err)
	}
}

// read returns the next message the server writes.
func (c *mcpClient) read() map[string]any {
	c.t.Helper()
	if !c.out.Scan() {
		c.t.Fatalf("the server wrote nothing more: %v", c.out.Err())
	}
	var m map[string]any
	if err := json.Unmarshal(c.out.Bytes(), &m); err != nil {
		c.t.Fatalf("not JSON: %s", c.out.Bytes())
	}
	return m
}

// call sends one request and returns its response.
func (c *mcpClient) call(method string, params any) map[string]any {
	c.t.Helper()
	c.next++
	b, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": c.next, "method": method, "params": params})
	if err != nil {
		c.t.Fatal(err)
	}
	c.send(string(b))
	m := c.read()
	if id, _ := m["id"].(float64); int(id) != c.next {
		c.t.Fatalf("answer to request %d has id %v: %v", c.next, m["id"], m)
	}
	return m
}

// tool calls a tool and returns its JSON answer, or its error text.
func (c *mcpClient) tool(name string, args any) (map[string]any, string) {
	c.t.Helper()
	m := c.call("tools/call", map[string]any{"name": name, "arguments": args})
	res, ok := m["result"].(map[string]any)
	if !ok {
		c.t.Fatalf("tools/call %s: %v", name, m)
	}
	text := res["content"].([]any)[0].(map[string]any)["text"].(string)
	if res["isError"] == true {
		return nil, text
	}
	var v map[string]any
	if err := json.Unmarshal([]byte(text), &v); err != nil {
		c.t.Fatalf("%s answered %q", name, text)
	}
	return v, ""
}

func runsOf(t *testing.T, v map[string]any) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, r := range v["runs"].([]any) {
		out = append(out, r.(map[string]any))
	}
	return out
}

// An agent is told what the server is and offered the tools, and approve and
// try are not among them: INV-5's approvals are a person's (A87).
func TestMCPInitializeAndTools(t *testing.T) {
	c := startMCP(t)
	init := c.call("initialize", map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{}, "clientInfo": map[string]any{"name": "test"}})
	res := init["result"].(map[string]any)
	if res["protocolVersion"] != "2025-06-18" {
		t.Errorf("protocolVersion = %v, want the client's", res["protocolVersion"])
	}
	if info := res["serverInfo"].(map[string]any); info["name"] != "latem" {
		t.Errorf("serverInfo = %v", info)
	}
	if !strings.Contains(res["instructions"].(string), "cannot approve") {
		t.Errorf("the instructions do not say approval is a person's: %s", res["instructions"])
	}
	c.send(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	if v := c.call("initialize", map[string]any{"protocolVersion": "1999-01-01"})["result"].(map[string]any)["protocolVersion"]; v != mcpVersions[0] {
		t.Errorf("an unknown revision was answered with %v, want %s", v, mcpVersions[0])
	}

	var names []string
	readOnly := map[string]bool{}
	for _, tl := range c.call("tools/list", nil)["result"].(map[string]any)["tools"].([]any) {
		tool := tl.(map[string]any)
		name := tool["name"].(string)
		names = append(names, name)
		readOnly[name] = tool["annotations"].(map[string]any)["readOnlyHint"] == true
		if tool["inputSchema"].(map[string]any)["type"] != "object" {
			t.Errorf("%s's inputSchema is not an object", name)
		}
	}
	want := []string{"listRuns", "getRun", "getRunLogs", "waitForRun", "startRun", "cancelRun", "checkWorkflows", "getReadiness"}
	if !slices.Equal(names, want) {
		t.Errorf("tools = %v, want %v", names, want)
	}
	// Nor anything that changes what this machine builds: setup's plan is
	// read here and applied in a shell on the machine (A95).
	for _, n := range names {
		for _, word := range []string{"approve", "try", "secret", "apply", "setup", "service", "install"} {
			if strings.Contains(strings.ToLower(n), word) {
				t.Errorf("%s is offered", n)
			}
		}
	}
	if readOnly["startRun"] || readOnly["cancelRun"] || !readOnly["listRuns"] || !readOnly["waitForRun"] || !readOnly["getReadiness"] {
		t.Errorf("readOnlyHint is wrong: %v", readOnly)
	}
}

// `latem mcp --help` gives the line that adds this machine with latem named
// by its path. ssh runs the command with a short PATH, which has neither
// ~/.local/bin nor Homebrew's bin, so `ssh HOST latem mcp` is "command not
// found". A Homebrew latem is named by the link each upgrade moves, never by
// the keg an upgrade deletes.
func TestMCPHelpNamesLatemByItsPath(t *testing.T) {
	help := func() string {
		t.Helper()
		root := newRootCommand()
		var out strings.Builder
		root.SetOut(&out)
		root.SetErr(&out)
		root.SetArgs([]string{"mcp", "--help"})
		if err := root.Execute(); err != nil {
			t.Fatalf("mcp --help: %v", err)
		}
		return out.String()
	}

	asHomebrews(t, "")
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	if got, want := help(), "  claude mcp add --scope user HOST -- ssh HOST "+shellWord(shellWord(exe))+" mcp\n"; !strings.Contains(got, want) {
		t.Errorf("the help does not name this binary:\n%s\nwant the line %q", got, want)
	}

	asHomebrews(t, "/opt/homebrew/opt/latem/bin/latem")
	if got, want := help(), " -- ssh HOST /opt/homebrew/opt/latem/bin/latem mcp\n"; !strings.Contains(got, want) {
		t.Errorf("the help does not name Homebrew's link:\n%s\nwant %q", got, want)
	}

	// Quoted once for the shell the line is pasted into, and again for the
	// one ssh hands it to, so a path with a space arrives as one word.
	if got, want := mcpHelp("/Users/a b/.local/bin/latem"), ` -- ssh HOST ''\''/Users/a b/.local/bin/latem'\''' mcp`+"\n"; !strings.Contains(got, want) {
		t.Errorf("a path with a space is not quoted for both shells:\n%s\nwant %q", got, want)
	}
}

// What is not a request is answered as JSON-RPC says, a notification gets no
// answer, and the server goes on.
func TestMCPProtocolErrors(t *testing.T) {
	c := startMCP(t)
	for _, tc := range []struct {
		line string
		code float64
	}{
		{`not json`, rpcParseError},
		{`[{"jsonrpc":"2.0","id":1,"method":"ping"}]`, rpcInvalidRequest},
		{`{"jsonrpc":"2.0","id":7,"method":"resources/list"}`, rpcMethodNotFound},
		{`{"jsonrpc":"2.0","id":8,"method":"tools/call","params":{"name":"approveRun","arguments":{}}}`, rpcInvalidParams},
		{`{"id":9,"method":"ping"}`, rpcInvalidRequest},
	} {
		c.send(tc.line)
		m := c.read()
		if e, ok := m["error"].(map[string]any); !ok || e["code"] != tc.code {
			t.Errorf("%s: got %v, want error %v", tc.line, m, tc.code)
		}
	}
	c.send(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	if m := c.call("ping", nil); m["error"] != nil || m["result"] == nil {
		t.Errorf("ping after a notification: %v", m)
	}
}

func TestMCPListAndGetRuns(t *testing.T) {
	dir := newClientConfigDir(t)
	db := clientStore(t, dir)
	const sha = "0123456789abcdef0123456789abcdef01234567"
	seedCLIRun(t, db, "r_done", protocol.StateSucceeded)
	seedParkedCLIRun(t, db, "r_held", sha)
	c := startMCP(t)

	all, _ := c.tool("listRuns", map[string]any{})
	if got := runsOf(t, all); len(got) != 2 {
		t.Fatalf("listRuns = %v, want both runs", got)
	}
	held, _ := c.tool("listRuns", map[string]any{"repo": "app", "sha": strings.ToUpper(sha)})
	runs := runsOf(t, held)
	if len(runs) != 1 || runs[0]["id"] != "r_held" || runs[0]["settled"] != false {
		t.Fatalf("listRuns by commit = %v", runs)
	}
	if w, _ := runs[0]["waiting"].(string); !strings.Contains(w, "latem approve r_held") || !strings.Contains(w, "cannot approve") {
		t.Errorf("a held run does not say who it waits for: %q", w)
	}
	if v, _ := c.tool("listRuns", map[string]any{"sha": strings.Repeat("9", 40)}); len(runsOf(t, v)) != 0 || v["note"] == nil {
		t.Errorf("an unknown commit: %v", v)
	}
	done, _ := c.tool("listRuns", map[string]any{"state": []string{"succeeded"}, "limit": 5})
	if runs := runsOf(t, done); len(runs) != 1 || runs[0]["id"] != "r_done" {
		t.Errorf("listRuns succeeded = %v", runs)
	}

	run, _ := c.tool("getRun", map[string]any{"runId": "r_done"})
	if run["state"] != "succeeded" || run["settled"] != true || run["origin"] != "cli" {
		t.Errorf("getRun = %v", run)
	}

	for _, tc := range []struct {
		tool string
		args map[string]any
		want string
	}{
		{"listRuns", map[string]any{"sha": "0123abc"}, "not a full commit"},
		{"listRuns", map[string]any{"state": []string{"done"}}, "not a run state"},
		{"getRun", map[string]any{"run_id": "r_done"}, `unknown field "run_id"`},
		{"getRun", map[string]any{"runId": "r_nothing"}, "no run r_nothing on this machine"},
		{"getRun", map[string]any{"runId": "../etc"}, "is not a run id"},
	} {
		if _, msg := c.tool(tc.tool, tc.args); !strings.Contains(msg, tc.want) {
			t.Errorf("%s %v: error %q, want it to say %q", tc.tool, tc.args, msg, tc.want)
		}
	}
}

func TestMCPGetRunLogs(t *testing.T) {
	dir := newClientConfigDir(t)
	db := clientStore(t, dir)
	seedCLIRun(t, db, "r_logged", protocol.StateFailed)
	seedCLIRun(t, db, "r_quiet", protocol.StateQueued)
	long := strings.Repeat("é", mcpMaxLineBytes) // two bytes a rune
	log := "one\ntwo\nthree\n" + long + "\nfive: exit 1"
	if err := os.MkdirAll(filepath.Join(dir, "logs"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "logs", "r_logged.log"), []byte(log), 0o600); err != nil {
		t.Fatal(err)
	}
	c := startMCP(t)

	lines := func(v map[string]any) []string {
		var out []string
		for _, l := range v["lines"].([]any) {
			m := l.(map[string]any)
			out = append(out, fmt.Sprintf("%v:%v", m["n"], m["text"]))
		}
		return out
	}
	tail, _ := c.tool("getRunLogs", map[string]any{"runId": "r_logged", "tail": 1})
	if got := lines(tail); !slices.Equal(got, []string{"5:five: exit 1"}) || tail["next"] != float64(5) || tail["settled"] != true {
		t.Errorf("tail 1 = %v, %v", got, tail)
	}
	whole, _ := c.tool("getRunLogs", map[string]any{"runId": "r_logged"})
	got := lines(whole)
	if len(got) != 5 || got[0] != "1:one" {
		t.Errorf("default tail = %v", got)
	}
	if fourth := got[3]; len(fourth) > mcpMaxLineBytes+40 || !strings.Contains(fourth, "bytes more)") || !strings.HasPrefix(fourth, "4:éé") {
		t.Errorf("a long line was not cut on a rune: %d bytes, ends %q", len(fourth), fourth[len(fourth)-30:])
	}
	page, _ := c.tool("getRunLogs", map[string]any{"runId": "r_logged", "after": 1, "limit": 2})
	if got := lines(page); !slices.Equal(got, []string{"2:two", "3:three"}) || page["more"] != true || page["next"] != float64(3) {
		t.Errorf("after 1 limit 2 = %v, %v", got, page)
	}
	end, _ := c.tool("getRunLogs", map[string]any{"runId": "r_logged", "after": 5})
	if len(lines(end)) != 0 || end["more"] != false || end["next"] != float64(5) {
		t.Errorf("after the last line = %v", end)
	}
	quiet, _ := c.tool("getRunLogs", map[string]any{"runId": "r_quiet"})
	if len(lines(quiet)) != 0 || !strings.Contains(quiet["note"].(string), "no log") {
		t.Errorf("a run with no log = %v", quiet)
	}
	if _, msg := c.tool("getRunLogs", map[string]any{"runId": "r_logged", "tail": 2, "after": 1}); !strings.Contains(msg, "give one") {
		t.Errorf("tail and after: %q", msg)
	}
}

// startRun goes where `latem run` goes — the agent's socket — and waitForRun
// answers when the run has settled.
func TestMCPStartAndWait(t *testing.T) {
	dir := newClientConfigDir(t)
	db := clientStore(t, dir)
	finish := protocol.StateSucceeded
	fakeRunAgent(t, dir, db, "built\n", &finish)
	c := startMCP(t)

	started, msg := c.tool("startRun", map[string]any{"repo": "app", "job": "test", "inputs": map[string]any{"count": 2, "dry_run": true, "lane": "beta"}})
	if msg != "" {
		t.Fatal(msg)
	}
	if started["agentRunning"] != true {
		t.Errorf("startRun = %v", started)
	}
	id := started["runId"].(string)
	waited, _ := c.tool("waitForRun", map[string]any{"runId": id, "timeoutSeconds": 10})
	run := waited["run"].(map[string]any)
	if waited["settled"] != true || run["state"] != "succeeded" {
		t.Fatalf("waitForRun = %v", waited)
	}
	got, _ := c.tool("getRun", map[string]any{"runId": id})
	if in := got["inputs"].(map[string]any); in["count"] != "2" || in["dry_run"] != "true" || in["lane"] != "beta" {
		t.Errorf("inputs = %v", in)
	}
	if !strings.HasSuffix(got["origin"].(string), " via latem mcp") {
		t.Errorf("origin = %v", got["origin"])
	}

	if _, msg := c.tool("startRun", map[string]any{"repo": "nope", "job": "test"}); !strings.Contains(msg, "it builds app") {
		t.Errorf("an unknown repository: %q", msg)
	}
	if _, msg := c.tool("startRun", map[string]any{"repo": "app", "job": "test", "inputs": map[string]any{"list": []int{1}}}); !strings.Contains(msg, "a string, a number or a boolean") {
		t.Errorf("an input that is a list: %q", msg)
	}
}

// Waiting cannot release a run held for a person, or start one with no agent
// to run it, so the wait says so at once rather than at its timeout.
func TestMCPWaitReturnsWhenWaitingCannotHelp(t *testing.T) {
	dir := newClientConfigDir(t)
	db := clientStore(t, dir)
	seedParkedCLIRun(t, db, "r_held", "0123456789abcdef0123456789abcdef01234567")
	seedCLIRun(t, db, "r_queued", protocol.StateQueued)
	c := startMCP(t)

	for id, want := range map[string]string{"r_held": "approval", "r_queued": "no agent is running"} {
		start := time.Now()
		v, msg := c.tool("waitForRun", map[string]any{"runId": id, "timeoutSeconds": 30})
		if msg != "" {
			t.Fatal(msg)
		}
		if v["settled"] != false || !strings.Contains(v["note"].(string), want) || time.Since(start) > 5*time.Second {
			t.Errorf("%s: %v after %s", id, v, time.Since(start))
		}
	}
}

func TestMCPCancelRun(t *testing.T) {
	dir := newClientConfigDir(t)
	db := clientStore(t, dir)
	seedCLIRun(t, db, "r_queued", protocol.StateQueued)
	c := startMCP(t)

	v, msg := c.tool("cancelRun", map[string]any{"runId": "r_queued", "reason": "superseded"})
	if msg != "" {
		t.Fatal(msg)
	}
	if run := v["run"].(map[string]any); run["state"] != "cancelled" || v["note"] != nil {
		t.Errorf("cancelRun with no agent = %v", v)
	}
	if _, msg := c.tool("cancelRun", map[string]any{"runId": "r_gone"}); !strings.Contains(msg, "no run r_gone") {
		t.Errorf("an unknown run: %q", msg)
	}
}

// A request the client cancels is stopped and answered with nothing, and the
// server answers what comes after it.
func TestMCPCancelledRequestGetsNoAnswer(t *testing.T) {
	dir := newClientConfigDir(t)
	db := clientStore(t, dir)
	seedCLIRun(t, db, "r_running", protocol.StateRunning)
	c := startMCP(t)

	c.send(`{"jsonrpc":"2.0","id":"wait-1","method":"tools/call","params":{"name":"waitForRun","arguments":{"runId":"r_running","timeoutSeconds":600}}}`)
	// Give the wait time to start before it is cancelled.
	time.Sleep(200 * time.Millisecond)
	c.send(`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":"wait-1","reason":"user"}}`)
	if m := c.call("ping", nil); m["result"] == nil {
		t.Errorf("the next answer was %v, want the ping's", m)
	}
}

func TestMCPCheckWorkflowsNeedsAKnownRepository(t *testing.T) {
	newClientConfigDir(t)
	c := startMCP(t)
	if _, msg := c.tool("checkWorkflows", map[string]any{}); !strings.Contains(msg, "needs repo") {
		t.Errorf("no repo: %q", msg)
	}
	if _, msg := c.tool("checkWorkflows", map[string]any{"repo": "nope", "offline": true}); !strings.Contains(msg, `no repo "nope"`) {
		t.Errorf("an unknown repository: %q", msg)
	}
}

// readinessMachine is a Mac with two repositories, an Xcode app and a Go
// service, each read from its own files, and xcodegen not installed.
func readinessMachine(t *testing.T) (dir string, rec *setupRecorder) {
	t.Helper()
	dir, rec = setupMachine(t, macRepo)
	rec.machine.dirs["xcodegen"], rec.machine.dirs["ruby"] = "/nowhere", "/nowhere"
	path := filepath.Join(dir, "config.toml")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	body = append(body, "\n[repos.site]\nurl = \"https://github.com/me/site.git\"\nallowed_refs = [\"main\"]\n"...)
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	files := map[string]fstest.MapFS{
		"app": macRepo,
		"site": {".github/workflows/ci.yml": {Data: []byte("on: push\njobs:\n  test:\n    runs-on: ubuntu-latest\n" +
			"    steps:\n      - run: go test ./...\n")}},
	}
	prev := readRepo
	readRepo = func(_ context.Context, _ *config.Config, key string, _ treeSource) (*repoFiles, error) {
		return &repoFiles{fsys: files[key], sha: strings.Repeat("ab", 20), where: "the files of " + key, close: func() {}}, nil
	}
	t.Cleanup(func() { readRepo = prev })
	useConfigDir(t, dir)
	return dir, rec
}

func planKinds(t *testing.T, v map[string]any) []string {
	t.Helper()
	var out []string
	for _, k := range v["kinds"].([]any) {
		out = append(out, k.(map[string]any)["name"].(string))
	}
	return out
}

// getReadiness is `latem setup --json` through the protocol: the plan, as
// text and as structured content, and with repo only that repository's
// kinds. It changes nothing (A95).
func TestMCPGetReadiness(t *testing.T) {
	dir, rec := readinessMachine(t)
	before := readConfigFile(t, dir)
	c := startMCP(t)

	all, msg := c.tool("getReadiness", map[string]any{})
	if msg != "" {
		t.Fatalf("getReadiness: %s", msg)
	}
	if got := planKinds(t, all); !slices.Equal(got, []string{"ios", "xcode", "android", "linux"}) {
		t.Errorf("kinds = %v", got)
	}
	// The same document `latem setup --json` prints.
	setupOut, err := latemIn(t, dir, "", "setup", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var want map[string]any
	if err := json.Unmarshal([]byte(setupOut), &want); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(all) != fmt.Sprint(want) {
		t.Errorf("getReadiness is not setup --json:\n%v\nvs\n%v", all, want)
	}

	// Structured content, beside the text every client shows.
	res := c.call("tools/call", map[string]any{"name": "getReadiness", "arguments": map[string]any{"repo": "app"}})["result"].(map[string]any)
	structured, ok := res["structuredContent"].(map[string]any)
	if !ok || res["isError"] == true {
		t.Fatalf("result = %v", res)
	}
	if got := planKinds(t, structured); !slices.Equal(got, []string{"xcode"}) {
		t.Errorf("app's kinds = %v", got)
	}
	xcode := structured["kinds"].([]any)[0].(map[string]any)
	var ids []string
	for _, it := range xcode["items"].([]any) {
		ids = append(ids, it.(map[string]any)["id"].(string))
	}
	if !slices.Equal(ids, []string{"ruby", "xcodegen"}) {
		t.Errorf("xcode's items = %v", ids)
	}

	site, msg := c.tool("getReadiness", map[string]any{"repo": "site"})
	if msg != "" || !slices.Equal(planKinds(t, site), []string{"linux"}) {
		t.Errorf("site: %v %s", site, msg)
	}

	if _, msg := c.tool("getReadiness", map[string]any{"repo": "nope"}); !strings.Contains(msg, `this machine builds no repository "nope": it builds app, site`) {
		t.Errorf("an unknown repository: %q", msg)
	}
	if _, msg := c.tool("getReadiness", map[string]any{"repo": "app", "apply": true}); !strings.Contains(msg, "unknown field") {
		t.Errorf("an argument it does not take: %q", msg)
	}

	if readConfigFile(t, dir) != before || len(rec.machine.installs) != 0 {
		t.Errorf("getReadiness changed something: installs %q", rec.machine.installs)
	}
}

// A look that runs past its bound is stopped and says why, rather than
// answering with a machine read in part.
func TestMCPGetReadinessIsBounded(t *testing.T) {
	readinessMachine(t)
	readRepo = func(ctx context.Context, _ *config.Config, _ string, _ treeSource) (*repoFiles, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	prev := mcpReadinessWait
	mcpReadinessWait = 50 * time.Millisecond
	t.Cleanup(func() { mcpReadinessWait = prev })

	c := startMCP(t)
	if _, msg := c.tool("getReadiness", map[string]any{}); !strings.Contains(msg, "took longer than 50ms") {
		t.Errorf("msg = %q", msg)
	}
}

// Nor is a look the caller cancelled: a client that closes stdin while it
// runs gets an error, never what was read before the stop.
func TestMCPGetReadinessCancelled(t *testing.T) {
	readinessMachine(t)
	ctx, cancel := context.WithCancel(context.Background())
	readRepo = func(context.Context, *config.Config, string, treeSource) (*repoFiles, error) {
		cancel()
		return nil, context.Canceled
	}
	v, err := mcpGetReadiness(ctx, json.RawMessage(`{}`))
	if err == nil || !strings.Contains(err.Error(), "cancelled before this machine was read") {
		t.Errorf("got %v, %v", v, err)
	}
}

func TestMCPGetReadinessWithNoConfig(t *testing.T) {
	fakeSetup(t, true)
	useConfigDir(t, filepath.Join(privateTempDir(t), "none"))
	c := startMCP(t)
	if _, msg := c.tool("getReadiness", map[string]any{}); !strings.Contains(msg, "no config yet") {
		t.Errorf("msg = %q", msg)
	}
}
