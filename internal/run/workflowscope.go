package run

import (
	"strings"

	"github.com/Abyssinia-Labs/latem/internal/workflow/expr"
)

// workflowScope builds the contexts a workflow expression may read at gate 11.
//
// Only what is knowable when a run is admitted. Everything else the expression
// language offers is refused by name in internal/workflow rather than filled
// with a plausible value: `steps.*` and `job.status` do not exist yet, and
// `secrets.*` must not be substituted at all (INV-4, §15).
//
// `vars` is present and empty on purpose. Repository variables are a hosted
// concept with no local source, and an expression reading one gets the same
// empty string it would get from an unset variable — which is the behaviour a
// workflow author already expects, unlike an error naming a context they did
// not know was unsupported.
func (x *execution) workflowScope() expr.Scope {
	refName := x.refNormalized
	refType := ""
	switch {
	case strings.HasPrefix(refName, "refs/heads/"):
		refName, refType = strings.TrimPrefix(refName, "refs/heads/"), "branch"
	case strings.HasPrefix(refName, "refs/tags/"):
		refName, refType = strings.TrimPrefix(refName, "refs/tags/"), "tag"
	}

	inputs := map[string]any{}
	for k, v := range x.req.Inputs {
		inputs[k] = v
	}

	workspace := ""
	if x.ws != nil {
		workspace = x.ws.Path
	}

	return expr.Scope{
		Contexts: map[string]expr.Value{
			"github": map[string]any{
				"repository": x.req.Repo,
				"ref":        x.refNormalized,
				"ref_name":   refName,
				"ref_type":   refType,
				"sha":        x.sha,
				"job":        x.req.Job,
				"run_id":     x.run.ID,
				// run_number is the machine's own sequence, which is what
				// A36 made the build number. A workflow that tags a build with
				// github.run_number therefore names the run that produced it,
				// the same as one using $LATEM_BUILD_NUMBER.
				"run_number":  float64(x.run.Number),
				"run_attempt": float64(1),
				"event_name":  eventName(x.req.Origin.Kind),
				"actor":       x.req.Origin.Actor,
				"workspace":   workspace,
			},
			"inputs": inputs,
			"env":    map[string]any{},
			"vars":   map[string]any{},
		},
		Status:       expr.StatusSuccess,
		WorkspaceDir: workspace,
	}
}

// eventName maps an origin kind onto the event name a workflow expects.
//
// §7's kinds are "manual", "webhook", "schedule" and "cli". A workflow asking
// `github.event_name == 'push'` is asking "did a push cause this", and a
// webhook-originated run is the one where the honest answer is yes. The others
// have no GitHub equivalent and keep their own names rather than borrowing one
// — answering "push" for a run somebody typed would make the condition lie.
func eventName(kind string) string {
	switch kind {
	case "webhook":
		return "push"
	case "schedule":
		return "schedule"
	case "manual", "cli":
		return "workflow_dispatch"
	}
	return kind
}
