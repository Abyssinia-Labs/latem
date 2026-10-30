package needs

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"path"
	"regexp"
	"slices"
	"strings"

	"github.com/Abyssinia-Labs/latem/internal/registry"
	"github.com/Abyssinia-Labs/latem/internal/workflow"
	"github.com/Abyssinia-Labs/latem/internal/workflow/expr"
)

// Bounds on what one job's reading follows.
const (
	maxScriptDepth = 2
	maxScriptFiles = 32
	maxScriptBytes = 64 << 10
	maxActionDepth = 3
)

// Leg is a job as one matrix leg of it runs: the labels the plan resolved for
// it, and its matrix values.
type Leg struct {
	Labels []string
	Matrix map[string]any
}

// Job reads one leg of a workflow job. fsys is the repository, for the
// scripts its steps run and the local actions they use.
func Job(fsys fs.FS, wf *workflow.Workflow, key string, leg Leg) Report {
	r := newReader(fsys, leg)
	j := wf.Jobs[key]
	if j == nil {
		return Report{Kind: KindUnknown}
	}
	if j.Uses != "" {
		r.c.unknown = append(r.c.unknown, Evidence{File: wf.Path, Where: "job " + key,
			Snippet: "calls the reusable workflow " + j.Uses + ", which is not read here"})
	}
	if present(j.Services) {
		r.c.add(Requirement{Key: Services, Strength: Declared,
			Evidence: Evidence{File: wf.Path, Where: "job " + key, Snippet: "services:"}})
	}
	if present(j.Container) {
		r.c.add(Requirement{Key: Container, Strength: Declared,
			Evidence: Evidence{File: wf.Path, Where: "job " + key, Snippet: "container:"}})
	}
	wd := ""
	if wf.Defaults != nil && wf.Defaults.Run != nil {
		wd = wf.Defaults.Run.WorkingDirectory
	}
	if j.Defaults != nil && j.Defaults.Run != nil && j.Defaults.Run.WorkingDirectory != "" {
		wd = j.Defaults.Run.WorkingDirectory
	}
	r.steps(wf.Path, j.Steps, wd, 0)
	r.repoFacts()
	return r.c.report(r.platform)
}

// RegistryJob reads a .latem/jobs.yaml job.
func RegistryJob(fsys fs.FS, job *registry.Job) Report {
	r := newReader(fsys, Leg{Labels: job.RunsOn})
	for i, st := range job.Steps {
		where := fmt.Sprintf("step %d", i+1)
		if st.Name != "" {
			where = fmt.Sprintf("step %q", st.Name)
		}
		if st.Container != nil {
			r.c.add(Requirement{Key: Container, Strength: Declared,
				Evidence: Evidence{File: registry.Path, Where: where, Snippet: "image: " + st.Container.Image}})
			continue
		}
		r.script(st.Run, registry.Path, where, "", 0)
	}
	r.repoFacts()
	return r.c.report(r.platform)
}

type reader struct {
	fsys     fs.FS
	leg      Leg
	platform string
	c        *collector
	files    int
	// read notes what the job's commands touched that a repository fact
	// qualifies: a wrangler deploy, a gradle build.
	wranglerConfigs []string
	gradle          bool
}

func newReader(fsys fs.FS, leg Leg) *reader {
	return &reader{fsys: fsys, leg: leg, platform: platformOf(leg.Labels), c: newCollector()}
}

func (r *reader) steps(file string, steps []workflow.Step, wd string, depth int) {
	for i, st := range steps {
		where := fmt.Sprintf("step %d", i+1)
		if st.Name != "" {
			where = fmt.Sprintf("step %q", st.Name)
		}
		if r.skippedHere(st.If) {
			continue
		}
		stepWD := wd
		if st.WorkingDirectory != "" {
			stepWD = st.WorkingDirectory
		}
		if st.Uses != "" {
			r.action(file, where, st, depth)
			continue
		}
		r.script(r.substitute(st.Run), file, where, stepWD, 0)
	}
}

// skippedHere reports whether an if: is false on this leg whatever the run
// finds: one that reads only runner.os and the matrix, as A58 decides at
// compile.
func (r *reader) skippedHere(cond string) bool {
	cond = strings.TrimSpace(cond)
	if cond == "" || r.platform == "" {
		return false
	}
	if expr.Has(cond) {
		spans, err := expr.Spans(cond)
		if err != nil || len(spans) != 1 || spans[0].Start != 0 || spans[0].End != len(cond) {
			return false
		}
		cond = spans[0].Prog.Source()
	}
	prog, err := expr.Parse(cond)
	if err != nil {
		return false
	}
	contexts, funcs := prog.References()
	for _, c := range contexts {
		if c != "runner" && c != "matrix" {
			return false
		}
	}
	for _, f := range funcs {
		switch f {
		case "contains", "startswith", "endswith", "format", "join", "tojson", "fromjson":
		default:
			return false
		}
	}
	if keys, computed := prog.ContextKeys("runner"); computed || slices.ContainsFunc(keys, func(k string) bool {
		return !strings.EqualFold(k, "os")
	}) {
		return false
	}
	v, err := prog.Eval(expr.Scope{Contexts: map[string]expr.Value{
		"runner": map[string]any{"os": r.platform}, "matrix": r.leg.Matrix,
	}})
	return err == nil && !expr.IsTruthy(v)
}

var matrixRef = regexp.MustCompile(`\$\{\{\s*matrix\.([A-Za-z0-9_-]+)\s*\}\}`)
var anyExpr = regexp.MustCompile(`\$\{\{[^}]*\}\}`)

// substitute puts the leg's matrix values into a step's text, and a
// placeholder word for any other expression.
func (r *reader) substitute(s string) string {
	s = matrixRef.ReplaceAllStringFunc(s, func(m string) string {
		key := matrixRef.FindStringSubmatch(m)[1]
		if v, ok := r.leg.Matrix[key]; ok {
			return fmt.Sprint(v)
		}
		return "$EXPR"
	})
	return anyExpr.ReplaceAllString(s, "$EXPR")
}

func present(raw json.RawMessage) bool {
	s := strings.TrimSpace(string(raw))
	return s != "" && s != "null" && s != "{}" && s != "[]"
}

// --- actions

type provider struct{ key, versionInput string }

// providers install a tool for the job: a requirement they meet asks the
// machine for nothing but what the action itself needs.
var providers = map[string]provider{
	"actions/setup-node":            {Node, "node-version"},
	"oven-sh/setup-bun":             {Bun, "bun-version"},
	"actions/setup-go":              {Go, "go-version"},
	"actions/setup-python":          {Python, "python-version"},
	"actions/setup-java":            {JDK, "java-version"},
	"android-actions/setup-android": {AndroidSDK, ""},
	"ruby/setup-ruby":               {Ruby, "ruby-version"},
	"gradle/actions/setup-gradle":   {Gradle, "gradle-version"},
	"dtolnay/rust-toolchain":        {Rust, "toolchain"},
	"pnpm/action-setup":             {Node, ""},
}

// silent actions need nothing of the machine that the job does not already.
var silent = map[string]bool{
	"actions/checkout": true, "actions/cache": true, "actions/upload-artifact": true,
	"actions/download-artifact": true, "dorny/paths-filter": true, "actions/github-script": true,
	"actions/cache/restore": true, "actions/cache/save": true,
}

func (r *reader) action(file, where string, st workflow.Step, depth int) {
	uses := st.Uses
	ev := Evidence{File: file, Where: where, Snippet: "uses: " + uses}
	with := map[string]any{}
	_ = json.Unmarshal(st.With, &with)
	input := func(k string) string {
		if v, ok := with[k]; ok {
			return r.substitute(fmt.Sprint(v))
		}
		return ""
	}

	if strings.HasPrefix(uses, "docker://") {
		r.c.add(Requirement{Key: Container, Strength: Invoked, Evidence: ev})
		return
	}
	if strings.HasPrefix(uses, "./") {
		r.localAction(file, where, uses, depth)
		return
	}
	name, _, _ := strings.Cut(strings.ToLower(uses), "@")
	if p, ok := providers[name]; ok {
		q := Requirement{Key: p.key, Strength: Invoked, ProvidedBy: name, Evidence: ev}
		if p.versionInput != "" {
			if v := input(p.versionInput); v != "" && !strings.Contains(v, "$EXPR") {
				q.Version, q.VersionStrength = v, Declared
			}
		}
		if p.key == Ruby && input("bundler-cache") == "true" {
			r.c.add(Requirement{Key: Bundler, Strength: Invoked, ProvidedBy: name, Evidence: ev})
		}
		r.c.add(q)
		return
	}
	switch {
	case silent[name]:
	case name == "maxim-lobanov/setup-xcode":
		q := Requirement{Key: Xcode, Strength: Invoked, Evidence: ev}
		if v := input("xcode-version"); v != "" && !strings.Contains(v, "$EXPR") {
			q.Version, q.VersionStrength = majorOf(v), Declared
		}
		r.c.add(q)
	case name == "reactivecircus/android-emulator-runner":
		r.c.add(Requirement{Key: AndroidEmulator, Strength: Invoked, Evidence: ev})
		r.c.add(Requirement{Key: AndroidSDK, Strength: Invoked, Version: input("api-level"), Evidence: ev})
		r.script(input("script"), file, where, "", 0)
	case strings.HasPrefix(name, "docker/"):
		r.c.add(Requirement{Key: DockerDaemon, Strength: Invoked, Evidence: ev})
	case name == "cloudflare/wrangler-action":
		r.c.add(Requirement{Key: Node, Strength: Invoked, Evidence: ev})
		cmd := input("command")
		if cmd == "" {
			cmd = "deploy"
		}
		r.wrangler(append([]string{"wrangler"}, strings.Fields(cmd)...), input("workingDirectory"), ev)
	case name == "expo/expo-github-action":
		r.c.add(Requirement{Key: Node, Strength: Invoked, Evidence: ev})
	default:
		r.c.unknown = append(r.c.unknown, Evidence{File: file, Where: where,
			Snippet: "uses: " + uses + " — a remote action, whose contents are not read here"})
	}
}

func (r *reader) localAction(file, where, uses string, depth int) {
	dir := path.Clean(strings.TrimPrefix(uses, "./"))
	if depth >= maxActionDepth || !fs.ValidPath(dir) {
		r.c.unknown = append(r.c.unknown, Evidence{File: file, Where: where, Snippet: "uses: " + uses})
		return
	}
	for _, name := range []string{"action.yml", "action.yaml"} {
		data, err := fs.ReadFile(r.fsys, path.Join(dir, name))
		if err != nil {
			continue
		}
		a, err := workflow.ParseAction(data, dir)
		if err != nil {
			break
		}
		actionFile := path.Join(dir, name)
		switch strings.ToLower(a.Runs.Using) {
		case "composite":
			r.steps(actionFile, a.Runs.Steps, "", depth+1)
		case "docker":
			r.c.add(Requirement{Key: Container, Strength: Invoked,
				Evidence: Evidence{File: file, Where: where, Snippet: "uses: " + uses + " (a Docker action)"}})
		default:
			r.c.add(Requirement{Key: Node, Strength: Invoked,
				Evidence: Evidence{File: file, Where: where, Snippet: "uses: " + uses + " (a JavaScript action)"}})
		}
		return
	}
	r.c.unknown = append(r.c.unknown, Evidence{File: file, Where: where,
		Snippet: "uses: " + uses + " — no action.yml there"})
}

// --- scripts

// script reads the commands a shell script runs. file and where name it for
// evidence; wd is the directory it runs in, relative to the repository.
func (r *reader) script(text, file, where, wd string, depth int) {
	for _, c := range commands(text) {
		ev := Evidence{File: file, Where: where, Snippet: c.text}
		if where == "" {
			ev = Evidence{File: file, Line: c.line, Snippet: c.text}
		}
		r.command(c, ev, wd, depth)
	}
}

func (r *reader) command(c command, ev Evidence, wd string, depth int) {
	add := func(key string) { r.c.add(Requirement{Key: key, Strength: Invoked, Evidence: ev}) }
	if dd := c.env["DEVELOPER_DIR"]; dd != "" {
		if v := xcodeVersionIn(dd); v != "" {
			r.c.add(Requirement{Key: Xcode, Version: v, Strength: Invoked, VersionStrength: Declared, Evidence: ev})
		}
	}
	words := c.words
	name := path.Base(words[0])
	args := words[1:]
	switch name {
	case "xcodebuild":
		add(Xcode)
		r.xcodebuild(args, ev)
	case "xcrun":
		add(Xcode)
		if tool := firstNonFlag(args); tool != "" {
			switch tool {
			case "simctl":
				add(IOSSimulator)
			case "notarytool", "stapler":
				add(Notarize)
			}
		}
	case "xcode-select":
		add(Xcode)
		for i, a := range args {
			if (a == "-s" || a == "--switch") && i+1 < len(args) {
				if v := xcodeVersionIn(args[i+1]); v != "" {
					r.c.add(Requirement{Key: Xcode, Version: v, Strength: Invoked, VersionStrength: Declared, Evidence: ev})
				}
			}
		}
	case "simctl":
		add(IOSSimulator)
	case "codesign":
		add(Codesign)
	case "security":
		switch firstNonFlag(args) {
		case "create-keychain", "import", "set-key-partition-list", "find-identity", "unlock-keychain":
			add(Codesign)
		}
	case "notarytool", "stapler":
		add(Notarize)
	case "swift":
		add(Swift)
	case "xcodegen":
		add(XcodeGen)
	case "pod":
		add(CocoaPods)
	case "fastlane":
		add(Fastlane)
		add(Ruby)
	case "bundle":
		add(Bundler)
		add(Ruby)
	case "gem", "ruby":
		add(Ruby)
	case "gradlew", "gradle":
		add(Gradle)
		add(JDK)
		r.gradle = true
		for _, a := range args {
			if strings.Contains(a, "assemble") || strings.Contains(a, "bundle") || strings.Contains(a, "AndroidTest") {
				add(AndroidSDK)
			}
		}
	case "sdkmanager", "avdmanager", "adb", "aapt", "aapt2", "apksigner", "zipalign", "bundletool":
		add(AndroidSDK)
	case "emulator":
		add(AndroidEmulator)
		add(AndroidSDK)
	case "java", "javac", "keytool", "jarsigner":
		add(JDK)
	case "expo":
		r.expo(args, ev)
	case "eas", "eas-cli":
		if slices.Contains(args, "build") && slices.Contains(args, "--local") {
			r.nativeBuild(flagValue(args, "--platform", "-p"), ev)
		}
	case "docker", "docker-compose", "podman":
		if sub := firstNonFlag(args); sub != "" || name != "docker" {
			add(DockerDaemon)
		}
	case "wrangler":
		add(Node)
		r.wrangler(words, wd, ev)
	case "node", "npm", "npx", "pnpm", "yarn", "corepack":
		add(Node)
		r.packageScript(name, args, wd, depth)
	case "bun":
		add(Bun)
		r.packageScript(name, args, wd, depth)
	case "go", "gofmt":
		add(Go)
	case "python", "python3", "pip", "pip3", "pipx", "uv", "poetry":
		add(Python)
	case "cargo", "rustc", "rustup":
		add(Rust)
	case "playwright":
		if slices.Contains(args, "install") {
			add(Playwright)
			if slices.Contains(args, "--with-deps") {
				add(Apt)
			}
		}
	case "brew":
		add(Brew)
	case "apt-get", "apt", "dpkg":
		add(Apt)
	case "eval":
		r.c.unknown = append(r.c.unknown, Evidence{File: ev.File, Line: ev.Line, Where: ev.Where,
			Snippet: ev.Snippet + " — eval runs text read at run time"})
	case "bash", "sh", "zsh", "source", ".":
		if i := slices.Index(args, "-c"); i >= 0 && i+1 < len(args) {
			r.script(args[i+1], ev.File, ev.Where, wd, depth)
			return
		}
		if s := firstNonFlag(args); s != "" {
			r.follow(s, wd, ev, depth)
		}
	default:
		if strings.Contains(words[0], "/") || strings.HasSuffix(words[0], ".sh") {
			r.follow(words[0], wd, ev, depth)
		}
		if strings.Contains(strings.ToLower(name), "xcode") {
			for _, a := range args {
				if versionWord.MatchString(a) {
					r.c.add(Requirement{Key: Xcode, Version: majorOf(a), Strength: Inferred, Evidence: ev})
				}
			}
		}
	}
	if slices.Contains(c.wrappers, "sudo") {
		add(Sudo)
	}
	if slices.Contains(c.wrappers, "bundle exec") {
		add(Bundler)
		add(Ruby)
	}
}

var versionWord = regexp.MustCompile(`^[0-9]+(\.[0-9]+)*$`)

func (r *reader) xcodebuild(args []string, ev Evidence) {
	add := func(key string) { r.c.add(Requirement{Key: key, Strength: Invoked, Evidence: ev}) }
	test := slices.Contains(args, "test") || slices.Contains(args, "test-without-building")
	for i, a := range args {
		if i+1 >= len(args) {
			break
		}
		v := args[i+1]
		switch a {
		case "-destination":
			switch {
			case strings.Contains(v, "platform=iOS"), strings.Contains(v, "platform=iPadOS"):
				add(IOSSDK)
				if strings.Contains(v, "Simulator") && !strings.Contains(v, "generic/") && test {
					add(IOSSimulator)
				}
			case strings.Contains(v, "id=") && test:
				add(IOSSDK)
				add(IOSSimulator)
			case strings.Contains(v, "platform=macOS"):
				add(MacOSApp)
			}
		case "-sdk":
			switch {
			case strings.HasPrefix(v, "iphone"):
				add(IOSSDK)
			case strings.HasPrefix(v, "macosx"):
				add(MacOSApp)
			}
		}
	}
	if slices.Contains(args, "-exportArchive") {
		add(Codesign)
	}
}

func (r *reader) expo(args []string, ev Evidence) {
	r.c.add(Requirement{Key: Node, Strength: Invoked, Evidence: ev})
	switch sub := firstNonFlag(args); sub {
	case "prebuild":
		r.nativeBuild(flagValue(args, "--platform", "-p"), ev)
	case "run:ios":
		r.nativeBuild("ios", ev)
	case "run:android":
		r.nativeBuild("android", ev)
	}
}

// nativeBuild is a React Native build done on this machine, for platform —
// ios, android, or both when it is all or not said.
func (r *reader) nativeBuild(platform string, ev Evidence) {
	add := func(key string) { r.c.add(Requirement{Key: key, Strength: Invoked, Evidence: ev}) }
	if platform == "" || platform == "all" || platform == "ios" {
		add(Xcode)
		add(IOSSDK)
		add(CocoaPods)
	}
	if platform == "" || platform == "all" || platform == "android" {
		add(AndroidSDK)
		add(JDK)
	}
}

func (r *reader) wrangler(words []string, wd string, ev Evidence) {
	args := words[1:]
	if slices.Contains(args, "containers") {
		r.c.add(Requirement{Key: DockerDaemon, Strength: Invoked, Evidence: ev})
		return
	}
	if !slices.Contains(args, "deploy") {
		return
	}
	// A deploy builds the image of a Worker with containers, so it talks to
	// a daemon whenever the Worker's config declares one.
	cfg := flagValue(args, "--config", "-c")
	var candidates []string
	if cfg != "" {
		candidates = []string{path.Join(wd, cfg)}
	} else {
		for _, n := range []string{"wrangler.jsonc", "wrangler.json", "wrangler.toml"} {
			candidates = append(candidates, path.Join(wd, n))
		}
	}
	for _, p := range candidates {
		p = path.Clean(p)
		data, err := readCapped(r.fsys, p)
		if err != nil {
			continue
		}
		if strings.Contains(string(data), `"containers"`) || strings.Contains(string(data), "[[containers]]") {
			r.c.add(Requirement{Key: DockerDaemon, Strength: Inferred,
				Evidence: Evidence{File: ev.File, Line: ev.Line, Where: ev.Where,
					Snippet: ev.Snippet + " — " + p + " declares containers"}})
		}
		return
	}
}

// packageScript follows `bun run X`, `npm run X`, `pnpm X` and the like to
// package.json's script X.
func (r *reader) packageScript(tool string, args []string, wd string, depth int) {
	if depth >= maxScriptDepth || len(args) == 0 {
		return
	}
	name := args[0]
	if name == "run" || name == "run-script" {
		if len(args) < 2 {
			return
		}
		name = args[1]
	} else if tool == "npm" || tool == "node" || tool == "npx" || tool == "corepack" {
		return // npm X is a command of npm's, not a script
	}
	file := path.Clean(path.Join(wd, "package.json"))
	data, err := readCapped(r.fsys, file)
	if err != nil {
		return
	}
	var pkg struct {
		Scripts map[string]string `json:"scripts"`
	}
	if json.Unmarshal(data, &pkg) != nil {
		return
	}
	if body, ok := pkg.Scripts[name]; ok && r.files < maxScriptFiles {
		r.files++
		r.script(body, file, "scripts."+name, wd, depth+1)
	}
}

// follow reads a script a step runs, when it is in the repository.
func (r *reader) follow(p, wd string, ev Evidence, depth int) {
	for _, prefix := range []string{"$EXPR/", "$GITHUB_WORKSPACE/", "${GITHUB_WORKSPACE}/", "./"} {
		p = strings.TrimPrefix(p, prefix)
	}
	if strings.ContainsAny(p, "$*?") {
		r.c.unknown = append(r.c.unknown, Evidence{File: ev.File, Line: ev.Line, Where: ev.Where,
			Snippet: ev.Snippet + " — a script named at run time"})
		return
	}
	full := path.Clean(path.Join(wd, p))
	if strings.HasPrefix(p, "/") || !fs.ValidPath(full) {
		return // a system command by path
	}
	if depth >= maxScriptDepth || r.files >= maxScriptFiles {
		r.c.unknown = append(r.c.unknown, Evidence{File: ev.File, Line: ev.Line, Where: ev.Where,
			Snippet: ev.Snippet + " — not followed: too deep or too many scripts"})
		return
	}
	data, err := readCapped(r.fsys, full)
	if err != nil {
		r.c.unknown = append(r.c.unknown, Evidence{File: ev.File, Line: ev.Line, Where: ev.Where,
			Snippet: ev.Snippet + " — " + full + " is not in the repository"})
		return
	}
	r.files++
	r.script(string(data), full, "", path.Dir(full), depth+1)
}

func readCapped(fsys fs.FS, p string) ([]byte, error) {
	if fsys == nil {
		return nil, fs.ErrNotExist
	}
	data, err := fs.ReadFile(fsys, p)
	if err != nil {
		return nil, err
	}
	if len(data) > maxScriptBytes {
		return data[:maxScriptBytes], nil
	}
	return data, nil
}

// --- repository facts

var (
	compileSdk   = regexp.MustCompile(`compileSdk(?:Version)?\s*[=(]?\s*([0-9]+)`)
	jvmToolchain = regexp.MustCompile(`jvmToolchain\s*\(\s*([0-9]+)\s*\)`)
)

// repoFacts qualifies what the commands asked for with what the repository
// declares: the Xcode in .xcode-version, an Android project behind a gradle
// build, the SDK and JDK its build files name.
func (r *reader) repoFacts() {
	if q, ok := r.c.reqs[Xcode]; ok && q.VersionStrength < Declared {
		if data, err := readCapped(r.fsys, ".xcode-version"); err == nil {
			if v := majorOf(strings.TrimSpace(string(data))); v != "" {
				r.c.add(Requirement{Key: Xcode, Version: v, Strength: Inferred, VersionStrength: Declared,
					Evidence: Evidence{File: ".xcode-version", Line: 1, Snippet: strings.TrimSpace(string(data))}})
			}
		}
	}
	if !r.gradle {
		return
	}
	for _, f := range []string{"app/build.gradle.kts", "app/build.gradle", "android/app/build.gradle.kts", "android/app/build.gradle"} {
		data, err := readCapped(r.fsys, f)
		if err != nil || !strings.Contains(string(data), "android") {
			continue
		}
		q := Requirement{Key: AndroidSDK, Strength: Inferred, Evidence: Evidence{File: f, Snippet: "an Android build"}}
		if m := compileSdk.FindSubmatch(data); m != nil {
			q.Version, q.VersionStrength = string(m[1]), Declared
		}
		r.c.add(q)
		if m := jvmToolchain.FindSubmatch(data); m != nil {
			r.c.add(Requirement{Key: JDK, Version: string(m[1]), Strength: Inferred, VersionStrength: Declared,
				Evidence: Evidence{File: f, Snippet: "jvmToolchain(" + string(m[1]) + ")"}})
		}
		return
	}
}

// --- helpers

var xcodePath = regexp.MustCompile(`Xcode[_-]?([0-9]+(?:\.[0-9]+)*)(?:[-_ ]?[Bb]eta)?\.app`)

// xcodeVersionIn reads the Xcode a path names: /Applications/Xcode_27.app.
func xcodeVersionIn(p string) string {
	if m := xcodePath.FindStringSubmatch(p); m != nil {
		return majorOf(m[1])
	}
	return ""
}

// majorOf is a version's first number: 27 of 27.0.1.
func majorOf(v string) string {
	v = strings.TrimSpace(v)
	if i := strings.IndexByte(v, '.'); i >= 0 {
		v = v[:i]
	}
	if !versionWord.MatchString(v) {
		return ""
	}
	return v
}

func firstNonFlag(args []string) string {
	for _, a := range args {
		if !strings.HasPrefix(a, "-") {
			return a
		}
	}
	return ""
}

// flagValue reads --flag value or --flag=value.
func flagValue(args []string, names ...string) string {
	for i, a := range args {
		for _, n := range names {
			if a == n && i+1 < len(args) {
				return args[i+1]
			}
			if v, ok := strings.CutPrefix(a, n+"="); ok {
				return v
			}
		}
	}
	return ""
}
