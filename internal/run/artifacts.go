package run

import (
	"context"
	"errors"
	"fmt"

	"github.com/Abyssinia-Labs/latem/internal/artifact"
	"github.com/Abyssinia-Labs/latem/internal/registry"
	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

// runArtifactStep performs an upload-artifact or download-artifact step
// (A49).
//
// It is the run loop's rather than an executor's because both ends of the
// copy — the workspace and this run's artifact store — are host paths the
// agent owns, on docker as on native; handing it to an executor would mean a
// container reaching the artifact store, which is exactly the boundary §17
// keeps.
func (x *execution) runArtifactStep(ctx context.Context, s registry.ResolvedStep, label string) outcome {
	a := s.Artifact
	if x.o.artifacts == nil || x.ws == nil {
		return fail(errors.New("this agent has no artifact store"), protocol.ErrArtifact)
	}

	if a.Download {
		n, err := x.o.artifacts.Restore(x.run.ID, x.ws.Path, a.Name, a.Path)
		if err != nil {
			if errors.Is(err, artifact.ErrNoSuchArtifact) {
				return outcome{
					state: protocol.StateFailed,
					code:  protocol.ErrArtifact,
					msg: fmt.Sprintf("step %q: %v — download-artifact reaches artifacts this run "+
						"uploaded, and one from another job or run is not something a single run has", label, err),
				}
			}
			return fail(err, protocol.ErrArtifact)
		}
		x.note("step %s: restored %d file(s)", label, n)
		return outcome{state: protocol.StateSucceeded}
	}

	refs, err := x.o.artifacts.CollectNamed(ctx, x.run.ID, x.ws.Path, artifact.Upload{
		Name:    a.Name,
		Include: a.Include,
		Exclude: a.Exclude,
		Retain:  a.Retain.Duration(),
	})
	if err != nil {
		return fail(err, protocol.ErrArtifact)
	}
	if len(refs) == 0 {
		switch a.IfNoFilesFound {
		case "error":
			return outcome{
				state: protocol.StateFailed,
				code:  protocol.ErrArtifact,
				msg:   fmt.Sprintf("step %q: nothing matched %v for artifact %q", label, a.Include, a.Name),
			}
		case "warn":
			x.note("step %s: nothing matched %v; artifact %q has no files", label, a.Include, a.Name)
		}
		return outcome{state: protocol.StateSucceeded}
	}
	x.stepArtifacts = append(x.stepArtifacts, refs...)
	x.note("step %s: uploaded artifact %q, %d file(s)", label, a.Name, len(refs))
	return outcome{state: protocol.StateSucceeded}
}
