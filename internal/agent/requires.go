package agent

import (
	"context"
	"encoding/json"
	"io/fs"
	"regexp"
	"slices"

	"github.com/Abyssinia-Labs/latem/internal/needs"
	"github.com/Abyssinia-Labs/latem/internal/source"
	"github.com/Abyssinia-Labs/latem/internal/system"
	"github.com/Abyssinia-Labs/latem/internal/workflow"
	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

// What a control plane is told about needs (A61): each planned job's
// requirements, and the machine's toolchains on its heartbeat. Keys and
// versions from a closed vocabulary — never evidence, a path or a step — and
// only for a control plane that said 2.2. Both explain; neither routes a job,
// and admission never reads them (INV-5 is unchanged).

// requiresSpeaker is a transport that can say whether its control plane takes
// 2.2's fields. Only the remote broker is one.
type requiresSpeaker interface{ SpeaksRequires() bool }

func speaksRequires(t transport) bool {
	s, ok := t.b.(requiresSpeaker)
	return ok && s.SpeaksRequires()
}

// maxRequires is how many requirements a job carries; the control plane
// refuses a plan with more.
const maxRequires = 32

// Patterns the control plane decodes a requirement with. A version that does
// not fit is left off, and the key kept.
var (
	requirementKey     = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)
	requirementVersion = regexp.MustCompile(`^[0-9][0-9.]{0,15}$`)
)

// addRequires says, on each planned job, what it needs of a machine that
// nothing in it provides, read from the commit as `latem workflows check`
// reads it — the scripts its steps run included — through the mirror's tree.
// Best effort: a job it cannot read carries none.
func addRequires(ctx context.Context, mirror *source.Mirror, sha string, workflows []protocol.PlannedWorkflow) {
	fsys, _, err := mirror.Tree(ctx, sha, source.DefaultTreeLimits)
	if err != nil {
		return
	}
	for i := range workflows {
		pw := &workflows[i]
		data, err := fs.ReadFile(fsys, pw.File)
		if err != nil {
			continue
		}
		wf, err := workflow.Parse(data, pw.File)
		if err != nil {
			continue
		}
		for j := range pw.Jobs {
			pj := &pw.Jobs[j]
			if pj.Error != "" || wf.Jobs[pj.Key] == nil {
				continue
			}
			leg := needs.Leg{Labels: pj.Labels}
			if len(pj.Matrix) > 0 {
				_ = json.Unmarshal(pj.Matrix, &leg.Matrix)
			}
			pj.Requires = requirementsOf(needs.Job(fsys, wf, pj.Key, leg))
		}
	}
}

// requirementsOf is a report's requirements that the machine must meet: not
// those an action provides.
func requirementsOf(r needs.Report) []protocol.Requirement {
	var out []protocol.Requirement
	for _, q := range r.Requires {
		if q.ProvidedBy != "" || !requirementKey.MatchString(q.Key) || len(out) == maxRequires {
			continue
		}
		req := protocol.Requirement{Key: q.Key}
		if requirementVersion.MatchString(q.Version) {
			req.Version = q.Version
		}
		if !slices.Contains(out, req) {
			out = append(out, req)
		}
	}
	return out
}

// toolchainsAdvert is what the heartbeat says of the machine's toolchains:
// versions and names, never a path.
func toolchainsAdvert(t system.Toolchains) *protocol.Toolchains {
	out := &protocol.Toolchains{
		IOSSimulator:     t.HasIOSRuntime(),
		JDK:              slices.Clone(t.JDKs),
		AndroidSDK:       t.AndroidSDK != "",
		AndroidPlatforms: slices.Clone(t.AndroidPlatforms),
		AndroidEmulator:  t.AndroidEmulator,
		PasswordlessSudo: t.PasswordlessSudo,
	}
	for _, x := range t.Xcodes {
		out.Xcode = append(out.Xcode, x.Version.Text)
	}
	for tool, found := range t.Tools {
		if found {
			out.Tools = append(out.Tools, tool)
		}
	}
	slices.Sort(out.Tools)
	return out
}
