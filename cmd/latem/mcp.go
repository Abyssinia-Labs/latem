package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"sync"

	"github.com/spf13/cobra"
)

// `latem mcp` serves this machine's builds to an agent over the Model Context
// Protocol, on stdin and stdout (A87). It is meant to be reached by ssh from
// the machine the agent works on — `ssh mini .local/bin/latem mcp` — so it
// listens on nothing (INV-1) and is exactly as trusted as the ssh login that
// started it. ssh runs that command with a short PATH, which has neither
// ~/.local/bin nor Homebrew's bin, so latem is named by its path.
//
// It speaks newline-delimited JSON-RPC 2.0, MCP's stdio transport, written
// here rather than taken from a library: it is four methods and a framing the
// local broker already uses, against a dependency list RUNNER_SPEC §3 keeps
// short. The tools are in mcp_tools.go.

func newMCPCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "mcp",
		Short: "Serve this machine's builds to an agent over MCP, on stdin and stdout",
		Long:  mcpHelp(".local/bin/latem"),
		Args:  noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			out := cmd.OutOrStdout()
			if f, ok := out.(*os.File); ok && f == os.Stdout {
				// The protocol owns stdout. Anything else that would write
				// there — a stray print deep in a package — goes to stderr
				// instead of corrupting the stream an agent is reading.
				os.Stdout = os.Stderr
				defer func() { os.Stdout = f }()
			}
			return newMCPServer(mcpTools()).serve(cmd.Context(), cmd.InOrStdin(), out)
		},
	}
	// The help names this binary by its path, looked up only when it is
	// asked for, so a person can copy the line as it is.
	help := cmd.HelpFunc()
	cmd.SetHelpFunc(func(c *cobra.Command, args []string) {
		if exe, err := installedPath(); err == nil {
			c.Long = mcpHelp(exe)
		}
		help(c, args)
	})
	return cmd
}

// mcpHelp is `latem mcp --help`, naming latem by exe in the line that adds
// this machine to an agent. The path passes through two shells, the one the
// line is pasted into and the one ssh runs it with here, so it is quoted for
// both.
func mcpHelp(exe string) string {
	return `Serve this machine's builds to an agent over MCP, on stdin and stdout.

An agent working on another machine adds this one as a server it starts over
ssh, and reads and drives the builds here the way the commands below do. For
Claude Code, run this there:

  claude mcp add --scope user HOST -- ssh HOST ` + shellWord(shellWord(exe)) + ` mcp

HOST is anything ssh takes, an alias from ~/.ssh/config included. latem is
named by its path because ssh runs a command with a short PATH, which has
neither ~/.local/bin, where install.sh puts latem, nor Homebrew's bin.

Its tools list runs (by repository, state or commit), show one with its
result — each step, and the errors its output named, by file and line — read
its log, wait for it to finish, start a .latem/jobs.yaml job, cancel a run,
and check which of a repository's workflow jobs this machine can run.

It never approves. A run held for approval (INV-5) waits for a person at this
machine to run ` + "`latem approve`" + `, and ` + "`latem try`" + `, which makes an approved run, is not
offered either. Nor are secrets, vars, repositories, the service or setup.

An agent can have its own unpushed changes built, with startRun's changes,
only for a repository whose entry here says ` + "`agent_changes = true`" + `: that line
is a person's approval, given once at this machine, of that repository's
jobs.yaml jobs running on a commit of an allowed ref with an agent's files on
top (A91). Without it the tool refuses and names the line.

It listens on nothing: it reads requests on stdin and writes answers on
stdout, and stops when stdin closes. Nothing else is written to stdout.

Exit codes:
  0  stdin closed
  1  stdin or stdout failed`
}

// mcpVersions are the protocol revisions this server answers to, newest
// first. A client asking for another is offered the newest (MCP's lifecycle:
// the client then decides whether it can go on).
var mcpVersions = []string{"2025-11-25", "2025-06-18", "2025-03-26", "2024-11-05"}

// mcpMaxMessage bounds one request. A tool's arguments are a few names, but
// for startRun's changes (A91), which are files: up to maxChangeBytes of
// them, a third larger as base64, inside JSON.
const mcpMaxMessage = 32 << 20

// mcpInstructions is what the server tells an agent once, at initialize.
const mcpInstructions = `This is one build machine running latem, reached over ssh. Every tool reads or acts on the builds of this machine only.

To see whether a commit built here: listRuns with its repo and the full sha (git rev-parse HEAD), then waitForRun on any run that has not settled. waitForRun answers with the run's result: each step and how it ended, and the errors its output named, each with a file and a line. getRunLogs is for what that leaves out — the end of the log says why.

startRun starts a job from the repository's .latem/jobs.yaml; a workflow job comes from a push or a pull request through latem-cloud, not from here. To check a change before pushing it, give startRun changes: the commit you started from and the files you changed, whole. It runs only for a repository whose machine allows an agent's changes, and the refusal says so when it does not. A run whose ref is outside the repository's allowed refs is held for a person: it shows state awaiting_approval, and only a person at this machine can let it go (latem approve). Say so and stop; this server cannot approve.`

// The JSON-RPC error codes MCP uses.
const (
	rpcParseError     = -32700
	rpcInvalidRequest = -32600
	rpcMethodNotFound = -32601
	rpcInvalidParams  = -32602
)

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// mcpServer answers one client. Each request runs on its own goroutine, so a
// waitForRun that blocks for a minute does not hold up a ping or a cancel.
type mcpServer struct {
	tools []mcpTool

	writeMu sync.Mutex
	enc     *json.Encoder

	mu      sync.Mutex
	running map[string]*mcpCall // in-flight requests, by id as sent
}

type mcpCall struct {
	cancel context.CancelFunc
	// dropped is set when the client cancelled the request: MCP says it
	// expects no answer, so none is sent.
	dropped bool
}

func newMCPServer(tools []mcpTool) *mcpServer {
	return &mcpServer{tools: tools, running: map[string]*mcpCall{}}
}

// serve reads requests until in closes, then cancels what is still running
// and waits for it.
func (s *mcpServer) serve(ctx context.Context, in io.Reader, out io.Writer) error {
	s.enc = json.NewEncoder(out)
	ctx, stop := context.WithCancel(ctx)
	var wg sync.WaitGroup
	defer func() { stop(); wg.Wait() }()

	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 0, 64<<10), mcpMaxMessage)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var req rpcRequest
		if line[0] == '[' {
			// Batches were in one revision of MCP and are in none since.
			if err := s.reply(json.RawMessage("null"), nil, &rpcError{rpcInvalidRequest, "batches are not supported: send one request per line"}); err != nil {
				return err
			}
			continue
		}
		if err := json.Unmarshal(line, &req); err != nil {
			if err := s.reply(json.RawMessage("null"), nil, &rpcError{rpcParseError, "not JSON: " + err.Error()}); err != nil {
				return err
			}
			continue
		}
		if len(req.ID) == 0 {
			s.notified(req)
			continue
		}
		if req.JSONRPC != "2.0" || req.Method == "" {
			if err := s.reply(req.ID, nil, &rpcError{rpcInvalidRequest, `a request has "jsonrpc": "2.0" and a method`}); err != nil {
				return err
			}
			continue
		}

		callCtx, cancel := context.WithCancel(ctx)
		call := &mcpCall{cancel: cancel}
		key := string(req.ID)
		s.mu.Lock()
		s.running[key] = call
		s.mu.Unlock()
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer cancel()
			result, rerr := s.handle(callCtx, req)
			s.mu.Lock()
			delete(s.running, key)
			dropped := call.dropped
			s.mu.Unlock()
			if dropped {
				return
			}
			// A failed write means stdout is gone; the read loop finds out
			// when stdin closes, which is when ssh does.
			_ = s.reply(req.ID, result, rerr)
		}()
	}
	if err := sc.Err(); err != nil {
		return fmt.Errorf("mcp: reading stdin: %w", err)
	}
	return nil
}

// notified acts on a notification. The only one with anything to do is a
// cancel: the rest — initialized among them — need no answer and get none.
func (s *mcpServer) notified(req rpcRequest) {
	if req.Method != "notifications/cancelled" {
		return
	}
	var p struct {
		RequestID json.RawMessage `json:"requestId"`
	}
	if json.Unmarshal(req.Params, &p) != nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if call, ok := s.running[string(p.RequestID)]; ok {
		call.dropped = true
		call.cancel()
	}
}

func (s *mcpServer) reply(id json.RawMessage, result any, rerr *rpcError) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return s.enc.Encode(rpcResponse{JSONRPC: "2.0", ID: id, Result: result, Error: rerr})
}

func (s *mcpServer) handle(ctx context.Context, req rpcRequest) (any, *rpcError) {
	switch req.Method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(req.Params, &p)
		v := mcpVersions[0]
		if slices.Contains(mcpVersions, p.ProtocolVersion) {
			v = p.ProtocolVersion
		}
		return map[string]any{
			"protocolVersion": v,
			"capabilities":    map[string]any{"tools": map[string]any{"listChanged": false}},
			"serverInfo":      map[string]any{"name": "latem", "title": "Latem", "version": version},
			"instructions":    mcpInstructions,
		}, nil
	case "ping":
		return struct{}{}, nil
	case "tools/list":
		list := make([]any, 0, len(s.tools))
		for _, t := range s.tools {
			list = append(list, t.describe())
		}
		return map[string]any{"tools": list}, nil
	case "tools/call":
		var p struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
			Meta      struct {
				ProgressToken json.RawMessage `json:"progressToken"`
			} `json:"_meta"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil {
			return nil, &rpcError{rpcInvalidParams, "tools/call takes {name, arguments}"}
		}
		i := slices.IndexFunc(s.tools, func(t mcpTool) bool { return t.name == p.Name })
		if i < 0 {
			return nil, &rpcError{rpcInvalidParams, fmt.Sprintf("there is no tool %q: tools/list names them", p.Name)}
		}
		if tok := p.Meta.ProgressToken; len(tok) > 0 && string(tok) != "null" {
			ctx = context.WithValue(ctx, mcpProgressKey{}, s.progress(tok))
		}
		return toolResult(s.tools[i].call(ctx, p.Arguments)), nil
	default:
		return nil, &rpcError{rpcMethodNotFound, fmt.Sprintf("this server has no method %q", req.Method)}
	}
}

type mcpProgressKey struct{}

// mcpProgressOf is how a tool tells its caller what has happened so far, or
// nil when the caller sent no progress token and so asked not to be told.
func mcpProgressOf(ctx context.Context) func(string) {
	fn, _ := ctx.Value(mcpProgressKey{}).(func(string))
	return fn
}

// progress returns a function that sends one `notifications/progress` for
// token each time it is called, counting up, as MCP asks. It is a tool that
// waits saying what it has seen while it waits (A92); the answer still comes
// as the call's result.
func (s *mcpServer) progress(token json.RawMessage) func(string) {
	var mu sync.Mutex
	n := 0
	return func(message string) {
		mu.Lock()
		n++
		count := n
		mu.Unlock()
		s.writeMu.Lock()
		defer s.writeMu.Unlock()
		_ = s.enc.Encode(map[string]any{
			"jsonrpc": "2.0", "method": "notifications/progress",
			"params": map[string]any{"progressToken": token, "progress": count, "message": message},
		})
	}
}

// toolResult is a tool's answer as MCP carries it: the JSON as text, which
// every client shows its model, and the same object as structured content.
// A tool that failed says why as text, with isError, so the model reads the
// reason rather than the client treating it as a broken server.
func toolResult(v any, err error) map[string]any {
	if err != nil {
		var exit *ExitError
		if errors.As(err, &exit) && exit.Err != nil {
			err = exit.Err
		}
		return map[string]any{
			"content": []any{map[string]any{"type": "text", "text": err.Error()}},
			"isError": true,
		}
	}
	text, err := json.Marshal(v)
	if err != nil {
		return toolResult(nil, err)
	}
	return map[string]any{
		"content":           []any{map[string]any{"type": "text", "text": string(text)}},
		"structuredContent": json.RawMessage(text),
	}
}
