package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/Abyssinia-Labs/latem/internal/config"
)

// ReloadStatus is the agent's account of its config, kept in
// [config.Paths.ReloadStatus] (A54).
//
// It is written when the agent is ready and again after every reload, and it
// exists for two readers. `latem service reload` reads it to say what a SIGHUP
// applied — a signal carries no answer, and "sent" is not the thing an operator
// who has just added a repo wants to know. And it is how that command knows
// the process holding the lock reloads at all: an agent built before SIGHUP
// was wired takes Go's default action for the signal, which is to exit, so
// signalling one would stop a build machine in order to add a repo to it.
type ReloadStatus struct {
	// PID is the agent that wrote this. A status whose pid is not the lock
	// holder's is left over from an earlier process and says nothing.
	PID int `json:"pid"`
	// Started is when that agent became ready.
	Started time.Time `json:"started"`
	// Loaded is when that agent read config.toml as it started, or a moment
	// before (A80). Started is no substitute: it comes after a startup that
	// can take minutes, and an edit made during it is in no config the agent
	// has. An agent built before A80 writes none.
	Loaded time.Time `json:"loaded,omitzero"`
	// Reloads counts the SIGHUPs it has acted on, so a reader can tell its
	// own reload's outcome from the one before.
	Reloads int `json:"reloads"`
	// At is when this status was written.
	At time.Time `json:"at"`
	// Error is why the last reload was refused. The agent kept the config it
	// had (§8).
	Error string `json:"error,omitempty"`
	// Deferred lists what the last reload read and did not apply, because it
	// needs a restart.
	Deferred []string `json:"deferred,omitempty"`

	// Version is what this agent runs, and Binary the file it runs, symlinks
	// resolved (A56). A status with a version is also the agent's word that it
	// handles SIGUSR2: one written before A56 has none.
	Version string `json:"version,omitempty"`
	Binary  string `json:"binary,omitempty"`
	// Switch is the answer to the last SIGUSR2, if there has been one.
	Switch *SwitchState `json:"switch,omitempty"`
}

// maxReloadStatusBytes bounds the read. The file is the agent's own, but a
// reader that trusted its size would be trusting whatever wrote it last.
const maxReloadStatusBytes = 64 << 10

// ReadReloadStatus reads the status an agent last wrote at path.
func ReadReloadStatus(path string) (ReloadStatus, error) {
	var st ReloadStatus
	// #nosec G304 -- path is config.Paths.ReloadStatus, under the operator's
	// own 0700 config directory.
	f, err := os.Open(path)
	if err != nil {
		return st, err
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, maxReloadStatusBytes+1))
	if err != nil {
		return st, err
	}
	if len(data) > maxReloadStatusBytes {
		return st, fmt.Errorf("%s is larger than %d bytes", path, maxReloadStatusBytes)
	}
	if err := json.Unmarshal(data, &st); err != nil {
		return st, fmt.Errorf("reading %s: %w", path, err)
	}
	return st, nil
}

// errNotReady is a SIGHUP that arrived while the agent was still starting.
var errNotReady = errors.New("agent: still starting; the config it is starting with is the one on disk")

// current is the live config: the one the agent started with, or the last one
// a reload applied. Restart-only fields are the same in both — the reloader
// carries them forward — so reading one through here is never wrong, only
// unnecessary.
func (a *Agent) current() *config.Config {
	if c := a.live.Load(); c != nil {
		return c
	}
	return a.cfg
}

// Reload re-reads config.toml and applies what can change without a restart:
// the repo allowlist with each repo's refs, vars and environments,
// [label_aliases], [actions], and log retention (§8, A54).
//
// It returns what it read and did not apply, and an error when the file would
// not load — in which case nothing changed. Either way the outcome is logged
// and written to [config.Paths.ReloadStatus]. A reload is never fatal: an
// operator mid-edit has a typo in their file, and killing builds in flight over
// it would be the worst available answer.
//
// Runs already admitted keep what they were admitted under. A reload decides
// what the next admission, the next plan and the next heartbeat see.
func (a *Agent) Reload() ([]string, error) {
	a.reloadMu.Lock()
	defer a.reloadMu.Unlock()
	if a.startedAt.IsZero() {
		a.log.Info("SIGHUP while the agent is starting; it is starting with the config on disk")
		return nil, errNotReady
	}

	if a.reloader == nil {
		a.reloader = config.NewReloader(a.current())
	}
	next, deferred, err := a.reloader.Reload()
	a.reloads++
	if err != nil {
		a.log.Error("config reload refused; the agent keeps the config it had", "err", err)
		a.writeReloadStatus(err, nil)
		return nil, err
	}

	a.live.Store(next)
	if a.orch != nil {
		a.orch.SetConfig(next)
	}

	if len(deferred) > 0 {
		a.log.Warn("config reloaded; some changes need a restart (`latem service restart`)",
			"repos", len(next.Repos), "not_applied", deferred)
	} else {
		a.log.Info("config reloaded", "repos", len(next.Repos))
	}
	a.writeReloadStatus(nil, deferred)
	// A runner's path_prepend decides where a step finds its tools (A60).
	a.reprobeToolchains()
	return deferred, nil
}

// startReloads marks the agent as one that reloads, once [Agent.start] has
// built everything a reload touches, and advertises it in the status file.
func (a *Agent) startReloads() {
	a.reloadMu.Lock()
	defer a.reloadMu.Unlock()
	a.startedAt = a.now()
	if bin, err := runningBinary(); err == nil {
		a.binary = bin
	}
	a.writeReloadStatus(nil, nil)
}

// writeReloadStatus records the agent's config state for `latem service
// reload`. reloadMu is held. A failure is logged and otherwise ignored: the
// status is a report about the agent, and an agent that could not write a
// report has still reloaded.
func (a *Agent) writeReloadStatus(reloadErr error, deferred []string) {
	a.reloadErr, a.deferred = "", deferred
	if reloadErr != nil {
		a.reloadErr = reloadErr.Error()
	}
	a.writeStatus()
}

// writeStatus writes the whole status: the last reload's outcome, what this
// agent runs, and its answer to the last SIGUSR2. reloadMu is held.
func (a *Agent) writeStatus() {
	path := a.cfg.Paths.ReloadStatus
	if path == "" {
		return
	}
	st := ReloadStatus{
		PID:      os.Getpid(),
		Started:  a.startedAt,
		Loaded:   a.opts.ConfigLoaded,
		Reloads:  a.reloads,
		At:       a.now(),
		Error:    a.reloadErr,
		Deferred: a.deferred,
		Version:  a.version,
		Binary:   a.binary,
		Switch:   a.switchState,
	}
	data, err := json.Marshal(st)
	if err != nil {
		a.log.Warn("encoding the reload status", "err", err)
		return
	}
	if err := writeAtomic(path, append(data, '\n')); err != nil {
		a.log.Warn("writing the reload status", "path", path, "err", err)
	}
}

// removeReloadStatus withdraws the status as the agent stops, so that a
// stopped agent never appears to be one that would answer a SIGHUP.
func (a *Agent) removeReloadStatus() {
	if path := a.cfg.Paths.ReloadStatus; path != "" {
		if st, err := ReadReloadStatus(path); err == nil && st.PID == os.Getpid() {
			_ = os.Remove(path)
		}
	}
}

// writeAtomic replaces path through a temporary file and a rename, so a reader
// sees the old status or the new one and never half of either.
func writeAtomic(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".agent.reload-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer func() { _ = os.Remove(name) }() // a no-op once renamed
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}

// Why [SignalReload] declined to signal. Each is a different answer for the
// operator, so each is its own sentinel.
var (
	// ErrNoAgent: nothing holds the lock. The next agent to start reads the
	// file as it is, so there is nothing to reload.
	ErrNoAgent = errors.New("no agent is running against this config directory")
	// ErrCannotReload: an agent holds the lock and has written no status
	// naming itself, so it predates A54 — and SIGHUP would stop it.
	ErrCannotReload = errors.New("the running agent does not reload its config")
)

// DefaultReloadWait is how long [SignalReload] waits for the agent's answer. A
// reload reads one file and swaps a pointer; an agent that has not answered in
// ten seconds is not going to, and its log is the place to look.
const DefaultReloadWait = 10 * time.Second

// SignalReload asks the agent holding paths.Lock to reload its config, and
// returns what it reported doing (A54).
//
// It signals only an agent that has said it reloads. The lock proves which
// process is the agent — a pid file alone could name a process that has since
// died and whose pid now belongs to something else — and the status file,
// written by that same pid, proves it handles SIGHUP rather than exiting on it.
//
// A returned status whose Error is set means the agent refused the file and
// kept the config it had.
func SignalReload(paths config.Paths, wait time.Duration) (ReloadStatus, error) {
	pid, held, err := Holder(paths.Lock)
	if err != nil {
		return ReloadStatus{}, err
	}
	if !held {
		return ReloadStatus{}, ErrNoAgent
	}
	before, err := ReadReloadStatus(paths.ReloadStatus)
	if err != nil || pid == 0 || before.PID != pid {
		return ReloadStatus{}, fmt.Errorf("%w (pid %d)", ErrCannotReload, pid)
	}
	if err := sendReload(pid); err != nil {
		return ReloadStatus{}, fmt.Errorf("signalling agent pid %d: %w", pid, err)
	}

	if wait <= 0 {
		wait = DefaultReloadWait
	}
	deadline := time.Now().Add(wait)
	for time.Now().Before(deadline) {
		time.Sleep(25 * time.Millisecond)
		st, err := ReadReloadStatus(paths.ReloadStatus)
		if err == nil && st.PID == pid && st.Reloads > before.Reloads {
			return st, nil
		}
	}
	return ReloadStatus{}, fmt.Errorf("agent pid %d was signalled and did not report a reload within %s; its log says why", pid, wait)
}
