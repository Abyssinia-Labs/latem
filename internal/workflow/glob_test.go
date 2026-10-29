package workflow

import "testing"

// GitHub's filter syntax, from its documentation's own examples.
func TestFilterPatterns(t *testing.T) {
	cases := []struct {
		pattern string
		yes     []string
		no      []string
	}{
		{"main", []string{"main"}, []string{"main2", "feature/main"}},
		{"feature/*", []string{"feature/a", "feature/"}, []string{"feature/a/b", "feature"}},
		{"feature/**", []string{"feature/a", "feature/a/b"}, []string{"features/a"}},
		{"**", []string{"a", "a/b/c"}, nil},
		{"*.md", []string{"README.md"}, []string{"docs/a.md"}},
		{"**.md", []string{"README.md", "docs/a.md"}, []string{"a.mdx"}},
		{"docs/**", []string{"docs/a", "docs/a/b.md"}, []string{"docs"}},
		{"**/docs/**", []string{"docs/a", "x/docs/y/z"}, []string{"documents/a"}},
		{"packages/web/**", []string{"packages/web/src/x.ts"}, []string{"packages/webx/a", "packages/api/a"}},
		{"v[12].[0-9]+.[0-9]+", []string{"v1.0.0", "v2.10.3"}, []string{"v3.0.0", "v1.0"}},
		{"releases/**-alpha", []string{"releases/beta/3-alpha"}, []string{"releases/3-beta"}},
		{"Chang?log.md", []string{"Chanlog.md", "Changlog.md"}, []string{"Changelog.md"}},
		{"*.jsx?", []string{"a.js", "a.jsx"}, []string{"a.jsxx"}},
		{"convex-v*.*.*", []string{"convex-v1.2.3"}, []string{"v1.2.3", "convex-v1.2"}},
		{".github/workflows/checks.yml", []string{".github/workflows/checks.yml"}, []string{".github/workflows/checksXyml"}},
	}
	for _, c := range cases {
		p, err := compileFilter(c.pattern)
		if err != nil {
			t.Fatalf("%s: %v", c.pattern, err)
		}
		for _, s := range c.yes {
			if !p.re.MatchString(s) {
				t.Errorf("%q should match %q", c.pattern, s)
			}
		}
		for _, s := range c.no {
			if p.re.MatchString(s) {
				t.Errorf("%q should not match %q", c.pattern, s)
			}
		}
	}
}

// The last matching pattern decides.
func TestFilterListOrder(t *testing.T) {
	l, err := compileFilters([]string{"sub-project/**", "!sub-project/docs/**", "sub-project/docs/keep.md"})
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]bool{
		"sub-project/src/a.go":     true,
		"sub-project/docs/a.md":    false,
		"sub-project/docs/keep.md": true,
		"other/a.go":               false,
	} {
		if got := l.matches(name); got != want {
			t.Errorf("%s: %v, want %v", name, got, want)
		}
	}
	if _, err := compileFilter("a[b"); err == nil {
		t.Error("an unclosed class should be refused")
	}
}
