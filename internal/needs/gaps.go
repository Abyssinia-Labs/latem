package needs

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/Abyssinia-Labs/latem/internal/system"
)

// Gap is something a job needs that the runner it would run on does not have
// (A59).
type Gap struct {
	Requirement Requirement
	// Missing is what is wanting: "Xcode 27", "an iOS simulator runtime".
	Missing string
	// Have is what the machine has instead, when it has something.
	Have string
	// Fix are commands that would close it, each an argv: `latem setup` runs
	// one as it is, once asked, and never through a shell (INV-3). Yours are
	// commands too, for a person to type: they want sudo, or a licence read
	// and accepted, which latem never does (A60). Manual are steps for a
	// person that no command is.
	Fix    [][]string
	Yours  [][]string
	Manual []string
}

// Fixes renders Fix as command lines a person could type.
func (g Gap) Fixes() []string { return commandLines(g.Fix) }

// YourLines renders Yours as lines to type.
func (g Gap) YourLines() []string { return commandLines(g.Yours) }

func commandLines(argvs [][]string) []string {
	out := make([]string, len(argvs))
	for i, argv := range argvs {
		out[i] = CommandLine(argv)
	}
	return out
}

var plainWord = regexp.MustCompile(`^[A-Za-z0-9_@%+=:,./-]+$`)

// CommandLine renders argv as one line to type, quoting what a shell would
// otherwise read: sdkmanager "platforms;android-36".
func CommandLine(argv []string) string {
	words := make([]string, len(argv))
	for i, a := range argv {
		words[i] = a
		if !plainWord.MatchString(a) {
			words[i] = `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`, `$`, `\$`, "`", "\\`").Replace(a) + `"`
		}
	}
	return strings.Join(words, " ")
}

// Executors, as a runner's config names them.
const (
	ExecutorNative = "native"
	ExecutorDocker = "docker"
	ExecutorTart   = "tart"
)

// toolFixes is what installs a tool on a Mac, and on Linux, when there is one
// command for it: an argv, or — where the only way is a pipe into a shell —
// a line for a person.
var toolFixes = map[string]struct {
	tool       string
	mac, linux []string
	byHand     string
}{
	XcodeGen:  {tool: "xcodegen", mac: brew("xcodegen")},
	CocoaPods: {tool: "pod", mac: brew("cocoapods")},
	Fastlane:  {tool: "fastlane", mac: brew("fastlane")},
	Bundler:   {tool: "bundle", mac: []string{"gem", "install", "bundler"}, linux: []string{"gem", "install", "bundler"}},
	Ruby:      {tool: "ruby", mac: brew("ruby")},
	Brew:      {tool: "brew", byHand: "install Homebrew, from https://brew.sh"},
	Python:    {tool: "python3", mac: brew("python")},
	Node:      {tool: "node", mac: brew("node")},
	Bun:       {tool: "bun", mac: brew("oven-sh/bun/bun"), byHand: "curl -fsSL https://bun.sh/install | bash"},
	Go:        {tool: "go", mac: brew("go")},
	Rust:      {tool: "cargo", mac: brew("rust")},
}

func brew(args ...string) []string { return append([]string{"brew", "install"}, args...) }

// packages names what installs a tool in each Linux family's packages.
var packages = map[string]map[string]string{
	Node:    {system.FamilyArch: "nodejs", system.FamilyDebian: "nodejs", system.FamilyFedora: "nodejs"},
	Ruby:    {system.FamilyArch: "ruby", system.FamilyDebian: "ruby-full", system.FamilyFedora: "ruby"},
	Bundler: {system.FamilyArch: "ruby-bundler", system.FamilyDebian: "ruby-bundler", system.FamilyFedora: "rubygem-bundler"},
	Python:  {system.FamilyArch: "python", system.FamilyDebian: "python3", system.FamilyFedora: "python3"},
	Go:      {system.FamilyArch: "go", system.FamilyDebian: "golang-go", system.FamilyFedora: "golang"},
	Rust:    {system.FamilyArch: "rust", system.FamilyDebian: "cargo", system.FamilyFedora: "cargo"},
	// Docker's own engine, under each family's name for it.
	DockerDaemon: {system.FamilyArch: "docker", system.FamilyDebian: "docker.io", system.FamilyFedora: "moby-engine"},
}

// packageInstall is the command that installs pkg on a family, for a person
// to type: it wants sudo.
func packageInstall(family, pkg string) []string {
	switch family {
	case system.FamilyArch:
		return []string{"sudo", "pacman", "-S", "--needed", pkg}
	case system.FamilyDebian:
		return []string{"sudo", "apt-get", "install", "-y", pkg}
	case system.FamilyFedora:
		return []string{"sudo", "dnf", "install", "-y", pkg}
	}
	return nil
}

// InstallLine is the command that installs a requirement on host, for doctor
// to say: a package on Linux, Homebrew on a Mac. "" when latem knows none.
func InstallLine(host system.Host, key, version string) string {
	if !host.Linux() {
		switch key {
		case JDK:
			return CommandLine(brew("openjdk@" + version))
		case Ruby:
			return CommandLine(brew("ruby"))
		}
		return ""
	}
	pkg := packages[key][host.Family]
	if key == JDK {
		pkg = jdkPackage(host.Family, version)
	}
	if argv := packageInstall(host.Family, pkg); pkg != "" && argv != nil {
		return CommandLine(argv)
	}
	return ""
}

// PackageLine is the line that installs a package a family calls name.
func PackageLine(family, name string) string { return CommandLine(packageInstall(family, name)) }

// jdkPackage names a JDK in a family's packages.
func jdkPackage(family, major string) string {
	switch family {
	case system.FamilyArch:
		return "jdk" + major + "-openjdk"
	case system.FamilyDebian:
		return "openjdk-" + major + "-jdk"
	case system.FamilyFedora:
		return "java-" + major + "-openjdk-devel"
	}
	return ""
}

// AndroidJDK is the JDK an Android job gets when it does not name one, which
// few do: AGP 9 takes it, and a newer one is refused rather than tolerated.
const AndroidJDK = 17

// WantedJDK is the major a job's JDK requirement comes to: the one it names,
// or Android's. 0 asks for any.
func WantedJDK(r Report, q Requirement) int {
	if n, err := strconv.Atoi(q.Version); err == nil {
		return n
	}
	if r.Kind == KindAndroid {
		return AndroidJDK
	}
	return 0
}

// Gaps compares what a job needs with what the machine has, for a job that
// would run on a runner of executor. Only a native runner's needs are the
// host's: a docker runner's are its image's, which this does not open, save
// for what no image can give — a Docker daemon, and services; a tart
// runner's live in its VM, and are not known here.
func Gaps(r Report, t system.Toolchains, executor string) []Gap {
	var gaps []Gap
	add := func(q Requirement, missing, have string, fix [][]string, manual ...string) {
		gaps = append(gaps, Gap{Requirement: q, Missing: missing, Have: have, Fix: fix, Manual: manual})
	}
	// The machine says, when it has been asked; a report's platform is what
	// the job's labels hoped for.
	linux := t.Host.Linux() || (t.Host.OS == "" && r.Platform == "Linux")
	for _, q := range r.Requires {
		switch q.Key {
		case DockerDaemon:
			if executor == ExecutorDocker {
				add(q, "a Docker daemon", "", nil,
					"latem's docker runner gives a step no Docker daemon; run this job on a native Linux runner")
				continue
			}
		case Services, Container:
			add(q, q.Key+":", "", nil, "latem does not run a job's "+q.Key+": (A44); it needs a job of its own")
			continue
		}
		if executor != ExecutorNative || q.ProvidedBy != "" {
			continue
		}
		switch q.Key {
		case Xcode:
			switch {
			case len(t.Xcodes) == 0:
				add(q, xcodeName(q.Version), "", nil, "install "+xcodeName(q.Version)+" from the App Store, or with xcodes")
			case q.Version != "" && !t.HasXcode(q.Version):
				add(q, xcodeName(q.Version), "Xcode "+t.XcodeVersions(), nil,
					"install "+xcodeName(q.Version)+" beside the one there; a step selects it with DEVELOPER_DIR")
			}
		case IOSSimulator:
			if len(t.Xcodes) > 0 && !t.HasIOSRuntime() {
				add(q, "an iOS simulator runtime", "", [][]string{{"xcodebuild", "-downloadPlatform", "iOS"}})
			}
		case JDK:
			want := WantedJDK(r, q)
			if !t.HasJDK(want) {
				have := ""
				if len(t.JDKs) > 0 {
					have = fmt.Sprintf("JDK %v", t.JDKs)
				}
				v := strconv.Itoa(want)
				if want == 0 {
					v = strconv.Itoa(AndroidJDK)
				}
				g := Gap{Requirement: q, Missing: "JDK " + v, Have: have}
				switch p := jdkPackage(t.Host.Family, v); {
				case linux && p != "":
					g.Yours = [][]string{packageInstall(t.Host.Family, p)}
				case linux:
					g.Manual = []string{"install JDK " + v + " with your distribution's package manager"}
				default:
					g.Fix = [][]string{brew("openjdk@" + v)}
				}
				gaps = append(gaps, g)
			}
		case AndroidSDK:
			gaps = append(gaps, androidGaps(q, t, linux)...)
		case AndroidEmulator:
			if !t.AndroidEmulator {
				add(q, "the Android emulator", "", [][]string{{"sdkmanager", "emulator"}},
					"accept the SDK's licences first, by hand: sdkmanager --licenses")
			}
		case DockerDaemon:
			if !t.Tools["docker"] {
				g := Gap{Requirement: q, Missing: "Docker", Manual: []string{"install Docker and start its daemon"}}
				if argv := packageInstall(t.Host.Family, packages[q.Key][t.Host.Family]); argv != nil && linux {
					g.Yours = [][]string{argv}
				}
				gaps = append(gaps, g)
			}
		case Sudo:
			if !t.PasswordlessSudo {
				add(q, "sudo without a password", "", nil,
					"the job runs sudo, and the agent has no terminal to type a password at")
			}
		default:
			f, ok := toolFixes[q.Key]
			if !ok || t.Tools[f.tool] {
				continue
			}
			fix := f.mac
			if linux {
				fix = f.linux
			}
			argv := packageInstall(t.Host.Family, packages[q.Key][t.Host.Family])
			switch {
			case linux && argv != nil && packages[q.Key][t.Host.Family] != "":
				gaps = append(gaps, Gap{Requirement: q, Missing: f.tool, Yours: [][]string{argv}})
			case fix != nil:
				add(q, f.tool, "", [][]string{fix})
			case f.byHand != "":
				add(q, f.tool, "", nil, f.byHand)
			default:
				add(q, f.tool, "", nil)
			}
		}
	}
	return gaps
}

// androidGaps is what stands between a machine and a working Android SDK, one
// step at a time and in the order they can be done: what is needed next
// depends on the step before, and sdkmanager runs on a JDK.
func androidGaps(q Requirement, t system.Toolchains, linux bool) []Gap {
	add := func(g Gap) []Gap {
		g.Requirement = q
		return []Gap{g}
	}
	licences := "accept the SDK's licences first, by hand: sdkmanager --licenses"
	switch {
	case t.AndroidSDK == "" && !linux:
		return add(Gap{Missing: "an Android SDK", Fix: [][]string{brew("--cask", "android-commandlinetools")},
			Manual: []string{"or install the SDK with Android Studio, and set ANDROID_HOME for the agent"}})
	case t.AndroidSDK == "" || (linux && t.SDKManager == "" && !t.Tools["sdkmanager"]):
		// No package gives this on every distribution, and the ones that do
		// put it where only root can install into it.
		return add(Gap{Missing: "the Android command-line tools", Fix: [][]string{{"latem", "setup", "android-sdk"}}})
	}
	if t.SDKManager != "" {
		sdkmanager := func(args ...string) []string {
			argv := append([]string{t.SDKManager}, args...)
			if home := t.JDKAtLeast(AndroidJDK); home != "" {
				return append([]string{"env", "JAVA_HOME=" + home}, argv...)
			}
			return argv
		}
		if t.JDKAtLeast(AndroidJDK) == "" {
			return nil // the JDK's own gap comes first
		}
		if !t.AndroidLicensed {
			return add(Gap{Missing: "the Android SDK's licences accepted", Yours: [][]string{sdkmanager("--licenses")}})
		}
		if !t.AndroidPlatformTools {
			return add(Gap{Missing: "Android platform-tools", Fix: [][]string{sdkmanager("platform-tools")}})
		}
		if q.Version != "" && !hasInt(t.AndroidPlatforms, q.Version) {
			return add(Gap{Missing: "Android platform " + q.Version, Have: fmt.Sprintf("platforms %v", t.AndroidPlatforms),
				Fix: [][]string{sdkmanager("platforms;android-" + q.Version)}})
		}
		return nil
	}
	if q.Version != "" && !hasInt(t.AndroidPlatforms, q.Version) {
		return add(Gap{Missing: "Android platform " + q.Version, Have: fmt.Sprintf("platforms %v", t.AndroidPlatforms),
			Fix:    [][]string{{"sdkmanager", "platforms;android-" + q.Version}},
			Manual: []string{licences}})
	}
	return nil
}

func xcodeName(version string) string {
	if version == "" {
		return "Xcode"
	}
	return "Xcode " + version
}

func hasInt(list []int, s string) bool {
	n, err := strconv.Atoi(s)
	if err != nil {
		return false
	}
	for _, v := range list {
		if v == n {
			return true
		}
	}
	return false
}
