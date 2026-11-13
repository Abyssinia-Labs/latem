package run

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/Abyssinia-Labs/latem/internal/source"
	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

// fakeWarm is what a [fakeWarmMirror] answers and what it was asked.
type fakeWarm struct {
	unsupported string
	err         error
	report      source.WarmReport

	keys     []source.WarmKey
	released int
}

// fakeWarmMirror is a fakeMirror that keeps workspaces (A90): one directory
// per job, which a release leaves where it is.
type fakeWarmMirror struct{ fakeMirror }

var _ WarmMirror = (*fakeWarmMirror)(nil)

func (m *fakeWarmMirror) ReadDir(context.Context, string, string, int, int) (fs.FS, error) {
	if m.src.registry == "" {
		return fstest.MapFS{}, nil
	}
	return fstest.MapFS{".latem/jobs.yaml": {Data: []byte(m.src.registry)}}, nil
}

func (m *fakeWarmMirror) WarmUnsupported(context.Context, string) (string, error) {
	return m.src.warm.unsupported, nil
}

func (m *fakeWarmMirror) WarmCheckout(
	_ context.Context, key source.WarmKey, runID, normalizedRef, sha string,
) (*source.Workspace, source.WarmReport, error) {
	s := m.src
	s.mu.Lock()
	defer s.mu.Unlock()
	s.warm.keys = append(s.warm.keys, key)
	if s.warm.err != nil {
		return nil, source.WarmReport{}, s.warm.err
	}
	path := filepath.Join(s.root, "warm", key.Runner, key.Job)
	if err := os.MkdirAll(filepath.Join(path, ".latem"), 0o750); err != nil {
		return nil, source.WarmReport{}, err
	}
	if err := os.WriteFile(filepath.Join(path, ".latem", "jobs.yaml"), []byte(s.registry), 0o600); err != nil {
		return nil, source.WarmReport{}, err
	}
	return source.KeptWorkspace(path, runID, "app", normalizedRef, sha, func() error {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.warm.released++
		return nil
	}), s.warm.report, nil
}

const warmRegistryYAML = `
version: 1
jobs:
  test:
    runs_on: [linux]
    workspace: warm
    steps:
      - name: test
        run: ./ci/test.sh
  cold:
    runs_on: [linux]
    steps:
      - name: test
        run: ./ci/test.sh
`

func warmHarness(t *testing.T) *harness {
	t.Helper()
	h := newHarness(t)
	h.source.registry = warmRegistryYAML
	h.source.warm = &fakeWarm{}
	return h
}

// A job that says `workspace: warm` builds in the directory its runner keeps
// for it, and the run ending gives the directory back without removing it.
func TestExecute_AWarmJobBuildsInItsKeptWorkspace(t *testing.T) {
	h := warmHarness(t)
	h.source.warm.report = source.WarmReport{PrevRun: "r_earlier", PrevSHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Written: 3, Removed: 1}
	o := h.build()
	req := h.request("test")
	res, err := o.Execute(t.Context(), h.admitAndClaim(o, req))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.State != protocol.StateSucceeded {
		t.Fatalf("state = %s (%s), want succeeded", res.State, res.ErrorMsg)
	}

	warm := h.source.warm
	if len(warm.keys) != 1 || warm.keys[0] != (source.WarmKey{Runner: "docker-1", Job: "test"}) {
		t.Fatalf("warm checkouts = %+v, want one for the job on its runner", warm.keys)
	}
	if len(h.source.checkedOut) != 0 {
		t.Errorf("a warm job also made a fresh clone: %v", h.source.checkedOut)
	}
	want := filepath.Join(h.source.root, "warm", "docker-1", "test")
	if got := h.env.spec.Workspace; got != want {
		t.Errorf("the steps ran in %s, want the kept workspace %s", got, want)
	}
	if warm.released != 1 {
		t.Errorf("the workspace was released %d times, want 1", warm.released)
	}
	if _, err := os.Stat(want); err != nil {
		t.Errorf("the run ending removed the kept workspace: %v", err)
	}
	log := h.logContents(req.ID)
	if !strings.Contains(log, "warm workspace: kept from run r_earlier at aaaaaaaaaaaa") ||
		!strings.Contains(log, "3 tracked file(s) differed") {
		t.Errorf("the log does not say what the warm workspace held:\n%s", log)
	}
}

// A job that does not ask keeps getting §12's fresh clone, on a machine that
// keeps warm workspaces for the jobs that do.
func TestExecute_AJobThatDoesNotAskGetsAFreshClone(t *testing.T) {
	h := warmHarness(t)
	o := h.build()
	req := h.request("cold")
	res, err := o.Execute(t.Context(), h.admitAndClaim(o, req))
	if err != nil || res.State != protocol.StateSucceeded {
		t.Fatalf("Execute: %v, state %s (%s)", err, res.State, res.ErrorMsg)
	}
	if len(h.source.warm.keys) != 0 {
		t.Errorf("a job with no `workspace: warm` was given one: %+v", h.source.warm.keys)
	}
	if len(h.source.checkedOut) != 1 {
		t.Errorf("fresh checkouts = %v, want one", h.source.checkedOut)
	}
}

// What a warm workspace cannot keep, and a machine that keeps none: the job
// builds in a fresh clone and its log says why.
func TestExecute_AWarmJobFallsBackToAFreshCloneAndSaysWhy(t *testing.T) {
	for _, tc := range []struct {
		name string
		set  func(*fakeWarm)
		want string
	}{
		{"submodules", func(w *fakeWarm) { w.unsupported = "the repository has submodules" },
			"asks for a warm workspace, and the repository has submodules"},
		{"no warm dir", func(w *fakeWarm) { w.err = source.ErrWarmUnavailable },
			"asks for a warm workspace, and this agent keeps none"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := warmHarness(t)
			tc.set(h.source.warm)
			o := h.build()
			req := h.request("test")
			res, err := o.Execute(t.Context(), h.admitAndClaim(o, req))
			if err != nil || res.State != protocol.StateSucceeded {
				t.Fatalf("Execute: %v, state %s (%s)", err, res.State, res.ErrorMsg)
			}
			if len(h.source.checkedOut) != 1 {
				t.Errorf("fresh checkouts = %v, want one", h.source.checkedOut)
			}
			if log := h.logContents(req.ID); !strings.Contains(log, tc.want) {
				t.Errorf("the log does not say why the build is not warm; want %q in:\n%s", tc.want, log)
			}
		})
	}
}

// A warm workspace that cannot be had for any other reason fails the run: a
// job that waited its whole budget for its turn has not built anything.
func TestExecute_AWarmWorkspaceThatFailsIsESource(t *testing.T) {
	h := warmHarness(t)
	h.source.warm.err = context.DeadlineExceeded
	o := h.build()
	req := h.request("test")
	res, err := o.Execute(t.Context(), h.admitAndClaim(o, req))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.State != protocol.StateFailed || res.ErrorCode != protocol.ErrSource {
		t.Fatalf("state = %s, code %s; want failed, E_SOURCE", res.State, res.ErrorCode)
	}
	if len(h.source.checkedOut) != 0 {
		t.Errorf("a failed warm checkout was followed by a fresh one: %v", h.source.checkedOut)
	}
}
