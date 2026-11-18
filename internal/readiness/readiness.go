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
	"maps"
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
	// Vault is what this machine has of the vault, which decides what closes
	// a secret or a variable no store here holds (A97).
	Vault Vault
	// Docker is the daemon the agent's docker runners use (A106).
	Docker Docker
}

// Vault is latem vault, formerly OpenVault, on this machine, as setup found
// it (A86, A96, A97, A104). Its zero value is a machine without it.
type Vault struct {
	// Installed: the vault's command is here, or a vault is, or its service
	// answers.
	Installed bool `json:"installed"`
	// Command is the vault's command by name, latem-vault or openvault, as
	// it was found; "" is latem-vault (A104).
	Command string `json:"command,omitempty"`
	// KeyOpensAlone: this device's key is known to open with nobody there,
	// in the Secure Enclave or a key file without a passphrase, so a service
	// started at login can open it (A104).
	KeyOpensAlone bool `json:"key_opens_alone,omitempty"`
	// Here: this device holds a vault, which `latem-vault join` makes.
	Here bool `json:"here"`
	// On: config.toml says `[secrets] vault = true`.
	On bool `json:"on"`
	// Answers: its service answered on its socket.
	Answers bool `json:"answers"`
	// Device is this device as `latem-vault scope` names it, when it answered.
	Device string `json:"device,omitempty"`
	// Held are the configured repositories this device holds a collection
	// for, when it answered.
	Held []string `json:"held,omitempty"`
	// Homebrew: brew is here, to install the vault with when it is wanted
	// and not installed (A110). Only asked then.
	Homebrew bool `json:"homebrew,omitempty"`
}

// command is the vault's command, by the name it was found by.
func (v Vault) command() string {
	if v.Command == "" {
		return VaultCommand
	}
	return v.Command
}

// VaultCommand is the vault's command when none was found.
const VaultCommand = "latem-vault"

// VaultFormula is the vault's Homebrew formula, in Abyssinia Labs' tap
// (A110).
const VaultFormula = "abyssinia-labs/tap/latem-vault"

// VaultInstall is what installs the vault with Homebrew, in order: a formula
// from a tap that is not Homebrew's own is refused until it is trusted.
func VaultInstall() [][]string {
	return [][]string{{"brew", "trust", "--formula", VaultFormula}, {"brew", "install", VaultFormula}}
}

// VaultInstallLine says how to install the vault, for a person.
const VaultInstallLine = "`brew trust --formula " + VaultFormula + "`, then `brew install " + VaultFormula + "`"

// Ready reports a vault this machine could read and does not: here, its
// service answering, and holding a collection for one of the machine's
// repositories. Its jobs' names are then counted as though it were read,
// and turning it on is the item that closes them.
func (v Vault) Ready() bool { return !v.On && v.Here && v.Answers && len(v.Held) > 0 }

// reads reports whether a name's fix is the vault's: it is read, or would
// be by turning it on.
func (v Vault) reads() bool { return (v.On || v.Ready()) && v.Answers }

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
	// Secrets are the names it reads that have no value here, Vars the
	// variables, and Env the GitHub environment it runs in, which scopes
	// them.
	Secrets []string
	Vars    []string
	Env     string
	// FromVault: a name it reads is held by the vault alone, so reading the
	// vault is what gives it one (A97).
	FromVault bool
	// Aliases are the labels it asks for that no runner here carries, each
	// with the labels of the runner its name says should take it: the
	// [label_aliases] entry that brings it here (A101).
	Aliases map[string][]string
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
	// environment when it has one: "TOKEN (staging)". Vars are a variables
	// item's, the same way (A97).
	Secrets []string `json:"secrets,omitempty"`
	Vars    []string `json:"vars,omitempty"`
}

// Item IDs that Advert treats apart, the first and last followed by a name.
const (
	itemAllowed    = "allowed-"
	itemMovingRefs = "moving-refs"
	itemImage      = "image-"
	// ItemRunsOn is the labels a label-less kind's jobs ask for and its
	// runner lacks (A107).
	ItemRunsOn  = "runs-on-labels"
	itemSecrets = "secrets-"
	itemVars    = "vars-"
	// itemLabelAlias is followed by the label (A101).
	itemLabelAlias = "label-alias-"
	// The vault's, the last followed by a repository (A97). Each was
	// openvault-… before latem vault was renamed (A104).
	itemVaultOn      = "vault-on"
	itemVaultService = "vault-service"
	itemVaultJoin    = "vault-join"
	// ItemVaultInstall is the vault's command, wanted and not here (A110).
	ItemVaultInstall = "vault-install"
	itemCollection   = "vault-collection-"
	oldVaultPrefix   = "openvault-"
)

// CurrentID is the id an item has now for id, and whether id was one it had
// before: openvault-on, openvault-service, openvault-join and
// openvault-collection-R are vault-… since A104. `setup apply` takes the old
// ones for one release.
func CurrentID(id string) (string, bool) {
	rest, ok := strings.CutPrefix(id, oldVaultPrefix)
	if !ok {
		return id, false
	}
	switch {
	case rest == "on", rest == "service", rest == "join", strings.HasPrefix(rest, "collection-"):
		return "vault-" + rest, true
	}
	return id, false
}

// Change is one edit to config.toml.
type Change struct {
	// What is one of the Change* values.
	What string `json:"what"`
	// Runner is the runner it applies to, or "" for the machine.
	Runner string `json:"runner,omitempty"`
	Key    string `json:"key,omitempty"`
	Value  string `json:"value,omitempty"`
	// Labels are what an alias maps Key onto (A101).
	Labels []string `json:"labels,omitempty"`
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
	ChangeVault      = "vault"      // [secrets] vault = true (A97, A104)
	// ChangeRunsOnLabel: Runner gains the label Value, which a job's runs-on
	// names. It sends the machine every job that asks for it, so only a
	// person at a terminal says yes to it (A107).
	ChangeRunsOnLabel = "runs-on-label"
	// ChangeLabelAlias is [label_aliases] Key = Labels (A101). Like the two
	// [actions] settings, it is a person's to agree to at a terminal (A100).
	ChangeLabelAlias = "label-alias"
)

// AliasWarning is said before a [label_aliases] entry is written (A100): it is
// why an alias is the person's to agree to.
const AliasWarning = "An alias sends this machine every job of its repositories that asks for the label, " +
	"and tells the control plane to send them here."

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
	k.Items = append(k.Items, aliasItems(jobs)...)
	k.Items = append(k.Items, settings(jobs, executor)...)
	if !k.On && d.label == "" && r != nil && r.Executor == needs.ExecutorNative {
		k.Items = append(k.Items, runsOnItem(k.Title, jobs, r)...)
	}
	k.Items = append(k.Items, secretItems(jobs, in.Vault)...)
	if d.name == needs.KindLinux && r != nil && r.Executor == needs.ExecutorDocker {
		k.Items = append(k.Items, dockerAgentItem(in.Docker, in.Host, k.Jobs)...)
	}
	k.Items = append(k.Items, imageItems(d, r, in.Images)...)
	k.Notes = routeNotes(d, jobs, r)
	k.Proof = proofOf(d, tc, r, in.Docker)

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

// runsOnItem is what turns on a kind that no label of its own switches (iOS,
// macOS apps, Xcode builds, macOS): its jobs reach a runner here once the
// runner carries every label their runs-on names. It names each one, and the
// labels line config.toml would then say (A107).
func runsOnItem(title string, jobs []Job, r *Runner) []Item {
	var lacking, refs []string
	for _, j := range jobs {
		if j.Runner != "" {
			continue
		}
		var mine []string
		for _, l := range j.Labels {
			// A label an alias would bring here is the alias item's: an
			// alias names a vendor's label once, for every runner (A101).
			if _, aliased := j.Aliases[l]; aliased {
				continue
			}
			if !hasLabel(r.Labels, l) {
				mine = append(mine, l)
			}
		}
		if len(mine) == 0 {
			continue
		}
		refs = append(refs, j.Ref)
		lacking = merge(lacking, mine)
	}
	if len(lacking) == 0 {
		return nil
	}
	after := append(slices.Clone(r.Labels), lacking...)
	quoted := make([]string, len(after))
	for i, l := range after {
		quoted[i] = fmt.Sprintf("%q", l)
	}
	it := Item{ID: ItemRunsOn, Title: fmt.Sprintf("the %s runner labelled for %s jobs", r.Name, title), Jobs: refs,
		Have: fmt.Sprintf("their runs-on names %s, which the %s runner lacks", strings.Join(lacking, ", "), r.Name),
		Warning: fmt.Sprintf("Every job of this machine's repositories whose runs-on names these labels will come to the %s runner. "+
			"The line in its [[runner]] table becomes: labels = [%s]", r.Name, strings.Join(quoted, ", "))}
	for _, l := range lacking {
		it.Config = append(it.Config, Change{What: ChangeRunsOnLabel, Runner: r.Name, Value: l,
			Line: fmt.Sprintf("the %s runner gains the label %s", r.Name, l)})
	}
	return []Item{it}
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

// aliasItems is one item for each label these jobs ask for that no runner here
// carries and whose name says which runner should (A101): the alias that
// brings them here. Without it a kind can look ready and never be sent its
// jobs. It is a person's to agree to, so it carries AliasWarning.
func aliasItems(jobs []Job) []Item {
	var out []Item
	at := map[string]int{}
	for _, j := range jobs {
		for _, label := range slices.Sorted(maps.Keys(j.Aliases)) {
			n, seen := at[label]
			if !seen {
				n = len(out)
				at[label] = n
				targets := j.Aliases[label]
				line := fmt.Sprintf("[label_aliases] %q = %s", label, quotedList(targets))
				out = append(out, Item{ID: itemLabelAlias + label, Title: "a label alias for " + label,
					Have: "no runner here carries it", Warning: AliasWarning,
					Config: []Change{{What: ChangeLabelAlias, Key: label, Labels: slices.Clone(targets), Line: line}}})
			}
			if !slices.Contains(out[n].Jobs, j.Ref) {
				out[n].Jobs = append(out[n].Jobs, j.Ref)
			}
		}
	}
	return out
}

// quotedList is labels as a TOML array of strings.
func quotedList(labels []string) string {
	q := make([]string, len(labels))
	for i, l := range labels {
		q[i] = fmt.Sprintf("%q", l)
	}
	return "[" + strings.Join(q, ", ") + "]"
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
			Warning: WarnMovingRefs,
			Config:  []Change{{What: ChangeMovingRefs, Line: "[actions] require_pinned = false"}}})
	}
	if len(unisolated) > 0 {
		out = append(out, Item{ID: "unisolated", Title: "remote actions allowed on this machine", Jobs: unisolated,
			Warning: WarnUnisolated,
			Config:  []Change{{What: ChangeUnisolated, Line: "[actions] allow_unisolated = true"}}})
	}
	return out
}

// WarnMovingRefs and WarnUnisolated say what each of the two settings that
// widen what may run here gives up. A person reads one before saying yes to
// it: on the setup screen, under `setup apply`, and under `setup --like`.
const (
	WarnMovingRefs = "A tag can be moved to other code after you have read it. The other way is to pin each action to a commit in the workflow."
	WarnUnisolated = "They run as you, with everything you can reach, and not in a container. Say yes on a machine that builds only repositories you trust."
)

func merge(into, more []string) []string {
	for _, m := range more {
		if !slices.Contains(into, m) {
			into = append(into, m)
		}
	}
	return into
}

// secretItems names, never shows, what these jobs read and have no value for
// here, and says what closes each: the vault first when the machine has it,
// and the step that is in the way of it before the names (A97). Secrets are
// per machine unless the vault holds them, so a machine new to a repository
// has none.
func secretItems(jobs []Job, v Vault) []Item {
	var lacking, fromVault []string
	for _, j := range jobs {
		if len(j.Secrets)+len(j.Vars) > 0 {
			lacking = append(lacking, j.Ref)
		}
		if j.FromVault || (v.Ready() && len(j.Secrets)+len(j.Vars) > 0) {
			fromVault = append(fromVault, j.Ref)
		}
	}
	var out []Item
	switch {
	case v.On && !v.Answers:
		// Every name it holds is unknown until it answers, so nothing past
		// it can be said.
		if len(lacking) == 0 {
			return nil
		}
		if !v.Installed {
			return []Item{installItem(v, lacking), joinItem(v, lacking, false)}
		}
		if !v.Here {
			return []Item{joinItem(v, lacking, false)}
		}
		return []Item{serviceItem(v, lacking, false)}
	case v.Ready() && len(fromVault) > 0:
		out = append(out, Item{ID: itemVaultOn, Title: "secrets and variables read from the vault", Jobs: fromVault,
			Have:   "not read: this device's vault holds collection " + strings.Join(v.Held, ", "),
			Config: []Change{{What: ChangeVault, Line: "[secrets] vault = true"}}})
	case !v.On && v.Installed && !v.Here && len(lacking) > 0:
		out = append(out, joinItem(v, lacking, true))
	case !v.On && v.Here && !v.Answers && len(lacking) > 0:
		out = append(out, serviceItem(v, lacking, true))
	}

	var repos []string
	byRepo := map[string][]Job{}
	for _, j := range jobs {
		if len(j.Secrets)+len(j.Vars) == 0 {
			continue
		}
		if _, ok := byRepo[j.Repo]; !ok {
			repos = append(repos, j.Repo)
		}
		byRepo[j.Repo] = append(byRepo[j.Repo], j)
	}
	for _, repo := range repos {
		if v.reads() && !slices.Contains(v.Held, repo) {
			out = append(out, collectionItem(v, repo, byRepo[repo]))
			continue
		}
		if it, ok := namesItem(repo, byRepo[repo], v, false); ok {
			out = append(out, it)
		}
		if it, ok := namesItem(repo, byRepo[repo], v, true); ok {
			out = append(out, it)
		}
	}
	return out
}

// installItem is a vault the machine reads and has no command for (A110):
// latem installs it from Abyssinia Labs' tap, trusting the formula first,
// when Homebrew is here, and says that Homebrew is not when it is not.
func installItem(v Vault, jobs []string) Item {
	it := Item{ID: ItemVaultInstall, Title: "latem vault installed", Have: "not installed", Jobs: jobs}
	if v.Homebrew {
		it.Run = VaultInstall()
		return it
	}
	it.Have = "not installed, and Homebrew is not here to install it with"
	it.Manual = []string{"install Homebrew from https://brew.sh, then " + VaultInstallLine +
		"; or put latem-vault on PATH from https://github.com/Abyssinia-Labs/latem-vault/releases"}
	return it
}

// joinItem is a device not in the vault: the person's, here and on a device
// already in it. Soft when the machine does not read the vault, where it is
// one way of several.
func joinItem(v Vault, jobs []string, soft bool) Item {
	return Item{ID: itemVaultJoin, Title: "this device in the vault", Have: "no vault on this device",
		Jobs: jobs, Soft: soft,
		Yours: [][]string{{v.command(), "join"}},
		Manual: []string{"then approve it on a device already in the vault: `" + v.command() + " approve`, " +
			"with --read for each repository's collection this machine builds"}}
}

// serviceItem is a service that does not answer. latem runs its install,
// which needs no sudo (OpenVault 0.2.2 and later). The install refuses a key
// that opens with a passphrase, which is said only when this device's key
// is not known to open alone (A104): a key in the Secure Enclave has none.
func serviceItem(v Vault, jobs []string, soft bool) Item {
	it := Item{ID: itemVaultService, Title: "the vault's service answering", Have: "it does not answer",
		Jobs: jobs, Soft: soft,
		Run: [][]string{{v.command(), "service", "install"}}}
	if !v.KeyOpensAlone {
		it.Manual = []string{"it refuses a key that opens with a passphrase, which nobody would be there to type: " +
			"`" + v.command() + " keys passphrase --remove`, or start `" + v.command() + " service` by hand after each reboot"}
	}
	return it
}

// collectionItem is a repository this device holds no collection for: the
// person's, on an admin device, which every one of its names waits on.
func collectionItem(v Vault, repo string, jobs []Job) Item {
	device := v.Device
	if device == "" {
		device = "<this device>"
	}
	it := Item{ID: itemCollection + repo, Title: repo + "'s collection in the vault on this device",
		Yours: [][]string{{v.command(), "scope", device, repo, "read"}},
		Manual: []string{"on an admin device of the vault, not this one; if the vault has no collection " + repo +
			" yet, `" + v.command() + " collections create " + repo + "` there first, then `" + v.command() + " set` each name"}}
	for _, j := range jobs {
		it.Secrets = mergeLabels(it.Secrets, j.Secrets, j.Env)
		it.Vars = mergeLabels(it.Vars, j.Vars, j.Env)
		if !slices.Contains(it.Jobs, j.Ref) {
			it.Jobs = append(it.Jobs, j.Ref)
		}
	}
	it.Have = fmt.Sprintf("none here; %d not set", len(it.Secrets)+len(it.Vars))
	return it
}

// namesItem is a repository's secrets, or with vars its variables, that no
// store here holds, each with the command that sets it where it will be read:
// the vault when the machine reads it, and otherwise this machine.
func namesItem(repo string, jobs []Job, v Vault, vars bool) (Item, bool) {
	it := Item{ID: itemSecrets + repo, Title: repo + "'s secrets set here"}
	if vars {
		it = Item{ID: itemVars + repo, Title: repo + "'s variables set here"}
	}
	vaulted := v.reads()
	for _, j := range jobs {
		names := j.Secrets
		if vars {
			names = j.Vars
		}
		for _, name := range names {
			label := name
			if j.Env != "" {
				label = name + " (" + j.Env + ")"
			}
			have := it.Secrets
			if vars {
				have = it.Vars
			}
			if slices.Contains(have, label) {
				continue
			}
			it.Yours = append(it.Yours, setArgv(v, repo, j.Env, name, vaulted, vars))
			if vars {
				it.Vars = append(it.Vars, label)
			} else {
				it.Secrets = append(it.Secrets, label)
			}
		}
		if len(names) > 0 && !slices.Contains(it.Jobs, j.Ref) {
			it.Jobs = append(it.Jobs, j.Ref)
		}
	}
	if len(it.Yours) == 0 {
		return Item{}, false
	}
	it.Have = fmt.Sprintf("%d not set", len(it.Yours))
	switch {
	case vaulted:
		it.Manual = []string{"on a device that may write collection " + repo + ", each value on stdin; " +
			"it reaches this machine when the vault syncs"}
	case !v.Installed:
		it.Manual = []string{"or keep them in the vault, one copy every machine reads: " +
			"docs/secrets.md, \"From the vault\""}
	}
	return it, true
}

// setArgv is the command that sets one name where a job of repo in env reads
// it first: the environment's own when the job has one.
func setArgv(v Vault, repo, env, name string, vaulted, vars bool) []string {
	switch {
	case vaulted:
		entry := repo + "/" + name
		if env != "" {
			entry = repo + "/" + env + "/" + name
		}
		kind := "--secret"
		if vars {
			kind = "--var"
		}
		return []string{v.command(), "set", entry, kind}
	case vars:
		argv := []string{"latem", "vars", "set", "--repo", repo}
		if env != "" {
			argv = append(argv, "--env", env)
		}
		return append(argv, name, "<value>")
	}
	argv := []string{"latem", "secrets", "set", "--repo", repo}
	if env != "" {
		argv = append(argv, "--env", env)
	}
	return append(argv, name)
}

func mergeLabels(into, names []string, env string) []string {
	for _, n := range names {
		if env != "" {
			n += " (" + env + ")"
		}
		if !slices.Contains(into, n) {
			into = append(into, n)
		}
	}
	return into
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
func proofOf(d definition, tc system.Toolchains, r *Runner, dk Docker) []Probe {
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
			// The agent's daemon, not the context's: the command follows
			// DOCKER_HOST before its context, and the agent reads nothing else
			// (A106).
			p := Probe{Title: "Docker answers", Argv: []string{"docker", "info", "--format", "{{.ServerVersion}}"}}
			if dk.Host != "" {
				p.Title = "Docker answers where the agent looks"
				p.Env = map[string]string{"DOCKER_HOST": dk.Host}
			}
			return []Probe{p}
		}
	}
	return nil
}
