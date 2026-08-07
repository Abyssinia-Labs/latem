// Package invariants holds one test per normative rule in RUNNER_SPEC §5.
//
// The names carry the rule number so a CI failure says which invariant broke
// before anyone opens a file. A pull request that weakens one of these is
// rejected regardless of its other merits.
//
// Every test here starts skipped. That is deliberate: a skipped test is
// visible debt that shows up on every `go test ./...`, whereas a test that
// does not exist yet is invisible and gets written late, or not at all. Each
// is unskipped by the workstream that makes it pass, in the same pull request
// that makes it pass — never separately.
//
// These are not build-tagged. They must run in the default `go test ./...` so
// their skip lines stay in front of people.
package invariants

import "testing"

// INV-1 — The agent only makes outbound connections.
//
// Start an agent, enumerate listening sockets for the process, assert every
// entry is a Unix socket or bound to loopback. There is no HTTP server, no
// port to forward, and no inbound firewall rule; getting this wrong turns
// onboarding into a router tutorial for every user.
//
// The loopback metrics listener (RUNNER_SPEC §20) is permitted and must be
// asserted as loopback rather than merely tolerated.
func TestINV1_NoInboundListeners(t *testing.T) {
	t.Skip("unskipped by WS-11 (agent & local broker)")
}

// INV-2 — The broker supplies names, never definitions.
//
// Fuzz both command kinds with hostile values in every field; assert no
// execution occurs outside the registry-declared command set, and that the
// decoder rejects any kind outside the closed set rather than defaulting.
//
// The decoder half is already covered by pkg/protocol/command_test.go. This
// test covers the other half: that a well-formed but hostile command cannot
// reach an exec.
func TestINV2_NamesNotDefinitions(t *testing.T) {
	t.Skip("unskipped by WS-01 (registry & admission)")
}

// INV-3 — Broker-supplied values are never interpolated into a shell.
//
// Table-driven, with --upload-pack=/bin/sh, "; rm -rf /", $(id),
// ../../etc/passwd, refs containing newlines, 10 KB refs, and unicode
// homoglyphs. Assert every git invocation passes argv as a slice with `--`
// before user-influenced arguments, and that nothing reaches `sh -c`.
func TestINV3_NoShellInterpolation(t *testing.T) {
	t.Skip("unskipped by WS-01 (registry & admission)")
}

// INV-4 — Secrets never leave the machine.
//
// Run a job that echoes every loaded secret; assert none appears in the broker
// transcript, the persisted log file, inputs_json, or the run's error field.
//
// The fixture set MUST include a multi-line PEM: header, at least four base64
// body lines, footer. A version of this test covering only single-line secrets
// passes while the leak described in RUNNER_SPEC §16 is live — that is exactly
// how draft 01 of the spec shipped an unredacted App Store Connect key with a
// green test suite. Single-line coverage alone does not satisfy INV-4.
func TestINV4_SecretsNeverLeave(t *testing.T) {
	t.Skip("unskipped by WS-02 (log pipeline & redaction)")
}

// INV-5 — The registry comes from disk, at the pinned commit.
//
// Assert jobs.yaml is read from the checked-out workspace and never from a
// broker; that policy is matched against the normalized ref name; that
// refs/heads/main and main receive the same policy answer; that a
// server-synthesised namespace such as refs/pull/* cannot run without an
// explicit config entry; and that approval is local-only — no inbound command
// can grant it.
func TestINV5_RegistryFromDisk(t *testing.T) {
	t.Skip("unskipped by WS-01 (registry & admission)")
}
