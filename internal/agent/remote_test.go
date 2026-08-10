package agent

import (
	"testing"
)

// TestRemoteBroker_AnUnreachableControlPlaneIsNotAStartupFailure.
//
// This is the M6 clause "continues serving the local broker" in its agent-level
// form, and it covers more than a revoked token: a control plane that is down,
// misconfigured, or behind a DNS name that has not propagated must all be
// survivable. §6 has to finish, `latem run` has to work, and every runner has to
// stay available.
//
// The url points at a port nothing is listening on, so the transport fails at
// connect and keeps retrying in the background for the whole test.
func TestRemoteBroker_AnUnreachableControlPlaneIsNotAStartupFailure(t *testing.T) {
	cfg := newConfigDir(t, configOptions{remoteURL: "ws://127.0.0.1:1/agent"})
	ta := newTestAgent(t, cfg, nil)
	ta.run(t)

	if ta.remoteBrk == nil {
		t.Fatal("broker.remote is enabled but no remote transport was constructed")
	}
	if ta.localBrk == nil {
		t.Fatal("the local broker is not serving; an unreachable control plane must not cost local operation")
	}
	if ta.availableRunners() != 1 {
		t.Errorf("%d runners are available; a control plane has nothing to do with whether a runner can build",
			ta.availableRunners())
	}

	// The gauge is the operator's way of telling "the control plane cannot see
	// us" from "the machine is down", so it has to be honest about a transport
	// that exists and is not connected.
	ta.publishHealth(t.Context())
	if got := ta.Metrics().BrokerUp.Value("remote"); got != 0 {
		t.Errorf("broker_up{remote} = %v while the control plane is unreachable, want 0", got)
	}
	if got := ta.Metrics().BrokerUp.Value("local"); got != 1 {
		t.Errorf("broker_up{local} = %v, want 1", got)
	}

	// A heartbeat fans out over every broker. The remote one must absorb the
	// outage rather than reporting it fifteen seconds at a time — the reconnect
	// loop already says so, once per attempt, with the backoff in it.
	if ta.remoteBrk.Connected() {
		t.Error("the remote broker reports a live connection to a port nothing is listening on")
	}
	if ta.remoteBrk.Revoked() {
		t.Error("an unreachable control plane was mistaken for a revoked token; the loop would stop retrying")
	}
}

// TestRemoteBroker_AbsentWhenNotConfigured. A machine that has never registered
// is a fully supported state (§7.3), and it must not acquire a transport, a
// goroutine, or a token read.
func TestRemoteBroker_AbsentWhenNotConfigured(t *testing.T) {
	cfg := newConfigDir(t, configOptions{})
	ta := newTestAgent(t, cfg, nil)
	ta.run(t)

	if ta.remoteBrk != nil {
		t.Error("a remote transport was constructed with no [broker.remote] in the config")
	}
	if len(ta.brokers) != 1 {
		t.Errorf("%d brokers are registered, want only the local one", len(ta.brokers))
	}
}
