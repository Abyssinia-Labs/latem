package system

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
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
	// Host is the machine these are on, which says how to install the rest.
	Host        Host
	Xcodes      []XcodeInstall
	SimRuntimes []SimRuntime
	JDKs        []int // majors
	// JDKHomes is where each JDK is, which is rarely on a step's PATH: a
	// second JDK on Linux, SDKMAN's, Android Studio's own (A69).
	JDKHomes []JDKInstall
	// StepJDK is the major of the java a step's PATH finds, or 0 when it
	// finds none, or macOS's stub.
	StepJDK int
	// AndroidSDK is the SDK's root, or "".
	AndroidSDK       string
	AndroidPlatforms []int // android-36 → 36
	AndroidEmulator  bool
	// SDKManager is the SDK's own sdkmanager, which is on no PATH unless
	// someone put it there, or "".
	SDKManager string
	// AndroidPlatformTools reports adb, and AndroidLicensed that someone has
	// accepted the SDK's licence: without it, every install in it is refused.
	AndroidPlatformTools bool
	AndroidLicensed      bool
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

// JDKInstall is one JDK, and the JAVA_HOME that names it.
type JDKInstall struct {
	Major int
	Home  string
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

// JDKHome is where a JDK of major is installed, or "".
func (t Toolchains) JDKHome(major int) string {
	for _, j := range t.JDKHomes {
		if j.Major == major {
			return j.Home
		}
	}
	return ""
}

// JDKAtLeast is the home of a JDK of major or newer, nearest first: what
// sdkmanager wants to run on, or "".
func (t Toolchains) JDKAtLeast(major int) string {
	best := 0
	home := ""
	for _, j := range t.JDKHomes {
		if j.Major >= major && (best == 0 || j.Major < best) {
			best, home = j.Major, j.Home
		}
	}
	return home
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
	t := Toolchains{Host: s.Host(), Tools: map[string]bool{}}
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
	t.JDKHomes = s.jdks(ctx)
	for _, j := range t.JDKHomes {
		if !slices.Contains(t.JDKs, j.Major) {
			t.JDKs = append(t.JDKs, j.Major)
		}
	}
	t.StepJDK = stepJDK(stepPath)
	t.AndroidSDK = s.androidSDK()
	if t.AndroidSDK != "" {
		t.AndroidPlatforms = androidPlatforms(t.AndroidSDK)
		_, err := os.Stat(filepath.Join(t.AndroidSDK, "emulator", "emulator"))
		t.AndroidEmulator = err == nil
		t.SDKManager = sdkManager(t.AndroidSDK)
		_, err = os.Stat(filepath.Join(t.AndroidSDK, "platform-tools", "adb"))
		t.AndroidPlatformTools = err == nil
		_, err = os.Stat(filepath.Join(t.AndroidSDK, "licenses", "android-sdk-license"))
		t.AndroidLicensed = err == nil
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

// jdks lists the JDKs installed: java_home's list and Homebrew's keg-only
// openjdk@N on macOS, /usr/lib/jvm and the like on Linux, and on either the
// places a person's tools put one (A69). Lowest major first, and one entry
// per home however many links reach it: /usr/lib/jvm/default is a link.
func (s *System) jdks(ctx context.Context) []JDKInstall {
	var out []JDKInstall
	seen := map[string]bool{}
	add := func(major int, home string) {
		if major <= 0 {
			return
		}
		key := home
		if real, err := filepath.EvalSymlinks(home); err == nil {
			key = real
		}
		if !seen[key] {
			seen[key] = true
			out = append(out, JDKInstall{Major: major, Home: home})
		}
	}
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
				add(javaMajor(f[0]), f[len(f)-1])
			}
		}
		for _, prefix := range []string{"/opt/homebrew/opt", "/usr/local/opt"} {
			matches, _ := filepath.Glob(filepath.Join(s.sysPath(prefix), "openjdk@*"))
			for _, m := range matches {
				add(majorIn(filepath.Base(m)), filepath.Join(m, "libexec", "openjdk.jdk", "Contents", "Home"))
			}
		}
	}
	for _, dir := range s.jdkDirs() {
		if home, major := jdkAt(dir); home != "" {
			add(major, home)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Major < out[j].Major })
	return out
}

// jdkDirs are the folders a JDK might be, by what puts one there: the
// distribution's packages, SDKMAN, mise, asdf, IntelliJ, Gradle's toolchains,
// and the runtime Android Studio ships.
func (s *System) jdkDirs() []string {
	var dirs []string
	for _, pattern := range []string{
		filepath.Join(s.home, ".sdkman", "candidates", "java", "*"),
		filepath.Join(s.home, ".local", "share", "mise", "installs", "java", "*"),
		filepath.Join(s.home, ".asdf", "installs", "java", "*"),
		filepath.Join(s.home, ".jdks", "*"),
		filepath.Join(s.home, ".gradle", "jdks", "*"),
	} {
		matches, _ := filepath.Glob(pattern)
		dirs = append(dirs, matches...)
	}
	if s.IsMacOS() {
		return append(dirs, s.sysPath("/Applications/Android Studio.app/Contents/jbr"))
	}
	for _, pattern := range []string{"/usr/lib/jvm/*", "/usr/java/*", "/opt/*jdk*"} {
		matches, _ := filepath.Glob(s.sysPath(pattern))
		dirs = append(dirs, matches...)
	}
	return append(dirs, s.sysPath("/opt/android-studio/jbr"), filepath.Join(s.home, "android-studio", "jbr"))
}

// jdkAt reads the JDK at dir, or in the Contents/Home a Mac bundle keeps one
// in: its home and major, or "" when it holds none. javac is what makes it a
// JDK: Arch's jre17-openjdk fills the same folder without one, and Gradle
// cannot compile on it.
func jdkAt(dir string) (string, int) {
	for _, home := range []string{dir, filepath.Join(dir, "Contents", "Home")} {
		if lookIn("javac", filepath.Join(home, "bin")) == "" {
			continue
		}
		if n := releaseMajor(home); n > 0 {
			return home, n
		}
		return home, majorIn(filepath.Base(dir))
	}
	return "", 0
}

// releaseMajor reads the release file every JDK has at its root, which says
// what the folder's name only sometimes does: temurin-17-jdk-amd64 and
// zulu17-ca-amd64 are 17 too.
func releaseMajor(home string) int {
	// #nosec G304 -- a JDK's own file, at a home found on this machine.
	data, err := os.ReadFile(filepath.Join(home, "release"))
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(data), "\n") {
		if v, ok := strings.CutPrefix(line, "JAVA_VERSION="); ok {
			return javaMajor(strings.Trim(strings.TrimSpace(v), `"`))
		}
	}
	return 0
}

// stepJDK is the major of the java on stepPath, read from the release file
// beside it through whatever links lead there: Arch's /usr/bin/java is a link
// to /usr/lib/jvm/default/bin/java, which is another.
func stepJDK(stepPath string) int {
	java := lookIn("java", stepPath)
	if java == "" {
		return 0
	}
	real, err := filepath.EvalSymlinks(java)
	if err != nil {
		return 0
	}
	return releaseMajor(filepath.Dir(filepath.Dir(real)))
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
		candidates = append(candidates, filepath.Join(s.home, "Android", "Sdk"), s.sysPath("/opt/android-sdk"),
			s.sysPath("/usr/lib/android-sdk"))
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

// sdkManager finds the SDK's own sdkmanager: the version `latest` when it is
// there, as Android Studio and Google's instructions arrange it, or any other.
func sdkManager(sdk string) string {
	candidates := []string{filepath.Join(sdk, "cmdline-tools", "latest", "bin", "sdkmanager")}
	versions, _ := filepath.Glob(filepath.Join(sdk, "cmdline-tools", "*", "bin", "sdkmanager"))
	for _, c := range append(candidates, versions...) {
		if info, err := os.Stat(c); err == nil && !info.IsDir() && info.Mode()&0o111 != 0 {
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
