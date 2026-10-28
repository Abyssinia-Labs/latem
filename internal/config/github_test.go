package config

import (
	"strings"
	"testing"
)

func TestGitHubRepository(t *testing.T) {
	cases := []struct {
		repo Repo
		want string
		ok   bool
	}{
		{Repo{URL: "https://github.com/Abyssinia-Labs/latem.git"}, "Abyssinia-Labs/latem", true},
		{Repo{URL: "https://github.com/Abyssinia-Labs/latem"}, "Abyssinia-Labs/latem", true},
		{Repo{URL: "git@github.com:me/my.app.git"}, "me/my.app", true},
		{Repo{URL: "ssh://git@github.com/me/myapp.git"}, "me/myapp", true},
		{Repo{URL: "https://GitHub.com/me/myapp/"}, "me/myapp", true},
		// Not GitHub, or not a repository: no answer rather than a wrong one.
		{Repo{URL: "/Users/me/src/myapp"}, "", false},
		{Repo{URL: "https://gitlab.com/me/myapp.git"}, "", false},
		{Repo{URL: "https://github.com/me"}, "", false},
		{Repo{URL: "https://github.com/me/myapp/tree/main"}, "", false},
		{Repo{URL: "file:///github.com/me/myapp"}, "", false},
		// The explicit key wins, and is the answer for a local path.
		{Repo{URL: "/Users/me/src/laemat", GitHub: "Laemat-labs/laemat"}, "Laemat-labs/laemat", true},
		{Repo{URL: "https://github.com/a/b", GitHub: "c/d"}, "c/d", true},
	}
	for _, c := range cases {
		got, ok := c.repo.GitHubRepository()
		if got != c.want || ok != c.ok {
			t.Errorf("%+v: got (%q, %v), want (%q, %v)", c.repo, got, ok, c.want, c.ok)
		}
	}
}

func TestGitHubSlugIsValidated(t *testing.T) {
	for slug, valid := range map[string]bool{
		"Abyssinia-Labs/latem": true,
		"me/my.app_2":          true,
		"me":                   false,
		"me/":                  false,
		"/repo":                false,
		"me/repo/extra":        false,
		"-me/repo":             false,
		"me/re po":             false,
	} {
		if got := githubSlugPattern.MatchString(slug); got != valid {
			t.Errorf("%q: valid = %v, want %v", slug, got, valid)
		}
	}
}

func TestLabelAliases(t *testing.T) {
	opts := Options{Home: "/nonexistent-home", Getenv: func(string) string { return "" }, LookupIP: testLookupIP}

	cfg, err := Parse([]byte(minimalValid+`
[label_aliases]
"blacksmith-4vcpu-ubuntu-2404" = ["linux"]
"blacksmith-6vcpu-macos-26"    = ["macos", "xcode"]
`), opts)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got := cfg.LabelAliases["blacksmith-6vcpu-macos-26"]; len(got) != 2 || got[1] != "xcode" {
		t.Errorf("aliases = %v", cfg.LabelAliases)
	}

	for name, c := range map[string]struct{ toml, want string }{
		"chained":     {`a = ["b"]` + "\n" + `b = ["linux"]`, `"b" is itself an alias`},
		"empty":       {`a = []`, "maps onto no labels"},
		"uppercase":   {`Ubuntu = ["linux"]`, "an alias must match"},
		"bad label":   {`a = ["Linux"]`, `label "Linux" must match`},
		"self-hosted": {`self-hosted = ["linux"]`, "can never match"},
	} {
		_, err := Parse([]byte(minimalValid+"\n[label_aliases]\n"+c.toml+"\n"), opts)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: got %v, want an error containing %q", name, err, c.want)
		}
	}
}
