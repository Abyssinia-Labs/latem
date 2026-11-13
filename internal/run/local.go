package run

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/Abyssinia-Labs/latem/internal/config"
	"github.com/Abyssinia-Labs/latem/internal/registry"
	"github.com/Abyssinia-Labs/latem/internal/source"
	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

// localCommit is gates 6, 7 and 9 for a commit a command on this machine
// made: `latem try`'s, from a working tree (A76), or a changes run's, from a
// commit and a set of changed files (A91).
//
// The command put the commit in the mirror before it queued the run, so
// nothing is fetched, and a try works with no network until a step needs one.
// The ref is the branch the tree was on: a name for the github context, which
// need not exist on any remote, so it is validated and not looked up.
//
// The run is admitted by its approval alone. One not approved for exactly its
// commit is refused, never parked: no broker can make a local run, and a row
// that claims to be one without the approval is not to be trusted with a
// person's attention.
func (x *execution) localCommit(ctx context.Context, repoCfg *config.Repo) (
	Mirror, string, string, registry.Decision, error,
) {
	sha := x.req.SHA
	base := x.run.ChangesBase()
	what := "latem try"
	if base != "" {
		what = "a changes run"
	}
	refuse := func(format string, args ...any) (Mirror, string, string, registry.Decision, error) {
		return nil, "", "", registry.Decision{
			State: protocol.StateRejected,
			Code:  protocol.ErrInvalidRef,
			Msg:   fmt.Sprintf(format, args...),
		}, nil
	}
	if registry.ApprovalFor(x.run.ApprovedSHA, sha) != registry.ApprovedForThisCommit {
		return refuse("a run %s made is admitted only by an approval of its own commit %s; this one carries %q",
			what, shortSHA(sha), x.run.ApprovedSHA)
	}
	ref := x.req.Ref
	if err := registry.ValidateRef(ref); err != nil {
		return refuse("a run %s made names the ref %q: %v", what, ref, err)
	}
	if !strings.HasPrefix(ref, "refs/heads/") && !strings.HasPrefix(ref, "refs/tags/") {
		return refuse("a run %s made names %q, which is neither a branch nor a tag", what, ref)
	}

	mirror, err := x.o.src.Open(source.Repo{Key: x.req.Repo, URL: repoCfg.URL})
	if errors.Is(err, source.ErrNoMirror) {
		return refuse("this machine has no mirror of %s, so the commit %s made is not here; make the run again", x.req.Repo, what)
	}
	if err != nil {
		return nil, "", "", registry.Decision{}, err
	}
	has, err := mirror.HasCommit(ctx, sha)
	if err != nil {
		return nil, "", "", registry.Decision{}, err
	}
	if !has {
		return refuse("the commit %s that %s made is not in this machine's mirror of %s; make the run again",
			shortSHA(sha), what, x.req.Repo)
	}
	if base != "" {
		x.noteChanges(ctx, mirror, base, sha, ref)
		return mirror, ref, sha, registry.Decision{}, nil
	}
	x.note("a commit latem try made from a working tree, on no remote ref: not fetched, and %s is a name only (A76)", ref)
	x.noteLocal()
	return mirror, ref, sha, registry.Decision{}, nil
}

// changesLister is a [Mirror] that can say which paths two commits differ in.
// *source.Mirror is one.
type changesLister interface {
	Changed(ctx context.Context, from, to string) ([]string, error)
}

// maxChangedNoted is how many changed paths a run's log names before it
// counts the rest.
const maxChangedNoted = 20

// noteChanges says in a changes run's log what it builds (A91), and keeps the
// paths that differ from its base for the job to read ($LATEM_CHANGED_FILES).
func (x *execution) noteChanges(ctx context.Context, mirror Mirror, base, sha, ref string) {
	if sha == base {
		x.note("a changes run whose changes leave %s as it is: this builds that commit, not fetched, "+
			"and %s is a name only (A91)", shortSHA(base), ref)
		return
	}
	x.note("a changes run: %s is %s with changed files on it, a commit on no ref that a command on this "+
		"machine made. Not fetched, and %s is a name only (A91)", shortSHA(sha), shortSHA(base), ref)
	lister, ok := mirror.(changesLister)
	if !ok {
		return
	}
	changed, err := lister.Changed(ctx, base, sha)
	if err != nil {
		x.note("could not list what differs from %s: %v", shortSHA(base), x.redact(err.Error()))
		return
	}
	x.changed = changed
	shown := changed[:min(len(changed), maxChangedNoted)]
	more := ""
	if n := len(changed) - len(shown); n > 0 {
		more = fmt.Sprintf(", and %d more", n)
	}
	x.note("%d path(s) differ from %s: %s%s", len(changed), shortSHA(base), strings.Join(shown, ", "), more)
}

// ChangedFilesVar names the file that lists a changes run's changed paths,
// one to a line, relative to the workspace (A91). BaseSHAVar is the commit
// they are changed from. A job that tests only what a change touches reads
// the first; one that wants the diff itself has both commits in its
// workspace, and `git diff "$LATEM_BASE_SHA" "$LATEM_SHA"` is it.
const (
	ChangedFilesVar = "LATEM_CHANGED_FILES"
	BaseSHAVar      = "LATEM_BASE_SHA"
)

// changedFilesName is the file's name in the job's temporary directory.
const changedFilesName = "latem-changed-files"

// putChangedFiles writes a changes run's changed paths into the job's
// temporary directory, once its environment is up and before its first step
// (A91). Outside the workspace, so the list is not a change itself.
func (x *execution) putChangedFiles(ctx context.Context) error {
	if x.run.ChangesBase() == "" {
		return nil
	}
	var b strings.Builder
	for _, p := range x.changed {
		b.WriteString(p)
		b.WriteByte('\n')
	}
	path, err := x.env.PutTemp(ctx, changedFilesName, []byte(b.String()))
	if err != nil {
		return fmt.Errorf("writing the list of changed files: %w", err)
	}
	x.changedPath = path
	return nil
}

// noteLocal says in a local run's log what a try leaves out (A76), so a job
// that behaves differently from its run on a push says why.
func (x *execution) noteLocal() {
	x.note("on: and the job's if: were not evaluated: latem try runs the job it names")
	if len(x.req.Needs) > 0 {
		names := slices.Sorted(maps.Keys(x.req.Needs))
		x.note("needs %s taken as succeeded, with no outputs: a try runs one job", strings.Join(names, ", "))
	}
}
