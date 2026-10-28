package executor

import (
	"reflect"
	"testing"
)

// GitHub's three shells, as its documentation gives them (A50). The run text
// is the last element and is never joined to anything (INV-3).
func TestGitHubArgv(t *testing.T) {
	run := `echo "a b"; false # '"`
	cases := []struct {
		shell Shell
		want  []string
	}{
		{ShellLatem, nil},
		{ShellGitHub, []string{"bash", "-e", "-c", run}},
		{ShellGitHubBash, []string{"bash", "--noprofile", "--norc", "-e", "-o", "pipefail", "-c", run}},
		{ShellGitHubSh, []string{"sh", "-e", "-c", run}},
	}
	for _, c := range cases {
		if got := GitHubArgv(c.shell, run); !reflect.DeepEqual(got, c.want) {
			t.Errorf("GitHubArgv(%q) = %#v, want %#v", c.shell, got, c.want)
		}
	}
}

func TestShellValid(t *testing.T) {
	for _, s := range []Shell{ShellLatem, ShellGitHub, ShellGitHubBash, ShellGitHubSh} {
		if !s.Valid() {
			t.Errorf("%q should be valid", s)
		}
	}
	for _, s := range []Shell{"zsh", "pwsh", "Bash"} {
		if s.Valid() {
			t.Errorf("%q should not be valid", s)
		}
	}
}
