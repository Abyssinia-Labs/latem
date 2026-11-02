package readiness

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/Abyssinia-Labs/latem/internal/needs"
	"github.com/Abyssinia-Labs/latem/internal/system"
	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

func advertKindOf(t *testing.T, a *protocol.Readiness, name string) protocol.ReadinessKind {
	t.Helper()
	i := slices.IndexFunc(a.Kinds, func(k protocol.ReadinessKind) bool { return k.Kind == name })
	if i < 0 {
		t.Fatalf("no kind %s in %+v", name, a.Kinds)
	}
	return a.Kinds[i]
}

// A machine holding everything that must not leave it: a JDK's home, an SDK's
// root, a private image, the actions its jobs name, and its secrets' names.
func aMachineFullOfThings() Input {
	in := desktop()
	in.Toolchains.AndroidSDK = "/home/me/Android/Sdk"
	in.Toolchains.SDKManager = "/home/me/Android/Sdk/cmdline-tools/latest/bin/sdkmanager"
	in.Toolchains.AndroidLicensed = true
	in.Toolchains.JDKHomes = append(in.Toolchains.JDKHomes, system.JDKInstall{Major: 17, Home: "/usr/lib/jvm/java-17-openjdk"})
	in.Toolchains.JDKs = []int{17, 27}
	in.Runners[1].Toolchains = in.Toolchains
	in.Jobs[0].Moving = []string{"oven-sh/setup-bun@v2"}
	in.Jobs[0].NotAllowed = []string{"oven-sh/setup-bun@v2", "ruby/setup-ruby@v1"}
	in.Images = []Image{{Runner: "linux", Ref: "registry.example.com/private/base:1"}}
	in.Jobs[0].Report.Requires = append(in.Jobs[0].Report.Requires, needs.Requirement{Key: needs.Xcode, Version: "27"})
	return in
}

func TestAdvert_NothingThatIsTheMachinesOwn(t *testing.T) {
	data, err := json.Marshal(Advert(Build(aMachineFullOfThings())))
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	if strings.Contains(text, "/") {
		t.Errorf("a slash left the machine: %s", text)
	}
	for _, leak := range []string{
		"setup-bun", "setup-ruby", "ANDROID_KEYSTORE", "PLAY_SERVICE", "registry.example.com",
		"sudo", "sdkmanager", "pacman", "preview.yml", "latem setup",
	} {
		if strings.Contains(text, leak) {
			t.Errorf("%q left the machine: %s", leak, text)
		}
	}
}

func TestAdvert_TheDesktop(t *testing.T) {
	a := Advert(Build(desktop()))

	android := advertKindOf(t, a, needs.KindAndroid)
	if android.State != string(Off) || android.Title != "Android" || android.Runner != "host" || android.Jobs != 1 ||
		!slices.Equal(android.Repos, []string{"tsedal"}) {
		t.Errorf("android = %+v", android)
	}
	if android.Elsewhere != 1 {
		t.Errorf("Elsewhere = %d, want the one job asking for a Mac's labels", android.Elsewhere)
	}
	var said []string
	for _, it := range android.Items {
		said = append(said, it.Title)
	}
	if !slices.Contains(said, "JDK 17") || !slices.Contains(said, "tsedal's secrets set here") {
		t.Errorf("items = %q", said)
	}
	if i := slices.IndexFunc(android.Items, func(it protocol.ReadinessItem) bool { return it.Title == "JDK 17" }); android.Items[i].Have != "JDK 27" {
		t.Errorf("JDK 17's have = %q", android.Items[i].Have)
	}
	if ios := advertKindOf(t, a, needs.KindIOS); ios.State != string(Impossible) || ios.Why != "needs a Mac" {
		t.Errorf("ios = %+v", ios)
	}
}

// A secret's name is the repository's, but how many are missing is what a
// person reading the page needs, and the only part of it they get.
func TestAdvert_SecretsAreCounted(t *testing.T) {
	android := advertKindOf(t, Advert(Build(desktop())), needs.KindAndroid)
	i := slices.IndexFunc(android.Items, func(it protocol.ReadinessItem) bool { return it.Title == "tsedal's secrets set here" })
	if i < 0 || android.Items[i].Have != "2 not set" {
		t.Errorf("items = %+v", android.Items)
	}
}

func TestAdvert_ActionsAreCounted(t *testing.T) {
	in := desktop()
	in.Jobs[0].NotAllowed = []string{"oven-sh/setup-bun@v2", "ruby/setup-ruby@v1"}
	android := advertKindOf(t, Advert(Build(in)), needs.KindAndroid)
	if !slices.ContainsFunc(android.Items, func(it protocol.ReadinessItem) bool { return it.Title == "2 actions allowed on this machine" }) {
		t.Errorf("items = %+v", android.Items)
	}
	in.Jobs[0].NotAllowed = in.Jobs[0].NotAllowed[:1]
	android = advertKindOf(t, Advert(Build(in)), needs.KindAndroid)
	if !slices.ContainsFunc(android.Items, func(it protocol.ReadinessItem) bool { return it.Title == "1 action allowed on this machine" }) {
		t.Errorf("items = %+v", android.Items)
	}
}

// A version is whatever a workflow wrote, and a job can name an Xcode by its
// path. The item is counted and its words stay here.
func TestAdvert_AnItemWithAPathInItIsCounted(t *testing.T) {
	p := Plan{Kinds: []Kind{{Name: "xcode", Title: "Xcode builds", State: Missing, Items: []Item{
		{ID: "x", Title: "Xcode /Applications/Xcode_27.app"},
		{ID: "y", Title: "JDK 17", Have: `C:\jdk`},
		{ID: "z", Title: "two\nlines"},
	}}}}
	k := Advert(p).Kinds[0]
	if k.More != 2 || len(k.Items) != 1 || k.Items[0] != (protocol.ReadinessItem{Title: "JDK 17"}) {
		t.Errorf("kind = %+v", k)
	}
}

func TestAdvert_StaysInsideTheControlPlanesBounds(t *testing.T) {
	k := Kind{Name: "android", Title: "Android", State: Missing}
	for i := range advertItems + 3 {
		k.Items = append(k.Items, Item{ID: fmt.Sprint(i), Title: fmt.Sprintf("thing %d", i)})
	}
	k.Items = append(k.Items, Item{ID: "long", Title: strings.Repeat("x", advertText+1)})
	for i := range advertRepos + 1 {
		k.Repos = append(k.Repos, fmt.Sprintf("repo-%d", i))
	}
	p := Plan{Unread: []string{"a: /private/path", "b: gone"}, Unclassified: []string{"tsedal x.yml y"}}
	for range advertKinds + 1 {
		p.Kinds = append(p.Kinds, k)
	}
	a := Advert(p)
	got := a.Kinds[0]
	if len(a.Kinds) != advertKinds || len(got.Items) != advertItems || got.More != 4 || len(got.Repos) != advertRepos {
		t.Errorf("kinds %d, items %d, more %d, repos %d", len(a.Kinds), len(got.Items), got.More, len(got.Repos))
	}
	if a.Unread != 2 || a.Unclassified != 1 {
		t.Errorf("unread %d, unclassified %d", a.Unread, a.Unclassified)
	}
}

// A kind is flagged over one job, so an item says how many it holds up. The
// mini's Linux is 48 jobs and four deploys short of a secret.
func TestAdvert_SaysHowManyJobsAnItemHoldsUp(t *testing.T) {
	k := Kind{Name: "linux", Title: "Linux", State: Missing, Jobs: []string{"a", "b", "c", "d"}, Items: []Item{
		{ID: "secrets-tsedal", Title: "tsedal's secrets set here", Have: "3 not set", Jobs: []string{"a"}},
		{ID: "moving-refs", Title: "actions named by a tag accepted", Have: "oven-sh/setup-bun@v2", Jobs: []string{"a", "b"}},
		{ID: "node", Title: "Node", Jobs: []string{"a", "b", "c", "d"}},
		{ID: "java-home", Title: "JAVA_HOME set"},
	}}
	got := Advert(Plan{Kinds: []Kind{k}}).Kinds[0].Items
	want := []protocol.ReadinessItem{
		{Title: "tsedal's secrets set here", Have: "3 not set, 1 of 4 jobs"},
		{Title: "actions named by a tag accepted", Have: "2 of 4 jobs"},
		{Title: "Node"},
		{Title: "JAVA_HOME set"},
	}
	if !slices.Equal(got, want) {
		t.Errorf("items = %+v", got)
	}
}
