package main

import (
	"slices"
	"strings"
	"testing"

	"github.com/Abyssinia-Labs/latem/internal/config"
)

func TestParseHeader(t *testing.T) {
	cases := []struct {
		line  string
		path  []string
		array bool
		ok    bool
	}{
		{"[repos.app]\n", []string{"repos", "app"}, false, true},
		{"  [runner.docker]\n", []string{"runner", "docker"}, false, true},
		{"[[runner]]\n", []string{"runner"}, true, true},
		{`[repos."next.js"]`, []string{"repos", "next.js"}, false, true},
		{`[ repos . 'a b' ]  # note`, []string{"repos", "a b"}, false, true},
		{"[repos.app.environments.production.vars]", []string{"repos", "app", "environments", "production", "vars"}, false, true},
		// Lines of a multi-line array, which start with [ and are not headers.
		{`  ["main", "release/*"],`, nil, false, false},
		{"  [1, 2]", nil, false, false},
		{"[[runner]", nil, false, false},
		{"allowed_refs = [\"main\"]", nil, false, false},
	}
	for _, tc := range cases {
		path, array, ok := parseHeader(tc.line)
		if ok != tc.ok || array != tc.array || !slices.Equal(path, tc.path) {
			t.Errorf("parseHeader(%q) = %v, %v, %v; want %v, %v, %v", tc.line, path, array, ok, tc.path, tc.array, tc.ok)
		}
	}
}

const editFixture = `schema_version = 1

# Repos this machine builds.
[repos.app]
url          = "https://github.com/me/app.git"
allowed_refs = ["main"]

  [repos.app.vars]
  REGION = "eu"

[repos.app.environments.production.vars]
URL = "https://example.com"

# The second repo, kept for the deploy job.
[repos.site]
url          = "https://github.com/me/site.git"
allowed_refs = [
  "main",
]

# --- runner ---
[[runner]]
name     = "linux"
executor = "docker"
labels   = ["linux"]

  [runner.docker]
  default_image = "ghcr.io/me/ci:latest"
`

func TestRemoveTables_TakesTheRepoAndItsSubtablesAndNothingElse(t *testing.T) {
	got, n := removeTables(editFixture, []string{"repos", "app"})
	if n != 3 {
		t.Errorf("removed %d tables, want 3", n)
	}
	for _, gone := range []string{"[repos.app]", "REGION", "environments.production", "me/app.git"} {
		if strings.Contains(got, gone) {
			t.Errorf("%q survived:\n%s", gone, got)
		}
	}
	for _, kept := range []string{"# Repos this machine builds.", "# The second repo", "[repos.site]",
		"# --- runner ---", "[runner.docker]", `  "main",`} {
		if !strings.Contains(got, kept) {
			t.Errorf("%q was lost:\n%s", kept, got)
		}
	}
	if strings.Contains(got, "\n\n\n") {
		t.Errorf("a removal left a hole:\n%s", got)
	}
}

func TestInsertAfter_GroupsANewRepoWithTheOthers(t *testing.T) {
	got := insertAfter(editFixture, []string{"repos"}, "[repos.added]\nurl = \"x\"\n")
	added := strings.Index(got, "[repos.added]")
	site := strings.Index(got, "[repos.site]")
	runner := strings.Index(got, "# --- runner ---")
	if added < site || added > runner {
		t.Errorf("the new repo is not between the last repo and the runner:\n%s", got)
	}
	if !strings.Contains(got, "]\n\n[repos.added]\nurl = \"x\"\n\n# --- runner ---") {
		t.Errorf("spacing around the insert:\n%s", got)
	}

	// No repos yet: at the end.
	got = insertAfter("schema_version = 1\n\n[[runner]]\nname = \"x\"\n", []string{"repos"}, "[repos.a]\n")
	if !strings.HasSuffix(got, "name = \"x\"\n\n[repos.a]\n") {
		t.Errorf("into a file with no repos:\n%q", got)
	}
}

func TestSetTableKey(t *testing.T) {
	t.Run("replaces a key where it is", func(t *testing.T) {
		got, err := setTableKey(editFixture, []string{"repos", "app", "vars"}, []string{"repos", "app"}, "REGION", `"us"`)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(got, "  [repos.app.vars]\n  REGION = \"us\"\n") {
			t.Errorf("not replaced in place, indentation kept:\n%s", got)
		}
	})
	t.Run("adds a key to an indented subtable, not the table above it", func(t *testing.T) {
		got, err := setTableKey(editFixture, []string{"repos", "app", "vars"}, []string{"repos", "app"}, "ZONE", `"b"`)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(got, "  REGION = \"eu\"\n  ZONE = \"b\"\n") {
			t.Errorf("ZONE is not in [repos.app.vars]:\n%s", got)
		}
	})
	t.Run("creates the table after the repo's own", func(t *testing.T) {
		got, err := setTableKey(editFixture, []string{"repos", "site", "vars"}, []string{"repos", "site"}, "A", `"1"`)
		if err != nil {
			t.Fatal(err)
		}
		at := strings.Index(got, "[repos.site.vars]\nA = \"1\"\n")
		if at < strings.Index(got, "[repos.site]") || at > strings.Index(got, "[[runner]]") {
			t.Errorf("the new table is not with its repo:\n%s", got)
		}
	})
	t.Run("refuses a value spanning lines", func(t *testing.T) {
		if _, err := setTableKey(editFixture, []string{"repos", "site"}, []string{"repos"}, "allowed_refs", `[]`); err == nil {
			t.Error("replaced the first line of a multi-line array")
		}
	})
}

func TestRemoveTableKey(t *testing.T) {
	got, found, err := removeTableKey(editFixture, []string{"repos", "app", "environments", "production", "vars"}, "URL")
	if err != nil || !found {
		t.Fatalf("found=%v err=%v", found, err)
	}
	if strings.Contains(got, "environments.production") {
		t.Errorf("an emptied table was left behind:\n%s", got)
	}
	got, found, err = removeTableKey(got, []string{"repos", "app", "vars"}, "NOPE")
	if err != nil || found {
		t.Errorf("an absent key: found=%v err=%v", found, err)
	}
	if !strings.Contains(got, "REGION") {
		t.Error("removing an absent key changed the file")
	}
}

// A value is written so the loader reads back exactly it, including the
// characters Go's %q would spell in a way TOML does not accept.
func TestTomlString_RoundTripsThroughTheLoader(t *testing.T) {
	for _, v := range []string{`plain`, `with "quotes" and \ backslash`, "tab\tnewline\nbell\a del\x7f", "ünïcode ✓"} {
		body := "schema_version = 1\n\n[repos.app]\nurl = \"x\"\nallowed_refs = [\"main\"]\n\n[repos.app.vars]\nV = " +
			tomlString(v) + "\n\n[[runner]]\nname = \"l\"\nexecutor = \"docker\"\nlabels = [\"linux\"]\n"
		cfg, err := config.Parse([]byte(body), config.Options{Dir: shortDir(t)})
		if err != nil {
			t.Errorf("%q: %v", v, err)
			continue
		}
		if got := cfg.Repos["app"].Vars["V"]; got != v {
			t.Errorf("round trip: got %q, want %q", got, v)
		}
	}
}
