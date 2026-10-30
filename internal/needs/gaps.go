package needs

import (
	"fmt"
	"strconv"

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
	// Fix are commands that would close it; Manual, steps for a person.
	Fix    []string
	Manual []string
}

// Executors, as a runner's config names them.
const (
	ExecutorNative = "native"
	ExecutorDocker = "docker"
	ExecutorTart   = "tart"
)

// toolFixes is what installs a tool on a Mac, and on Linux, when there is one
// command for it.
var toolFixes = map[string]struct{ tool, mac, linux string }{
	XcodeGen:  {"xcodegen", "brew install xcodegen", ""},
	CocoaPods: {"pod", "brew install cocoapods", ""},
	Fastlane:  {"fastlane", "brew install fastlane", ""},
	Bundler:   {"bundle", "gem install bundler", "gem install bundler"},
	Ruby:      {"ruby", "brew install ruby", ""},
	Brew:      {"brew", "", ""},
	Python:    {"python3", "brew install python", ""},
	Node:      {"node", "brew install node", ""},
	Bun:       {"bun", "brew install oven-sh/bun/bun", "curl -fsSL https://bun.sh/install | bash"},
	Go:        {"go", "brew install go", ""},
	Rust:      {"cargo", "brew install rust", ""},
}

// Gaps compares what a job needs with what the machine has, for a job that
// would run on a runner of executor. Only a native runner's needs are the
// host's: a docker runner's are its image's, which this does not open, save
// for what no image can give — a Docker daemon, and services; a tart
// runner's live in its VM, and are not known here.
func Gaps(r Report, t system.Toolchains, executor string) []Gap {
	var gaps []Gap
	add := func(q Requirement, missing, have string, fix []string, manual ...string) {
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
				add(q, "an iOS simulator runtime", "", []string{"xcodebuild -downloadPlatform iOS"})
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
				add(q, "JDK "+v, have, []string{"brew install openjdk@" + v})
			}
		case AndroidSDK:
			switch {
			case t.AndroidSDK == "":
				add(q, "an Android SDK", "", []string{"brew install --cask android-commandlinetools"},
					"or install the SDK with Android Studio, and set ANDROID_HOME for the agent")
			case q.Version != "" && !hasInt(t.AndroidPlatforms, q.Version):
				add(q, "Android platform "+q.Version, fmt.Sprintf("platforms %v", t.AndroidPlatforms),
					[]string{fmt.Sprintf(`sdkmanager "platforms;android-%s"`, q.Version)})
			}
		case AndroidEmulator:
			if !t.AndroidEmulator {
				add(q, "the Android emulator", "", []string{"sdkmanager emulator"})
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
			var fixes []string
			if fix != "" {
				fixes = []string{fix}
			}
			add(q, f.tool, "", fixes)
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
