//go:build integration

package integration

// §17's containment rules, and the §27 divergence that is still open.

import (
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/Abyssinia-Labs/latem/internal/artifact"
	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

// escapeJobs is the pair of escapes that only exist at run time: a symlink is
// something a build writes, so the registry parses cleanly and the refusal has
// to come from the collector.
const escapeJobs = `version: 1

jobs:
  symlink:
    runs_on: [test]
    timeout: 5m
    artifacts:
      - path: dist/*
    steps:
      - name: write a convenience symlink out of the workspace
        run: |
          mkdir -p dist
          echo real > dist/real.txt
          ln -s /etc dist/escape

  symlinkfile:
    runs_on: [test]
    timeout: 5m
    artifacts:
      - path: dist/*.conf
    steps:
      - name: point a matching name at a file outside
        run: |
          mkdir -p dist
          ln -s /etc/hosts dist/stolen.conf
`

// textEscapeJobs is one job whose artifact glob escapes by its own text.
//
// It is a separate registry per case, and that is a finding rather than a
// stylistic choice: the registry is validated as a whole, so a single bad
// artifact path fails every job in the file. Bundling these with the symlink
// jobs above meant the symlink jobs never ran at all and their subtests were
// asserting against E_BAD_REGISTRY from a different job — a fixture that
// looked like it was testing four things and was testing two.
func textEscapeJobs(path string) string {
	return `version: 1

jobs:
  escape:
    runs_on: [test]
    timeout: 5m
    artifacts:
      - path: ` + path + `
    steps:
      - name: nothing
        run: 'true'
`
}

// TestIntegration_ArtifactGlobEscapesAreRejected.
//
// §17 makes this a security boundary rather than a file copier: the glob comes
// from a repository, the workspace contains whatever the build wrote, and a
// symlink is a perfectly ordinary thing for a build to produce. The failure
// mode the package comment names is the real one — "a build tool writes a
// convenience symlink, a glob picks it up, and the artifact archive quietly
// acquires the contents of somebody's home directory".
//
// An escape must *fail the run* rather than be skipped. A silent skip means an
// operator believes an artifact was collected when it was not, and it means a
// symlink to ~/.ssh/id_rsa in a build output directory produces no signal at
// all.
func TestIntegration_ArtifactGlobEscapesAreRejected(t *testing.T) {
	cases := []struct {
		name string
		jobs string
		job  string
		id   string
		what string
		// code is the §21 code the refusal must carry. A glob that escapes by
		// its own text is caught when the registry is parsed, before anything
		// runs; one that escapes through the filesystem is caught when the
		// match is resolved, during collecting. Both are refusals and they are
		// not the same refusal, and a test that accepted either would pass
		// against a collector that had stopped checking.
		code protocol.ErrorCode
	}{
		{"symlink to a directory", escapeJobs, "symlink", "rescdir",
			"a symlink to a directory outside the workspace", protocol.ErrArtifact},
		{"symlink to a file", escapeJobs, "symlinkfile", "rescfile",
			"a symlink to a file outside the workspace", protocol.ErrArtifact},
		{"glob containing ..", textEscapeJobs("../../../etc/passwd"), "escape", "rescdots",
			"a glob containing ..", protocol.ErrBadRegistry},
		{"absolute glob", textEscapeJobs("/etc/passwd"), "escape", "rescabs",
			"an absolute glob", protocol.ErrBadRegistry},
		{"glob escaping through a nested ..", textEscapeJobs("dist/../../outside/*"), "escape",
			"rescnested", "a glob whose .. is not leading", protocol.ErrBadRegistry},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := newMachine(t, machineOptions{jobs: c.jobs})
			m.submit(t, c.id, c.job, "main", nil)
			r := m.terminal(t, c.id, 5*time.Minute)

			if r.State == protocol.StateSucceeded {
				t.Fatalf("%s succeeded; §17 says an escape is a hard failure rather than a "+
					"skipped file. collected: %v", c.what, m.artifactPaths(t, c.id))
			}
			// The code matters as much as the verdict. A run that failed for
			// some unrelated reason — a missing image, a shell that is not
			// there — would satisfy "did not succeed" while proving nothing
			// about containment.
			if r.ErrorCode != c.code {
				t.Errorf("%s failed with %q (%s), want %q", c.what, r.ErrorCode, r.ErrorMsg, c.code)
			}

			// Nothing from outside may have been stored, whatever the run's
			// verdict. This is the assertion that matters: a refusal that had
			// already copied /etc/hosts would be a refusal after the fact.
			for _, ref := range m.artifacts(t, c.id) {
				if strings.Contains(ref.Path, "..") || strings.HasPrefix(ref.Path, "/") ||
					strings.Contains(ref.Path, "passwd") || strings.Contains(ref.Path, "hosts") ||
					strings.Contains(ref.Path, "escape") || strings.Contains(ref.Path, "stolen") {
					t.Errorf("%s produced the artifact %q", c.what, ref.Path)
				}
			}
			assertNothingOutsideWasStored(t, m, c.id)

			// A glob that escapes by its own text is caught when the registry
			// is parsed, which is gate 11 — before gate 18 and before anything
			// is provisioned. So a text escape must cost no container at all,
			// while a symlink escape necessarily costs one because the build
			// had to run to write the link.
			prepared := m.prepared.prepares()
			if c.code == protocol.ErrBadRegistry && prepared != 0 {
				t.Errorf("a glob rejected at registry parse still provisioned %d environment(s); "+
					"§10 rejects before anything starts", prepared)
			}
			if c.code == protocol.ErrArtifact && prepared == 0 {
				t.Error("the symlink case provisioned nothing, so the build never ran and the " +
					"collector was never asked the question this test is about")
			}
		})
	}
}

// assertNothingOutsideWasStored walks whatever did land on disk.
//
// The database is the agent's account of what it collected; this is the
// filesystem's. They can disagree — a copy that succeeded and then failed
// containment would leave the file with no row — and the filesystem is the one
// that matters when the file is /etc/shadow.
func assertNothingOutsideWasStored(t *testing.T, m *machine, runID string) {
	t.Helper()
	for _, base := range []string{
		filepath.Join(m.cfg.Paths.Artifacts, runID),
		filepath.Join(m.cfg.Paths.Artifacts, "artifacts", runID),
	} {
		_ = filepath.WalkDir(base, func(path string, _ os.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			body, rerr := os.ReadFile(path)
			if rerr != nil {
				return nil
			}
			if strings.Contains(string(body), "root:") || strings.Contains(string(body), "localhost") {
				t.Errorf("%s appears to hold the contents of a file outside the workspace", path)
			}
			return nil
		})
	}
}

// TestIntegration_ArtifactsAreWhereRetentionLooksForThem replaces a
// characterisation test for a defect that has since been fixed.
//
// The defect: internal/agent passed cfg.Paths.Artifacts to
// artifact.NewCollector, which appended "artifacts" again, so every file landed
// in ~/.latem/artifacts/artifacts/<run-id>/. The consequence was not the ugly
// path — it was that the daily retention sweep prunes cfg.Paths.Artifacts, one
// level above anything ever written, so §17 retention and the 50 GB budget
// silently reclaimed nothing on a machine whose .ipa files are 50-300 MB each.
//
// So this asserts the property that actually matters: a collected artifact sits
// where the sweep looks. Asserting only the path would pass again the next time
// the two drift in the same direction.
func TestIntegration_ArtifactsAreWhereRetentionLooksForThem(t *testing.T) {
	m := newMachine(t, machineOptions{jobs: fullRunJobs})

	m.submit(t, "rpath", "build", "main", nil)
	r := m.terminal(t, "rpath", 5*time.Minute)
	if r.State != protocol.StateSucceeded {
		t.Fatalf("run finished %s (%s): %s", r.State, r.ErrorCode, r.ErrorMsg)
	}

	want := filepath.Join(m.cfg.Paths.Artifacts, "rpath", "dist", "one.txt")
	if _, err := os.Stat(want); err != nil {
		doubled := filepath.Join(m.cfg.Paths.Artifacts, "artifacts", "rpath", "dist", "one.txt")
		if _, dErr := os.Stat(doubled); dErr == nil {
			t.Fatalf("the doubled-segment defect is back: the artifact is at %s, "+
				"one level below the directory retention prunes", doubled)
		}
		t.Fatalf("no artifact at %s: %v", want, err)
	}

	// The sweep's own view: everything it would consider must be reachable from
	// the directory it is given. A run directory found here is one whose
	// artifacts can expire; one found deeper is one that lives forever.
	entries, err := os.ReadDir(m.cfg.Paths.Artifacts)
	if err != nil {
		t.Fatalf("reading the artifact directory retention sweeps: %v", err)
	}
	var runDirs []string
	for _, e := range entries {
		if e.IsDir() {
			runDirs = append(runDirs, e.Name())
		}
	}
	if len(runDirs) != 1 || runDirs[0] != "rpath" {
		t.Fatalf("the retention sweep sees %v directly under %s; it should see exactly "+
			"the run id, because that is what it ages out", runDirs, m.cfg.Paths.Artifacts)
	}
}

// TestIntegration_ADirectoryInArtifactsFailsTheRun asserts the decision §17
// used to leave open.
//
// The history is worth keeping, because it took three shapes. Originally
// internal/artifact.Resolve expanded a bare directory to the files beneath it
// while both executors' Env.Fetch returned nothing for one — so `artifacts:
// [dist]` collected different files depending on which path ran, and a test
// here characterised the disagreement. Amendment A29 removed Env.Fetch, which
// left one implementation and so one answer. A30 then settled which answer:
// neither. A bare directory is E_BAD_REGISTRY.
//
// Both silent readings were worse than failing. Expanding is unbounded against
// a 50 GB budget evicted oldest-first, so one `artifacts: [dist]` over a
// directory of build intermediates quietly deletes *other runs'* artifacts —
// and the cost lands somewhere the author never looks. Matching nothing leaves
// an operator believing a release was collected when it was not. Failing in
// seconds beats both, whichever the author meant.
func TestIntegration_ADirectoryInArtifactsFailsTheRun(t *testing.T) {
	ws := t.TempDir()
	for rel, body := range map[string]string{
		"dist/one.txt":       "one",
		"dist/two.txt":       "two",
		"dist/sub/three.txt": "three",
	} {
		full := filepath.Join(ws, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	// The declaration a job author is most likely to write.
	if _, err := artifact.Resolve(ws, "dist"); err == nil {
		t.Fatal("`artifacts: [dist]` was accepted; A30 makes it E_BAD_REGISTRY")
	} else {
		if got := artifact.Code(err); got != protocol.ErrBadRegistry {
			t.Errorf("code = %s, want %s — the fix is a line in jobs.yaml, so a "+
				"control plane must not offer this as a retry", got, protocol.ErrBadRegistry)
		}
		for _, want := range []string{"dist/**", "dist/*"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("the message does not name %q as the fix: %v", want, err)
			}
		}
	}

	// And the two spellings it points at behave as the doc has always claimed:
	// a double star crosses separators, a single star does not.
	deep, err := artifact.Resolve(ws, "dist/**")
	if err != nil {
		t.Fatalf("Resolve(dist/**): %v", err)
	}
	shallow, err := artifact.Resolve(ws, "dist/*")
	if err != nil {
		t.Fatalf("Resolve(dist/*): %v", err)
	}

	got, want := relsOf(deep), []string{"dist/one.txt", "dist/sub/three.txt", "dist/two.txt"}
	if !slices.Equal(got, want) {
		t.Errorf("dist/** = %v, want %v", got, want)
	}
	got, want = relsOf(shallow), []string{"dist/one.txt", "dist/two.txt"}
	if !slices.Equal(got, want) {
		t.Errorf("dist/* = %v, want %v — a single star must not cross a separator", got, want)
	}
}

func relsOf(ms []artifact.Match) []string {
	out := make([]string, 0, len(ms))
	for _, m := range ms {
		out = append(out, m.Rel)
	}
	sort.Strings(out)
	return out
}

// ---------------------------------------------------------------------------
