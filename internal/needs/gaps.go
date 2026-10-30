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
	// one as it is, once asked, and never through a shell (INV-3). Manual
	// are steps for a person.
	Fix    [][]string
	Manual []string
}

// Fixes renders Fix as command lines a person could type.
func (g Gap) Fixes() []string {
	out := make([]string, len(g.Fix))
	for i, argv := range g.Fix {
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
			want, _ := strconv.Atoi(q.Version)
			if !t.HasJDK(want) {
				have := ""
				if len(t.JDKs) > 0 {
					have = fmt.Sprintf("JDK %v", t.JDKs)
				}
				v := q.Version
				if v == "" {
					v = "17"
				}
				add(q, "JDK "+v, have, [][]string{brew("openjdk@" + v)})
			}
		case AndroidSDK:
			switch {
			case t.AndroidSDK == "":
				add(q, "an Android SDK", "", [][]string{brew("--cask", "android-commandlinetools")},
					"or install the SDK with Android Studio, and set ANDROID_HOME for the agent")
			case q.Version != "" && !hasInt(t.AndroidPlatforms, q.Version):
				add(q, "Android platform "+q.Version, fmt.Sprintf("platforms %v", t.AndroidPlatforms),
					[][]string{{"sdkmanager", "platforms;android-" + q.Version}},
					"accept the SDK's licences first, by hand: sdkmanager --licenses")
			}
		case AndroidEmulator:
			if !t.AndroidEmulator {
				add(q, "the Android emulator", "", [][]string{{"sdkmanager", "emulator"}},
					"accept the SDK's licences first, by hand: sdkmanager --licenses")
			}
		case DockerDaemon:
			if !t.Tools["docker"] {
				add(q, "Docker", "", nil, "install Docker and start its daemon")
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
			if r.Platform == "Linux" {
				fix = f.linux
			}
			switch {
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
