// Package protocol defines every type that crosses the boundary between a
// Latem agent and a control plane.
//
// This is the only exported package in the module. The control plane imports
// this and nothing else, which is what makes the two halves independently
// buildable and the wire independently versionable (RUNNER_SPEC §4).
//
// # The trust boundary
//
// Types here fall into two groups, and the distinction is the whole point of
// the package:
//
//   - Inbound — [Command], and its three payloads [RunRequest],
//     [CancelRequest] and [PlanRequest]. This is data the agent receives from
//     outside its trust boundary. It is not trusted. Adding a field to any
//     payload, or a fourth [CommandKind], is a security review and a change
//     to RUNNER_SPEC §5 (INV-2), not a feature.
//
//   - Outbound — [Update], [LogFrame], [MachineHealth], [Resume],
//     [PlanResult]. The agent
//     produces these. [LogFrame] in particular has already passed the
//     redactor by the time it exists (INV-4); a frame that has been
//     constructed is safe to transmit.
//
// # What validation lives here
//
// Only structural well-formedness: that a command's kind is one of the two
// known values, that its payload matches its kind, and that identifiers are
// present. Semantic validation — that a repo appears in local config, that a
// ref survives the INV-3 regex, that an input is declared by the job — is
// deliberately *not* here. It belongs to internal/registry, runs against
// state this package cannot see, and is specified as an ordered gate chain in
// RUNNER_SPEC §10.
//
// Callers must not treat a successfully decoded [Command] as a safe one.
package protocol

// Version is the wire protocol version. An agent and a control plane
// disagreeing on the major component cannot usefully talk to each other.
//
// Increment the major component when a field is removed or its meaning
// changes; the minor when a field is added. Because inbound decoding is
// strict (see [Command.UnmarshalJSON]), even an additive change is visible to
// older agents as a decode failure rather than as silence, which is the
// trade this package makes deliberately: at a trust boundary, a loud refusal
// beats a quiet misinterpretation.
const Version = "2.0"
