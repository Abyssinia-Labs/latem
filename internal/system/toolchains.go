package system

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Toolchains is what a machine has installed for builds, as a step would
// find it (A59): every Xcode and its version, the iOS simulator runtimes,
// the JDKs, the Android SDK and its platforms, the tools on the PATH a step
// gets, and whether sudo asks for a password.
//
// It reports; it judges nothing. Which of it a job needs is
// internal/needs's to say.
type Toolchains struct {
	Xcodes      []XcodeInstall
	SimRuntimes []SimRuntime
	JDKs        []int // majors
	// AndroidSDK is the SDK's root, or "".
	AndroidSDK       string
	AndroidPlatforms []int // android-36 → 36
	AndroidEmulator  bool
	// Tools is each probed command, and whether a step's PATH finds it.
	Tools map[string]bool
	// PasswordlessSudo reports whether `sudo -n true` succeeds.
	PasswordlessSudo bool
}

// XcodeInstall is one Xcode*.app.
type XcodeInstall struct {
	Path    string
	Version Version
}

// HasXcode reports whether an Xcode of major is installed; major "" asks for
// any.
func (t Toolchains) HasXcode(major string) bool {
	for _, x := range t.Xcodes {
		if major == "" || strconv.Itoa(x.Version.Major) == major {
			return true
		}
	}
	return false
}

// XcodeVersions names the installed Xcodes: "26.0.1, 27.0".
func (t Toolchains) XcodeVersions() string {
	var vs []string
	for _, x := range t.Xcodes {
		vs = append(vs, x.Version.Text)
	}
	return strings.Join(vs, ", ")
}

// HasIOSRuntime reports whether a simulator runtime for iOS is installed.
func (t Toolchains) HasIOSRuntime() bool {
	for _, r := range t.SimRuntimes {
		if r.Platform == "iOS" && r.Available {
			return true
		}
	}
	return false
}

// HasJDK reports whether a JDK of major is installed; 0 asks for any.
func (t Toolchains) HasJDK(major int) bool {
	for _, j := range t.JDKs {
		if major == 0 || j == major {
			return true
		}
	}
	return false
}

// toolchainTools are the commands whose presence a job's needs ask about.
var toolchainTools = []string{
	"xcodebuild", "xcodegen", "pod", "fastlane", "bundle", "ruby", "brew", "python3", "node", "bun",
	"go", "cargo", "docker", "swift", "java", "apt-get", "sdkmanager",
}

// Toolchains probes this machine. stepPath is the PATH a step gets — the
// service's, not this shell's, since a command typed here inherits a login
// profile the agent never reads.
func (s *System) Toolchains(ctx context.Context, stepPath string) Toolchains {
	t := Toolchains{Tools: map[string]bool{}}
	// Where the tools that install themselves for one user put their
	// binaries. A job's own script commonly puts these on its PATH, as
	// GitHub's images do: a tool there is installed, if not on the
	// service's PATH.
	userBins := strings.Join([]string{
		filepath.Join(s.home, ".bun", "bin"), filepath.Join(s.home, ".cargo", "bin"),
		filepath.Join(s.home, ".local", "bin"), filepath.Join(s.home, "go", "bin"),
	}, string(filepath.ListSeparator))
	for _, name := range toolchainTools {
		t.Tools[name] = lookIn(name, stepPath) != "" || lookIn(name, userBins) != ""
	}
	if s.IsMacOS() {
		t.Xcodes = s.xcodeInstalls(ctx)
		if rts, err := s.SimulatorRuntimes(ctx); err == nil {
			t.SimRuntimes = rts
		}
	}
	t.JDKs = s.jdks(ctx)
	t.AndroidSDK = s.androidSDK()
	if t.AndroidSDK != "" {
		t.AndroidPlatforms = androidPlatforms(t.AndroidSDK)
		_, err := os.Stat(filepath.Join(t.AndroidSDK, "emulator", "emulator"))
		t.AndroidEmulator = err == nil
	}
	_, err := s.Exec(ctx, "sudo", "-n", "true")
	t.PasswordlessSudo = err == nil
	return t
}

// applicationDirs are where an Xcode is installed.
func (s *System) applicationDirs() []string {
	return []string{s.sysPath("/Applications"), filepath.Join(s.home, "Applications")}
}

// xcodeInstalls lists every Xcode*.app, versions read from their Info.plist
// rather than by running each one's xcodebuild, which can take seconds and
// asks a beta to be launched once first.
func (s *System) xcodeInstalls(ctx context.Context) []XcodeInstall {
	var out []XcodeInstall
	for _, dir := range s.applicationDirs() {
		apps, _ := filepath.Glob(filepath.Join(dir, "Xcode*.app"))
		for _, app := range apps {
			b, err := s.Exec(ctx, "plutil", "-extract", "CFBundleShortVersionString", "raw", "-o", "-",
				filepath.Join(app, "Contents", "Info.plist"))
			if err != nil {
				continue
			}
			if v, ok := ParseVersion(strings.TrimSpace(string(b))); ok {
				out = append(out, XcodeInstall{Path: app, Version: v})
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Version.Text < out[j].Version.Text })
	return out
}

var jdkMajor = regexp.MustCompile(`(?:openjdk@|jdk-?|java-|^)([0-9]+)(?:[.\-_]|$)`)

// jdks lists the installed JDKs' majors: java_home's list on macOS,
// Homebrew's keg-only openjdk@N, and /usr/lib/jvm on Linux.
func (s *System) jdks(ctx context.Context) []int {
	seen := map[int]bool{}
	if s.IsMacOS() {
		// java_home -V lists to stderr; the output is read either way.
		b, err := s.Exec(ctx, "/usr/libexec/java_home", "-V")
		if err != nil {
			b = Output(err)
		}
		for _, line := range strings.Split(string(b), "\n") {
			// The browser plug-in's JRE is listed too, and builds nothing.
			if strings.Contains(line, "JavaAppletPlugin.plugin") {
				continue
			}
			if f := strings.Fields(line); len(f) > 0 {
				if n := javaMajor(f[0]); n > 0 {
					seen[n] = true
				}
			}
		}
		for _, prefix := range []string{"/opt/homebrew/opt", "/usr/local/opt"} {
			matches, _ := filepath.Glob(filepath.Join(s.sysPath(prefix), "openjdk@*"))
			for _, m := range matches {
				if n := majorIn(filepath.Base(m)); n > 0 {
					seen[n] = true
				}
			}
		}
	} else {
		dirs, _ := os.ReadDir(s.sysPath("/usr/lib/jvm"))
		for _, d := range dirs {
			if n := majorIn(d.Name()); n > 0 {
				seen[n] = true
			}
		}
	}
	var out []int
	for n := range seen {
		out = append(out, n)
	}
	sort.Ints(out)
	return out
}

// javaMajor is a Java version's major: 11 for 11.0.20, and 8 for 1.8.0_292,
// which is how Java named itself before 9.
func javaMajor(version string) int {
	v, ok := ParseVersion(version)
	switch {
	case !ok:
		return 0
	case v.Major == 1:
		return v.Minor
	}
	return v.Major
}

func majorIn(name string) int {
	if m := jdkMajor.FindStringSubmatch(name); m != nil {
		n, _ := strconv.Atoi(m[1])
		return n
	}
	return 0
}

// androidSDK finds the SDK where a build would: $ANDROID_HOME,
// $ANDROID_SDK_ROOT, or where Android Studio and the command-line tools put
// it.
func (s *System) androidSDK() string {
	candidates := []string{s.getenv("ANDROID_HOME"), s.getenv("ANDROID_SDK_ROOT")}
	if s.IsMacOS() {
		candidates = append(candidates, filepath.Join(s.home, "Library", "Android", "sdk"),
			s.sysPath("/opt/homebrew/share/android-commandlinetools"))
	} else {
		candidates = append(candidates, filepath.Join(s.home, "Android", "Sdk"), s.sysPath("/opt/android-sdk"))
	}
	for _, c := range candidates {
		if c == "" {
			continue
		}
		if info, err := os.Stat(c); err == nil && info.IsDir() {
			return c
		}
	}
	return ""
}

func androidPlatforms(sdk string) []int {
	entries, _ := os.ReadDir(filepath.Join(sdk, "platforms"))
	var out []int
	for _, e := range entries {
		if n, err := strconv.Atoi(strings.TrimPrefix(e.Name(), "android-")); err == nil {
			out = append(out, n)
		}
	}
	sort.Ints(out)
	return out
}

// sysPath is an absolute system path under the configured root. It is absolute
// whatever it is given: with no root, which is every real machine, Join alone
// returns a relative path unchanged, and one that resolves from `/` and from
// nowhere else passes everywhere a test sets a root.
func (s *System) sysPath(p string) string { return filepath.Join("/", s.root, p) }

// lookIn finds name in a PATH list, as exec.LookPath does in $PATH.
func lookIn(name, pathList string) string {
	for _, dir := range filepath.SplitList(pathList) {
		if dir == "" {
			continue
		}
		p := filepath.Join(dir, name)
		if info, err := os.Stat(p); err == nil && !info.IsDir() && info.Mode()&0o111 != 0 {
			return p
		}
	}
	return ""
}

// XcodeFingerprint is a cheap summary of what an App Store update to Xcode, a
// switch of xcode-select or an accepted licence changes: each Xcode*.app's
// Info.plist, where /var/db/xcode_select_link points, and the machine's record
// of accepted licences. Only stats and a readlink, so the agent can ask it
// every half a minute and look again properly when it changes (A64). Empty
// off macOS.
func (s *System) XcodeFingerprint() string {
	if !s.IsMacOS() {
		return ""
	}
	var b strings.Builder
	stamp := func(path string) {
		if fi, err := os.Stat(path); err == nil {
			fmt.Fprintf(&b, "%s %d %d\n", path, fi.Size(), fi.ModTime().UnixNano())
		} else {
			fmt.Fprintf(&b, "%s absent\n", path)
		}
	}
	for _, dir := range s.applicationDirs() {
		apps, _ := filepath.Glob(filepath.Join(dir, "Xcode*.app"))
		for _, app := range apps {
			stamp(filepath.Join(app, "Contents", "Info.plist"))
		}
	}
	link, _ := os.Readlink(s.sysPath("/var/db/xcode_select_link"))
	fmt.Fprintf(&b, "selected %s\n", link)
	stamp(s.sysPath("/Library/Preferences/com.apple.dt.Xcode.plist"))
	return b.String()
}
