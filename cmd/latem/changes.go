package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/Abyssinia-Labs/latem/internal/broker/local"
	"github.com/Abyssinia-Labs/latem/internal/config"
	"github.com/Abyssinia-Labs/latem/internal/registry"
	"github.com/Abyssinia-Labs/latem/internal/source"
	"github.com/Abyssinia-Labs/latem/internal/store"
	"github.com/Abyssinia-Labs/latem/internal/workflow"
	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

// A changes run (A91) builds a commit this machine already has with a set of
// files changed on top: what `latem try` is to someone with a checkout, for a
// caller that has only the files. `latem run --changes` takes them from a
// person, and `latem mcp`'s startRun from an agent, in the same JSON.
//
// The command makes the commit in the agent's mirror, on no ref, and writes
// the run's row itself, approved for exactly that commit, as latem try does
// (A76). What differs is who is approving. A person running the command is
// the approval. An agent is not a person, so its changes run only where the
// machine's config says they may, on a base the repository's allowed refs
// already hold, as a job that base declares.

// changeSet is the changes as a caller writes them.
type changeSet struct {
	// Base is the commit the changes are on: all 40 hex characters.
	Base string `json:"base"`
	// Files are written, each whole. Remove lists paths the base has and the
	// result does not.
	Files  []changeFile `json:"files"`
	Remove []string     `json:"remove"`
}

// changeFile is one file to write: its content as text, or as base64 for
// bytes that are not UTF-8.
type changeFile struct {
	Path   string  `json:"path"`
	Text   *string `json:"text"`
	Base64 *string `json:"base64"`
	// Mode is "644" or "755". Left out, the file keeps the mode the base has
	// it at, and a new file is 644.
	Mode string `json:"mode"`
}

// Bounds on one changes run. It is for what one change touches, not for
// moving a repository: a base it has already is what makes it small.
const (
	maxChangePaths = 5000
	maxChangeBytes = 16 << 20
)

// readChangeSet reads the changes from a file, or from in for "-".
func readChangeSet(path string, in io.Reader) (*changeSet, error) {
	var data []byte
	var err error
	// One more byte than could be valid, so too much is told from enough.
	limit := int64(maxChangeBytes)*2 + 1<<20
	if path == "-" {
		data, err = io.ReadAll(io.LimitReader(in, limit+1))
	} else {
		var f *os.File
		// #nosec G304 -- a file the person at the terminal named.
		if f, err = os.Open(path); err == nil {
			data, err = io.ReadAll(io.LimitReader(f, limit+1))
			_ = f.Close()
		}
	}
	if err != nil {
		return nil, fmt.Errorf("reading the changes: %w", err)
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("the changes are over %d MiB: a changes run sends what a change touches", limit>>20)
	}
	var cs changeSet
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cs); err != nil {
		return nil, fmt.Errorf("the changes are not the JSON a changes run takes "+
			`({"base": "<commit>", "files": [{"path": "…", "text": "…"}], "remove": ["…"]}): %v`,
			strings.TrimPrefix(err.Error(), "json: "))
	}
	return &cs, nil
}

// toSource checks the shape of the changes and decodes them. Whether a path
// can be a file's place in the repository, and what the base has there, is
// decided where the commit is made (source.CommitChanges).
func (cs *changeSet) toSource() (source.Changes, error) {
	base := strings.ToLower(cs.Base)
	if !isFullSHA(base) {
		return source.Changes{}, fmt.Errorf("base %q is not a full commit: give all 40 hex characters, "+
			"as git rev-parse prints them", cs.Base)
	}
	n := len(cs.Files) + len(cs.Remove)
	switch {
	case n == 0:
		return source.Changes{}, errors.New("the changes write no file and remove none: to build the base as it is, start the run without changes")
	case n > maxChangePaths:
		return source.Changes{}, fmt.Errorf("the changes name %d paths, limit %d", n, maxChangePaths)
	}
	out := source.Changes{Base: base, Removes: cs.Remove, Writes: make([]source.FileChange, 0, len(cs.Files))}
	total := 0
	for _, f := range cs.Files {
		var data []byte
		switch {
		case f.Text != nil && f.Base64 != nil:
			return source.Changes{}, fmt.Errorf("%s has both text and base64: a file has one content", f.Path)
		case f.Text != nil:
			data = []byte(*f.Text)
		case f.Base64 != nil:
			var err error
			if data, err = base64.StdEncoding.DecodeString(*f.Base64); err != nil {
				return source.Changes{}, fmt.Errorf("%s: base64 is not standard base64: %v", f.Path, err)
			}
		default:
			return source.Changes{}, fmt.Errorf("%s has neither text nor base64: an empty file is \"text\": \"\", "+
				"and a file to delete goes in remove", f.Path)
		}
		mode := ""
		switch f.Mode {
		case "":
		case "644", "0644", "100644":
			mode = "100644"
		case "755", "0755", "100755":
			mode = "100755"
		default:
			return source.Changes{}, fmt.Errorf("%s has mode %q: a file is 644 or 755", f.Path, f.Mode)
		}
		if total += len(data); total > maxChangeBytes {
			return source.Changes{}, fmt.Errorf("the changed files are over %d MiB together: a changes run sends "+
				"what a change touches", maxChangeBytes>>20)
		}
		out.Writes = append(out.Writes, source.FileChange{Path: f.Path, Data: data, Mode: mode})
	}
	return out, nil
}

// changesRepo is what a changes run asks of a repository's mirror. A seam: no
// test of a command writes a mirror or runs git.
type changesRepo interface {
	// Commit makes the commit the run builds.
	Commit(ctx context.Context, c source.Changes) (source.Changed, error)
	// OnRef reports whether commit is in ref's history, and the ref's commit
	// that answered.
	OnRef(ctx context.Context, commit, ref string) (bool, string, error)
	// JobsAt is .latem/jobs.yaml at commit, and whether there is one.
	JobsAt(ctx context.Context, commit string) ([]byte, bool, error)
}

var openChangesRepo = func(cfg *config.Config, key string) (changesRepo, error) {
	mgr, err := source.New(source.Config{MirrorDir: cfg.Paths.Mirrors, WorkDir: cfg.Paths.Work})
	if err != nil {
		return nil, err
	}
	return mirrorChanges{mgr: mgr, repo: source.Repo{Key: key, URL: cfg.Repos[key].URL}}, nil
}

type mirrorChanges struct {
	mgr  *source.Manager
	repo source.Repo
}

func (m mirrorChanges) Commit(ctx context.Context, c source.Changes) (source.Changed, error) {
	return m.mgr.CommitChanges(ctx, m.repo, c)
}

func (m mirrorChanges) OnRef(ctx context.Context, commit, ref string) (bool, string, error) {
	mi, err := m.mgr.Open(m.repo)
	if err != nil {
		return false, "", err
	}
	return mi.OnRef(ctx, commit, ref)
}

func (m mirrorChanges) JobsAt(ctx context.Context, commit string) ([]byte, bool, error) {
	mi, err := m.mgr.Open(m.repo)
	if err != nil {
		return nil, false, err
	}
	fsys, err := mi.ReadDir(ctx, commit, ".latem", 64, workflow.MaxWorkflowBytes)
	if err != nil {
		return nil, false, err
	}
	data, err := fs.ReadFile(fsys, registry.Path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, false, nil
	}
	return data, err == nil, err
}

// changesRun is one changes run to make.
type changesRun struct {
	repo, job string
	// ref is the branch or tag the run is named for, fully qualified. An
	// agent's base must be in its history.
	ref    string
	inputs map[string]string
	actor  string
	// agent is set for changes an agent sent through `latem mcp`. A person's
	// run is their own approval; an agent's is held to the rules below.
	agent bool
	// requireAgent refuses to queue with no agent listening, for a caller
	// that is going to wait for the run.
	requireAgent bool
}

// changesQueued is a changes run that has been queued.
type changesQueued struct {
	RunID string
	// SHA is the commit it builds, Base the commit its changes are on, and
	// Paths how many paths differ between them.
	SHA, Base    string
	Paths        int
	AgentRunning bool
}

// startChangesRun makes the commit and queues the run pinned to it.
//
// An agent's run is held to four things a person's is not, and each is
// checked before anything is queued:
//
//   - the repository's config says agents may (`agent_changes = true`);
//   - the ref is one the repository builds without approval, and the base is
//     in its history, so an agent cannot build on a commit nobody admitted:
//     a pull request's head, or one that only an approval let through;
//   - the changes leave .latem/jobs.yaml alone, and the job is one the base
//     declares there, so the commands that check a change are not the
//     change's own to write.
func startChangesRun(ctx context.Context, st *state, id string, r changesRun, cs *changeSet) (changesQueued, error) {
	repoCfg, ok := st.cfg.Repos[r.repo]
	if !ok {
		return changesQueued{}, fmt.Errorf("this machine builds no repository %q: it builds %s",
			r.repo, strings.Join(slices.Sorted(maps.Keys(st.cfg.Repos)), ", "))
	}
	changes, err := cs.toSource()
	if err != nil {
		return changesQueued{}, err
	}
	if r.agent {
		if !repoCfg.AgentChanges {
			return changesQueued{}, fmt.Errorf("this machine does not run an agent's changes for %s. A person "+
				"allows it there with `agent_changes = true` under [repos.%s] in its config.toml, then "+
				"`latem service reload`. Until then, push the change and start the run on its branch", r.repo, r.repo)
		}
		if !registry.MatchPolicy(r.ref, registry.ExpandPolicy(repoCfg.AllowedRefs)) {
			return changesQueued{}, fmt.Errorf("%s is not a ref %s builds without a person's approval "+
				"(its allowed_refs), so an agent's changes cannot be based on it", r.ref, r.repo)
		}
		for _, p := range append(pathsOf(changes.Writes), changes.Removes...) {
			// By any capitals: a Mac's disk, and so its native runner's
			// workspace, opens .LATEM/Jobs.yaml as the same file, and a
			// checkout of both would leave whichever git wrote last.
			if strings.EqualFold(p, registry.Path) {
				return changesQueued{}, fmt.Errorf("the changes touch %s. The job that checks an agent's change "+
					"is the one its base declares, so a change to that file is pushed, not sent", registry.Path)
			}
		}
	}

	repo, err := openChangesRepo(st.cfg, r.repo)
	if err != nil {
		return changesQueued{}, err
	}
	made, err := repo.Commit(ctx, changes)
	if err != nil {
		return changesQueued{}, err
	}
	if r.agent {
		on, tip, err := repo.OnRef(ctx, changes.Base, r.ref)
		if err != nil {
			return changesQueued{}, err
		}
		if !on {
			at := "which this machine has not fetched"
			if tip != "" {
				at = fmt.Sprintf("which is at %.12s here", tip)
			}
			return changesQueued{}, fmt.Errorf("base %.12s is not in the history of %s, %s. An agent's changes "+
				"go on a commit of a ref %s builds without approval: push the base there, or name the ref it is on",
				changes.Base, r.ref, at, r.repo)
		}
		data, found, err := repo.JobsAt(ctx, changes.Base)
		if err != nil {
			return changesQueued{}, err
		}
		if !found {
			return changesQueued{}, fmt.Errorf("%s has no %s at %.12s. An agent's changes run a job the base "+
				"declares there", r.repo, registry.Path, changes.Base)
		}
		reg, err := registry.Parse(data)
		if err != nil {
			return changesQueued{}, fmt.Errorf("%s at %.12s: %w", registry.Path, changes.Base, err)
		}
		if reg.Jobs[r.job] == nil {
			return changesQueued{}, fmt.Errorf("%s at %.12s declares no job %q; it declares %s. An agent's "+
				"changes run a job the base declares there", registry.Path, changes.Base, r.job,
				strings.Join(slices.Sorted(maps.Keys(reg.Jobs)), ", "))
		}
	}

	if r.requireAgent {
		if err := st.requireAgent(ctx); err != nil {
			return changesQueued{}, err
		}
	}
	now := time.Now()
	row := store.Run{
		ID: id, Repo: r.repo, Job: r.job, Ref: r.ref, State: protocol.StateQueued,
		OriginKind: "cli", OriginActor: r.actor, Inputs: r.inputs,
		Workflow:   &store.WorkflowRun{SHA: made.SHA, Local: true, Base: changes.Base},
		ApprovedAt: &now, ApprovedSHA: made.SHA,
	}
	if err := st.db.Create(ctx, row); err != nil {
		return changesQueued{}, err
	}
	// The row is written here, as latem try writes its own (A76): its
	// approval and its commit are things no run command carries. Submit then
	// queues the row that exists, from the agent's side of the socket when
	// one is listening and from this one when none is.
	req := protocol.RunRequest{
		ID: id, Repo: r.repo, Job: r.job, Ref: r.ref, Inputs: r.inputs,
		Origin: protocol.Origin{Kind: "cli", Actor: r.actor, RequestedAt: now},
	}
	out := changesQueued{RunID: id, SHA: made.SHA, Base: changes.Base, Paths: made.Paths}
	expires := now.Add(st.queueTTL())
	sub, err := st.client.Submit(ctx, req, local.SubmitOptions{ExpiresAt: expires})
	if err != nil {
		if qerr := st.db.Enqueue(ctx, id, 0); qerr != nil {
			return changesQueued{}, errors.Join(err, qerr)
		}
		return out, st.db.SetExpiry(ctx, id, expires)
	}
	out.AgentRunning = sub.AgentRunning
	return out, nil
}

func pathsOf(writes []source.FileChange) []string {
	out := make([]string, 0, len(writes))
	for _, w := range writes {
		out = append(out, w.Path)
	}
	return out
}
