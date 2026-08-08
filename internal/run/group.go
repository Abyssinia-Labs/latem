package run

import "sync"

// groups implements gate 16: at most one run per concurrency_group per machine.
//
// "Per machine" and not per runner is the whole point of the field. RUNNER_SPEC
// §9 gives the iOS example — a group serialises across jobs, so `ios-release`
// and `ios-beta` do not both try to hold the signing keychain — and a
// per-runner lock would let two runners do exactly that.
//
// Gate 16's outcome is "queued — wait, do not fail". Nothing here blocks: a run
// that cannot take its group returns to the queue and is claimed again later,
// which keeps the runner slot free for work that can proceed instead of parking
// a goroutine on a mutex for the length of somebody else's 90 minute build.
type groups struct {
	mu   sync.Mutex
	held map[string]string // group → run id holding it
}

func newGroups() *groups { return &groups{held: map[string]string{}} }

// acquire takes the group for a run, or reports that someone else has it.
// Re-acquiring a group this run already holds succeeds, so a retry after a
// transient failure is not a self-deadlock.
func (g *groups) acquire(group, runID string) bool {
	if group == "" {
		return true
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if holder, ok := g.held[group]; ok && holder != runID {
		return false
	}
	g.held[group] = runID
	return true
}

// release drops a group, but only if this run is the holder. The guard matters
// on the teardown path: a run that failed gate 16 must not release the group
// belonging to the run that beat it.
func (g *groups) release(group, runID string) {
	if group == "" {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.held[group] == runID {
		delete(g.held, group)
	}
}

// holder reports which run holds a group, for diagnostics and tests.
func (g *groups) holder(group string) (string, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	id, ok := g.held[group]
	return id, ok
}
