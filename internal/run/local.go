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

// localCommit is gates 6, 7 and 9 for a commit `latem try` made from a working
// tree (A76).
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
	refuse := func(format string, args ...any) (Mirror, string, string, registry.Decision, error) {
		return nil, "", "", registry.Decision{
			State: protocol.StateRejected,
			Code:  protocol.ErrInvalidRef,
			Msg:   fmt.Sprintf(format, args...),
		}, nil
	}
	if registry.ApprovalFor(x.run.ApprovedSHA, sha) != registry.ApprovedForThisCommit {
		return refuse("a run latem try made is admitted only by an approval of its own commit %s; this one carries %q",
			shortSHA(sha), x.run.ApprovedSHA)
	}
	ref := x.req.Ref
	if err := registry.ValidateRef(ref); err != nil {
		return refuse("a run latem try made names the ref %q: %v", ref, err)
	}
	if !strings.HasPrefix(ref, "refs/heads/") && !strings.HasPrefix(ref, "refs/tags/") {
		return refuse("a run latem try made names %q, which is neither a branch nor a tag", ref)
	}

	mirror, err := x.o.src.Open(source.Repo{Key: x.req.Repo, URL: repoCfg.URL})
	if errors.Is(err, source.ErrNoMirror) {
		return refuse("this machine has no mirror of %s, so the commit latem try made is not here; run latem try again", x.req.Repo)
	}
	if err != nil {
		return nil, "", "", registry.Decision{}, err
	}
	has, err := mirror.HasCommit(ctx, sha)
	if err != nil {
		return nil, "", "", registry.Decision{}, err
	}
	if !has {
		return refuse("the commit %s latem try made is not in this machine's mirror of %s; run latem try again",
			shortSHA(sha), x.req.Repo)
	}
	x.note("a commit latem try made from a working tree, on no remote ref: not fetched, and %s is a name only (A76)", ref)
	x.noteLocal()
	return mirror, ref, sha, registry.Decision{}, nil
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
