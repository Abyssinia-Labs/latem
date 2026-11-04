// Package readiness says what a machine can build (A69): for each kind of job
// in its repositories whether a runner here takes it, whether this operating
// system could, and what stands in the way, with what would close each thing.
//
// It is the one answer behind `latem setup`'s screen, its --json and its plain
// output, so that a person, a script and a log read the same thing. It decides
// nothing and touches nothing: it is given a machine and returns a plan, and
// what acts on the plan is the command. Where a job runs stays the business
// of its labels: a kind is switched on by a label a person confirms, and what
// a job needs never routes it (INV-5).
package readiness

import (
	"fmt"
	"slices"
	"strings"

	"github.com/Abyssinia-Labs/latem/internal/needs"
	"github.com/Abyssinia-Labs/latem/internal/system"
)

// State is where a kind of job stands on this machine.
type State string

const (
	// Ready: a runner here takes these jobs, and nothing they need is missing.
	Ready State = "ready"
	// Missing: a runner here takes these jobs, and something is in the way.
	Missing State = "missing"
	// Off: this operating system could build these, and no runner here does.
	Off State = "off"
	// Impossible: these want another operating system.
	Impossible State = "impossible"
)

// Labels a kind is switched on by, beyond the ones `latem init` writes.
const (
	LabelAndroid    = "android"
	LabelDockerHost = "docker-host"
	// LabelNative is what a native runner carries before any kind is on it:
	// a runner must have a label, and no job asks for this one.
	LabelNative = "native"
)

// NativeRunner is the name of the native runner setup adds to a machine that
// has none: Linux's default config is a docker runner alone.
const NativeRunner = "host"

// Input is the machine.
type Input struct {
	Machine string
	Host    system.Host
	Runners []Runner
	// Toolchains is what a native step would find here with no runner's
	// settings, for a machine with no native runner yet.
	Toolchains system.Toolchains
	Jobs       []Job
	Images     []Image
	// Unread are repositories whose jobs could not be read, each with why.
	Unread []string
}

// Runner is one runner in config.toml, and what its steps find.
type Runner struct {
	Name, Executor string
	Labels         []string
	Env            map[string]string
	Toolchains     system.Toolchains
}

// Job is one job in one of the machine's repositories, whichever machine its
// labels send it to.
type Job struct {
	// Ref names it for a person: "tsedal preview.yml android".
	Ref    string
	Repo   string
	Labels []string // what it asks for, aliases applied
	Report needs.Report
	// Runner is the runner here that its labels reach, or "".
	Runner string
	// Unisolated: it uses remote actions, which a native runner refuses
	// until [actions] allow_unisolated says otherwise.
	Unisolated bool
	// Moving are the actions it names by a tag or branch, on a machine that
	// wants a commit; NotAllowed those [actions] allowed leaves out.
	Moving, NotAllowed []string
	// Secrets are the names it reads that have no value here, and Env the
	// GitHub environment it runs in, which scopes them.
	Secrets []string
	Env     string
}

// Image is a docker runner's image.
type Image struct {
	Runner, Ref string
	Present     bool
}

// Plan is the answer.
type Plan struct {
	Machine string      `json:"machine"`
	Host    system.Host `json:"host"`
	Kinds   []Kind      `json:"kinds"`
	// Unclassified are jobs whose kind could not be read from them.
	Unclassified []string `json:"unclassified,omitempty"`
	Unread       []string `json:"unread,omitempty"`
}

// Kind is one kind of build, on this machine.
type Kind struct {
	Name  string `json:"name"` // needs.Kind*
	Title string `json:"title"`
	State State  `json:"state"`
	// Why says what is not obvious from State: "needs a Mac".
	Why string `json:"why,omitempty"`
	// On reports whether a runner here takes this kind.
	On bool `json:"on"`
	// Runner is the runner that takes it, or would.
	Runner string `json:"runner,omitempty"`
	// Label is what a person switches to turn it on or off, when one word
	// does; "" when no label here can. Labelled says the runner has it.
	Label    string   `json:"label,omitempty"`
	Labelled bool     `json:"labelled,omitempty"`
	Repos    []string `json:"repos,omitempty"`
	Jobs     []string `json:"jobs,omitempty"`
	Items    []Item   `json:"items,omitempty"`
	// Notes are true things no action closes here.
	Notes []string `json:"notes,omitempty"`
	// Proof is what shows the kind works here, in the environment a step
	// gets, before a label sends it jobs.
	Proof []Probe `json:"proof,omitempty"`
}

// Blocking counts what stands between the kind and working here.
func (k Kind) Blocking() int {
	n := 0
	for _, it := range k.Items {
		if !it.Soft {
			n++
		}
	}
	return n
}

// Item is one thing in the way.
type Item struct {
	// ID is stable across probes, so a screen can keep its place.
	ID    string `json:"id"`
	Title string `json:"title"`
	// Have is what the machine has instead.
	Have string   `json:"have,omitempty"`
	Jobs []string `json:"jobs,omitempty"`
	// Soft: what a job already running here has got by without, which is
	// worth saying and not worth failing for.
	Soft bool `json:"soft,omitempty"`

	// Run are commands latem runs, each an argv, once asked (A60). Yours are
	// for a person to type. Config is what setup writes to config.toml, once
	// asked. Manual is what no command does.
	Run    [][]string `json:"run,omitempty"`
	Yours  [][]string `json:"yours,omitempty"`
	Config []Change   `json:"config,omitempty"`
	Manual []string   `json:"manual,omitempty"`
	// Warning is said before a change that gives something up.
	Warning string `json:"warning,omitempty"`
	// Secrets are the names a secrets item's Yours set, each with its
	// environment when it has one: "TOKEN (staging)".
	Secrets []string `json:"secrets,omitempty"`
}

// Item IDs that Advert treats apart, the first and last followed by a name.
const (
	itemAllowed    = "allowed-"
	itemMovingRefs = "moving-refs"
	itemImage      = "image-"
	itemSecrets    = "secrets-"
)

// Change is one edit to config.toml.
type Change struct {
	// What is one of the Change* values.
	What string `json:"what"`
	// Runner is the runner it applies to, or "" for the machine.
	Runner string `json:"runner,omitempty"`
	Key    string `json:"key,omitempty"`
	Value  string `json:"value,omitempty"`
	// Line is how it reads, for a person.
	Line string `json:"line"`
}

// Changes setup can make.
const (
	ChangeEnv        = "env"          // Runner's [runner.native.env] Key = Value
	ChangeLabelAdd   = "label-add"    // Runner gains the label Value
	ChangeLabelDrop  = "label-remove" // Runner loses the label Value
	ChangeUnisolated = "allow-unisolated"
	ChangeMovingRefs = "allow-moving-refs"
	ChangeAllowed    = "allowed"    // [actions] allowed gains Value
	ChangeNewRunner  = "new-runner" // a native runner, named Value
)

// Probe is one command that has to work in a step's environment.
type Probe struct {
	Title string   `json:"title"`
	Argv  []string `json:"argv"`
	// Env is the runner's, for what a step would be given.
	Env map[string]string `json:"env,omitempty"`
}

// definition is what is true of a kind anywhere.
type definition struct {
	name, title string
	mac, linux  bool // which systems build it
	native      bool // on the host's own toolchain, not in a container
	label       string
	// always shows it with no job asking, for a machine that is deciding.
	always bool
	// requires is what it needs before a job can say.
	requires []needs.Requirement
}

var definitions = []definition{
	{name: needs.KindIOS, title: "iOS", mac: true, native: true, always: true,
		requires: []needs.Requirement{{Key: needs.Xcode}, {Key: needs.IOSSimulator}}},
	{name: needs.KindMacOSApp, title: "macOS apps", mac: true, native: true,
		requires: []needs.Requirement{{Key: needs.Xcode}}},
	{name: needs.KindXcode, title: "Xcode builds", mac: true, native: true,
		requires: []needs.Requirement{{Key: needs.Xcode}}},
	{name: needs.KindAndroid, title: "Android", mac: true, linux: true, native: true, label: LabelAndroid, always: true,
		requires: []needs.Requirement{{Key: needs.JDK, Version: "17"}, {Key: needs.AndroidSDK}}},
	{name: needs.KindLinux, title: "Linux", mac: true, linux: true, always: true},
	{name: needs.KindLinuxDocker, title: "Linux with Docker", linux: true, native: true, label: LabelDockerHost,
		requires: []needs.Requirement{{Key: needs.DockerDaemon}}},
	{name: needs.KindMacOS, title: "macOS", mac: true, native: true},
}

// Build works it out.
func Build(in Input) Plan {
	p := Plan{Machine: in.Machine, Host: in.Host, Unread: in.Unread}
	byKind := map[string][]Job{}
	for _, j := range in.Jobs {
		if j.Report.Kind == needs.KindUnknown || j.Report.Kind == "" {
			p.Unclassified = append(p.Unclassified, j.Ref)
			continue
		}
		byKind[j.Report.Kind] = append(byKind[j.Report.Kind], j)
	}
	for _, d := range definitions {
		jobs := byKind[d.name]
		if len(jobs) == 0 && !d.always {
			continue
		}
		p.Kinds = append(p.Kinds, kindOn(d, jobs, in))
	}
	return p
}

func kindOn(d definition, jobs []Job, in Input) Kind {
	k := Kind{Name: d.name, Title: d.title, Label: d.label}
	for _, j := range jobs {
		k.Jobs = append(k.Jobs, j.Ref)
		if !slices.Contains(k.Repos, j.Repo) {
			k.Repos = append(k.Repos, j.Repo)
		}
	}
	if possible := d.mac && in.Host.Mac() || d.linux && in.Host.Linux(); !possible {
		k.State, k.Why = Impossible, whyNot(d)
		k.Label = ""
		return k
	}

	r := runnerFor(d, jobs, in)
	k.On = takes(d, jobs, r)
	if r != nil {
		k.Runner = r.Name
		k.Labelled = d.label != "" && hasLabel(r.Labels, d.label)
	}

	tc := in.Toolchains
	executor := needs.ExecutorNative
	if r != nil {
		tc, executor = r.Toolchains, r.Executor
	}
	if r == nil && d.native {
		k.Runner = NativeRunner
		k.Items = append(k.Items, newRunnerItem(in.Host))
	}
	if d.name == needs.KindLinux && r == nil {
		k.Why = "no runner here takes Linux jobs"
		k.Items = append(k.Items, Item{ID: "no-linux-runner", Title: "a runner labelled linux",
			Manual: []string{"start Docker, then add a docker runner to config.toml"}})
	}

	reports := make([]needs.Report, 0, len(jobs))
	for _, j := range jobs {
		reports = append(reports, j.Report)
	}
	if len(jobs) == 0 {
		reports = []needs.Report{{Kind: d.name, Requires: d.requires}}
	}
	k.Items = append(k.Items, gapItems(jobs, readingsOf(reports, jobs, in, r, tc, executor))...)
	if executor == needs.ExecutorNative {
		k.Items = append(k.Items, wiring(k, reports, tc, r)...)
	}
	k.Items = append(k.Items, settings(jobs, executor)...)
	k.Items = append(k.Items, secretItems(jobs)...)
	k.Items = append(k.Items, imageItems(d, r, in.Images)...)
	k.Notes = routeNotes(d, jobs, r)
	k.Proof = proofOf(d, tc, r)

	switch {
	case !k.On:
		k.State = Off
		if len(jobs) == 0 {
			k.Why = "no job in this machine's repositories needs it yet"
		}
	case k.Blocking() > 0:
		k.State = Missing
	default:
		k.State = Ready
	}
	return k
}

func whyNot(d definition) string {
	if d.mac {
		return "needs a Mac"
	}
	return "needs Linux"
}

// runnerFor is the runner a kind goes to: one of its jobs' that suits it, the
// first of its jobs' if none does, or else the one that could take it. A kind
// can be on two runners, and it is the one that suits it that says what the
// kind is here: Docker's jobs are native, and one that a label sends to a
// container says something about that job.
func runnerFor(d definition, jobs []Job, in Input) *Runner {
	want := needs.ExecutorDocker
	if d.native {
		want = needs.ExecutorNative
	}
	var first *Runner
	for _, j := range jobs {
		i := runnerAt(in, j.Runner)
		if i < 0 {
			continue
		}
		if in.Runners[i].Executor == want {
			return &in.Runners[i]
		}
		if first == nil {
			first = &in.Runners[i]
		}
	}
	if first != nil {
		return first
	}
	for i := range in.Runners {
		if r := &in.Runners[i]; r.Executor == want {
			return r
		}
	}
	return nil
}

func runnerAt(in Input, name string) int {
	if name == "" {
		return -1
	}
	return slices.IndexFunc(in.Runners, func(r Runner) bool { return r.Name == name })
}

// takes reports whether a runner here takes the kind: a job's labels reach
// it, or with no job asking yet, it carries the label that would.
func takes(d definition, jobs []Job, r *Runner) bool {
	if r == nil {
		return false
	}
	if slices.ContainsFunc(jobs, func(j Job) bool { return j.Runner != "" }) {
		return true
	}
	switch {
	case d.label != "":
		return hasLabel(r.Labels, d.label)
	case d.name == needs.KindLinux:
		return hasLabel(r.Labels, "linux")
	case len(jobs) == 0:
		return hasLabel(r.Labels, "xcode") || hasLabel(r.Labels, "macos")
	}
	return false
}

func hasLabel(labels []string, want string) bool {
	return slices.ContainsFunc(labels, func(l string) bool { return strings.EqualFold(l, want) })
}

func newRunnerItem(h system.Host) Item {
	return Item{ID: "new-runner", Title: "a native runner",
		Have: "this machine has only container runners",
		Config: []Change{{What: ChangeNewRunner, Value: NativeRunner,
			Line: fmt.Sprintf("a native runner named %s, which runs jobs on this %s one at a time", NativeRunner, hostWord(h))}}}
}

func hostWord(h system.Host) string {
	if h.Mac() {
		return "Mac"
	}
	return "machine"
}

// reading is what one job needs, against the runner it goes to.
type reading struct {
	report   needs.Report
	tc       system.Toolchains
	executor string
	// other names that runner when it is not the kind's.
	other string
}

// readingsOf puts each report beside the runner its job goes to: the kind's,
// unless a label sends the job somewhere else. Reports with no job, which a
// kind has before anything asks for it, are read against the kind's.
func readingsOf(reports []needs.Report, jobs []Job, in Input, r *Runner, tc system.Toolchains, executor string) []reading {
	out := make([]reading, 0, len(reports))
	for n, rep := range reports {
		one := reading{report: rep, tc: tc, executor: executor}
		if n < len(jobs) {
			if i := runnerAt(in, jobs[n].Runner); i >= 0 && (r == nil || in.Runners[i].Name != r.Name) {
				one.tc, one.executor, one.other = in.Runners[i].Toolchains, in.Runners[i].Executor, in.Runners[i].Name
			}
		}
		out = append(out, one)
	}
	return out
}

// gapItems is needs.Gaps over the kind's jobs, one item per thing missing,
// the JDK before what runs on it. A gap on a runner that is not the kind's
// says which, because the kind's own may have it.
func gapItems(jobs []Job, readings []reading) []Item {
	var out []Item
	at := map[string]int{}
	for i, rd := range readings {
		for _, g := range needs.Gaps(rd.report, rd.tc, rd.executor) {
			n, seen := at[g.Missing]
			if !seen {
				n = len(out)
				at[g.Missing] = n
				have := g.Have
				if have == "" && rd.other != "" {
					have = "on the " + rd.other + " runner"
				}
				out = append(out, Item{ID: g.Missing, Title: titleOf(g.Missing), Have: have, Run: g.Fix, Yours: g.Yours, Manual: g.Manual})
			}
			if i < len(jobs) && !slices.Contains(out[n].Jobs, jobs[i].Ref) {
				out[n].Jobs = append(out[n].Jobs, jobs[i].Ref)
			}
		}
	}
	slices.SortStableFunc(out, func(a, b Item) int { return rank(a) - rank(b) })
	return out
}

// toolTitles are the commands needs.Gap names, as a person calls what they
// belong to.
var toolTitles = map[string]string{
	"bundle": "Bundler", "node": "Node", "ruby": "Ruby", "bun": "Bun", "go": "Go", "cargo": "Rust",
	"python3": "Python", "pod": "CocoaPods", "xcodegen": "XcodeGen", "brew": "Homebrew",
}

func titleOf(missing string) string {
	if t, ok := toolTitles[missing]; ok {
		return t
	}
	return missing
}

func rank(it Item) int {
	if strings.HasPrefix(it.Title, "JDK ") {
		return 0
	}
	return 1
}

// wiring is what a step needs told: a toolchain can be on the machine and out
// of a step's sight, and the second JDK on Linux always is. A machine with a
// job running here already got by without it, so it is said, not demanded.
func wiring(k Kind, reports []needs.Report, tc system.Toolchains, r *Runner) []Item {
	var env map[string]string
	if r != nil {
		env = r.Env
	}
	name := k.Runner
	var out []Item
	for _, rep := range reports {
		q, ok := rep.Get(needs.JDK)
		if !ok || q.ProvidedBy != "" {
			continue
		}
		want := needs.WantedJDK(rep, q)
		home := tc.JDKHome(want)
		if want == 0 || home == "" || tc.StepJDK == want || hasHome(tc, want, env["JAVA_HOME"]) {
			continue
		}
		have := "a step finds no JDK"
		if tc.StepJDK > 0 {
			have = fmt.Sprintf("a step finds JDK %d", tc.StepJDK)
		}
		out = append(out, Item{ID: "java-home", Title: fmt.Sprintf("JAVA_HOME set to JDK %d", want), Have: have, Soft: k.On,
			Config: []Change{{What: ChangeEnv, Runner: name, Key: "JAVA_HOME", Value: home, Line: "JAVA_HOME = " + home}}})
		break
	}
	if tc.AndroidSDK != "" && env["ANDROID_HOME"] == "" && slices.ContainsFunc(reports, func(rep needs.Report) bool {
		return rep.Has(needs.AndroidSDK)
	}) {
		out = append(out, Item{ID: "android-home", Title: "ANDROID_HOME set", Soft: k.On,
			Config: []Change{{What: ChangeEnv, Runner: name, Key: "ANDROID_HOME", Value: tc.AndroidSDK, Line: "ANDROID_HOME = " + tc.AndroidSDK}}})
	}
	return out
}

func hasHome(tc system.Toolchains, major int, home string) bool {
	return home != "" && slices.ContainsFunc(tc.JDKHomes, func(j system.JDKInstall) bool {
		return j.Major == major && j.Home == home
	})
}

// settings is what this machine refuses of these jobs until config.toml says
// otherwise. Two of them give something up, and say what.
func settings(jobs []Job, executor string) []Item {
	var out []Item
	var unisolated, moving, allowed []string
	var refs []string
	for _, j := range jobs {
		if j.Unisolated && executor == needs.ExecutorNative {
			unisolated = append(unisolated, j.Ref)
		}
		if len(j.Moving) > 0 {
			moving = append(moving, j.Ref)
			refs = merge(refs, j.Moving)
		}
		allowed = merge(allowed, j.NotAllowed)
	}
	for _, a := range allowed {
		out = append(out, Item{ID: itemAllowed + a, Title: "the action " + a + " allowed",
			Config: []Change{{What: ChangeAllowed, Value: a, Line: fmt.Sprintf("[actions] allowed += %q", a)}}})
	}
	if len(moving) > 0 {
		out = append(out, Item{ID: itemMovingRefs, Title: "actions named by a tag accepted", Have: strings.Join(refs, ", "), Jobs: moving,
			Warning: "A tag can be moved to other code after you have read it. The other way is to pin each action to a commit in the workflow.",
			Config:  []Change{{What: ChangeMovingRefs, Line: "[actions] require_pinned = false"}}})
	}
	if len(unisolated) > 0 {
		out = append(out, Item{ID: "unisolated", Title: "remote actions allowed on this machine", Jobs: unisolated,
			Warning: "They run as you, with everything you can reach, and not in a container. Say yes on a machine that builds only repositories you trust.",
			Config:  []Change{{What: ChangeUnisolated, Line: "[actions] allow_unisolated = true"}}})
	}
	return out
}

func merge(into, more []string) []string {
	for _, m := range more {
		if !slices.Contains(into, m) {
			into = append(into, m)
		}
	}
	return into
}

// secretItems names, never shows, what these jobs read and have no value for
// here. Secrets are per machine, so a machine new to a repository has none.
func secretItems(jobs []Job) []Item {
	var out []Item
	for _, j := range jobs {
		if len(j.Secrets) == 0 {
			continue
		}
		id := itemSecrets + j.Repo
		i := slices.IndexFunc(out, func(it Item) bool { return it.ID == id })
		if i < 0 {
			out = append(out, Item{ID: id, Title: j.Repo + "'s secrets set here"})
			i = len(out) - 1
		}
		for _, name := range j.Secrets {
			argv := []string{"latem", "secrets", "set", "--repo", j.Repo, name}
			label := name
			if j.Env != "" {
				// A job in an environment reads the environment's value
				// first, and that is the one to set.
				argv = []string{"latem", "secrets", "set", "--repo", j.Repo, "--env", j.Env, name}
				label = name + " (" + j.Env + ")"
			}
			if !slices.Contains(out[i].Secrets, label) {
				out[i].Yours = append(out[i].Yours, argv)
				out[i].Secrets = append(out[i].Secrets, label)
			}
		}
		if !slices.Contains(out[i].Jobs, j.Ref) {
			out[i].Jobs = append(out[i].Jobs, j.Ref)
		}
	}
	for i := range out {
		out[i].Have = fmt.Sprintf("%d not set", len(out[i].Yours))
	}
	return out
}

func imageItems(d definition, r *Runner, images []Image) []Item {
	if d.name != needs.KindLinux || r == nil {
		return nil
	}
	var out []Item
	for _, img := range images {
		if img.Runner == r.Name && !img.Present {
			out = append(out, Item{ID: itemImage + img.Ref, Title: "its image, " + img.Ref, Soft: true,
				Run: [][]string{{"docker", "pull", "--", img.Ref}}})
		}
	}
	return out
}

// routeNotes says which jobs a label here would still not bring: a workflow
// that names another machine's labels has to name this kind's instead, and
// that is a change to the repository, not to this machine.
func routeNotes(d definition, jobs []Job, r *Runner) []string {
	if r == nil || d.label == "" {
		return nil
	}
	var out []string
	for _, j := range jobs {
		if j.Runner != "" {
			continue
		}
		have := append(slices.Clone(r.Labels), d.label)
		if !slices.ContainsFunc(j.Labels, func(l string) bool { return !hasLabel(have, l) }) {
			continue
		}
		out = append(out, fmt.Sprintf("%s asks for %s; it comes here once its workflow says runs-on: [%s]",
			j.Ref, strings.Join(j.Labels, ", "), d.label))
	}
	return out
}

// proofOf is what shows a kind works here. It is a check that the tools run in
// a step's environment, not a build: a real one of anything takes minutes,
// and the first job is that.
func proofOf(d definition, tc system.Toolchains, r *Runner) []Probe {
	var env map[string]string
	if r != nil {
		env = r.Env
	}
	switch d.name {
	case needs.KindAndroid:
		home, sdk := env["JAVA_HOME"], env["ANDROID_HOME"]
		if home == "" {
			home = tc.JDKHome(needs.AndroidJDK)
		}
		if sdk == "" {
			sdk = tc.AndroidSDK
		}
		if home == "" || sdk == "" {
			return nil
		}
		return []Probe{
			{Title: "the JDK compiles", Argv: []string{home + "/bin/javac", "-version"}, Env: env},
			{Title: "the SDK's tools run", Argv: []string{sdk + "/platform-tools/adb", "version"}, Env: env},
		}
	case needs.KindLinuxDocker:
		return []Probe{{Title: "Docker answers", Argv: []string{"docker", "info", "--format", "{{.ServerVersion}}"}, Env: env}}
	case needs.KindLinux:
		if r != nil && r.Executor == needs.ExecutorDocker {
			return []Probe{{Title: "Docker answers", Argv: []string{"docker", "info", "--format", "{{.ServerVersion}}"}}}
		}
	}
	return nil
}
