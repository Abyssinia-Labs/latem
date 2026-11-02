package main

import (
	"context"
	"fmt"
	"os"
	"path"
	"slices"
	"strconv"
	"strings"

	"github.com/Abyssinia-Labs/latem/internal/config"
	"github.com/Abyssinia-Labs/latem/internal/needs"
	"github.com/Abyssinia-Labs/latem/internal/system"
	"github.com/Abyssinia-Labs/latem/internal/workflow/ghcmd"
)

// What this machine's repositories need of it (A59).
//
// Each configured repository is read from the agent's mirror — never fetched:
// the agent owns its mirrors — and its workflows are planned and classified
// as `latem workflows check` does. For every job a runner here would take,
// what it needs is compared with what the machine has, as a step would find
// it. Warnings only, for now: doctor's exit code is a CI gate, and a reading
// of a workflow is not yet trusted to fail one.

// servicePath is the PATH a step gets from the service: launchd's, which
// latem writes into its plist, or systemd's default for a user unit.
func servicePath(sys *system.System) string {
	if sys.IsMacOS() {
		return system.DefaultLaunchdPath
	}
	return "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
}

// needsSummary is what checkNeeds learned, for the checks it qualifies.
type needsSummary struct {
	// known is true when every configured repository was read, so that a
	// requirement absent from needed is one no job here has.
	known bool
	// needed maps a requirement key to the jobs, on a native runner here,
	// that need it: "take-one checks.yml apple".
	needed map[string][]string
}

func (d *doctor) checkNeeds(ctx context.Context) []check {
	const id, title = "needs", "what jobs need"
	d.needs = needsSummary{needed: map[string][]string{}}
	if d.cfg == nil {
		return []check{skip(id, title, "config did not load")}
	}
	if len(d.cfg.Repos) == 0 {
		return []check{skip(id, title, "no repositories yet")}
	}
	tcFor := func(r *config.Runner) system.Toolchains { return d.toolchainsFor(ctx, r) }
	empty, err := os.MkdirTemp("", "latem-doctor-workspace-")
	if err != nil {
		return []check{fail(id, title, err.Error())}
	}
	defer func() { _ = os.RemoveAll(empty) }()

	d.needs.known = true
	var out []check
	for _, key := range sortedStrings(d.cfg.Repos) {
		cid, ctitle := "needs."+key, "needs / "+key
		files, err := d.readRepo(ctx, d.cfg, key, fromMirror)
		if err != nil {
			d.needs.known = false
			out = append(out, skip(cid, ctitle, "no mirror of it yet; read once the agent has fetched it"))
			continue
		}
		target := repoCheckTarget(d.cfg, key)
		target.fsys, target.where, target.commit, target.workspace = files.fsys, files.where, files.sha, empty
		rep, err := analyzeWorkflows(ctx, d.cfg, target, secretStores{})
		files.close()
		if err != nil {
			d.needs.known = false
			out = append(out, warn(cid, ctitle, "its workflows could not be read: "+err.Error()))
			continue
		}
		out = append(out, d.needsCheck(cid, ctitle, key, rep, tcFor))
	}
	return out
}

// toolchainsFor is what a step on runner r finds, probed once per PATH a
// native runner's steps get (A60), and only when asked: a docker or tart
// runner's toolchain is not the host's, and reads as nothing here.
func (d *doctor) toolchainsFor(ctx context.Context, r *config.Runner) system.Toolchains {
	if r.Executor != config.ExecutorNative {
		return system.Toolchains{Tools: map[string]bool{}}
	}
	probe := d.toolchains
	if probe == nil {
		probe = d.sys.Toolchains
	}
	if d.probed == nil {
		d.probed = map[string]system.Toolchains{}
	}
	p := ghcmd.PrependPath(r.NativePathPrepend(), servicePath(d.sys))
	tc, ok := d.probed[p]
	if !ok {
		tc = probe(ctx, p)
		d.probed[p] = tc
	}
	return tc
}

// labelPromises are labels that say a runner can do what only a toolchain
// makes true. A job asking for one is routed here by it, and fails at its
// first build step when the machine cannot — and `latem init` writes xcode
// on every Mac, whether or not it has one.
var labelPromises = []struct {
	label, what string
	has         func(system.Toolchains) bool
}{
	{"xcode", "no Xcode", func(t system.Toolchains) bool { return len(t.Xcodes) > 0 }},
	{"ios", "no iOS simulator runtime", func(t system.Toolchains) bool { return len(t.Xcodes) > 0 && t.HasIOSRuntime() }},
	{"android", "no Android SDK", func(t system.Toolchains) bool { return t.AndroidSDK != "" }},
}

// checkRunnerLabels warns of a native runner whose labels promise a
// toolchain its steps would not find.
func (d *doctor) checkRunnerLabels(ctx context.Context) []check {
	if d.cfg == nil {
		return nil
	}
	var out []check
	for _, r := range d.cfg.Runners {
		if r.Executor != config.ExecutorNative {
			continue
		}
		var broken []string
		for _, p := range labelPromises {
			if !slices.ContainsFunc(r.Labels, func(l string) bool { return strings.EqualFold(l, p.label) }) {
				continue
			}
			if !p.has(d.toolchainsFor(ctx, r)) {
				broken = append(broken, fmt.Sprintf("%s, and its steps find %s", p.label, p.what))
			}
		}
		id, title := "labels."+r.Name, "labels / "+r.Name
		if len(broken) == 0 {
			continue
		}
		out = append(out, warn(id, title, "runner "+r.Name+" is labelled "+strings.Join(broken, "; ")).
			withNotes("A job that asks for the label is sent here, and fails at its first build step.").
			withManual("install what the label promises (`latem setup` offers what a command can), "+
				"or take the label off the runner in config.toml"))
	}
	return out
}

// needsCheck is one repository's: what its jobs on this machine need that
// the machine lacks.
func (d *doctor) needsCheck(id, title, key string, rep checkReport, tcFor func(*config.Runner) system.Toolchains) check {
	var lines, fixes, manual []string
	here, elsewhere := 0, 0
	mine := checkReport{} // this machine's jobs, for the count of kinds
	for _, wr := range rep.workflows {
		mineWR := workflowReport{file: wr.file}
		for _, j := range wr.jobs {
			if j.needs == nil || j.stopped {
				continue
			}
			runner, ok := d.cfg.Runner(j.carrier)
			if j.carrier == "" || !ok {
				elsewhere++
				continue
			}
			here++
			mineWR.jobs = append(mineWR.jobs, j)
			ref := fmt.Sprintf("%s %s", path.Base(wr.file), j.name)
			if runner.Executor == config.ExecutorNative {
				for _, q := range j.needs.Requires {
					if q.ProvidedBy == "" {
						d.needs.needed[q.Key] = append(d.needs.needed[q.Key], key+" "+ref)
					}
				}
			}
			for _, g := range needs.Gaps(*j.needs, tcFor(runner), string(runner.Executor)) {
				line := fmt.Sprintf("%s needs %s", ref, g.Missing)
				if g.Have != "" {
					line += "; this machine has " + g.Have
				}
				if !slices.Contains(lines, line) {
					lines = append(lines, line)
				}
				for _, f := range append(g.Fixes(), g.YourLines()...) {
					if !slices.Contains(fixes, f) {
						fixes = append(fixes, f)
					}
				}
				for _, m := range g.Manual {
					if !slices.Contains(manual, m) {
						manual = append(manual, m)
					}
				}
			}
		}
		mine.workflows = append(mine.workflows, mineWR)
	}
	summary := fmt.Sprintf("%d job%s run here", here, plural(here))
	if k := kindsSummary(mine); k != "" {
		summary += " (" + k + ")"
	}
	if elsewhere > 0 {
		summary += fmt.Sprintf("; %d %s for another machine", elsewhere, map[bool]string{true: "is", false: "are"}[elsewhere == 1])
	}
	if len(lines) == 0 {
		return pass(id, title, summary+"; nothing they need is missing")
	}
	return warn(id, title, summary+"; "+strconv.Itoa(len(lines))+" of what they need is missing here", fixes...).
		withNotes(lines...).withManual(manual...)
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// needGates are the toolchain checks a job's needs decide (A59): run when a
// job here needs one of the keys, skipped when every repository was read and
// none does.
var needGates = map[string][]string{
	"ruby.version":     {needs.Ruby, needs.Bundler, needs.Fastlane, needs.CocoaPods},
	"ruby.bundler":     {needs.Bundler, needs.Fastlane, needs.CocoaPods},
	"java.version":     {needs.JDK},
	"android.sdk":      {needs.AndroidSDK, needs.AndroidEmulator},
	"xcode.simruntime": {needs.IOSSimulator},
}

// gateByNeeds skips the toolchain checks no job here needs, and names the
// jobs that need the rest.
func (d *doctor) gateByNeeds(checks []check) []check {
	out := make([]check, 0, len(checks))
	for _, c := range checks {
		keys, gated := needGates[c.ID]
		if !gated || !d.needs.known {
			out = append(out, c)
			continue
		}
		var by []string
		for _, k := range keys {
			by = append(by, d.needs.needed[k]...)
		}
		if len(by) == 0 {
			out = append(out, skip(c.ID, c.Title, "no job this machine takes needs it"))
			continue
		}
		slices.Sort(by)
		by = slices.Compact(by)
		if len(by) > 3 {
			by = append(by[:3], fmt.Sprintf("and %d more", len(by)-3))
		}
		out = append(out, c.withNotes("Needed by "+strings.Join(by, ", ")+"."))
	}
	return out
}
