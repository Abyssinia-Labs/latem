//go:build integration

package integration

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

// checkoutWorkflow moves the working tree twice and checks what each step
// sees, as a build script would; `shell: sh` because the test image has no
// bash. The checkout steps themselves run as the job's own git (A53), so on
// docker they are the container's git, not the host's.
const checkoutWorkflow = `on: [push]
jobs:
  co:
    runs-on: [test]
    steps:
      - uses: actions/checkout@v5
      - shell: sh
        run: |
          test "$(git symbolic-ref HEAD)" = refs/heads/main
          test "$(git rev-parse --abbrev-ref '@{upstream}')" = origin/main
          test "$(cat marker.txt)" = two
      - uses: actions/checkout@v5
        with:
          ref: v1.0.0
          clean: false
          fetch-depth: 0
      - shell: sh
        run: |
          test "$(cat marker.txt)" = one
          if git symbolic-ref -q HEAD; then echo "a tag left HEAD on a branch"; exit 1; fi
          test -z "$(git config --get include.path)"
  sneak:
    runs-on: [test]
    steps:
      - uses: actions/checkout@v5
        with:
          ref: feature
      - shell: sh
        run: echo SHOULD-NOT-RUN
`

// TestIntegration_CheckoutMovesTheTreeNotTheJob is A53 on both executors:
// the default checkout lands on the run's branch with its upstream, `ref:`
// reaches a tag in main's history and detaches, and a branch the policy does
// not admit fails the job before any of it runs.
func TestIntegration_CheckoutMovesTheTreeNotTheJob(t *testing.T) {
	for _, kind := range []string{"docker", "native"} {
		t.Run(kind, func(t *testing.T) {
			m := newMachine(t, machineOptions{
				jobs:         fullRunJobs,
				executorKind: kind,
				files: map[string]string{
					"marker.txt":               "one\n",
					".github/workflows/co.yml": checkoutWorkflow,
				},
			})
			// The tag stays at "one"; main moves on to "two"; a branch the
			// policy does not name moves to "three".
			writeFile(t, filepath.Join(m.repo, "marker.txt"), "two\n")
			git(t, m.repo, "commit", "-q", "-am", "main moves on")
			git(t, m.repo, "checkout", "-q", "-b", "feature")
			writeFile(t, filepath.Join(m.repo, "marker.txt"), "three\n")
			git(t, m.repo, "commit", "-q", "-am", "not reviewed")
			git(t, m.repo, "checkout", "-q", "main")

			m.submit(t, "rco", "co", "main", nil)
			r := m.terminal(t, "rco", 5*time.Minute)
			if r.State != protocol.StateSucceeded {
				t.Fatalf("run finished %s (%s): %s\n--- log ---\n%s", r.State, r.ErrorCode, r.ErrorMsg, m.log("rco"))
			}
			if log := m.log("rco"); !strings.Contains(log, "checkout: ref v1.0.0 → refs/tags/v1.0.0 at") {
				t.Errorf("the log does not record the checkout:\n%s", log)
			}

			m.submit(t, "rsneak", "sneak", "main", nil)
			s := m.terminal(t, "rsneak", 5*time.Minute)
			if s.State != protocol.StateFailed || s.ErrorCode != protocol.ErrStep {
				t.Fatalf("checking out an unadmitted branch finished %s (%s): %s", s.State, s.ErrorCode, s.ErrorMsg)
			}
			if strings.Contains(m.log("rsneak"), "SHOULD-NOT-RUN") {
				t.Error("a step after the refused checkout ran")
			}
		})
	}
}
