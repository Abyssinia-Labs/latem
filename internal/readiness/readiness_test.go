package readiness

import (
	"slices"
	"strings"
	"testing"

	"github.com/Abyssinia-Labs/latem/internal/needs"
	"github.com/Abyssinia-Labs/latem/internal/system"
)

var arch = system.Host{OS: "Linux", Arch: "amd64", Name: "Arch Linux", Family: system.FamilyArch}

func req(key string) needs.Requirement {
	return needs.Requirement{Key: key, Strength: needs.Invoked}
}

// androidJob is Tsedal's preview build as the classifier reads it: named for
// a Mac, because that is where it has run.
func androidJob() Job {
	return Job{Ref: "tsedal preview.yml android", Repo: "tsedal", Labels: []string{"macos", "xcode"},
		Report: needs.Report{Platform: "macOS", Kind: needs.KindAndroid, Requires: []needs.Requirement{
			req(needs.AndroidSDK), req(needs.Bundler), req(needs.Gradle), req(needs.JDK), req(needs.Node),
			{Key: needs.Bun, ProvidedBy: "oven-sh/setup-bun"}, {Key: needs.Ruby, ProvidedBy: "bundler"},
		}},
		Unisolated: true,
		Secrets:    []string{"ANDROID_KEYSTORE_B64", "PLAY_SERVICE_ACCOUNT_JSON"}}
}

func iosJob() Job {
	return Job{Ref: "tsedal preview.yml ios", Repo: "tsedal", Labels: []string{"macos", "xcode"},
		Report: needs.Report{Platform: "macOS", Kind: needs.KindIOS, Requires: []needs.Requirement{req(needs.Xcode), req(needs.IOSSDK)}}}
}

func linuxJob() Job {
	return Job{Ref: "tsedal checks.yml web", Repo: "tsedal", Labels: []string{"linux"}, Runner: "linux",
		Report: needs.Report{Platform: "Linux", Kind: needs.KindLinux}}
}

// desktop is the Linux desktop on the day the Java question was asked: JDK 27
// and nothing else of Android's, a docker runner and a native one for
// integration, Tsedal on it.
func desktop() Input {
	tc := system.Toolchains{Host: arch, JDKs: []int{27}, StepJDK: 27,
		JDKHomes: []system.JDKInstall{{Major: 27, Home: "/usr/lib/jvm/java-27-openjdk"}},
		Tools:    map[string]bool{"docker": true, "java": true, "python3": true}}
	return Input{Machine: "desktop", Host: arch, Toolchains: tc,
		Runners: []Runner{
			{Name: "linux", Executor: needs.ExecutorDocker, Labels: []string{"linux", "amd64", "docker"}},
			{Name: "host", Executor: needs.ExecutorNative, Labels: []string{"docker-host"}, Toolchains: tc},
		},
		Jobs: []Job{androidJob(), iosJob(), linuxJob()}}
}

func kind(t *testing.T, p Plan, name string) Kind {
	t.Helper()
	i := slices.IndexFunc(p.Kinds, func(k Kind) bool { return k.Name == name })
	if i < 0 {
		t.Fatalf("no kind %s in %+v", name, p.Kinds)
	}
	return p.Kinds[i]
}

func item(t *testing.T, k Kind, id string) Item {
	t.Helper()
	i := slices.IndexFunc(k.Items, func(it Item) bool { return it.ID == id })
	if i < 0 {
		t.Fatalf("no item %s in %+v", id, k.Items)
	}
	return k.Items[i]
}

func titles(k Kind) []string {
	var out []string
	for _, it := range k.Items {
		out = append(out, it.Title)
	}
	return out
}

// What the desktop should say: Android is possible and off, with everything
// that has to happen, in an order that works; iOS is out of the question.
func TestBuild_TheDesktop(t *testing.T) {
	p := Build(desktop())

	ios := kind(t, p, needs.KindIOS)
	if ios.State != Impossible || ios.Why != "needs a Mac" || ios.Label != "" {
		t.Errorf("ios = %+v", ios)
	}
	if linux := kind(t, p, needs.KindLinux); linux.State != Ready || !linux.On || linux.Runner != "linux" {
		t.Errorf("linux = %+v", linux)
	}

	a := kind(t, p, needs.KindAndroid)
	if a.State != Off || a.On || a.Runner != "host" || a.Label != LabelAndroid {
		t.Errorf("android = %+v", a)
	}
	want := []string{"JDK 17", "the Android command-line tools", "Bundler", "Node",
		"remote actions allowed on this machine", "tsedal's secrets set here"}
	if got := titles(a); !slices.Equal(got, want) {
		t.Errorf("android items = %q", got)
	}
	jdk := item(t, a, "JDK 17")
	if jdk.Have != "JDK 27" || needs.CommandLine(jdk.Yours[0]) != "sudo pacman -S --needed jdk17-openjdk" {
		t.Errorf("jdk = %+v", jdk)
	}
	if !strings.Contains(item(t, a, "unisolated").Warning, "as you") {
		t.Errorf("giving up isolation says nothing of what it gives up")
	}
	s := item(t, a, "secrets-tsedal")
	if s.Have != "2 not set" || needs.CommandLine(s.Yours[0]) != "latem secrets set --repo tsedal ANDROID_KEYSTORE_B64" {
		t.Errorf("secrets = %+v", s)
	}
	if len(a.Notes) != 1 || !strings.Contains(a.Notes[0], "runs-on: [android]") {
		t.Errorf("notes = %q", a.Notes)
	}
}

// JDK 17 is there and a step's java is 27: a build would get the wrong one,
// so the runner has to say which. Found, not reinstalled.
func TestBuild_AJDKAStepWouldNotFind(t *testing.T) {
	in := desktop()
	tc := &in.Runners[1].Toolchains
	tc.JDKs = []int{17, 27}
	tc.JDKHomes = append(tc.JDKHomes, system.JDKInstall{Major: 17, Home: "/usr/lib/jvm/java-17-openjdk"})
	a := kind(t, Build(in), needs.KindAndroid)
	if slices.Contains(titles(a), "JDK 17") {
		t.Errorf("offered a JDK it has: %q", titles(a))
	}
	it := item(t, a, "java-home")
	if it.Have != "a step finds JDK 27" || len(it.Config) != 1 || it.Config[0].Value != "/usr/lib/jvm/java-17-openjdk" || it.Soft {
		t.Errorf("java-home = %+v", it)
	}

	in.Runners[1].Env = map[string]string{"JAVA_HOME": "/usr/lib/jvm/java-17-openjdk"}
	if a := kind(t, Build(in), needs.KindAndroid); slices.Contains(titles(a), "JAVA_HOME set to JDK 17") {
		t.Errorf("JAVA_HOME is set and still asked for: %q", titles(a))
	}
}

// A kind already running here is not called broken for doing what it has
// always done: its scripts found the JDK themselves. The mini.
func TestBuild_WiringIsSoftWhereJobsAlreadyRun(t *testing.T) {
	mac := system.Host{OS: "macOS", Arch: "arm64"}
	tc := system.Toolchains{Host: mac, JDKs: []int{17}, Tools: map[string]bool{"bundle": true, "node": true},
		JDKHomes:   []system.JDKInstall{{Major: 17, Home: "/opt/homebrew/opt/openjdk@17/libexec/openjdk.jdk/Contents/Home"}},
		AndroidSDK: "/sdk", AndroidPlatforms: []int{34}, SDKManager: "/sdk/sdkmanager", AndroidLicensed: true, AndroidPlatformTools: true}
	j := androidJob()
	j.Runner, j.Unisolated, j.Secrets = "macos", false, nil
	in := Input{Machine: "mini", Host: mac, Toolchains: tc, Jobs: []Job{j},
		Runners: []Runner{{Name: "macos", Executor: needs.ExecutorNative, Labels: []string{"macos", "arm64", "xcode"}, Toolchains: tc}}}
	a := kind(t, Build(in), needs.KindAndroid)
	if a.State != Ready || !a.On || a.Blocking() != 0 {
		t.Errorf("android = %+v", a)
	}
	if len(a.Items) != 2 || !a.Items[0].Soft || !a.Items[1].Soft {
		t.Errorf("items = %+v", a.Items)
	}
}

// Fresh Linux: a docker runner alone, so a kind that builds on the host needs
// a runner first, and setup can add one.
func TestBuild_NoNativeRunnerYet(t *testing.T) {
	in := desktop()
	in.Runners = in.Runners[:1]
	a := kind(t, Build(in), needs.KindAndroid)
	if a.Runner != NativeRunner || a.Items[0].ID != "new-runner" || a.Items[0].Config[0].Value != NativeRunner {
		t.Errorf("android = %+v", a)
	}
	if !slices.Contains(titles(a), "JDK 17") {
		t.Errorf("the host's toolchain was not read: %q", titles(a))
	}
}

// With no repository, the common kinds still show, so a machine can be got
// ready before anything needs it.
func TestBuild_NoRepositories(t *testing.T) {
	in := desktop()
	in.Jobs = nil
	p := Build(in)
	if len(p.Kinds) != 3 {
		t.Fatalf("kinds = %+v", p.Kinds)
	}
	a := kind(t, p, needs.KindAndroid)
	if a.State != Off || !strings.Contains(a.Why, "no job") || !slices.Contains(titles(a), "JDK 17") {
		t.Errorf("android = %+v", a)
	}
}

// The label is what says a machine takes a kind, job or no job.
func TestBuild_ALabelWithNoJobAskingYet(t *testing.T) {
	in := desktop()
	in.Jobs = nil
	in.Runners[1].Labels = append(in.Runners[1].Labels, LabelAndroid)
	if a := kind(t, Build(in), needs.KindAndroid); !a.On || a.State != Missing {
		t.Errorf("android = %+v", a)
	}
}

// Once a workflow asks for the label, nothing is left to say about routing.
func TestBuild_AWorkflowThatAsksForTheLabel(t *testing.T) {
	in := desktop()
	in.Jobs[0].Labels = []string{LabelAndroid}
	if a := kind(t, Build(in), needs.KindAndroid); len(a.Notes) != 0 {
		t.Errorf("notes = %q", a.Notes)
	}
}

// A job whose kind nothing here defines is not dropped.
func TestBuild_WhatCouldNotBeClassified(t *testing.T) {
	in := desktop()
	in.Jobs = append(in.Jobs, Job{Ref: "x y.yml z", Report: needs.Report{Kind: needs.KindUnknown}})
	in.Unread = []string{"app: no mirror"}
	p := Build(in)
	if !slices.Equal(p.Unclassified, []string{"x y.yml z"}) || len(p.Unread) != 1 {
		t.Errorf("plan = %+v", p)
	}
}

func TestBuild_ImagesAndProof(t *testing.T) {
	in := desktop()
	in.Images = []Image{{Runner: "linux", Ref: "ghcr.io/x@sha256:1"}}
	k := kind(t, Build(in), needs.KindLinux)
	if len(k.Items) != 1 || !k.Items[0].Soft || k.State != Ready {
		t.Errorf("a missing image waits a first job, and stops nothing: %+v", k)
	}

	tc := &in.Runners[1].Toolchains
	tc.JDKHomes = []system.JDKInstall{{Major: 17, Home: "/jvm/17"}}
	tc.AndroidSDK = "/sdk"
	a := kind(t, Build(in), needs.KindAndroid)
	if len(a.Proof) != 2 || a.Proof[0].Argv[0] != "/jvm/17/bin/javac" || a.Proof[1].Argv[0] != "/sdk/platform-tools/adb" {
		t.Errorf("proof = %+v", a.Proof)
	}
}

func TestSettings_NamesWhatIsNotAllowed(t *testing.T) {
	items := settings([]Job{{Ref: "a", NotAllowed: []string{"oven-sh/setup-bun"}, Moving: []string{"oven-sh/setup-bun@v2"}}}, needs.ExecutorDocker)
	if len(items) != 2 || items[0].Config[0].What != ChangeAllowed || items[1].Config[0].What != ChangeMovingRefs ||
		items[1].Warning == "" {
		t.Errorf("items = %+v", items)
	}
}
