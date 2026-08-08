package main

import (
	"context"
	"errors"
	"fmt"
	"os/user"
	"time"

	"github.com/Abyssinia-Labs/latem/internal/broker/local"
	"github.com/Abyssinia-Labs/latem/internal/config"
	"github.com/Abyssinia-Labs/latem/internal/logstream"
	"github.com/Abyssinia-Labs/latem/internal/store"
)

// state is the handle a client subcommand holds: the config, the queue, and
// the socket.
//
// All three travel together because RUNNER_SPEC §7.3 makes them one thing. A
// run submitted with an agent up goes over the socket; the identical run with
// no agent up goes straight into sqlite and starts when an agent does. A
// command that held only one of the two could implement half of that.
type state struct {
	cfg    *config.Config
	db     *store.DB
	client *local.Client
}

// openState loads the config and opens the local queue.
//
// # Why the CLI migrates
//
// store.Open deliberately does not migrate, and its comment asks the agent to
// do it at §6 step 3 so a schema change belongs to the process holding the
// flock. That is right, and it cannot be the whole rule: §7.3 requires `latem
// run` to queue a run on a machine where no agent has ever started, and on
// that machine there are no tables to insert into. The migrations are embedded,
// versioned, and applied one transaction per file, and sqlite serialises
// writers — so calling it here costs a version check on the common path and is
// the only thing that makes the agent-down case work at all. Recorded as a
// tension in the store's contract rather than resolved silently.
func openState(ctx context.Context) (*state, func(), error) {
	cfg, err := loadConfig()
	if err != nil {
		return nil, nil, err
	}

	db, err := store.Open(ctx, cfg.Paths.DB, logstream.NewRedactor())
	if err != nil {
		return nil, nil, err
	}
	closeFn := func() { _ = db.Close() }

	if err := db.Migrate(ctx); err != nil {
		closeFn()
		return nil, nil, err
	}

	client, err := local.NewClient(local.ClientOptions{Socket: cfg.Paths.Socket, Store: db})
	if err != nil {
		closeFn()
		return nil, nil, err
	}
	return &state{cfg: cfg, db: db, client: client}, closeFn, nil
}

// queueTTL is the deadline a newly queued run carries (§11, §22).
func (s *state) queueTTL() time.Duration {
	if d := s.cfg.Agent.QueueTTL.Duration(); d > 0 {
		return d
	}
	return config.DefaultQueueTTL
}

// agentDown converts internal/broker/local's sentinel into the CLI's exit
// code. The number is the contract, not the sentence: §7.3 fixes it at 3 so a
// script can tell "the build failed" from "nothing is running".
func (s *state) agentDown(err error) error {
	if errors.Is(err, local.ErrAgentDown) {
		return errAgentDown(s.cfg.Paths.Socket)
	}
	return err
}

// requireAgent refuses when nothing is listening on the socket.
func (s *state) requireAgent(ctx context.Context) error {
	if s.client.AgentRunning(ctx) {
		return nil
	}
	return errAgentDown(s.cfg.Paths.Socket)
}

// currentActor is who to record as having asked, when --actor was not given.
// It is display and audit only: RUNNER_SPEC §7.1 says an origin is never
// trusted for anything.
func currentActor() string {
	if u, err := user.Current(); err == nil && u.Username != "" {
		return u.Username
	}
	return "local"
}

// notFound reports whether err is the store saying there is no such run.
func isNotFound(err error) bool { return errors.Is(err, store.ErrNotFound) }

// noSuchRun is the exit-1 answer the help text promises for `logs`, `cancel`,
// and `approve` against an id nothing knows about.
func noSuchRun(id string) error {
	return fmt.Errorf("no run %s on this machine — `latem list --all` shows what there is", id)
}
