package main

import (
	"errors"
	"strings"
	"testing"

	"github.com/Abyssinia-Labs/latem/internal/agent"
	"github.com/Abyssinia-Labs/latem/internal/config"
)

func TestServiceReload_PrintsWhatTheAgentDid(t *testing.T) {
	dir := configDirWith(t, baseConfig)
	calls := fakeReload(t, func(p config.Paths) (agent.ReloadStatus, error) {
		if p.Dir != dir {
			t.Errorf("signalled the agent for %s, want %s", p.Dir, dir)
		}
		return agent.ReloadStatus{PID: 4242, Reloads: 1, Deferred: []string{`runner "mac" was added`}}, nil
	})

	out, err := latemIn(t, dir, "", "service", "reload")
	if err != nil {
		t.Fatalf("service reload: %v\n%s", err, out)
	}
	if *calls != 1 {
		t.Errorf("signalled %d times", *calls)
	}
	for _, want := range []string{"pid 4242", "reloaded", `runner "mac" was added`, "latem service restart"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
}

// A file that does not load is not sent: the agent would refuse it, and the
// problems read better here.
func TestServiceReload_DoesNotSignalForAConfigThatDoesNotLoad(t *testing.T) {
	dir := configDirWith(t, baseConfig+"\n[repos.broken]\nurl = 7\n")
	calls := fakeReload(t, func(config.Paths) (agent.ReloadStatus, error) {
		return agent.ReloadStatus{}, nil
	})

	_, err := latemIn(t, dir, "", "service", "reload")
	if err == nil || !strings.Contains(err.Error(), "nothing was sent") {
		t.Errorf("want a refusal saying nothing was sent, got %v", err)
	}
	if *calls != 0 {
		t.Error("an unloadable config was sent to the agent")
	}
}

func TestServiceReload_ExitCodes(t *testing.T) {
	cases := []struct {
		name   string
		answer agent.ReloadStatus
		err    error
		code   int
		want   string
	}{
		{"no agent", agent.ReloadStatus{}, agent.ErrNoAgent, ExitAgentDown, "next agent to start"},
		{"an agent that cannot reload", agent.ReloadStatus{}, agent.ErrCannotReload, ExitFailure, "latem service restart"},
		{"refused by the agent", agent.ReloadStatus{Error: "unknown key"}, nil, ExitFailure, "kept the config it had"},
		{"no answer", agent.ReloadStatus{}, errors.New("did not report a reload"), ExitFailure, "did not report"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := configDirWith(t, baseConfig)
			fakeReload(t, func(config.Paths) (agent.ReloadStatus, error) { return tc.answer, tc.err })
			_, err := latemIn(t, dir, "", "service", "reload")
			if got := exitCode(err); got != tc.code {
				t.Errorf("exit %d, want %d (%v)", got, tc.code, err)
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %v lacks %q", err, tc.want)
			}
		})
	}
}
