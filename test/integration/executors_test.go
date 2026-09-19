//go:build integration

package integration

// The two §24 cases that are about more than one thing at a time: the step
// contract holding identically across two executors, and a cache surviving
// concurrent runs on one runner.

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

// stateJobs is finding F1, written as a job.
//
// Draft 01 claimed `cd`, `export` and installed tools all persist across
// steps. Only the last is true, and shipping the claim would have generated a
// bug report from the first user who wrote `export PATH=…` in one step and
// read it in the next.
const stateJobs = `version: 1

jobs:
  state:
    runs_on: [test]
    timeout: 5m
    artifacts:
      - path: dist/*.txt
    steps:
      - name: leave state behind
        run: |
          mkdir -p dist
          echo persisted > dist/marker.txt
          export LEAKED_VAR=leaked
          LEAKED_SHELL_VAR=also-leaked
          leaked_function() { echo no; }
          alias leaked_alias=true
          cd /tmp
      - name: look for it
        run: |
          mkdir -p dist
          pwd > dist/pwd.txt
          if [ -n "$LEAKED_VAR" ]; then echo leaked > dist/exported.txt; else echo unset > dist/exported.txt; fi
          if [ -n "$LEAKED_SHELL_VAR" ]; then echo leaked > dist/shellvar.txt; else echo unset > dist/shellvar.txt; fi
          if command -v leaked_function > /dev/null 2>&1; then echo present > dist/function.txt; else echo absent > dist/function.txt; fi
          if [ -f dist/marker.txt ]; then echo present > dist/filesystem.txt; else echo absent > dist/filesystem.txt; fi
`

// Note for a reader wondering why none of the jobs in this package uses
// ${VAR:-default}: internal/registry rejects every ${...} in steps[].run, not
// only ${input.NAME}. §9 confines the interpolation syntax to steps[].env and
// the validator implements "any expansion at all", so ordinary POSIX parameter
// expansion — ${FOO:-bar}, ${HOME}, ${VAR%%.*} — fails the registry with
// E_BAD_REGISTRY. Reported, not worked around anywhere but here.

// stateAnswers is what one executor said.
type stateAnswers struct {
	exported   string
	shellVar   string
	function   string
	filesystem string
	pwd        string
}

// TestIntegration_ShellStateDoesNotLeakButFilesystemStateDoes runs the same job
// on the docker executor and on the native executor and requires the same
// answers.
//
// §9 says the step contract is "identical across all three executors", and
// that sentence is the only reason a job author can move a repository from a
// Linux runner to a Mac. It is also the sentence nothing enforces:
// internal/executor/docker asserts the contract against docker,
// internal/executor/native asserts it against native, and neither can notice
// that the two now disagree — which is what §27 records as the duplicated
// $LATEM_ENV handling waiting to drift.
//
// So this asserts the answers, and then asserts that they match.
func TestIntegration_ShellStateDoesNotLeakButFilesystemStateDoes(t *testing.T) {
	requireDocker(t)

	answers := map[string]stateAnswers{}
	for _, kind := range []string{"docker", "native"} {
		t.Run(kind, func(t *testing.T) {
			m := newMachine(t, machineOptions{jobs: stateJobs, executorKind: kind})
			id := "rstate" + kind

			m.submit(t, id, "state", "main", nil)
			r := m.terminal(t, id, 5*time.Minute)
			if r.State != protocol.StateSucceeded {
				t.Fatalf("run finished %s (%s): %s\n--- log ---\n%s",
					r.State, r.ErrorCode, r.ErrorMsg, m.log(id))
			}

			a := stateAnswers{
				exported:   read(t, m, id, "dist/exported.txt"),
				shellVar:   read(t, m, id, "dist/shellvar.txt"),
				function:   read(t, m, id, "dist/function.txt"),
				filesystem: read(t, m, id, "dist/filesystem.txt"),
				pwd:        read(t, m, id, "dist/pwd.txt"),
			}

			// Each step is a new process (§9, finding F1).
			if a.exported != "unset" {
				t.Errorf("an exported variable survived into the next step: %q", a.exported)
			}
			if a.shellVar != "unset" {
				t.Errorf("a shell variable survived into the next step: %q", a.shellVar)
			}
			if a.function != "absent" {
				t.Error("a shell function survived into the next step")
			}
			// And every step starts in the workspace root regardless of what
			// the previous one did.
			if a.pwd == "/tmp" {
				t.Error("the working directory survived a `cd /tmp`")
			}
			// The filesystem persists for the whole run, which is the half
			// that must keep working. A test asserting only the leaks would
			// pass against an executor that gave every step a fresh workspace.
			if a.filesystem != "present" {
				t.Error("a file written by step 1 was gone in step 2; the filesystem is " +
					"supposed to persist for the whole run (§9)")
			}

			answers[kind] = a
		})
	}

	d, okD := answers["docker"]
	n, okN := answers["native"]
	if !okD || !okN {
		t.Skip("one of the two executors did not report; the comparison below needs both")
	}
	if d.exported != n.exported || d.shellVar != n.shellVar ||
		d.function != n.function || d.filesystem != n.filesystem {
		t.Errorf("the two executors disagree about §9's step contract:\n docker: %+v\n native: %+v\n"+
			"a job author moving a repository between runners gets different behaviour, which is "+
			"exactly what \"identical across all three executors\" forbids", d, n)
	}
}

// cacheJobs shares one cache between concurrent runs.
//
// The cache name carries a nonce and the volume is removed at the end of the
// test. Both are necessary and the reason is §14 itself: a cache is mounted,
// not archived, and these machines are not ephemeral — so the docker volume
// outlives the test process. Without the nonce, a second run of this suite
// finds the first run's files still in /cache, every listing contains every
// other run id, and the concurrency control below passes without two runs ever
// having overlapped. Which is how it was written the first time, and it took a
// full-suite run to notice.
func cacheJobs(cacheName string) string {
	return `version: 1

caches:
  - name: ` + cacheName + `
    path: /cache

jobs:
  writer:
    runs_on: [test]
    timeout: 5m
    caches: [` + cacheName + `]
    artifacts:
      - path: dist/*.txt
    steps:
      - name: write a file only this run owns
        run: |
          mkdir -p dist /cache
          : > "/cache/$LATEM_RUN_ID.txt"
          for i in 1 2 3 4 5 6 7 8 9 10; do
            echo "$LATEM_RUN_ID line $i" >> "/cache/$LATEM_RUN_ID.txt"
            sleep 0.05
          done
      - name: report what the cache holds
        run: |
          mkdir -p dist
          ls /cache > dist/listing.txt
          cat "/cache/$LATEM_RUN_ID.txt" > dist/mine.txt
          wc -l < "/cache/$LATEM_RUN_ID.txt" > dist/lines.txt
`
}

// TestIntegration_ConcurrentRunsDoNotCorruptASharedCache.
//
// §14.3's claim is that a cache is mounted, not archived: there is no save
// step and no restore step, so concurrent runs share the live directory and
// the agent's job is to hand each of them the same one without losing anybody's
// writes. The interleaved appends and the sleep are there to make the runs
// genuinely overlap rather than to queue politely one after another.
//
// The assertion is per run rather than on the aggregate, because a corrupted
// cache does not usually lose everything — it loses one run's file, or half of
// one, and an aggregate byte count would not notice.
func TestIntegration_ConcurrentRunsDoNotCorruptASharedCache(t *testing.T) {
	const runs = 3

	nonce := fmt.Sprintf("it%d", time.Now().UnixNano()%1e9)
	m := newMachine(t, machineOptions{jobs: cacheJobs(nonce), concurrency: runs})
	t.Cleanup(func() {
		// The runner is named "it" in the harness, so §14.1's
		// latem-cache-<runner>-<cache> is this.
		_, _ = execDocker(t, "volume", "rm", "-f", "latem-cache-it-"+nonce)
	})

	ids := make([]string, runs)
	for i := range ids {
		ids[i] = fmt.Sprintf("r%sc%d", nonce, i)
	}

	var wg sync.WaitGroup
	for _, id := range ids {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			m.submit(t, id, "writer", "main", nil)
		}(id)
	}
	wg.Wait()

	overlapped := false
	for _, id := range ids {
		r := m.terminal(t, id, 6*time.Minute)
		if r.State != protocol.StateSucceeded {
			t.Fatalf("%s finished %s (%s): %s\n--- log ---\n%s",
				id, r.State, r.ErrorCode, r.ErrorMsg, m.log(id))
		}

		// Its own file is intact: ten lines, all its own.
		if got := strings.TrimSpace(read(t, m, id, "dist/lines.txt")); got != "10" {
			t.Errorf("%s wrote %s lines into the shared cache, want 10:\n%s",
				id, got, read(t, m, id, "dist/mine.txt"))
		}
		mine := read(t, m, id, "dist/mine.txt")
		for _, line := range strings.Split(strings.TrimSpace(mine), "\n") {
			if !strings.HasPrefix(line, id+" line ") {
				t.Errorf("%s's cache file contains a line it did not write: %q", id, line)
			}
		}

		// At least one run must have seen another's file, or the runner
		// serialised them and the test measured nothing. This is the control:
		// three runs that happened to execute one after another would satisfy
		// every assertion above while proving nothing about concurrency.
		listing := read(t, m, id, "dist/listing.txt")
		for _, other := range ids {
			if other != id && strings.Contains(listing, other) {
				overlapped = true
			}
		}
	}

	if !overlapped {
		t.Error("no run ever saw another run's file in the shared cache; either they did not " +
			"overlap or they were not given the same cache, and in both cases this test did not " +
			"exercise concurrency")
	}
}

func read(t *testing.T, m *machine, runID, rel string) string {
	t.Helper()
	return strings.TrimSpace(readArtifact(t, m, runID, rel))
}
