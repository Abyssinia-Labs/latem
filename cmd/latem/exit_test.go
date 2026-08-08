package main

import (
	"io"
	"testing"
)

// A malformed command line exits 2, and that has to include cobra's own
// errors. For a command with subcommands and no explicit Args, cobra applies
// legacyArgs and produces `unknown command "x"` during argument validation —
// before RunE, where the root's own usage error lives. So the exit-code table,
// the --help text and every command's own validation all said 2 while an
// unknown subcommand quietly exited 1.
func TestExitCode_UnknownCommandsAreUsageErrors(t *testing.T) {
	tests := map[string]struct {
		args []string
		want int
	}{
		"unknown top-level command": {[]string{"nosuchcommand"}, ExitUsage},
		"unknown subcommand":        {[]string{"cache", "nosuchsub"}, ExitUsage},
		"unknown flag":              {[]string{"doctor", "--nosuchflag"}, ExitUsage},
		"stray arg after a group":   {[]string{"service", "bogus"}, ExitUsage},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			root := newRootCommand()
			root.SetOut(io.Discard)
			root.SetErr(io.Discard)
			root.SetArgs(tc.args)

			if got := exitCode(root.Execute()); got != tc.want {
				t.Errorf("exit = %d, want %d", got, tc.want)
			}
		})
	}
}

// A bare `latem` prints help and succeeds, rather than being treated as a
// missing command.
func TestExitCode_BareInvocationSucceeds(t *testing.T) {
	root := newRootCommand()
	root.SetOut(io.Discard)
	root.SetErr(io.Discard)
	root.SetArgs(nil)

	if got := exitCode(root.Execute()); got != ExitOK {
		t.Errorf("exit = %d, want %d", got, ExitOK)
	}
}
