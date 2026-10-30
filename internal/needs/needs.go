// Package needs works out what a job needs from the machine that runs it — an
// Xcode, an iOS simulator, a JDK and an Android SDK, a Docker daemon — by
// reading the job as written: its steps, the actions it uses, and the scripts
// its steps run (A59).
//
// It reads; it never runs anything, and it needs no network. Every
// requirement carries the evidence it was found from and how strongly that
// evidence says so, and what the reading could not see — a remote action's
// contents, a script that is not in the repository, an eval — is reported as
// unknown rather than guessed at. What it says explains a job; it never
// decides where one runs or whether it may.
package needs

import (
	"slices"
	"sort"
	"strings"
)

// Requirement keys: a closed vocabulary, so a control plane can be told about
// them without being told about steps (INV-5).
const (
	Xcode           = "xcode"            // an Xcode, at Version if one is named
	IOSSDK          = "ios-sdk"          // builds for iOS: device or simulator
	IOSSimulator    = "ios-simulator"    // boots one
	MacOSApp        = "macos-app"        // builds a macOS app with Xcode
	Codesign        = "codesign"         // signs with a keychain identity
	Notarize        = "notarize"         // notarytool, stapler
	Swift           = "swift"            // swift build/test
	XcodeGen        = "xcodegen"         //
	CocoaPods       = "cocoapods"        //
	Fastlane        = "fastlane"         //
	Ruby            = "ruby"             // at Version if one is named
	Bundler         = "bundler"          //
	JDK             = "jdk"              // at Version if one is named
	Gradle          = "gradle"           //
	AndroidSDK      = "android-sdk"      // at Version (a platform) if one is named
	AndroidEmulator = "android-emulator" //
	DockerDaemon    = "docker-daemon"    // a step talks to a Docker daemon
	Services        = "services"         // services: containers beside the job
	Container       = "container"        // the job runs in container:
	Node            = "node"             //
	Bun             = "bun"              //
	Go              = "go"               //
	Python          = "python"           //
	Rust            = "rust"             //
	Playwright      = "playwright"       // a browser it installs
	Brew            = "brew"             //
	Sudo            = "sudo"             //
	Apt             = "apt"              // Debian's package manager
)

// Kinds, one per job, from its platform and what it needs.
const (
	KindIOS         = "ios"
	KindMacOSApp    = "macos-app"
	KindXcode       = "xcode" // needs Xcode; what for is not said
	KindAndroid     = "android"
	KindLinuxDocker = "linux-docker" // Linux, and a Docker daemon or services
	KindLinux       = "linux"
	KindMacOS       = "macos" // macOS, and nothing of Apple's toolchain
	KindUnknown     = "unknown"
)

// Strength is how directly the job says it needs a thing.
type Strength int

const (
	// Inferred: read from a name, a convention — find-xcode.sh 27.
	Inferred Strength = iota + 1
	// Invoked: a step runs the command.
	Invoked
	// Declared: a version is pinned — DEVELOPER_DIR, setup-*'s with:, a
	// .xcode-version file, compileSdk.
	Declared
)

// MarshalText writes a strength by name.
func (s Strength) MarshalText() ([]byte, error) { return []byte(s.String()), nil }

func (s Strength) String() string {
	switch s {
	case Declared:
		return "declared"
	case Invoked:
		return "invoked"
	case Inferred:
		return "inferred"
	}
	return "unknown"
}

// Evidence is where a requirement was read from.
type Evidence struct {
	File string `json:"file"`
	// Line is the line in File, for a script; for a workflow step it is 0
	// and Where names the step.
	Line    int    `json:"line,omitempty"`
	Where   string `json:"where,omitempty"`
	Snippet string `json:"snippet"`
}

// Requirement is one thing a job needs.
type Requirement struct {
	Key     string `json:"key"`
	Version string `json:"version,omitempty"`
	// Strength is how directly the job needs the thing at all;
	// VersionStrength how directly it names Version. An xcodebuild invokes
	// Xcode, and a find-xcode.sh 27 beside it only infers which.
	Strength        Strength `json:"strength"`
	VersionStrength Strength `json:"version_strength,omitempty"`
	// ProvidedBy names the action that installs it for the job, when one
	// does: setup-node, setup-java. Such a requirement asks nothing of the
	// machine but what the action needs.
	ProvidedBy string   `json:"provided_by,omitempty"`
	Evidence   Evidence `json:"evidence"`
}

// Report is what one job needs.
type Report struct {
	// Platform is runner.os as the job's labels settle it: "macOS", "Linux",
	// or "" when they do not.
	Platform string        `json:"platform,omitempty"`
	Kind     string        `json:"kind"`
	Requires []Requirement `json:"requires,omitempty"`
	// Unknown is what could not be read, and so might need more.
	Unknown []Evidence `json:"unknown,omitempty"`
}

// Has reports whether the report requires key.
func (r Report) Has(key string) bool {
	return slices.ContainsFunc(r.Requires, func(q Requirement) bool { return q.Key == key })
}

// Get returns the requirement for key.
func (r Report) Get(key string) (Requirement, bool) {
	for _, q := range r.Requires {
		if q.Key == key {
			return q, true
		}
	}
	return Requirement{}, false
}

// collector gathers requirements, one per key, keeping the strongest evidence
// and its version.
type collector struct {
	reqs    map[string]Requirement
	unknown []Evidence
}

func newCollector() *collector { return &collector{reqs: map[string]Requirement{}} }

func (c *collector) add(q Requirement) {
	if q.Version != "" && q.VersionStrength == 0 {
		q.VersionStrength = q.Strength
	}
	old, ok := c.reqs[q.Key]
	if !ok {
		c.reqs[q.Key] = q
		return
	}
	if q.Strength > old.Strength {
		old.Strength, old.Evidence = q.Strength, q.Evidence
	}
	if q.Version != "" && q.VersionStrength > old.VersionStrength {
		old.Version, old.VersionStrength = q.Version, q.VersionStrength
	}
	if old.ProvidedBy == "" {
		old.ProvidedBy = q.ProvidedBy
	}
	c.reqs[q.Key] = old
}

func (c *collector) report(platform string) Report {
	r := Report{Platform: platform, Unknown: c.unknown}
	for _, q := range c.reqs {
		r.Requires = append(r.Requires, q)
	}
	sort.Slice(r.Requires, func(i, j int) bool { return r.Requires[i].Key < r.Requires[j].Key })
	r.Kind = kindOf(r)
	return r
}

// kindOf names a job by the most specific thing it needs.
func kindOf(r Report) string {
	switch {
	case r.Has(IOSSDK) || r.Has(IOSSimulator):
		return KindIOS
	case r.Has(MacOSApp) || r.Has(Notarize):
		return KindMacOSApp
	case r.Has(Xcode) || r.Has(XcodeGen) || r.Has(CocoaPods) || (r.Has(Swift) && r.Platform == "macOS"):
		return KindXcode
	case r.Has(AndroidSDK) || r.Has(AndroidEmulator):
		return KindAndroid
	case r.Has(DockerDaemon) || r.Has(Services) || r.Has(Container):
		return KindLinuxDocker
	case r.Platform == "Linux":
		return KindLinux
	case r.Platform == "macOS":
		return KindMacOS
	}
	return KindUnknown
}

// platformOf is the platform labels name: macos or linux, as the compiler
// produces them, or a vendor's label that says which (blacksmith-…-ubuntu-
// 2404, macos-26). Looser than the compiler's own (A58), which decides what
// runs: this only explains.
func platformOf(labels []string) string {
	var mac, linux bool
	for _, l := range labels {
		l = strings.ToLower(l)
		switch {
		case strings.Contains(l, "macos"):
			mac = true
		case strings.Contains(l, "linux"), strings.Contains(l, "ubuntu"):
			linux = true
		}
	}
	switch {
	case mac && !linux:
		return "macOS"
	case linux && !mac:
		return "Linux"
	}
	return ""
}
