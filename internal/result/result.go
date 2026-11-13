// Package result is a run's outcome as data: how each step ended, and the
// errors its output named, each with the file and line a tool put on it
// (A92).
//
// A run's log is for a person. It says everything, in the order it happened,
// and whoever reads it knows where a compiler puts the line that matters. A
// program that started the run to check a change wants three things out of
// it: did it pass, which step did not, and what broke, where. Until now it
// got the first from the run's state and guessed the rest from the log's
// last lines.
//
// The result is written beside the log as a step ends and again when the run
// does, read by `latem result`, and returned by `latem mcp` with a run that
// has settled. It is made from the log's own frames, after the redactor, so
// it holds nothing the log does not (INV-4), and like the log it stays on the
// machine: no transport carries it.
package result

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

// Version is the result document's own, raised when a field changes meaning.
const Version = 1

// Suffix is what a result's file name adds to its run's id, in the log
// directory: <run-id>.result.json beside <run-id>.log.
const Suffix = ".result.json"

// Bounds on one result. A build that prints ten thousand errors has one
// problem, and it is in the first hundred.
const (
	MaxErrors   = 100
	MaxWarnings = 50
	// MaxMessageBytes and MaxHelpBytes cut one problem's text.
	MaxMessageBytes = 2000
	MaxHelpBytes    = 1000
)

// The severities of a [Problem].
const (
	SeverityError   = "error"
	SeverityWarning = "warning"
)

// The outcomes of a [Step].
const (
	OutcomeSuccess   = "success"
	OutcomeFailure   = "failure"
	OutcomeSkipped   = "skipped"
	OutcomeCancelled = "cancelled"
	OutcomeTimedOut  = "timed_out"
	// OutcomeRunning is a step that had not ended when the result was
	// written: a run still going, or one its agent did not see end.
	OutcomeRunning = "running"
)

// Result is one run's outcome.
type Result struct {
	Version int    `json:"version"`
	RunID   string `json:"runId"`
	Repo    string `json:"repo"`
	Job     string `json:"job"`
	Ref     string `json:"ref,omitempty"`
	SHA     string `json:"sha,omitempty"`
	// Base is the commit a changes run's files were changed from (A91).
	Base string `json:"base,omitempty"`
	// Inputs are the run's inputs as it was given them, which is how a
	// caller tells its own runs apart: a tier, a shard, an attempt.
	Inputs map[string]string `json:"inputs,omitempty"`
	Runner string            `json:"runner,omitempty"`
	// Workspace is "warm" for a run that built in the workspace its job
	// keeps (A90), and "fresh" for one that built in a new clone.
	Workspace string `json:"workspace,omitempty"`

	// State is the run's state when this was written. Settled says whether
	// that is its last.
	State     protocol.RunState `json:"state"`
	Settled   bool              `json:"settled"`
	ErrorCode string            `json:"errorCode,omitempty"`
	Error     string            `json:"error,omitempty"`
	ExitCode  *int              `json:"exitCode,omitempty"`

	StartedAt  *time.Time `json:"startedAt,omitempty"`
	FinishedAt *time.Time `json:"finishedAt,omitempty"`
	DurationMs int64      `json:"durationMs,omitempty"`

	Steps []Step `json:"steps"`
	// Problems are what the steps' output named as broken, errors first, in
	// the order they were printed.
	Problems []Problem `json:"problems"`
	Errors   int       `json:"errors"`
	Warnings int       `json:"warnings"`
	// Dropped is how many more problems the output held than are kept.
	Dropped int `json:"dropped,omitempty"`
	// Note says what a reader should know about this result that its fields
	// do not: that it was put together from the run's row, say.
	Note string `json:"note,omitempty"`
}

// Step is how one step ended.
type Step struct {
	Name    string `json:"name"`
	Outcome string `json:"outcome"`
	// Forgiven is a failure the job carried on past (continue-on-error).
	Forgiven   bool   `json:"forgiven,omitempty"`
	ExitCode   *int   `json:"exitCode,omitempty"`
	Reason     string `json:"reason,omitempty"`
	DurationMs int64  `json:"durationMs"`
	// Errors and Warnings count this step's problems, kept or not.
	Errors   int `json:"errors"`
	Warnings int `json:"warnings"`
}

// Problem is one thing a step's output named as broken.
//
// Every field but Severity, Message and Tool is there when the tool that
// printed it said it. Help is the tool's own suggestion — a compiler's
// `help:`, a linter's safe fix — and never one latem made up.
type Problem struct {
	Severity string `json:"severity"`
	File     string `json:"file,omitempty"`
	Line     int    `json:"line,omitempty"`
	Column   int    `json:"column,omitempty"`
	// Code is the tool's name for the kind of problem: E0308, TS2322, a
	// lint rule.
	Code    string `json:"code,omitempty"`
	Message string `json:"message"`
	Help    string `json:"help,omitempty"`
	// Test is the failing test's name, for a test failure.
	Test string `json:"test,omitempty"`
	// Tool is which reader recognised it: rustc, go, go-test, cargo-test,
	// tsc, cc, bun-test, vitest, pytest, biome, or annotation for one a step
	// wrote itself with `::error file=…::`.
	Tool string `json:"tool"`
	Step string `json:"step,omitempty"`
}

// Path is where runID's result is kept under the log directory.
func Path(logDir, runID string) (string, error) {
	if err := protocol.ValidateID(runID); err != nil {
		return "", fmt.Errorf("result: refusing to name a result for an invalid run id: %w", err)
	}
	path := filepath.Join(logDir, runID+Suffix)
	if filepath.Dir(path) != filepath.Clean(logDir) {
		return "", fmt.Errorf("result: run id %q escapes the log directory", runID)
	}
	return path, nil
}

// maxFileBytes bounds what Read will take for a result. One written here is
// a few hundred kilobytes at its largest.
const maxFileBytes = 8 << 20

// Read returns runID's result, or an error wrapping [os.ErrNotExist] when the
// run has none: it never started a step, or its log's retention removed it.
func Read(logDir, runID string) (*Result, error) {
	path, err := Path(logDir, runID)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if info.Size() > maxFileBytes {
		return nil, fmt.Errorf("result: %s is %d bytes, which is not a result", path, info.Size())
	}
	// #nosec G304 -- <logs>/<validated run id>.result.json; see Path.
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var r Result
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, fmt.Errorf("result: %s does not parse: %w", path, err)
	}
	return &r, nil
}

// write stores r at its path by rename, so a reader sees the last whole
// result or the one before it, never part of one.
func write(logDir string, r *Result) error {
	path, err := Path(logDir, r.RunID)
	if err != nil {
		return err
	}
	data, err := json.Marshal(r)
	if err != nil {
		return fmt.Errorf("result: encoding: %w", err)
	}
	if err := os.MkdirAll(logDir, 0o700); err != nil {
		return fmt.Errorf("result: creating %s: %w", logDir, err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o600); err != nil {
		return fmt.Errorf("result: writing %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("result: writing %s: %w", path, err)
	}
	return nil
}
