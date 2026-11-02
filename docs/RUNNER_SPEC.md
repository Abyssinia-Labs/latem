# Latem Runner — Technical Specification

**Status:** Draft 02 · implementation-ready
**Licence:** Apache-2.0 for the runner stack. The patent grant is the reason to prefer it over MIT for a project that ships binaries; the hosted control plane is licensed separately.
**Module path:** `github.com/Abyssinia-Labs/latem`
**Scope:** the open-source runner stack only (agent, job registry, executors, cache, mobile pipeline, local trigger)
**Out of scope:** the hosted control plane (auth, Convex schema, web app). It is referenced here only through the `Broker` interface in §7.

> **Draft 02 resolves fourteen findings from the draft 01 review.** The material changes: source acquisition moved from git worktrees to hardlinked local clones (§12), the broker gained a command channel so cancellation has a path at all (§7), log frames gained sequence numbers (§7, §16), multi-line secret redaction became normative (§16), per-step process semantics are now stated correctly (§9, §13.1), and `awaiting_approval` acquired an exit (§11). §27 lists every change with its rationale.

> **Naming.** The project is `latem` — "metal" reversed — pronounced **LAY-tem**. Use that spelling in the first line of the README, since the name is otherwise ambiguous between LAY-tem and LAT-em and inconsistent pronunciation fragments word of mouth.
>
> The name appears in exactly three places in code: the binary name, the config directory `~/.latem`, and the `LATEM_` env prefix. Everything else refers to "the agent," "the runner," or "the registry." Do not scatter it further — a rename should stay a three-line change.
>
> Reserved: GitHub org, `latem.dev`, `latem.sh`, and placeholder packages on npm / crates.io / PyPI. Unrelated and non-conflicting: Latem Industries (Canadian metal finishing, holds `latem.com` and the same wordplay) and Polish *latem*, "in the summer."

---

## Table of contents

1. [Goals and non-goals](#1-goals-and-non-goals)
2. [Glossary](#2-glossary)
3. [Technology choices](#3-technology-choices)
4. [Repository layout](#4-repository-layout)
5. [Security invariants](#5-security-invariants-normative)
6. [Agent lifecycle](#6-agent-lifecycle)
7. [Broker interface](#7-broker-interface)
8. [Configuration](#8-configuration)
9. [Job registry format](#9-job-registry-format)
10. [Job resolution and admission](#10-job-resolution-and-admission)
11. [Run state machine](#11-run-state-machine)
12. [Source acquisition](#12-source-acquisition)
13. [Executors](#13-executors)
14. [Cache subsystem](#14-cache-subsystem)
15. [Secrets](#15-secrets)
16. [Log pipeline and redaction](#16-log-pipeline-and-redaction)
17. [Artifacts](#17-artifacts)
18. [Mobile build pipeline](#18-mobile-build-pipeline)
19. [Local trigger and CLI](#19-local-trigger-and-cli)
20. [Health, heartbeat, self-update](#20-health-heartbeat-self-update)
21. [Error taxonomy](#21-error-taxonomy)
22. [Persistence](#22-persistence)
23. [Installation and service management](#23-installation-and-service-management)
24. [Testing requirements](#24-testing-requirements)
25. [Work breakdown](#25-work-breakdown)
26. [Open questions](#26-open-questions)
27. [Changes from draft 01](#27-changes-from-draft-01)

---

## 1. Goals and non-goals

### Goals

- G1 — Run named jobs on hardware the operator owns, triggered locally or remotely, with no per-minute quota.
- G2 — Work fully standalone. No account, no network dependency on any hosted service, for every feature except remote triggering and remote log viewing.
- G3 — Build, sign, and ship iOS and Android applications without a third-party build service.
- G4 — Keep caches warm across runs while keeping the run workspace disposable.
- G5 — Be safe to point at an untrusted control plane. Compromise of the control plane must not yield arbitrary code execution on the machine.
- G6 — Install in under five minutes on a clean machine, with no inbound network configuration.

### Non-goals

- N1 — Not a GitHub Actions workflow-syntax reimplementation. There is no `uses:` marketplace, no composite actions, no expression language. Jobs shell out to scripts.
- N2 — Not a scheduler across machines. Each run targets exactly one machine, chosen by the trigger. Fan-out is a v2 concern.
- N3 — Not a hosted artifact store. Artifacts stay local or are pushed to their real destination (TestFlight, Play, a registry, S3) by the job itself.
- N4 — Not multi-tenant on a single machine. One machine, one owner, one set of credentials.

---

## 2. Glossary

| Term | Meaning |
|---|---|
| **Agent** | Long-lived daemon process on a machine. One per machine per role. |
| **Runner** | A configured execution slot inside the agent, with an executor, labels, and a concurrency limit. An agent hosts one or more runners. |
| **Job** | A named, statically-defined unit of work declared in the job registry. |
| **Run** | One execution of a job. Has an id, a state, logs, and a result. |
| **Executor** | The strategy that provides an environment for a run (docker, tart, native). |
| **Broker** | The source of run requests. Local or remote; the agent does not care which. |
| **Job registry** | `.latem/jobs.yaml`, checked into the repo, the sole source of what may execute. |
| **Machine token** | Per-machine credential authenticating the agent to a remote broker. |

---

## 3. Technology choices

**Language: Go 1.25+.** Single static binary, trivial cross-compilation to `darwin/arm64` and `linux/amd64`, first-class process and signal handling, mature Docker SDK. No runtime to install on the target machine, which matters for G6. Pin the toolchain line in `go.mod`: Go supports only the two most recent releases, and shipping the security-critical paths in §5 on an unsupported toolchain is not defensible.

**One binary, subcommands.** `latem agent`, `latem run`, `latem register`, `latem doctor`, `latem cache`, `latem logs`. Ships as `latem`. Avoids a second artifact and keeps the CLI and daemon on identical config-parsing code.

**Dependencies — keep this list short and justify additions in review.**

| Purpose | Library |
|---|---|
| Docker control | `github.com/docker/docker/client` |
| Config (TOML) | `github.com/BurntSushi/toml` |
| Job registry (YAML) | `sigs.k8s.io/yaml` (parses YAML via JSON tags, gives strict-mode for free) |
| Local state | `modernc.org/sqlite` (cgo-free — required, cgo breaks static cross-compilation) |
| Structured logging | `log/slog` (stdlib) |
| CLI | `github.com/spf13/cobra` |
| WebSocket | `github.com/coder/websocket` |
| BLAKE2b | `golang.org/x/crypto/blake2b` (§20's minisign verification) |

**One note on the eighth entry.** The minisign verifier in
`internal/selfupdate` is 150 lines of format parsing over `crypto/ed25519`, and
it would have needed no dependency at all but for one measured fact: **minisign
0.12 prehashes by default**, so the signature is over BLAKE2b-512 of the file
rather than over the file. Implementing BLAKE2b here was not on the table, and
accepting only the legacy PureEdDSA form would mean rejecting every signature
the current tool produces. A verifier that handles one of the two forms its
signing tool can emit is a verifier that one day calls a legitimate release
forged, with no diagnosis available to the operator beyond "minisign says it is
valid and latem says it is not".

**One note on YAML.** `sigs.k8s.io/yaml` routes YAML through JSON, and JSON has no duration type. Every duration field in §8 and §9 (`timeout`, `shutdown_grace`, `log_retention`, `boot_timeout`, `approval_ttl`, retention strings) is a `protocol.Duration` — a wrapper over `time.Duration` with a custom `UnmarshalJSON` accepting Go duration strings. Define it once in `pkg/protocol` and use it in both the TOML and YAML paths. Three independent implementations of this is the predictable failure.

**Explicitly rejected:** any embedded scripting engine, any expression evaluator, any plugin system loading code at runtime. Each is a route around §5.

---

## 4. Repository layout

```
latem/
├── cmd/latem/                  # cobra entrypoint, subcommand wiring only
├── internal/
│   ├── agent/                  # supervisor, runner pool, shutdown
│   ├── broker/
│   │   ├── broker.go           # the interface (§7)
│   │   ├── local/              # unix socket + sqlite queue
│   │   └── remote/             # websocket client, reconnect, backoff
│   ├── registry/               # jobs.yaml parse + validate (§9, §10)
│   ├── run/                    # state machine, orchestration (§11)
│   ├── executor/
│   │   ├── executor.go         # the interface (§13)
│   │   ├── docker/
│   │   ├── tart/
│   │   └── native/
│   ├── source/                 # mirrors, hardlinked run clones (§12)
│   ├── cache/                  # volume management, locks, prune (§14)
│   ├── secrets/                # provider chain (§15)
│   ├── logstream/              # framing, redaction, sinks (§16)
│   ├── artifact/               # collection, retention (§17)
│   ├── config/                 # config.toml
│   ├── store/                  # sqlite; migrations embedded
│   ├── metrics/                # loopback-only expvar handler (§20)
│   └── system/                 # platform detection, service install
├── pkg/protocol/               # wire types shared with the control plane
├── docs/
├── examples/
│   ├── expo-app/               # reference jobs.yaml for §18
│   └── go-service/
└── test/
    ├── integration/
    └── fixtures/
```

`pkg/protocol` is the only exported package. Everything else is `internal/`.

**Everything that crosses the wire is defined in `pkg/protocol`** — `Command`, `RunRequest`, `CancelRequest`, `Origin`, `Update`, `MachineHealth`, `RunnerInfo`, `ArtifactRef`, `RunState`, `LogFrame`, `Duration`, and the error-code constants of §21. `internal/broker` defines the *interface* and the two transports; it defines no types that appear on the wire. The control plane imports `pkg/protocol` and nothing else. This is what makes the two halves independently buildable, the protocol independently versionable, and the trust boundary reviewable by reading one package.

---

## 5. Security invariants (normative)

These are requirements, not guidance. Each has a test in `test/integration/invariants_test.go` and a review checklist item. A pull request that weakens one is rejected regardless of its other merits.

### INV-1 — The agent only makes outbound connections

The agent MUST NOT bind a TCP listener on any non-loopback interface. The local broker listens on a Unix domain socket at `~/.latem/agent.sock`, mode `0600`. There is no HTTP server, no port to forward, no inbound firewall rule.

*Test:* start an agent, enumerate listening sockets for the process, assert every entry is a Unix socket or bound to `127.0.0.1`.

> **The obvious implementation of that test is vacuous on macOS.** `getsockopt(SO_ACCEPTCONN)` returns `ENOPROTOOPT` on Darwin **for a genuine listening socket** — verified on macOS 26. A detector written that way enumerates zero descriptors, finds nothing to object to, and reports the invariant clean having examined nothing.
>
> A conformant test therefore falls back to `SOCK_STREAM` plus no peer (`ENOTCONN`) plus a non-zero port, asserts explicitly that it enumerated *something*, and carries a negative control that binds `0.0.0.0:0` and requires the detector to see it. Without the control there is nothing to distinguish "no violations" from "no vision" — which is the same failure the INV-4 test has, for the same reason.

### INV-2 — The broker supplies names, never definitions

The inbound command set is **closed at two kinds**: `run` and `cancel` (§7). A third kind is a §5 change requiring a written threat model, not a feature.

A `run` carries `{id, repo, job, ref, inputs, origin}`. It MUST NOT carry commands, scripts, images, environment definitions, file paths, executor selection, cache configuration, timeouts, or label overrides. `RunRequest` has no field capable of expressing any of those.

A `cancel` carries `{id, actor, reason}` and nothing executable. `reason` is display-only: capped at 200 runes, never interpolated, never passed to a shell, redacted before it is logged or persisted. It exists so a build stopped from a phone says why.

The agent resolves `job` against the registry it read from disk. An unknown name is rejected with `E_UNKNOWN_JOB` before any process starts.

*Test:* fuzz every field of both command kinds with hostile values; assert no execution occurs outside the registry-declared command set, and assert the JSON decoder rejects any `kind` outside the closed set rather than defaulting.

### INV-3 — Broker-supplied values are never interpolated into a shell

`ref` and `inputs` originate outside the trust boundary. They are passed to the executor as environment variables only. The agent MUST NOT build a command string by concatenating them, and MUST NOT pass them to `sh -c`.

Validation, applied before use:

- `id` — must match `^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`. Draft 02 omitted this and it was a real hole: the agent builds `work/<id>`, `logs/<id>.log`, `artifacts/<id>/`, and the container and VM name `latem-run-<id>` from this field, so an id of `../../../../tmp/x` writes wherever the agent's user can write. The class excludes every separator **and every dot**, so no id can traverse a directory or collide with `.` or `..`. Enforced in `protocol.ValidateID`, at decode, before a command is ever acted on — and re-checked by each consumer that turns an id into a path, because one validation point everything else trusts is the shape of the next incident.
- `repo` — must match a `[repos.*]` key present in local config. Exact string match against a map. No path construction from this value.
- `ref` — must match `^[A-Za-z0-9._/-]{1,255}$`, must not begin with `-`, must not contain `..`, must not be `HEAD@{…}`. This blocks git argument injection (`--upload-pack=`, `--exec=`).
- `inputs` — every key must be declared in the job's `inputs:` block. Values are type-checked per declaration. Undeclared keys are a hard rejection, not a warning.

All git invocations pass argv as a slice via `exec.Command`, with an **options terminator** before any user-influenced argument. Never `exec.Command("sh", "-c", …)`.

**The terminator is `--` or `--end-of-options`, and which one is not a style choice** (amendment A7). Draft 02 said `--` universally, which is wrong for exactly the two commands §12 depends on, because to git `--` means "a pathspec follows" rather than "options are over":

```console
$ git checkout --detach -- "$SHA"
fatal: git checkout: --detach does not take a path argument
$ git rev-parse --verify -- "${SHA}^{commit}"
fatal: Needed a single revision
```

So: use `--` where the following arguments really are paths, and `--end-of-options` (git ≥ 2.24) everywhere a revision or a ref name follows. Both stop option parsing, which is the property INV-3 needs; only one of them also claims the remaining arguments are files.

*Test:* table-driven, with `--upload-pack=/bin/sh`, `; rm -rf /`, `$(id)`, `../../etc/passwd`, `refs/heads/x\nrefs/heads/y`, 10 KB refs, and unicode homoglyphs.

### INV-4 — Secrets never leave the machine

Secret values MUST NOT appear in any structure sent to a broker, including run metadata and error messages. Every log line passes the redactor (§16) before reaching any sink. The redactor is applied at the point of emission, not at the sink, so a sink cannot be added that bypasses it.

**Multi-line secrets are redacted line by line.** The log pipeline splits into lines before redacting, so registering only the whole value of a multi-line secret redacts nothing but the first line. `APP_STORE_CONNECT_KEY_P8` is a PEM block and is declared in this spec's own example job; leaking its body is losing the App Store Connect key. Registration of a secret whose value contains a newline MUST additionally register every non-empty line of it, and the whitespace-stripped concatenation, as independent redaction targets. See §16 for the exact rule.

*Test:* run a job that echoes every loaded secret, including a fixture secret shaped like a real multi-line PEM (`-----BEGIN PRIVATE KEY-----`, ≥4 base64 body lines, footer). Assert that no line of it appears in the broker transcript, the persisted log file, `inputs_json`, or the run's error field. A test that only exercises single-line secrets does not satisfy INV-4.

### INV-5 — The registry comes from disk, at the pinned commit

`jobs.yaml` is read from the checked-out workspace after checkout, never fetched from the broker. Jobs are resolved against the registry at the resolved commit, so a run is reproducible from the commit alone.

A job's definition is the pinned commit's. A checkout step may move its working tree to the pinned commit or to one reachable from a ref the policy admits, and the run's log names it (amendment A53). A run is reproducible from its commit and the commits its log names.

Because a `ref` can point at a branch a contributor controls, config gates which refs may run without approval:

```toml
[repos.myapp]
allowed_refs = ["main", "develop", "release/*"]
require_approval_otherwise = true
approval_ttl = "24h"                              # unapproved → rejected
```

#### Ref normalization, stated exactly

Draft 01 said the glob was "matched after resolution," which is meaningless — resolution produces a SHA, and globbing `release/*` against a SHA matches nothing. The order is **normalize, resolve, then match policy**:

1. **Validate** the raw `ref` string (INV-3 regex).
2. **Normalize to a fully-qualified ref name** against the mirror, in this order: if it already begins with `refs/`, take it as-is; otherwise try `refs/heads/<ref>`, then `refs/tags/<ref>`. Exactly one must exist. Zero is `E_INVALID_REF`; more than one is `E_AMBIGUOUS_REF` — never guess.
3. **Expand each `allowed_refs` entry** the same way: an entry without a `refs/` prefix means `refs/heads/<entry>`. So `main` matches `refs/heads/main` and does **not** match `refs/tags/main`. To allow tags, write `refs/tags/v*` explicitly.
4. **Resolve** the normalized name to an immutable SHA, and record both in the run row.
5. **Match** the expanded globs against the normalized *name* — never against the SHA — with path-aware globbing, where `*` does not cross `/`. `release/*` matches `refs/heads/release/1.2` and not `refs/heads/release/1.2/hotfix`; use `release/**` for that.

Draft 02 had steps 4 and 5 the other way round, and amendment A27 swapped them. Matching still happens against the name, so nothing about the policy answer changes; what changes is that the commit is known *before* the policy decision, which is what lets a run refused by step 5 be parked with the commit an operator is being asked to approve. Resolving first costs a `git rev-parse` against a mirror that has already been fetched — a local read, no clone, and nothing in the commit is looked at, let alone executed.

Two consequences worth stating because they are the point of the rule. `refs/heads/main` and `main` are the same ref and get the same policy answer — a fully-qualified spelling cannot smuggle a branch past a short-form allowlist. And `refs/pull/*`, `refs/merge-requests/*`, and every other server-synthesised namespace fail step 3 by construction, so an untrusted pull request cannot execute without an explicit, deliberate config entry.

#### Approval

Runs on non-matching refs enter `awaiting_approval` and require a local `latem approve <run-id>`. Approval is a local action only; a broker cannot grant it, and no inbound command kind can express it (INV-2).

**Approval is bound to a commit, not to a ref.** `latem approve` records `approved_at` and `approved_sha` (§22), where `approved_sha` is the commit the run was parked at, and gate 8 admits an out-of-policy run only while the freshly resolved commit still equals it. This is the whole content of the mechanism rather than an implementation detail. The reason this invariant exists at all is that a `ref` can point at a branch a contributor controls — so an approval that granted the *ref* could be obtained for a benign commit and spent on a hostile one: push, ask, get approved, force-push, and the run builds what arrived last. That is a time-of-check-to-time-of-use hole wide enough to defeat the invariant completely, and an approval mechanism with it is decoration.

If the ref moves between the approval and the build, the run is **parked again** rather than built, the approval is discarded, and the message says the ref moved. The operator is then asked about the commit that actually arrived. Two consequences follow:

- **A run is never parked without a resolved commit.** Gate 4 — the conservative pre-check, which runs before anything has been fetched — therefore does not park. It rejects when the repo does not permit approval at all, and otherwise queues the run for gate 8 to decide once the commit is known. A run parked with no commit would be one nothing could ever approve, since there would be nothing to pin.
- **`latem approve` refuses a run with no `sha`.** In practice that is a row parked by an agent older than amendment A27; there is no safe reading of such an approval, because the only commit it could name is "whatever the ref points at by the time a runner picks this up".

`awaiting_approval` is not terminal, so it needs an exit or it accumulates forever and the partial index in §22 grows without bound. A run unapproved after `approval_ttl` (default 24h) transitions to `rejected` with `E_APPROVAL_EXPIRED`. The same applies to `queued`: `queue_ttl` (default 72h) → `rejected` with `E_QUEUE_EXPIRED`. Both are swept by the daily ticker and at agent startup.

---

## 6. Agent lifecycle

```
latem agent
  │
  ├─ 1. load config.toml; fail fast on invalid
  ├─ 2. acquire ~/.latem/agent.lock (flock) — exactly one agent per config dir
  ├─ 3. open sqlite, run migrations
  ├─ 4. preflight (§20): docker reachable, tart present, git present,
  │       disk headroom, keychain unlocked (macOS native)
  ├─ 5. reconcile: any run left in a non-terminal state from a crash is
  │       marked `interrupted`; orphaned containers/VMs/workspaces are reaped
  │       and expired queued/awaiting_approval runs are swept (§11)
  ├─ 6. start runner pool (one goroutine per runner, bounded by concurrency)
  ├─ 7. connect brokers; local always, remote if configured
  ├─ 8. start heartbeat ticker
  └─ 9. block until SIGINT/SIGTERM → graceful shutdown
```

**Graceful shutdown.** Stop accepting new runs, mark the machine `draining`, allow in-flight runs to finish up to `shutdown_grace` (default 30m, since an iOS build is long), then send the run's process group SIGTERM, wait 30s, SIGKILL. Always tear down containers and VMs, even on the kill path — leaked Tart VMs consume disk fast. Release the lock last.

**Crash recovery.** Step 5 is not optional. On startup, list containers with label `latem.run-id`, list Tart VMs matching `latem-run-*`, list workspaces under `~/.latem/work/`, and delete anything whose run id is not in a non-terminal state in sqlite. Because §12's workspaces are standalone clones, deleting one is a plain `rm -rf` with no administrative state left behind in the mirror.

---

## 7. Broker interface

The single seam between the runner stack and any control plane. Both implementations satisfy it; the agent contains no branch on which is in use.

The interface lives in `internal/broker`. Every type it mentions lives in `pkg/protocol` (§4), because those types are the wire and the wire is the trust boundary.

```go
package broker

import "github.com/Abyssinia-Labs/latem/pkg/protocol"

type Broker interface {
    // Commands yields inbound commands. It closes only on ctx cancellation.
    // Implementations reconnect internally; a dropped connection is not
    // an error the caller sees.
    Commands(ctx context.Context) (<-chan protocol.Command, error)

    // Claim attempts exclusive ownership. Returns ErrAlreadyClaimed if
    // another runner won. MUST be atomic at the broker.
    Claim(ctx context.Context, runID string) error

    // Report publishes a state transition. Idempotent by (runID, state).
    Report(ctx context.Context, runID string, u protocol.Update) error

    // Logs returns a sequenced sink for streamed output.
    Logs(ctx context.Context, runID string) (LogSink, error)

    // Heartbeat publishes liveness and capacity.
    Heartbeat(ctx context.Context, h protocol.MachineHealth) error
}

// LogSink replaces draft 01's io.WriteCloser. A plain writer cannot
// express resumption: after a reconnect the agent must know what the
// far side already has, or it duplicates whatever it re-sends.
type LogSink interface {
    // Write submits a batch. Frames carry monotonic sequence numbers.
    Write(ctx context.Context, frames []protocol.LogFrame) error

    // Acked is the highest sequence THIS TRANSPORT'S far side has durably
    // accepted for this run. The transport persists it under its own key in
    // broker_state (§22) and resumes from Acked()+1.
    Acked() uint64

    Close(ctx context.Context) error
}
```

**The ack position belongs to a transport, not to a machine** (amendment A28). Both implementations satisfy one interface and the agent fans every run's frames out to all of them, so two sinks are live for one run at once and they are at different positions: the local one is at whatever this disk holds, the remote one at whatever the network has confirmed. §22 originally gave the machine one `last_acked_seq`, which made the higher of the two win — silently, and in the direction that loses the frames covering an outage. `broker_state` keys both the resume point and `reported_state` by transport; see §22.

### 7.1 Inbound — the complete trust boundary

Two command kinds. This is a closed set (INV-2); the decoder rejects anything else rather than zero-valuing it.

```go
package protocol

type CommandKind string

const (
    CommandRun    CommandKind = "run"
    CommandCancel CommandKind = "cancel"
)

type Command struct {
    Kind   CommandKind    `json:"kind"`
    Run    *RunRequest    `json:"run,omitempty"`
    Cancel *CancelRequest `json:"cancel,omitempty"`
}

// RunRequest is the complete set of data crossing the trust boundary
// inbound for an execution. Adding a field here is a security review.
type RunRequest struct {
    ID     string            `json:"id"`
    Repo   string            `json:"repo"`             // must exist in local config
    Job    string            `json:"job"`              // must exist in registry
    Ref    string            `json:"ref"`              // validated, INV-3
    Inputs map[string]string `json:"inputs,omitempty"` // declared inputs only
    Origin Origin            `json:"origin"`           // for audit only
}

// CancelRequest carries an identifier and nothing executable.
type CancelRequest struct {
    ID     string `json:"id"`
    Actor  string `json:"actor,omitempty"`
    Reason string `json:"reason,omitempty"` // display only, ≤200 runes, redacted
}

type Origin struct {
    Kind        string    `json:"kind"` // "manual" | "webhook" | "schedule" | "cli"
    Actor       string    `json:"actor,omitempty"`
    RequestedAt time.Time `json:"requested_at"` // audit/display only — never trusted
}
```

**Why cancel is a command and not a hole in INV-2.** Draft 01 defined a `cancelled` state reachable by "operator or broker cancellation" but gave the broker no channel to express it — the product promises a Stop button on a phone that the protocol could not deliver. A cancel carries a run id the broker already knows, and its entire effect is to context-cancel work the agent itself started. It cannot name a job, a ref, a command, or a machine. It is the smallest possible inbound verb.

Cancel semantics: idempotent, and a no-op for an unknown, unclaimed, or already-terminal id. A cancel arriving during `preparing` or `running` cancels the run context, which triggers the §11 teardown path. A cancel arriving during `collecting` is ignored — artifacts and logs from a build that already ran are worth more than a few seconds of teardown latency.

**Inputs are strings on the wire.** `map[string]string` is deliberate: JSON's type system is not the registry's. Values are coerced to the declared type (`string`|`bool`|`int`|`enum`) at gate 9 (§10); a coercion failure is `E_BAD_INPUT`. Nothing downstream of gate 9 ever sees an uncoerced value.

### 7.2 Outbound

```go
package protocol

type Update struct {
    State      RunState      `json:"state"`
    ExitCode   *int          `json:"exit_code,omitempty"`
    ErrorCode  ErrorCode     `json:"error_code,omitempty"`
    ErrorMsg   string        `json:"error_message,omitempty"` // redacted
    StartedAt  *time.Time    `json:"started_at,omitempty"`
    FinishedAt *time.Time    `json:"finished_at,omitempty"`
    Artifacts  []ArtifactRef `json:"artifacts,omitempty"`     // names+sizes, not content
}

// Resume is the first frame after a reconnect (§7.4). Without it the agent
// cannot tell the control plane where its log stream left off, and the
// sequence numbers below have nothing to resume from.
type Resume struct {
    MachineID string      `json:"machine_id"`
    Runs      []RunResume `json:"runs"`
}

type RunResume struct {
    RunID        string   `json:"run_id"`
    State        RunState `json:"state"`
    LastAckedSeq uint64   `json:"last_acked_seq"`
}

// LogFrame is the unit of log transport. Seq is per-run, monotonic
// from 1, assigned at the point of emission — after redaction (§16),
// so a frame that reaches this struct is already safe to send.
type LogFrame struct {
    RunID  string    `json:"run_id"`
    Seq    uint64    `json:"seq"`
    Stream string    `json:"stream"` // "stdout" | "stderr" | "agent"
    At     time.Time `json:"at"`
    Line   string    `json:"line"`
}

type MachineHealth struct {
    MachineID  string       `json:"machine_id"`
    AgentVer   string       `json:"agent_version"`
    OS         string       `json:"os"`
    Arch       string       `json:"arch"`
    Runners    []RunnerInfo `json:"runners"`
    DiskFreeGB float64      `json:"disk_free_gb"`
    Draining   bool         `json:"draining"`
}
```

**Why sequence numbers.** The remote broker buffers logs across a partition and flushes on reconnect, and the agent is required to tolerate replays. State transitions are idempotent by `(runID, state)`, so replaying them is free. Log lines are idempotent by nothing — replaying them duplicates output, and the duplicated region is exactly the interesting part of a build that was happening when the network dropped. `(run_id, seq)` gives the server a dedupe key and the agent a resume point. The server keeps the first frame it sees for a given pair and discards later ones.

Sequence numbers are assigned once per run, by the emitter, before fan-out (§16), so every transport sees the same numbering. Resume *points* are per transport (§22): the numbers are machine-wide, the positions in them are not.

### 7.3 Local broker

Unix socket at `~/.latem/agent.sock`, mode `0600`, newline-delimited JSON, backed by the sqlite queue in §22. Claim is a conditional `UPDATE … WHERE state='queued'`, relying on sqlite's write serialization.

The queue is not only a buffer for a busy agent — it is also what `latem run` writes to when **no agent is running at all**. Draft 01 said "momentarily busy," which under-promises. Concretely:

- Agent up → `latem run` connects to the socket, enqueues, and the run starts.
- Agent down → the socket connect fails; `latem run` enqueues directly into sqlite, prints `queued (agent not running)` and the run id, and exits 0. The agent picks it up at step 6 of §6.
- `latem run --wait` or `--follow` against a down agent is an error, not a hang: `E_AGENT_DOWN`, exit 3, with the start command in the message.

Cancel arrives on the local broker as the same `protocol.Command`, emitted by `latem cancel`. One code path, both transports.

### 7.4 Remote broker

WebSocket to the control plane. `Authorization: Bearer <machine-token>`.

- Reconnect with exponential backoff: 1s → 2s → 4s … capped at 60s, ±20% jitter.
- On reconnect the agent sends a resume frame carrying, for every non-terminal run it owns, `{run_id, state, last_acked_seq}`. The server replays commands the agent missed and resumes log ingestion from `last_acked_seq + 1`. Every handler is idempotent by run id. The `last_acked_seq` in that frame is **this transport's own** — the live sink's position when there is one, and `broker_state` under `broker = 'remote'` otherwise (§22, amendment A28). It is never the machine's high-water mark, which a local broker writing to the same disk keeps far ahead of any network.
- Log frames buffer to disk while disconnected, bounded at 50 MB per run; beyond that, drop and emit one marker frame recording how many lines were lost. Sequence numbers keep incrementing across the gap so the gap is visible rather than silent.
- If the token is rejected (`401`), stop retrying, log loudly, and continue serving the local broker. A revoked token must never disable local operation.
- `[agent] remote_logs = "always" | "on_failure"` (default `always`) controls whether frames ship live or are buffered and sent only if the run reaches a failing terminal state. `on_failure` exists for verbose Gradle output; it trades live viewing for bandwidth.

---

## 8. Configuration

`~/.latem/config.toml`, mode `0600`. Reloaded on `SIGHUP` — `latem service reload` sends it and prints the answer — which applies `[repos.*]` with their refs, vars and environments, `[label_aliases]`, `[actions]` and `log_retention`; changes to `[[runner]]`, `[machine] id`, `[broker.*]`, `[secrets]` and the other `[agent]` keys require a restart, and the agent names each one it read and did not apply (amendment A54).

```toml
schema_version = 1

[machine]
name  = "mac-mini"                 # display only
id    = "mch_01HQZX3N8G4K2MVBWT"  # assigned at register; stable
labels = ["macos", "arm64", "xcode-16", "ios"]

[agent]
shutdown_grace = "30m"
work_dir       = "~/.latem/work"
log_retention  = "14d"
queue_ttl      = "72h"              # queued longer than this → rejected
auto_update    = false   # true needs a build with an embedded signing key (§20)
remote_logs    = "always"           # "always" | "on_failure"
metrics_addr   = "127.0.0.1:9838"   # loopback only; "" disables (INV-1)

[broker.local]
enabled = true
socket  = "~/.latem/agent.sock"

[broker.remote]
enabled    = true
url        = "wss://api.example.com/agent"
token_file = "~/.latem/agent.key"   # 0600, never inline in this file

# --- repos: the allowlist. A repo absent here cannot be built. ---
[repos.myapp]
url          = "git@github.com:me/myapp.git"
allowed_refs = ["main", "release/*"]   # expanded to refs/heads/* — INV-5
require_approval_otherwise = true
approval_ttl = "24h"

[repos.api]
url          = "ssh://git@forgejo.internal/me/api.git"
allowed_refs = ["main", "refs/tags/v*"]

# --- runners: execution slots ---
[[runner]]
name        = "linux-x64"
executor    = "docker"
labels      = ["linux", "amd64", "docker"]
concurrency = 2

  [runner.docker]
  default_image = "ghcr.io/me/ci-base:latest"
  gpu           = false
  network       = "bridge"
  cpus          = 4.0
  memory        = "8g"
  min_free_gb   = 40                # refuse new runs below this (§14.4)

[[runner]]
name        = "gpu"
executor    = "docker"
labels      = ["linux", "amd64", "gpu", "cuda"]
concurrency = 1

  [runner.docker]
  default_image = "nvidia/cuda:12.4.0-devel-ubuntu22.04"
  gpu           = true
  memory        = "32g"

# The default macOS runner: the host's own OS, no VM (§13.3). This is what
# `latem init` writes, and it needs nothing installed that a Mac with Xcode on
# it does not already have. Read §13's opening before deploying it — it has no
# isolation, and that is a decision about whose code you let run here, not a
# detail.
[[runner]]
name        = "ios"
executor    = "native"
labels      = ["macos", "arm64", "xcode", "ios"]
concurrency = 1                     # enforced; §13.3

  [runner.native]
  min_free_gb = 60
  # Where a toolchain lives that the service's environment does not reach
  # (A60): a keg-only Homebrew install is on no PATH launchd gives the agent.
  # Beneath the job's env, which wins; never a secret.
  # path_prepend = ["/opt/homebrew/opt/openjdk@17/bin"]
  # env          = { JAVA_HOME = "/opt/homebrew/opt/openjdk@17/libexec/openjdk.jdk/Contents/Home" }

# The isolation upgrade (§13.2). Uncomment — and delete the native block above,
# since both carry the same labels — when this machine builds a ref anyone
# outside your team can influence. It costs a ~69 GB image pull, ~140 GB of
# disk, and 30–60s of boot per run.
#
# [[runner]]
# name        = "ios"
# executor    = "tart"
# labels      = ["macos", "arm64", "xcode", "ios"]
# concurrency = 1                   # hard ceiling is 2 — Apple's licence, §13.2
#
#   [runner.tart]
#   base_image   = "latem-xcode-26"
#   cpus         = 6
#   memory_gb    = 16
#   ssh_user     = "admin"
#   boot_timeout = "3m"
#   min_free_gb  = 200              # provisioned size is ~140 GB; §13.2
#   derived_data = "vm"             # "vm" | "shared" — set by measurement, §13.2
```

**One agent, several runners.** The example above is one machine — an Apple silicon Mac — hosting three execution slots, one of them Docker and one of them the host's own macOS. That is the model: an agent owns a config directory, a machine id, and a set of labelled runners. "Two roles on one box" does **not** mean two agents; it means two `[[runner]]` blocks with different labels, and Apple work never lands on the container runner because the labels do not match.

**On the two macOS blocks carrying identical labels.** That is deliberate: a job asking for `[macos, xcode]` should not have to know which executor answers, so switching a machine from native to Tart is a config edit and not a change to every repo's `jobs.yaml`. Gate 13 picks the first runner in config order whose labels match, so enabling both is not ambiguous — it is worse than ambiguous. The second block never runs, and reads in the config file as though it does. Enable one.

Running two agents on one machine is possible and unsupported-by-default: pass `--config-dir` or set `LATEM_CONFIG_DIR`, which relocates the lock, socket, sqlite, work dir, and caches together. Each such agent is a distinct machine to the control plane, with its own token. Reach for it only when the two agents genuinely need different owners, which N4 says they should not.

**Validation at load.** All of these are fatal, not warnings — a runner that starts with a broken config fails later, in a build, where it is expensive to diagnose.

- Unknown keys are an error (strict decode). `schema_version` mismatch is fatal with a migration hint.
- Every `[[runner]]` name is unique. Every `allowed_refs` glob compiles.
- `executor = "tart"` requires `concurrency <= 2` (Apple's licence, §13.2). `executor = "native"` requires `concurrency == 1` (§13.3). Both are enforced here rather than discovered at runtime.
- `token_file` is mode `0600`, and `~/.latem` itself is mode `0700`. A world-readable config directory holding `secrets.env` and a machine token is the whole threat model failing quietly.
- `metrics_addr`, when non-empty, resolves to a loopback address. A non-loopback value is rejected at load — that is INV-1 enforced in the parser, which is the only place it can be enforced cheaply.
- `remote_logs` is one of the two declared values; `derived_data` is one of `vm`|`shared`.

---

## 9. Job registry format

`.latem/jobs.yaml`, committed to the repo. Strict-decoded; unknown fields are errors.

```yaml
version: 1

defaults:
  timeout: 30m
  shell: bash

caches:
  - name: pnpm
    path: /home/runner/.pnpm-store
  - name: gradle
    path: /home/runner/.gradle
  - name: cocoapods
    path: ~/Library/Caches/CocoaPods
  - name: derived-data
    path: ~/Library/Developer/Xcode/DerivedData

jobs:
  test:
    description: Unit tests
    runs_on: [linux, docker]
    image: ghcr.io/me/ci-base:20
    timeout: 15m
    caches: [pnpm]
    steps:
      - name: Install
        run: ./ci/install.sh
        timeout: 5m
      - name: Test
        run: ./ci/test.sh
        timeout: 10m

  ios-release:
    description: Build and upload an iOS release to TestFlight
    runs_on: [macos, xcode]
    timeout: 90m
    caches: [cocoapods, derived-data, pnpm]
    concurrency_group: ios          # serialises across jobs, not just runners
    inputs:
      channel:
        type: enum
        values: [internal, external]
        default: internal
      notes:
        type: string
        max_length: 500
        default: ""
    secrets:
      - MATCH_PASSWORD
      - APP_STORE_CONNECT_KEY_ID
      - APP_STORE_CONNECT_ISSUER_ID
      - APP_STORE_CONNECT_KEY_P8
    artifacts:
      - path: build/*.ipa
        retain: 7d
    steps:
      - name: Unlock keychain
        run: ./ci/ios/unlock-keychain.sh
        timeout: 2m
      - name: Sync certificates
        run: ./ci/ios/match.sh
        timeout: 5m
      - name: Build and upload
        run: ./ci/ios/build.sh
        timeout: 75m
        env:
          RELEASE_CHANNEL: ${input.channel}
          RELEASE_NOTES: ${input.notes}
```

### Field reference

| Field | Type | Notes |
|---|---|---|
| `runs_on` | `[]string` | Label set. A runner must carry **all** listed labels. |
| `image` | string | Docker executors only. Overrides `default_image`. Ignored by tart/native. |
| `timeout` | duration | Wall clock, whole job. Enforced by the agent, not the executor. |
| `caches` | `[]string` | References `caches[].name`. Undefined name is a parse error. |
| `concurrency_group` | string | At most one run per group per machine. Queued, not rejected. |
| `inputs` | map | Declares name, type (`string`\|`bool`\|`int`\|`enum`), constraints, default. |
| `secrets` | `[]string` | Names resolved by §15. Undeclared secrets are not injected. |
| `artifacts` | list | Glob relative to workspace, plus retention. |
| `steps[].run` | string | **Literal.** The only place a command may be written. |
| `steps[].env` | map | May reference `${input.NAME}`. May not reference secrets. |
| `steps[].timeout` | duration | Optional. Wall clock for one step. Defaults to the remaining job budget. |

### Step execution semantics

Draft 01 claimed that because steps run in one container, "`cd`, `export`, and installed tools persist across steps." Only the last of those is true, and shipping the claim would generate a bug report from the first user who writes `export PATH=…` in one step and reads it in the next.

The accurate contract, identical across all three executors:

- **Each step is a new process.** Docker runs a fresh `exec` into the same container; Tart opens a fresh `ssh` session; native spawns a fresh `bash -lc`. Working directory, exported variables, shell functions, and `trap`s do not survive the end of a step.
- **The filesystem persists** for the whole run. Tools installed in step 1 are on `PATH` in step 3, files written are still there, and caches mounted at `preparing` stay mounted through `collecting`.
- **Every step starts in the workspace root** with the same environment: the run environment (§15 secrets, `LATEM_*`, `steps[].env`), regardless of what the previous step did.
- **The environment carries a locale.** `LANG` is set to `en_US.UTF-8` beneath everything else, so `steps[].env` and the run's own variables both override it. It is a default rather than an assertion, unlike `LATEM_*`, which a job may not redefine. **Amendment A33**, and the reason it is spelled out here: a step is a new process with only the environment the agent hands it, which is correct and was missing something every interactive shell has. `pod install` aborts with `Unicode Normalization not appropriate for ASCII-8BIT` — a message naming Unicode rather than the locale — and Ruby, Python and Java tools each fail differently for the same cause. The macOS executor (§13.3) supplies it; the Docker executor does not, because an image brings its own locale and forcing one an image never generated makes glibc worse rather than better.

This is GitHub Actions' model, so it is already what people expect; it just has to be written down. The consequence for job authors is the obvious one: if two commands must share shell state, they are one step, and the sequencing belongs in the shell script. That is N1 working as intended.

**Passing values between steps.** Because shell state does not survive, there is one sanctioned channel. The agent creates an empty file per run and exports its path as `$LATEM_ENV` — and, since A43, as `$GITHUB_ENV` too, the same file under the name a workflow written for GitHub reads. A step may append `KEY=value` lines to it, or a `KEY<<DELIMITER` block for a value that spans lines; between steps, the agent reads, truncates, and merges them into the run environment for every subsequent step.

```bash
echo "BUILD_NUMBER=$(git rev-list --count HEAD)" >> "$LATEM_ENV"
{ echo "NOTES<<EOF"; git log -3 --format=%s; echo "EOF"; } >> "$LATEM_ENV"
```

Constrained, because this is a file a job can write and the agent then trusts:

- Keys must match `^[A-Za-z_][A-Za-z0-9_]*$`. Malformed lines are dropped with a warning, not silently. A `KEY<<DELIMITER` block that is never closed sets nothing — where the value was meant to end is not knowable, and "end of file" would be a guess.
- Keys colliding with a declared secret name, or with any `LATEM_*` name, are **ignored** — a job cannot overwrite an injected credential or spoof the agent's own variables.
- The file is capped at 64 KB per step; values pass through the redactor before appearing in any log.
- It does not cross the trust boundary in either direction. It is job-authored data affecting the job's own environment, which is a different thing from §5's concern.

**Step timeouts.** Draft 01 tracked wall clock only at the job level, which means a 90-minute iOS job hiding a hung 5-minute step wastes an hour before anyone learns anything. `steps[].timeout` defaults to whatever remains of the job budget, so it is opt-in and never makes a job stricter by accident. Exceeding it kills the step's process group, fails the run with `E_STEP_TIMEOUT`, and still runs `collecting` — a hung step's partial logs are the diagnostic.

### Interpolation rules

`${input.NAME}` is the **only** interpolation syntax, and it is legal **only** inside `steps[].env` values. It is substituted into the env map after type-checking, never into `run`. There is no `${{ }}`, no expressions, no conditionals, no `if:`. Branching belongs in the shell script.

Secrets are never interpolated. They arrive in the environment by name because they were declared in `secrets:`.

---

## 10. Job resolution and admission

Ordered. Every gate before "execute" must pass, and each failure is a distinct error code (§21).

Two gates from draft 01 are split in two here, because each had a cheap conservative half and an authoritative half that needs I/O. Ref policy cannot be decided without knowing whether the ref is a branch or a tag, and input types are declared in a registry that does not exist until a commit is checked out. Doing the cheap half first keeps a hostile request cheap to reject; doing the authoritative half later keeps the answer correct.

```
                              ─── no I/O, no process, no network ───
 1. repo lookup                 → E_UNKNOWN_REPO    map hit in config; no path building
 2. ref syntax                  → E_INVALID_REF     regex + prefix + ".." (INV-3)
 3. input sanity                → E_BAD_INPUT       ≤64 keys, identifier-shaped, ≤4 KB each
 4. ref policy — conservative   → E_INVALID_REF     no expansion can match, approval disabled
 5. disk headroom               → E_DISK            agent floor = min(runner min_free_gb)

                              ─── first contact with the outside world ───
 6. mirror fetch                → E_SOURCE          per-repo mutex (§12)
 7. ref normalization           → E_INVALID_REF     exactly one of heads/tags must exist
                                → E_AMBIGUOUS_REF   both exist — never guess
 9. resolve to SHA              → E_SOURCE          ${REF}^{commit} — before 8, see below
 8. ref policy — authoritative  → awaiting_approval glob vs normalized name, and the
                                                    approved commit, if any (INV-5)
10. workspace clone + checkout  → E_SOURCE          hardlinked local clone at that SHA (§12)
11. read .latem/jobs.yaml       → E_NO_REGISTRY / E_BAD_REGISTRY
12. job lookup                  → E_UNKNOWN_JOB
13. label match                 → E_NO_RUNNER       runner labels ⊇ job.runs_on
14. input validation — typed    → E_BAD_INPUT       declared, coerced, bounded
15. secret resolution           → E_MISSING_SECRET  all declared names resolve non-empty
16. concurrency group           → queued            wait, do not fail
17. runner disk headroom        → E_DISK            selected runner's own min_free_gb
18. cache mount prep            → E_CACHE           volume create + flock (§14.3)
19. execute
```

Gates 1–5 run **before** any network or filesystem activity, so a hostile request costs a few microseconds and a rejected row. Gate 6 is the first action that touches the outside world.

**Gates 8 and 9 run in the order shown**, which is not their numbering (amendment A27). Gate 8 is the gate that parks a run for a human, and a park has to name the commit being approved — approval is approval *of a commit* (INV-5), so parking before resolving produces a run nobody can approve. Gate 9 is a `git rev-parse` against the mirror gate 6 has already fetched: a local read, no clone, no execution, and nothing from the commit is trusted or even looked at. The numbering is left alone because the codes and the prose elsewhere refer to it.

**Gate 10 checks out the SHA gate 8 admitted**, rather than resolving the ref a second time. Two resolutions are two answers, and the interval between them is exactly the interval an approval is supposed to close.

**Gate 4, the conservative pre-check.** The ref is not yet known to be a branch or a tag, so expand it both ways — `refs/heads/<ref>` and `refs/tags/<ref>`, plus the literal value if it is already fully qualified — and test all candidates against the policy globs. If *none* can match:

- when the repo sets `require_approval_otherwise = false`, no fetch can change the answer and the run is rejected with `E_INVALID_REF`, having touched nothing;
- otherwise the run is **queued**, and gate 8 decides once gate 9 knows the commit.

Draft 02 had this gate park the run instead, and amendment A27 removed that. A run parked here carries no `sha` — nothing has been fetched, so the ref has not been resolved and cannot be — and `latem approve` binds an approval to a commit. The invariant's second half was therefore unreachable from its first: the runs the cheap gate parked were precisely the ones no approval could release. The cost of deferring is one incremental mirror fetch and one `rev-parse` for an out-of-policy ref on a repo that permits approval. That is bounded by the runner pool and is strictly less than what the same repo already spends on a ref that *is* in policy, which is a full clone and a build.

If *any* expansion could match, proceed and let gate 8 decide for real against the one that actually exists. The check is sound in the direction that matters: it never lets something through that gate 8 would admit on its own.

**Gate 5 vs gate 17.** The runner is not selected until gate 13, so gate 5 can only enforce the weakest floor across enabled runners. Gate 17 re-checks the selected runner's own `min_free_gb` — the Tart runner's 150 GB is not a sensible bar for admitting a Docker job. Checking twice costs one `statfs` and prevents both a false rejection and a build that dies at 80% with a full disk.

---

## 11. Run state machine

```
                       ┌───────────────────┐
   gate 8 (only) ───▶  │ awaiting_approval │──── latem approve ────┐
   from preparing      └───────────────────┘     (pins the SHA)    │
                                 │                                 │
                    approval_ttl │                                 │
                                 ▼                                 ▼
  queued ── claim ──▶ claimed ──────────────────────▶ preparing ──────▶ running ──▶ collecting ──▶ succeeded
     │                   │                               │                 │            │
     │ queue_ttl         │ cancel                        │ fail            │            └──▶ failed
     ▼                   ▼                               ▼                 ├──▶ failed
  rejected ◀── gates 1-3,5 ──  cancelled              failed               ├──▶ timed_out
     ▲                                                                     ├──▶ cancelled
     └──────────── gates 6-18 reject/fail ─────────────────────────────────┴──▶ interrupted
```

Both non-terminal waiting states have an exit. Draft 01's `awaiting_approval` had none: a run nobody approved sat forever, and since §22's partial index deliberately covers only non-terminal states, every abandoned run stayed in the hot index permanently. `approval_ttl` and `queue_ttl` close that.

| State | Terminal | Meaning |
|---|---|---|
| `queued` | no | Accepted, waiting for a slot or a concurrency group |
| `awaiting_approval` | no | Ref outside policy (INV-5) |
| `claimed` | no | This runner owns it; nothing has executed |
| `preparing` | no | Fetch, checkout, registry read, cache mount, env build |
| `running` | no | Steps executing |
| `collecting` | no | Artifact globbing, cache flush, teardown |
| `succeeded` | yes | Every step exited 0 |
| `failed` | yes | A step exited non-zero, or a gate failed |
| `timed_out` | yes | Wall clock exceeded |
| `cancelled` | yes | Operator or broker cancellation |
| `rejected` | yes | Admission gate failure (§10), or an `approval_ttl` / `queue_ttl` expiry |
| `interrupted` | yes | Agent died mid-run; set during startup reconciliation |

**Rules.**

- Transitions are persisted to sqlite **before** being reported to the broker. A broker failure never loses local truth.
- `Report` is retried with backoff; the run does not block on it.
- `collecting` always runs, including after `failed`, `timed_out`, and `cancelled`, so artifacts and logs from a broken or stopped build survive. Only a hard agent kill skips it, and that path produces `interrupted`.
- Teardown is `defer`-registered at resource creation, so no path leaks a container or VM.
- **Cancellation.** A `cancel` command (§7.1) cancels the run's context. In `queued`, `awaiting_approval`, or `claimed` the effect is immediate — nothing has executed. In `preparing` or `running` it propagates to the executor, which signals the step's process group `SIGTERM`, waits 30s, then `SIGKILL`, then proceeds to `collecting`. In `collecting` it is ignored. Cancelling an unknown or already-terminal run is a no-op that reports success, so a phone tapping Stop twice is not an error.
- **TTL sweep.** The daily ticker and agent startup both sweep `awaiting_approval` older than `approval_ttl` and `queued` older than `queue_ttl` into `rejected`, with `E_APPROVAL_EXPIRED` and `E_QUEUE_EXPIRED`. Sweeping at startup matters because an agent that was off for a week comes back to a queue full of runs nobody wants any more.
- **Step timeouts** fail the run with `E_STEP_TIMEOUT` and terminal state `timed_out`, distinct from the job-level `E_TIMEOUT` only by error code. Both run `collecting`.
- **`preparing` → `queued` is legal**, and the diagram above does not draw it (amendment A14). Gate 16 sends a run whose concurrency group is occupied back to `queued` — "wait, do not fail" — and gates 6 through 18 all execute while the run is in `preparing`. A run can therefore reach `preparing`, discover its group is busy, and return to the queue. Any implementation validating transitions against the diagram alone will reject a legal one.
- **`preparing` → `awaiting_approval` is the only way into `awaiting_approval`** (amendments A17 and A27). Gate 8 is the only gate that parks a run, it runs during `preparing` because it needs a fetched mirror, and the edge had to be added for it (A17). Gate 4 no longer parks anything: a run parked before a commit is resolved cannot be approved (INV-5).
- **Entering `awaiting_approval` clears `approved_at` and `approved_sha`.** A run parked a second time — because its ref moved after it was approved — must not carry the earlier approval, or the next attempt would compare a fresh commit against a pin the operator has not been shown since. The clear happens in the same statement that records the new commit, so there is no window in which the row says "approved" about a commit nobody was asked about.

---

## 12. Source acquisition

**Mirror plus hardlinked clone.** A persistent bare mirror per repo, a disposable *self-contained clone* per run. Fetches are incremental, checkouts are clean, parallel runs share no working directory, and the workspace is a real git repository from any vantage point.

### Why not worktrees

Draft 01 specified `git worktree add`, which is the obvious choice and is wrong here for one specific reason. `git worktree` writes a `.git` **file**, not a directory, containing an absolute path:

```
gitdir: /Users/me/.latem/mirrors/api.git/worktrees/run_01H...
```

The executors bind-mount or share **only** the workspace. That path does not exist inside a container or a Tart VM, so every `git` command run by a build script fails with `not a git repository`. Build scripts call `git rev-parse HEAD`, `git describe --tags`, and `git status --porcelain` constantly — version stamping, build numbers, dirty-tree checks. This breaks on the first real repo, and the workarounds (mounting the mirror at an identical absolute path, splitting read-only and read-write mounts across the worktree admin directory) all end up with a different `.git` shape per executor.

A local clone is self-contained, costs almost nothing, and gives all three executors identical behaviour with one code path.

```
~/.latem/
  mirrors/<repo-key>.git/      # bare, persistent, --mirror
  work/<run-id>/               # full clone, deleted after collecting
```

```bash
# once per repo
git clone --mirror -- "$URL" "$MIRRORS/$KEY.git"
git -C "$MIRRORS/$KEY.git" config gc.auto 0

# per run
git -C "$MIRRORS/$KEY.git" fetch --prune --tags -- origin
SHA=$(git -C "$MIRRORS/$KEY.git" rev-parse --verify --end-of-options "${NORMALIZED_REF}^{commit}")

git clone --local --no-checkout -- "$MIRRORS/$KEY.git" "$WORK/$RUN_ID"
git -C "$WORK/$RUN_ID" checkout --detach --end-of-options "$SHA"
git -C "$WORK/$RUN_ID" remote set-url origin -- "$URL"
git -C "$WORK/$RUN_ID" config gc.auto 0
```

Note the two `--end-of-options` (amendment A7). Draft 02 wrote `--` on both of those lines and neither command would have run: to `checkout` and `rev-parse`, `--` announces a pathspec. `clone` and `remote set-url` genuinely do take paths and URLs, so they keep `--`.

### What this costs

`git clone --local` hardlinks the object store rather than copying it, so cloning a 2 GB repo writes a few hundred kilobytes of refs and index metadata plus the working tree — which a worktree checkout writes anyway. The saving worktrees offered over this was never the significant number.

Three requirements make the hardlinking safe and keep it fast:

- The mirror and `work/` must be on the **same filesystem**. Enforced at config load and reported by `latem doctor`; if they differ, git silently falls back to copying the whole object store and every run gets slower in a way nobody attributes to the right cause.
- `--no-checkout` then `checkout --detach` does exactly one checkout. Cloning without it checks out the mirror's default branch first and then the target — doubling I/O on large repos for nothing.
- Git objects are immutable, so a hardlinked object can be read by many runs at once. A `git gc` inside a run creates new packs and unlinks old paths, which only decrements a link count; the mirror is unaffected.

### Notes

- `${REF}^{commit}` fails loudly on annotated tags and non-commit refs rather than checking out something surprising. `$NORMALIZED_REF` is the fully-qualified name from gate 7 (§10), never the raw broker-supplied string.
- Every invocation passes argv as a slice with an options terminator before user-influenced arguments — `--` for paths, `--end-of-options` for revisions and refs (INV-3, amendment A7).
- **LFS needs a fetch in the workspace, not only in the mirror** (amendment A7). `git clone --local` hard-links `objects/` and nothing else, so `.git/lfs/objects` does not come across and the workspace starts with an empty LFS store; `git lfs checkout` alone will not populate it. The sequence is `git lfs fetch --all` in the mirror, then `git lfs fetch` **in the workspace while origin still points at the mirror**, then `git lfs checkout` — and only then `remote set-url`. Draft 02 omitted the middle step. This is untested against a real LFS repository and belongs in the M3 integration suite.
- `remote set-url origin` points the workspace at the real upstream, so `git fetch` inside a build behaves sensibly instead of pointing at a host path that does not exist in the container.
- **`gc.auto 0` on the mirror is not optional.** A force-push followed by `fetch --prune` can make the previously-fetched tip unreachable, and git's automatic gc is free to collect it — mid-run, 40 minutes into a build, on a repo the run already resolved. Disable automatic gc and run `git gc --prune=30.days.ago` from the daily ticker (§14.4) only when no run holds that mirror.
- Submodules: `git submodule update --init --recursive --depth 1` inside the workspace, only when `.gitmodules` exists.
- LFS: `git lfs fetch --all` in the mirror, `git lfs checkout` in the workspace, only when `.gitattributes` mentions LFS.
- A per-repo mutex serialises fetches into one mirror; concurrent `fetch` into a bare repo is not reliably safe. Clones out of the mirror are reads and need no lock, so parallel runs on the same repo do not queue behind each other.
- Credentials are the machine's own — SSH agent or a credential helper. The agent never stores or forwards git credentials, and receives two kinds, both held in memory only: a workflow run's job token (A52), which a checkout step makes the workspace's credential for that job, from a file outside the workspace (A53); and a read-only fetch token that comes with a plan or a job (A72), which the agent's own fetches into the mirror authenticate with, through one git process's environment.
- The agent exports `LATEM_SHA`, `LATEM_REF` (normalized), `LATEM_REPO`, `LATEM_RUN_ID`, `LATEM_JOB`, and `LATEM_BUILD_NUMBER` (§22's run counter, amendment A36) into the run environment regardless, so a job that wants the commit does not have to shell out to git at all.
- Startup reconciliation deletes `work/<run-id>` for any run id not in a non-terminal state. With clones there is no worktree administrative state in the mirror, so there is nothing to prune and nothing to leak — one of the smaller reasons this model is easier to make crash-safe.
- **An Xcode scheme must be committed, and `latem doctor` checks it** (amendment A34). This section guarantees a fresh clone for every run, which turns an uncommitted scheme from a machine quirk into a certainty: `xcodebuild -scheme X` fails with "does not contain a scheme named X" because Xcode auto-creates schemes into `xcuserdata/`, which nobody commits. It must be under `<project>.xcodeproj/xcshareddata/xcschemes/`. `doctor` reads it from the repo's own bare mirror rather than from a checkout, since between runs that is the only copy this machine has; before the first fetch it reports that it has nothing to read yet rather than passing.

---

## 13. Executors

```go
package executor

type Executor interface {
    // Prepare provisions an environment. Returned Env is used for all steps
    // in one run, then closed.
    Prepare(ctx context.Context, spec Spec) (Env, error)
    Name() string
    Preflight(ctx context.Context) error  // called at agent startup (§20)
}

type Env interface {
    // Exec runs one step. argv is NOT shell-parsed by the agent; steps[].run
    // is handed to the configured shell as a single argument.
    Exec(ctx context.Context, step Step, out io.Writer) (exitCode int, err error)
    // Fetch copies a path out of the environment (artifacts).
    Fetch(ctx context.Context, glob string) ([]File, error)
    Close(ctx context.Context) error   // MUST be idempotent
}

type Spec struct {
    RunID      string
    Workspace  string            // host path to the run workspace (§12)
    Image      string
    Env        map[string]string // includes secrets, registered with the redactor
    Caches     []CacheMount
    Labels     map[string]string // includes latem.run-id for reaping
    Resources  Resources
}

type Step struct {
    Name    string
    Run     string            // literal; handed to the shell as ONE argument
    Env     map[string]string // merged over Spec.Env for this step only
    Timeout time.Duration     // 0 means "remainder of the job budget"
    EnvFile string            // container-visible path exported as $LATEM_ENV
}
```

**Every `Env` implementation obeys the same step contract (§9).** A step is a new process: no working directory, exported variable, or shell function survives it. The filesystem persists for the run. `Exec` starts each step in the workspace root with `Spec.Env` overlaid by `Step.Env`, plus whatever previous steps wrote to `$LATEM_ENV`. Implementations that cannot honour this — by, say, reusing a persistent shell — are not conformant, because job authors would then get different behaviour per executor.

### 13.0 Choosing one

Two executors ship: Docker (§13.1) and native (§13.3). Both are choices on every OS that runs them — native is Linux and macOS today, Windows being the gap A47 names — and the spec ships a default per platform. Tart (§13.2) is specified and backlogged (A31, A47).

**The default for macOS is `native` (§13.3) — the host's own OS, no VM.** It requires nothing beyond a Mac that already has Xcode on it, adds no boot latency, and costs no disk. `latem init` writes it.

**One question decides whether that default is wrong for a given machine: whose code runs on it?**

If the answer is "repositories my team controls, and nothing else", native is correct and the rest of this section is a v2 concern. If the answer includes a fork, a pull request from outside the team, or any ref a contributor can move, native is not defensible and the machine needs `tart` (§13.2).

The reason is that native's blast radius is the operator's account. A step runs as the logged-in user with that user's keychain — which on a Mac mini doing iOS work is the machine holding the signing certificates and the App Store Connect key. INV-5's approval mechanism (§5, §10) still applies under native and still stops unapproved code from running at all, but it is the *only* thing standing there: once a commit is approved, native gives it everything. Under Tart the same commit gets a VM that is deleted afterwards. The invariant is unchanged; what changes is the cost of it being wrong once.

This is a deliberate reversal of draft 02, which made Tart the default and described native as a fallback. See amendment A31 for why the ordering was wrong: Tart is a ~69 GB pull and ~140 GB of provisioned disk to isolate against a threat that a single-team, self-hosted runner does not have, and making it the default meant every operator paid for a boundary most of them did not need before their first green build. The section numbers are unchanged — 54 code comments cite `§13.2` and `§13.3` by number, and renumbering to reflect precedence would be churn with real regression risk in comments for a purely cosmetic gain.

### 13.1 Docker executor

Target: Linux x86_64 (desktop) and Linux arm64 (the container role on an Apple silicon Mac).

- Workspace bind-mounted at `/workspace`, `rw`. Container `WORKDIR` is `/workspace`.
- Cache volumes mounted at the paths from `caches[].path` (§14).
- Label every container `latem.run-id=<id>` and `latem.managed=true`. Startup reconciliation depends on this.
- `--rm` is **not** used; the agent removes containers explicitly after `Fetch`, so artifacts remain retrievable if a step fails.
- Resources: `--cpus`, `--memory`, `--pids-limit 4096`.
- GPU: when `runner.docker.gpu = true`, add `DeviceRequests` for all GPUs (equivalent to `--gpus all`). Preflight must verify NVIDIA Container Toolkit by running `nvidia-smi` in a throwaway container; fail the runner at startup, not mid-build.
- Android emulator jobs need `--device /dev/kvm`; preflight checks it exists and is readable by the daemon's user (§18.2).
- Disk floor `min_free_gb` defaults to 40 for a docker runner — enough for a base image, a layer cache, and one build's intermediates.

**Step execution.** Each step is one `docker exec` into the run's container. Per §9, that means a **new process every step**: `cd` and `export` do not carry over, the filesystem does. The exec sets `WorkingDir` to `/workspace` and passes the full merged environment explicitly rather than relying on anything the previous exec did. `Step.Run` is passed to the configured shell as a single argument — `["bash", "-c", step.Run]` as an argv slice, never a concatenated string (INV-3).

**Base image contract.** The container image is the operator's, but three things are required of it, and `latem doctor` checks them by running the image once:

1. A non-root user `runner` with uid 1000 and gid 1000, and a **writable `$HOME` that exists**. Passing `--user` for a uid with no `/etc/passwd` entry leaves `$HOME` unset or unwritable, and the resulting failures surface as npm, Gradle, and CocoaPods errors that look like tooling bugs.
2. `bash`, `git`, and `ca-certificates` present. The agent shells out to none of these itself, but effectively every job does.
3. Every path named in `caches[]` either exists and is owned by uid 1000, or is absent so the agent can create it with the right ownership.

On Linux the agent additionally passes `--user $(id -u):$(id -g)` so bind-mounted workspace files are owned by the invoking user. On Docker Desktop for macOS, ownership is virtualised by the VM's filesystem layer and this flag is neither needed nor harmful — document both, because the difference is exactly the kind of thing that makes a job pass on one machine and fail on another.

### 13.2 Tart executor (macOS, ephemeral)

Target: a Mac building refs the team does not control. This is the isolation upgrade over §13.3, not the default — see §13.0 for which one a given machine wants.

```
Prepare:
  tart clone <base_image> latem-run-<run-id>
  tart set latem-run-<run-id> --cpu N --memory M
  tart run --no-graphics --dir=workspace:<host-workspace> latem-run-<run-id> &
  poll `tart ip latem-run-<run-id>` until routable or boot_timeout
  ssh -o StrictHostKeyChecking=accept-new <ssh_user>@<ip> true   # readiness

Exec:
  ssh … 'bash -lc <step>'    # step passed as a single argv element

Fetch:
  rsync from the shared dir, or scp by glob

Close:
  tart stop  → wait → tart delete   (both idempotent; delete even if stop fails)
```

- Apple's licence permits **two** guest VMs per host. `concurrency` for a tart runner MUST be `<= 2`, enforced by the config loader (§8). The reference config uses 1 for a different reason — parallel Xcode builds contend — but 2 is the licence ceiling, and §8's draft-01 comment pointing at §13.3 for this was a cross-reference error.
- Each step is a fresh `ssh` session, which gives the §9 step contract for free: no shell state survives, the VM's filesystem does.
- Caches cannot be Docker volumes here. Use host directories shared with `--dir`, one per cache name (§14.2) — with the exception below.
- The base image (`latem-xcode-*`) is built once by a documented Packer-style script in `examples/`: Xcode, command-line tools, Homebrew, Node, fastlane, CocoaPods. Rebuilding on Xcode upgrades is a documented operator task.
- Boot cost is real (30–60s). Acceptable against a 20-minute iOS build; do not use tart for short jobs.
- **Disk.** Draft 02's "on the order of 90 GB" was a single number standing in for two that differ by a factor of two, and it was wrong in both directions. Measured against `cirruslabs/macos-tahoe-xcode:latest`:

  | Quantity | Measured | What it costs you |
  |---|---|---|
  | Compressed download | **68.8 GB** (261 layers) | One-time, per image version, over your network |
  | Provisioned disk | **140 GB** (`org.cirruslabs.tart.uncompressed-disk-size`) | Permanent, on the runner |
  | Base image, no Xcode | 27.1 GB | The floor if you build your own |

  The 113 GB between the base image and the full one is mostly simulator runtimes — iOS 18.5 is 19 GB and iOS 26.0 is 15 GB installed, against 4.6 GB for Xcode.app itself. An image carrying one runtime instead of four is dramatically smaller, which is the main lever if the number matters to you.

  `tart clone` is an APFS copy-on-write clone, so cloning is fast and initially near-free, but a running VM diverges and DerivedData is tens of gigabytes on its own. `min_free_gb` therefore defaults to **200**, not draft 02's 150: that figure was derived from the wrong 90 GB and left under 10 GB of headroom above a single provisioned image. `latem doctor` reports the base image's real size rather than assuming any of this.

  If the download rather than the disk is what you object to, `tart create --from-ipsw` builds the image locally from a ~15 GB restore image and you install Xcode into it yourself. Same end state, a fifth of the transfer, several hours of your time.

#### The DerivedData question — measure before implementing the tart executor

`--dir` is VirtioFS, and Xcode's DerivedData over VirtioFS is materially slower than the VM's own disk. §18.1 asserted DerivedData was "the largest single win" from shared cache directories. Both things cannot be true: if the share is slow enough, a warm cache over VirtioFS loses to a cold build on local disk, and the iOS caching story inverts.

**This no longer blocks M5.** It blocks the tart executor, and only that. Native is the default (§13.0) and has no VirtioFS anywhere in it — the workspace is the clone and DerivedData sits on local APFS — so a machine running the default never meets this question. Draft 02 put this measurement on the critical path because it assumed every macOS deployment was a VM.

**Half of it is now measured.** The native baseline, on an M4 Max with DerivedData on local APFS, building a 900-file SwiftUI/UIKit target for the iOS simulator:

| | Wall clock |
|---|---|
| Cold (DerivedData removed) | **13.79 s** |
| Warm (one file touched) | **4.97 s** (4.91 / 5.05 / 4.96) |

DerivedData was 651 MB across 13,975 files. The cache is worth **2.8×** on this workload, which is the number the remaining measurement has to beat:

> **VirtioFS must be less than 2.8× slower than local APFS, or `derived_data = "shared"` is the wrong default.**

Stated that way it is one comparison rather than draft 02's three-way table, and it can be run on any Mac with Tart installed rather than on the target hardware specifically — the ratio is what matters, not the absolute times. The harness is a generated Swift project rather than a real app, chosen so the result is reproducible and not a statement about one codebase's module graph.

`derived_data = "vm" | "shared"` (§8) exists so the answer is a config value rather than a rewrite. If `"vm"` wins, keeping DerivedData warm across runs additionally requires snapshotting the VM rather than cloning from a clean base — a real scope change to this section, and still worth knowing before the tart executor is written rather than during it.

### 13.3 Native executor (macOS, no VM)

**The default macOS executor** (§13.0). The host's own macOS, running steps as the logged-in user. Correct when this machine builds only refs your team controls; wrong when it does not, and §13.0 is the decision.

Being the default raises the stakes on everything below rather than lowering them. Draft 02 wrote these as caveats on a fallback nobody was expected to run; they are now the operating conditions of the shipped configuration, and every one of them is a `latem doctor` check for that reason.

- Runs directly on the host as the logged-in user. **There is no isolation of any kind** — no filesystem, network, process, or resource boundary. A job's `run` can read `~/.ssh`, delete `~/Library`, install a launch agent, or use every core. On a Mac doing iOS work that includes the keychain holding the signing certificates. This is not a caveat to be softened in the README; it is the thing the operator is agreeing to, and §13.0 states the criterion for when agreeing is reasonable.
- `concurrency` MUST be 1. Parallel Xcode builds contend on DerivedData and the keychain and produce failures that look like code bugs. This is the one respect in which the default is *less* capable than Tart, which permits 2 — a machine that needs macOS throughput needs §13.2, or a second machine.
- **Nothing resets between runs.** A VM is cloned from a base image and is identical every time; this host accumulates — Homebrew upgrades, keychain state, a `xcode-select` someone changed weeks ago, files a previous job left outside the workspace. §12 makes the *workspace* clean per run, and that is the boundary: everything outside it persists. A build that passes here and fails elsewhere is usually this.
- Requires a **LaunchAgent under an auto-login user**, not a LaunchDaemon. `codesign` needs a real user session with keychain access; daemon contexts fail with errors that point nowhere useful. This is the single most common install failure — `latem doctor` must detect and name it.
- Requires **auto-login enabled**, and auto-login is defeated by FileVault. See §23 — this interacts badly enough with "survives reboots" that it is a `latem doctor` check, not a footnote.
- Every step runs `bash -lc` as a **new process**, so the login environment (Homebrew paths, rbenv, nvm) is present and, per §9, nothing else carries between steps.
- **`LANG=en_US.UTF-8` is set in the step's environment** (§9, amendment A33), below the run environment so a job can override it. The hole this leaves is worth stating: `-lc` is a *login* shell, and a profile runs after execve, so an `export LC_ALL=C` in `~/.bash_profile` still wins. `latem doctor` therefore probes the locale by running this same shell the same way rather than by reading the executor's code — the only answer that accounts for the profile is the one the shell gives.
- **The runner's `[runner.native]` `env` and `path_prepend`** (amendment A60) lie beneath the run environment, with `LANG`: `env` is set unless the job or step sets the name, and `path_prepend` goes in front of the PATH the step inherits from the agent — a job that sets PATH replaces both, and `$GITHUB_PATH` additions go in front of them. They say where a toolchain lives that launchd's PATH does not reach, which on a Mac is every keg-only Homebrew install.
- Workspace is the run's clone directly; no copy. Because §12 produces a self-contained repository, the workspace looks identical here and inside a container.
- Disk floor defaults to 60 GB.

---

## 14. Cache subsystem

The design point: hosted CI needs save/restore because its machines are ephemeral. Ours are not. Caches are **mounted**, not archived. There is no tarball, no upload, no size ceiling, and no cache step in the job.

### 14.1 Docker caches

One named volume per (runner, cache):

```
latem-cache-<runner-name>-<cache-name>
```

Scoping by runner rather than by repo is deliberate: package stores are content-addressed and benefit from sharing across repos, while per-runner scoping avoids the concurrency hazard below without a lock in the hot path.

### 14.2 Tart / native caches

Host directories, shared into the VM with `--dir`, or used in place for native:

```
~/.latem/caches/<runner-name>/<cache-name>/
```

### 14.3 Concurrency

Two runs writing one `~/.gradle` or `~/.cargo` corrupt each other, and the resulting failures are confusing and intermittent. Required behaviour:

- Runners never share a cache volume — the name embeds the runner.
- Within a runner, concurrency > 1 requires per-cache `flock` on `<cache-root>/.latem-lock`, acquired during `preparing` and released at `collecting`.
- Lock acquisition has a 10-minute timeout, after which the run fails `E_CACHE` rather than hanging forever.

### 14.4 Pruning

`latem cache prune`, plus an internal daily ticker:

- Docker build layers: `docker buildx prune --filter until=168h --force`.
- Cache volumes exceeding `max_size` (default 25 GB per volume): evict by atime, oldest first, down to 80% of the cap.
- Workspaces under `work/` older than 24h with no live run: delete. Nothing else to clean — §12's clones leave no administrative state behind in the mirror.
- Mirror maintenance: `git gc --prune=30.days.ago` per mirror, skipped for any mirror a live run is reading, since automatic gc is disabled there (§12).
- Tart: `tart list` for VMs matching `latem-run-*` with no live run; delete. A leaked VM starts as a near-free copy-on-write clone and grows toward the image's provisioned 140 GB (§13.2) as the build writes, which is the fastest way to fill a Mac mini.
- Mirrors untouched for 90 days: report in `latem doctor`, never auto-delete (a mirror may be the only local copy).
- When free disk drops below the runner's `min_free_gb`, prune aggressively and refuse new runs with `E_DISK` rather than failing halfway through a build. The floor is **per runner**, not global — 40 GB for docker, 150 GB for tart, 60 GB for native (§13). Draft 01's single global 20 GB was sized for containers and would admit an iOS build onto a machine that cannot hold one.

---

## 15. Secrets

Resolution chain, first hit wins. Only names declared in the job's `secrets:` block are looked up.

1. `LATEM_SECRET_<NAME>` in the agent's own environment
2. `~/.latem/secrets.env` — `KEY=value`, mode `0600` enforced, agent refuses to start otherwise
3. macOS Keychain — `security find-generic-password -s latem -a <NAME> -g`
4. `pass` / `gopass`, if configured
5. A shell command per secret, defined in config

```toml
[secrets.commands]
# An argv array, not a string (amendment A15).
MATCH_PASSWORD = ["op", "read", "op://Private/match/password"]
```

**Commands are argv arrays.** Draft 02 wrote them as shell-ish strings while INV-3 requires an argv slice and forbids `sh -c`, and never said how one becomes the other — leaving every implementation to write its own tokeniser and its own answer to what `"op read 'my vault/pw'"` means. An array deletes the question: the operator writes the split themselves, quoting is TOML's problem, and there is no layer that could accidentally interpret a metacharacter.

A string form remains accepted for one release, tokenised by shell *quoting* rules with no shell *semantics* — any of `| & ; < > ( ) $ \` newline`, a leading `~`, or `$`/backtick inside double quotes is rejected at config load rather than passed through as a literal that looks like it worked.

Draft 02's `[secrets] provider = "chain"` key is **removed**: §15 defines exactly one legal value, so the key could only ever be set to its default or be wrong.

Rules:

- **Secret values are `[]byte`, not `string`, everywhere inside `internal/secrets`.** Draft 01 promised values "zeroed on run completion where the language allows," which Go does not: a `string` is immutable and lives until the GC decides otherwise, so the promise was unenforceable as written. Holding `[]byte` makes it real — the buffer is explicitly zeroed when the run reaches a terminal state. Conversion to `string` happens exactly once, at the last moment, when building the executor's environment map, and that map is dropped with the run.
- Registered with the redactor (§16) **before** the first step starts, and before the value is used for anything else.
- Never written to sqlite, never included in `Update.ErrorMsg`, never sent to a broker (INV-4).
- A declared secret that resolves empty is `E_MISSING_SECRET` at admission, not a runtime surprise 40 minutes in.
- `latem doctor --secrets` reports resolvable/unresolvable **names only**, never values.
- `latem secrets set [--repo R] [--env E] NAME` writes a value where links 2 and 3 read it, under the scoped name the run's lookup builds (A50, A54); the value arrives on stdin or at a prompt that does not echo, never in argv. `latem secrets list` prints names and scopes and reads no value.

---

## 16. Log pipeline and redaction

```
step stdout/stderr
   → line splitter (64 KB max line; longer lines truncated with a marker)
   → REDACTOR  ← every secret value, every secret LINE, every token, every pattern
   → sequencer (assigns run-scoped monotonic seq, from 1)
   → fan-out ─┬─ local file  ~/.latem/logs/<run-id>.log
              ├─ broker LogSink (batched: 100 frames or 250 ms)
              └─ `latem logs -f` subscribers (ring buffer, 1000 lines)
```

Redaction is applied **once, at the point of emission**, before sequencing and before fan-out. A new sink therefore cannot bypass it (INV-4), and a `LogFrame` that exists at all is already safe to transmit.

Sequence numbers are assigned after redaction and before fan-out, so the local file, the broker, and a `-f` subscriber all agree on numbering. `LogFrame.Seq` is what makes resumption after a partition non-duplicating (§7.2).

### Multi-line secrets

The pipeline splits into lines and then redacts each line. It follows that registering only the whole value of a multi-line secret redacts **nothing** — the whole value never appears in any single line. Draft 01 did exactly this while also declaring `APP_STORE_CONNECT_KEY_P8` in its own example job, and listing a `-----BEGIN [A-Z ]*PRIVATE KEY-----` pattern that matches the header line and lets every base64 body line through. `echo "$APP_STORE_CONNECT_KEY_P8"` would have printed a usable App Store Connect key into the log file and shipped it to the control plane, with the INV-4 test passing.

Normative rule. When a secret value is registered, the redactor registers:

1. The **whole value**, verbatim.
2. If the value contains a newline: **every non-empty line of it**, individually, after trimming trailing whitespace.
3. The value with all whitespace stripped — tooling frequently re-emits PEMs as one long line.
4. The base64 and URL-encoded forms of items 1 and 3.

Items 2 and 3 are what draft 01 was missing. Lines shorter than the minimum length below are skipped, which for a PEM means the `-----BEGIN`/`-----END` delimiters are handled by the always-on pattern list rather than by value matching.

### Redactor contents

- Every resolved secret value, expanded per the four rules above.
- The machine token, and the contents of `CancelRequest.Reason` are redacted before that string is logged or persisted. The token is **not** a declared secret — nothing in `internal/secrets` resolves it, because it is the credential that authenticates the machine rather than anything a job asks for — so the agent registers it explicitly, at startup, with both the store's redactor and every run's (amendment A28). Registering only declared secrets left it unregistered entirely, and a build that printed the file wrote the token to `~/.latem/logs/<run-id>.log` and shipped it to the control plane over the link it authenticates. A rotation takes effect on the transport at the next reconnect and on redaction at the next restart; the previous value stays registered, which is the safe direction.
- Always-on patterns: AWS keys `AKIA[0-9A-Z]{16}`, PEM delimiters `-----BEGIN [A-Z ]*PRIVATE KEY-----`, GitHub tokens `gh[pousr]_[A-Za-z0-9]{36,}`, Slack tokens `xox[baprs]-`, JWTs `eyJ[A-Za-z0-9_-]{10,}\.`.
- Replacement is `***` plus a stable 6-char hash of the matched value, so `***a3f9c1` lets an operator correlate occurrences without learning the secret. The hash is over the value, so the same secret reads the same everywhere.
- **Values shorter than 6 characters are not redacted** — matching them produces log soup where every occurrence of `true` or `dev` becomes `***`. Warn at config load when a declared secret resolves to a short value, naming the secret and not the value.
- The matcher is a single Aho-Corasick pass over the literal set plus one compiled alternation for the patterns. A per-line loop over N secrets is O(N × line length) and shows up as measurable overhead on verbose Gradle output, which is precisely the log volume this has to survive.

Retention: `agent.log_retention` (default 14d), enforced by the daily ticker. Logs are gzipped after 24h.

---

## 17. Artifacts

- Globs are relative to the workspace root and MUST NOT escape it. Resolve, then verify the result is within the workspace; reject symlinks pointing outside (`E_ARTIFACT`).
- **Glob semantics match the ref policy globs of §5**, so a job author learns one set of rules: `*` matches within a single path segment and does not cross a `/`; `**` matches zero or more whole segments.
- **A declaration naming a directory is `E_BAD_REGISTRY`** (amendment A30). `artifacts: [dist]` is refused, and the message names the two spellings that work — `dist/**` for everything beneath, `dist/*` for the immediate children.

  Both silent alternatives are worse than failing. Expanding a directory is unbounded against the artifact budget below, which is evicted oldest-first — so one `artifacts: [dist]` pointed at a directory of build intermediates quietly deletes *other runs'* artifacts, and the cost lands somewhere the author never looks. Matching nothing leaves an operator believing a release was collected when it was not. The author's intent is unknowable from `dist` alone; asking, in seconds, beats guessing.

  This applies only to a **literal path**. A pattern that matches a directory simply does not collect it — a directory is not an artifact — which is what makes `*` stop at one segment.
- Copied to `~/.latem/artifacts/<run-id>/`, preserving relative paths.
- Retention per declaration, enforced daily.
- Only `ArtifactRef{name, size, sha256}` is reported to a broker. Content never crosses the boundary (§5).
- Large-artifact hygiene: `.ipa` and `.aab` files are 50–300 MB. Default total artifact budget 50 GB, evicted oldest-first, surfaced in `latem doctor`.

---

## 18. Mobile build pipeline

The wedge feature. Everything here lives in the repo's own `ci/` directory — the agent has no mobile-specific code. `examples/expo-app/` is the reference implementation and should be treated as part of the deliverable.

### 18.1 iOS

Runner: `ios` (native by default, concurrency 1 — §13.0). Nothing below changes if that machine is switched to `tart`: the job matches on labels, and both blocks in §8 carry the same ones.

> **This section describes the Expo pipeline, which has never run** — see §18.3
> and amendment A32. The pipeline that *has* run end to end, to a TestFlight
> build installed on a physical device, is `examples/ios-native/`: plain
> `xcodebuild archive` → `-exportArchive` → `fastlane pilot upload`, no Expo and
> no hosted account. Where the two disagree, the one that ran wins.

**Credentials via fastlane match.** Certificates and profiles live encrypted in a private repo the operator controls. The agent supplies `MATCH_PASSWORD`; nothing else about signing touches the agent, and nothing at all touches a control plane.

```bash
# ci/ios/unlock-keychain.sh
security unlock-keychain -p "$KEYCHAIN_PASSWORD" "$HOME/Library/Keychains/login.keychain-db"
security set-keychain-settings -t 3600 -u "$HOME/Library/Keychains/login.keychain-db"
```

```bash
# ci/ios/match.sh
bundle exec fastlane match appstore --readonly
```

```bash
# ci/ios/build.sh
npx eas build --platform ios --profile production --local --non-interactive \
  --output "build/app.ipa"
bundle exec fastlane pilot upload --ipa build/app.ipa \
  --changelog "$RELEASE_NOTES"
```

Required environment: `MATCH_PASSWORD`, `MATCH_GIT_URL`, `APP_STORE_CONNECT_KEY_ID`, `APP_STORE_CONNECT_ISSUER_ID`, `APP_STORE_CONNECT_KEY_P8`, `KEYCHAIN_PASSWORD`.

Caches: `cocoapods` and `pnpm` unconditionally — both are content-addressed, cheap to share, and unambiguously a win.

`derived-data` depends on the executor, and under the default it is no longer a question. **Native** mounts nothing: the cache is a host directory on local APFS (§14.2), which is the configuration the §13.2 baseline measured at **2.8× faster warm than cold**, so it is on and there is no trade-off to weigh. **Tart** puts it behind VirtioFS, which is where draft 01's claim that DerivedData was "the largest single win" collides with the share possibly being slower than the win — that is `runner.tart.derived_data`, and §13.2 requires it be settled by measurement before the tart executor is written. Do not design around either answer for Tart until that number exists.

**The build number comes from the run, not the project** (amendment A36). The agent exports `LATEM_BUILD_NUMBER` into every step — the run's own number from §22 — and a release job passes it as `CURRENT_PROJECT_VERSION`. A project that keeps its build number in the `.xcodeproj` uploads once and then fails forever, because App Store Connect refuses a build number it has already accepted for a marketing version, and it refuses it *after* the archive, the export and the upload.

Two things an operator needs to know before the first upload, because both fail at the end of a long build rather than the start:

- **It counts runs on one machine.** Two agents building the same app both count from 1. Release from one machine, or set the value in the job's own `steps[].env`, which overrides the agent's (§9).
- **It goes down, not up, on an app that has already uploaded time-based numbers.** A machine that uploaded build `1786501043` cannot then upload build `7`: Apple wants the newest build in a train to be the highest. Bump the marketing version, or override for that app until its version changes. This is the one-time cost of moving from a value that was unique to a value that means something.

**Known failure modes to document in `docs/ios.md`:**

| Symptom | Cause |
|---|---|
| `The build number must be higher than the previously uploaded build` | The app already carries a larger, time-based build number; see above |
| `User interaction is not allowed` | Keychain locked, or running under a LaunchDaemon rather than a LaunchAgent |
| `No signing certificate "iOS Distribution" found` | `match` not run, or wrong `MATCH_GIT_URL` |
| Build hangs at the end | `--non-interactive` missing; EAS is waiting on a prompt |
| `xcrun: error: SDK "iphoneos" cannot be located` | Xcode CLT selected instead of full Xcode; `xcode-select -s` |

### 18.2 Android

Runner: `linux-x64` (docker) or the GPU box.

```bash
# ci/android/build.sh
npx eas build --platform android --profile production --local --non-interactive \
  --output build/app.aab
bundle exec fastlane supply --aab build/app.aab --track internal
```

Required: `ANDROID_KEYSTORE_B64`, `ANDROID_KEYSTORE_PASSWORD`, `ANDROID_KEY_ALIAS`, `ANDROID_KEY_PASSWORD`, `PLAY_SERVICE_ACCOUNT_JSON`.

Caches: `gradle` (largest win), `pnpm`, and the Android SDK baked into the image rather than cached.

Emulator tests need `/dev/kvm` — add `--device /dev/kvm` and confirm in preflight. This is a capability the hosted services largely do not offer, and worth calling out in the README.

### 18.3 What depending on `--local` actually costs

Draft 02 titled this section "Why `--local` is safe to depend on" and argued: it is a supported Expo flag running the same pipeline locally, the runner invokes it as an ordinary command, there is no reverse engineering and no private API, and if Expo changes the flag the blast radius is one shell script in `ci/` rather than the agent.

**All of that is true and it answered the wrong question.** Running it found two exposures the argument does not cover (amendment A32):

1. **`eas build --local` requires an Expo account.** It fails before compiling anything with `EAS project not configured. Must configure EAS project by running 'eas init'`, and `eas init` requires a login. So a "local" build has a hosted dependency and a login the operator must hold — on a runner whose entire pitch is that it works with no account. That is not a flag-compatibility risk, which is what the original argument bounded; it is a second service in the critical path of every iOS build.
2. **The Expo SDK constrains the Xcode version.** SDK 57.0.12 does not compile against Swift 6.2 (Xcode 26.0.1): 31 errors inside `expo-modules-jsi`, in three distinct classes of Swift 6 strictness. Xcode on a macOS runner is a shared, slow-moving, operator-managed resource — §13.3's whole point is that nothing resets it between runs — so "upgrade Xcode" and "upgrade the SDK" are coupled in a way a single `ci/` script cannot absorb.

Neither makes Expo a wrong choice for a repository that already uses it. Both mean **the mobile story cannot rest on Expo alone**, so §18 now carries two reference examples:

| Example | Builds today | Needs an account | Toolchain surface |
|---|---|---|---|
| `examples/ios-native/` | **yes** — 20.2 s to a simulator `.app`, archives to arm64 | no | `xcodebuild` + `fastlane` |
| `examples/expo-app/` | no — blocked on the SDK/Xcode incompatibility above | yes, an Expo account | `xcodebuild` + `fastlane` + Node + pnpm + CocoaPods + EAS |

`examples/ios-native/` is the one to bring a new Mac runner up on, because it isolates the question. It needs no Apple credential either — a simulator build is unsigned — so it answers "is this machine configured correctly?" without waiting on Apple credential lead times. If it passes and the signed job fails, the problem is credentials, not the machine.

The original argument survives for what it did cover: the runner still invokes `eas` as an ordinary command, and there is still no private API anywhere in this.

---

## 19. Local trigger and CLI

```
latem setup [--url U] [--repo owner/name …] [--no-register] [--no-service]   config, repositories, register, service, doctor; idempotent (A54, A57)
latem agent                          Run the daemon (foreground; service wrappers in §23)
latem run <repo> <job> [--ref R] [--input k=v] [--follow] [--wait]
latem list [--state S] [--repo R]
latem logs <run-id> [-f]
latem cancel <run-id>
latem approve <run-id>               Local-only; pins the run's commit (INV-5)
latem register [--url U] [--token-file F]   Write config + machine token, verify connectivity; no token: approve in the browser (A54)
latem service reload                 Apply config.toml to the running agent (SIGHUP, A54)
latem repo add <owner/name> | list | remove <key>   Edit [repos.*] and reload (A54)
latem secrets set | list | rm [--repo R] [--env E] NAME   Values from stdin or a hidden prompt (A54)
latem vars set | list | rm --repo R [--env E] NAME [VALUE]   Edit [repos.*.vars] and reload (A54)
latem workflows check [path] [--repo R] [--offline] [--json] [-v] [--fix]   Plan and compile a checkout's workflows, or with --repo and no path a repository's, read without a checkout, and say what each job needs (A54, A57, A59)
latem store check [path | --repo R] [--env E] [--app D] [--bundle-id B] [--package P] [--track T] [--platform ios|android] [--offline] [--json]   Whether a preview would reach testers from this machine, and the fix for what would stop it; read-only (A65)
latem doctor [--secrets] [--fix]     Preflight, diagnose, name the fix
latem cache ls | prune | rm <name>
latem update [--check]               Install the latest release; the agent moves onto it when idle (A56)
latem version
```

**`latem doctor` is a first-class deliverable, not a nice-to-have.** It is what makes G6 achievable and what turns the failure table in §18.1 into a self-service fix. It must check, and for each failure print the exact command to run:

- git, docker, tart availability and versions
- Docker daemon reachable; current user in the `docker` group
- **`docker` and the agent reach one daemon**, compared by daemon id: the agent uses `DOCKER_HOST` or the system socket and ignores `docker context`, which Docker Desktop moves, so a step's `docker` command can be asking another daemon. A warning, because a job that does not call `docker` is unaffected
- Docker base image satisfies the §13.1 contract — runs the image once and checks uid 1000 exists, `$HOME` is writable, and `bash`/`git` are present
- NVIDIA toolkit functional when `gpu = true`; `/dev/kvm` present and readable when an emulator job is configured
- macOS: LaunchAgent vs LaunchDaemon context, auto-login enabled, **FileVault status** (§23), keychain unlockable, Xcode selected, licence accepted
- Tart base image exists, and its actual on-disk size versus `min_free_gb`
- **Mirrors and `work_dir` are on the same filesystem** — if not, §12's clones silently stop hardlinking and every run gets slower for no visible reason
- Every configured repo reachable with current credentials. One this machine's login cannot read is a warning rather than a failure when it is an https GitHub repository and a control plane is configured, because jobs bring a token for it (A72)
- **A GitHub login the agent cannot reach at boot**, on Linux (A72): `gh` keeps its login in a desktop keyring, which stays locked until someone logs in, and the agent starts before that. doctor runs in the session with the keyring open, so the repo check above passes on the machine this is about
- Every declared secret resolvable (names only), with a warning for any resolving shorter than 6 characters (§16)
- **App Store Connect credential *agreement*, not presence** (amendment A35). Under `--secrets`, and only when all three of `APP_STORE_CONNECT_KEY_ID`, `APP_STORE_CONNECT_ISSUER_ID` and `APP_STORE_CONNECT_KEY_P8` resolve: the key id's shape, the issuer id being a UUID rather than a key id pasted into the wrong field, the `.p8` parsing as EC P-256, whether the key material fingerprints to an `AuthKey_*.p8` on this machine whose name *disagrees* with the key id, and finally a read-only `GET /v1/apps` with both token forms so Apple can say which kind of key this is. Every one of those failures is the same 401 from `fastlane pilot upload`, two minutes into an upload, after a full build. Diagnostics print key ids, fingerprints and bundle ids and never key material (INV-4)
- **The locale a step's shell resolves to**, on macOS runners (amendment A33), probed through the same `bash -lc` a step gets
- **A committed shared Xcode scheme**, per repo with a mirror, on macOS runners (amendment A34)
- Disk headroom against each runner's own `min_free_gb`
- `~/.latem` is mode `0700`; `token_file` and `secrets.env` are `0600`
- `metrics_addr` is loopback (INV-1)
- Clock skew against the broker (token validation fails confusingly otherwise)
- **The release feed, when and only when `auto_update` is on** (amendment A41). It is the one setting whose failure mode is silence: everything it needs is off the machine, and when any of it is unreachable the agent does the correct thing — warns, once a day, into a log nobody is reading. The config loaded, the value was accepted, and nothing happens for months. doctor therefore asks the question the agent's ticker will ask, through the same code, at the moment somebody is watching the answer. Measured on this repository: a **private** repo answers 404 to `releases/latest`, because the agent sends no credential to the feed and holds none — so a machine with `auto_update = true` on a private repo never updates and never says why

---

## 20. Health, heartbeat, self-update

**Heartbeat** every 15s carrying `MachineHealth`. The control plane marks a machine offline after 3 missed beats (45s). Heartbeats continue while draining, with `Draining: true`.

**Preflight** runs at startup, per executor, and every 5 minutes thereafter. A runner failing preflight is marked unavailable and stops accepting runs; recovery is automatic when the check passes again. Never accept a run onto a runner that cannot execute it.

**Self-update** is opt-in and off by default (`[agent] auto_update = false`). When enabled: check a release feed daily, verify a minisign signature, download to a temp path, atomic rename, re-exec only when no run is active. Never update mid-run.

The five things that turn that sentence into something implementable, each recorded here because a reimplementation needs all five and none of them is obvious (amendment A41):

1. **What is signed is the checksums manifest, not the binary.** One signature covers every platform's archive, and the manifest binds each archive to a filename that contains the version. The consequence is the useful part: the *release feed does not have to be trusted*. It supplies a version string, and every byte that reaches disk is covered by a signature from a key compiled into the running binary. A hostile feed can deny service or offer a version this key signed, and nothing else.
2. **A downgrade is refused.** An update must be strictly newer than the running binary, compared as `vMAJOR.MINOR.PATCH` with semver's prerelease ordering. Without this, replaying an old and perfectly genuine release is an attack.
3. **A relabelled release is refused** — v0.1.0's real signed manifest, republished under a tag claiming v0.9.0 — because the asset name is derived from the claimed version and looked up *inside* the signed manifest. `latem_0.9.0_darwin_arm64.tar.gz` is not a line anyone but the signer can produce. The version is bound by signed content rather than by the feed's word for it.
4. **The downloaded binary is asked what it is** before it replaces anything. The signature proves who built it; it says nothing about whether the release was assembled correctly. An archive built from the wrong tag, or published under the wrong platform's name, is signed exactly as faithfully as a correct one.
5. **"No run active" is arranged, not observed.** Checking that nothing is in flight and then re-execing is a race with a worker about to claim, and losing it orphans a build — which §6's reconciliation then correctly marks `interrupted` and reaps. So the agent *stops claiming first*, then waits for idle, with two constraints: the pause must be reversible (`draining` is a one-way latch whose only caller is shutdown, and a machine that failed to download an update must go back to work), and the wait must be bounded. Ten minutes: this is capacity given up for an update rather than for a shutdown, and an update that does not happen today happens tomorrow.

**A build that trusts no signing key refuses `auto_update = true` at load.** The signing key is embedded at build time, so this is a property of the binary rather than of the machine: a `go build` from a tree whose `internal/selfupdate/trusted.pub` is still the placeholder can verify nothing, and would check a feed daily and decline every release into a log nobody reads. That is the same decision A27 made about an approval that recorded nothing and A38 made about a documented `~` no executor expanded — **a control that reads as a control and does nothing is worse than an absent one**, because it stops the operator looking. The refusal is narrow: it is keyed on the key, not on the calendar, so a release binary accepts the value and a hand-built one does not.

**By hand, and the notice.** `latem update` is the same chain run once, on request, against the binary the command runs from; SIGUSR2 then moves the running agent onto it without cancelling a run. Every other command says once a day, on a terminal, when a newer release exists. A build from commits after a tag compares as newer than that tag. Amendment A56.

**Metrics.** INV-1 forbids binding a non-loopback listener; it does not forbid loopback. `metrics_addr` (default `127.0.0.1:9838`, empty to disable) serves counters and gauges — runs by terminal state, run duration, queue depth, cache volume sizes, disk free per runner, broker connection state, log frames dropped. The config loader rejects a non-loopback value, so the invariant is enforced where it cannot be forgotten. Anyone wanting these off-machine points a local Prometheus at it or tunnels; the agent does not grow a remote surface for observability.

---

## 21. Error taxonomy

Stable codes; the web app maps them to human copy, and they appear in `Update.ErrorCode`.

| Code | State | Retryable | Meaning |
|---|---|---|---|
| `E_UNKNOWN_REPO` | rejected | no | Repo not in local config allowlist |
| `E_INVALID_REF` | rejected | no | Ref failed validation, or normalized to nothing (INV-3, §10 gates 2 and 7) |
| `E_AMBIGUOUS_REF` | rejected | no | Both `refs/heads/<ref>` and `refs/tags/<ref>` exist; the agent will not guess |
| `E_UNKNOWN_JOB` | rejected | no | Job absent from the registry at that commit |
| `E_NO_RUNNER` | rejected | no | No runner carries all `runs_on` labels |
| `E_BAD_INPUT` | rejected | no | Undeclared input, or a type/bound violation |
| `E_MISSING_SECRET` | rejected | no | Declared secret did not resolve |
| `E_NO_REGISTRY` | failed | no | `.latem/jobs.yaml` missing at that commit |
| `E_BAD_REGISTRY` | failed | no | Registry failed strict parse or validation |
| `E_SOURCE` | failed | yes | Fetch, resolve, or checkout failed |
| `E_EXECUTOR` | failed | yes | Container or VM would not start |
| `E_CACHE` | failed | yes | Volume mount or lock acquisition failed |
| `E_DISK` | failed | yes | Below `min_free_gb` |
| `E_STEP` | failed | no | A step exited non-zero (`ExitCode` carries it) |
| `E_TIMEOUT` | timed_out | no | Job wall clock exceeded |
| `E_STEP_TIMEOUT` | timed_out | no | A single step exceeded `steps[].timeout` (§9) |
| `E_ARTIFACT` | failed | no | Glob escaped the workspace, or collection failed |
| `E_INTERRUPTED` | interrupted | yes | Agent died mid-run; set at reconciliation |
| `E_APPROVAL_EXPIRED` | rejected | no | Unapproved for longer than `approval_ttl` (INV-5) |
| `E_QUEUE_EXPIRED` | rejected | no | Queued for longer than `queue_ttl` |
| `E_SECRET_BACKEND` | failed | **yes** | A secret backend is unusable: keychain locked, `op` not signed in, `secrets.env` world-readable |

`E_SECRET_BACKEND` is new in amendment A16. Draft 02 mapped every secrets failure to `E_MISSING_SECRET` — rejected, non-retryable — which is right for a name nobody configured and wrong for the most common case on a Mac mini: a keychain that is locked because the machine rebooted. That is a machine condition, it resolves without anyone editing a commit, and it should offer a retry. Distinguishing "you never set this" from "I cannot reach where you set it" is the difference between a useful error and a misleading one.

Retryable errors are safe for the control plane to offer as a one-click retry. Non-retryable ones need a human or a commit.

One code never appears in `Update.ErrorCode` because it never belongs to a run: `E_AGENT_DOWN` is what `latem run --wait` or `--follow` exits with (exit 3) when no agent is listening (§7.3). It is a CLI condition, not a run outcome.

---

## 22. Persistence

sqlite at `~/.latem/state.db`, WAL mode, embedded migrations run at startup.

```sql
CREATE TABLE runs (
  id                TEXT PRIMARY KEY,
  repo              TEXT NOT NULL,
  job               TEXT NOT NULL,
  ref               TEXT NOT NULL,  -- as supplied, post-validation
  ref_normalized    TEXT,           -- refs/heads/main — what policy matched (INV-5)
  sha               TEXT,
  runner            TEXT,
  state             TEXT NOT NULL,
  exit_code         INTEGER,
  error_code        TEXT,
  error_message     TEXT,           -- redacted
  origin_kind       TEXT NOT NULL,
  origin_actor      TEXT,
  cancel_reason     TEXT,           -- redacted, display only
  inputs_json       TEXT NOT NULL DEFAULT '{}',
  created_at        INTEGER NOT NULL,
  started_at        INTEGER,
  finished_at       INTEGER,
  expires_at        INTEGER,        -- approval_ttl / queue_ttl deadline (§11)
  approved_at       INTEGER,        -- local `latem approve` (INV-5, amendment A27)
  approved_sha      TEXT,           -- the commit that approval pinned
  number            INTEGER NOT NULL DEFAULT 0,  -- this machine's run counter (amendment A36)
  UNIQUE(id)
);
CREATE INDEX runs_state_idx ON runs(state) WHERE state NOT IN
  ('succeeded','failed','cancelled','rejected','timed_out','interrupted');
CREATE INDEX runs_repo_created_idx ON runs(repo, created_at DESC);
CREATE INDEX runs_expiry_idx ON runs(expires_at)
  WHERE state IN ('queued','awaiting_approval');

-- What each transport has acknowledged about each run (amendment A28).
CREATE TABLE broker_state (
  run_id          TEXT NOT NULL REFERENCES runs(id),
  broker          TEXT NOT NULL,  -- "local" | "remote"
  reported_state  TEXT,           -- last state THIS transport acked
  last_acked_seq  INTEGER NOT NULL DEFAULT 0,  -- THIS transport's resume point (§7.2)
  PRIMARY KEY (run_id, broker)
);

-- Monotonic counters, none of which retention may prune (amendment A36).
CREATE TABLE sequences (
  name  TEXT PRIMARY KEY,   -- "run_number"
  value INTEGER NOT NULL    -- the highest value ever issued on this machine
);

CREATE TABLE queue (
  run_id      TEXT PRIMARY KEY REFERENCES runs(id),
  priority    INTEGER NOT NULL DEFAULT 0,
  enqueued_at INTEGER NOT NULL
);

CREATE TABLE artifacts (
  run_id     TEXT NOT NULL REFERENCES runs(id),
  path       TEXT NOT NULL,
  size_bytes INTEGER NOT NULL,
  sha256     TEXT NOT NULL,
  expires_at INTEGER,
  PRIMARY KEY (run_id, path)
);

CREATE TABLE cache_meta (
  name          TEXT PRIMARY KEY,   -- volume or directory name
  runner        TEXT NOT NULL,
  size_bytes    INTEGER,
  last_used_at  INTEGER
);
```

`broker_state` is keyed by `(run_id, broker)`, and the key is the whole point (amendment A28). Draft 02 put `reported_state` and `last_acked_seq` on `runs`, one of each for the machine, which is correct for a machine with one broker and wrong for a machine with two — and §7 defines two.

`reported_state` is what makes broker reporting recoverable: on reconnect, replay every run where that transport's `reported_state != runs.state`. The two transports mean different things by the word. The local broker's honest meaning is "persisted on this disk", and it marks a transition reported the moment `Transition` returns; the remote broker's is "the control plane has durably accepted it", which a partition can leave arbitrarily far behind. Sharing a column meant the local broker satisfied the remote transport's replay condition on every transition, so a run that finished while the network was down was never reported to anybody, and the control plane showed it as running forever.

`last_acked_seq` is the log equivalent, and it is why §7 returns a `LogSink` rather than an `io.WriteCloser`. On reconnect the agent resumes streaming from that transport's `last_acked_seq + 1` instead of re-sending a buffer the server already has. Persisting it means the resume point survives an agent restart, not just a reconnect. Here the shared column was worse than merely wrong: the write is monotonic, the local broker advances it on every batch it writes to disk, and the remote transport is behind by however much the network is — so the resume frame told the server "you already have everything up to N" for an N it had never seen, and ingestion restarted past exactly the frames covering the outage. That is the failure `LogSink` and finding F6 exist to prevent, reintroduced by the schema.

Each transport writes and reads only its own row, and nothing else chooses a key for it: "what has this transport acknowledged" is a question only that transport can answer. A row is created lazily, because a run nobody has reported and nobody has streamed reads identically whether the row is absent or present and zero.

A keyed table rather than `reported_state_local` / `reported_state_remote` columns: per-transport columns do not scale past two and encode the list of transports in the schema, where adding one is a migration rather than a constant.

Migration `0003` moves the existing values under `broker = 'local'` — the only transport that ever wrote them — and then **drops** the two columns. Leaving them would be a second source of truth for exactly the values a partition depends on, and no reader could tell which was live.

`ref_normalized` is stored separately from `ref` because policy was evaluated against the normalized form (INV-5). Keeping both makes an audit answerable — "this run was requested as `main` and matched `refs/heads/main`" — without re-deriving it from a mirror that has since moved on.

`expires_at` is set when a run enters `queued` or `awaiting_approval` and cleared when it leaves. The partial index over it makes the TTL sweep a single indexed scan rather than a table walk.

`approved_at` and `approved_sha` are what a local `latem approve` recorded (amendment A27), and `approved_sha` is a commit rather than a ref on purpose — see INV-5. They are written together with the `awaiting_approval` → `queued` transition and the queue row, in one transaction, because reading the commit and pinning it must not be separable: a sequence that read `sha` and then wrote it back as approved would have a window in it, and that window is the one a force-push exploits. Entering `awaiting_approval` clears both. Nothing on the wire carries them; approval is local-only (INV-2, INV-5), and a broker learns only that the run reached `queued`.

`number` is this machine's run counter, and the agent exports it to every step as `LATEM_BUILD_NUMBER` (amendment A36, §9). App Store Connect refuses a build number it has already accepted for a marketing version, so a project with `CURRENT_PROJECT_VERSION` pinned uploads exactly once and fails forever afterwards — after archiving, exporting and uploading. Unix time solves that and says nothing: a build in TestFlight cannot be traced back to the run that produced it, which is most of what this table is for.

It is issued from the `sequences` table rather than derived from the rows, and the difference is retention. §14.4 deletes old runs, so `MAX(number) + 1` — and sqlite's implicit rowid, which reuses the largest deleted value without `AUTOINCREMENT` — both reissue a number after a prune. The reissued value is precisely the one an upload already carries, and the rejection arrives months later from Apple rather than from a test. The sequence row is never pruned, so it holds the highest number ever issued rather than the highest currently stored. Issuing happens inside the same `BEGIN IMMEDIATE` transaction as the insert, so two concurrent creates cannot be given one number and a crash between the two is not a reachable state.

**It is unique on this machine and nowhere else.** Two agents building the same app both count from 1 and collide at upload. That is a deployment question rather than a schema one — §18.1 states it where an operator meets it — and the answer is to release from one machine, or to set the build number in the job's own `steps[].env`, which overrides the agent's (§9).

They are nullable with no default, so migration `0002` applies forward over an existing history: every run already on disk reads as never approved, which is what it is. A row left in `awaiting_approval` by a pre-A27 agent has no `sha` and cannot be approved — there is nothing to pin — and the honest answer is to cancel it and submit the run again.

`inputs_json` and `cancel_reason` MUST be scrubbed through the redactor before persistence.

---

## 23. Installation and service management

**Linux — systemd user unit** at `~/.config/systemd/user/latem.service`, with `loginctl enable-linger $USER` so it survives logout.

A user unit has the groups its user manager started with, and linger keeps that manager running through a logout. So a group added to the account reaches every new login and does not reach the agent: `docker` in particular has to be in place before the manager starts, or the machine rebooted after. `latem doctor` compares the running agent's own groups with the Docker socket's (A66).

```ini
[Unit]
Description=Latem runner agent
After=network-online.target docker.service

[Service]
ExecStart=%h/.local/bin/latem agent
Restart=always
RestartSec=5
KillSignal=SIGTERM
TimeoutStopSec=1830        # shutdown_grace + margin

[Install]
WantedBy=default.target
```

**macOS — LaunchAgent** at `~/Library/LaunchAgents/com.latem.agent.plist`, `RunAtLoad`, `KeepAlive`, `ProcessType` `Interactive` (A62).

Three things `latem doctor` must verify and explain, because getting any of them wrong produces opaque signing failures (§13.3) or a machine that silently never comes back:

1. **This is a LaunchAgent, not a LaunchDaemon.** `codesign` needs a real user session with keychain access; a daemon context fails with errors that point nowhere useful.
2. **Auto-login is enabled** for the user, so a session exists after reboot.
3. **FileVault is off, or the operator knows what it costs.** This is the one draft 01 missed, and it quietly falsifies "survives reboots." With FileVault enabled, an unattended reboot stops at the pre-boot unlock screen: no login, no session, no LaunchAgent, no builds — until somebody walks over to the machine and types a password. A headless Mac mini in a cupboard is exactly the deployment this breaks, and the symptom is "my machine went offline overnight and nothing in the logs explains it," because the agent never started to log anything.

   `latem doctor` reports FileVault status and, when it is on, names the two options rather than picking one: disable FileVault (accepting that a stolen machine yields its certificates and keystores), or reboot only via `sudo fdesetup authrestart`, which unlocks the disk for exactly the next boot. The trade-off is genuinely the operator's — a machine holding signing certificates is a reasonable thing to want encrypted — but it has to be a decision rather than a surprise. Document both in `docs/ios.md`.

**Directory and file modes**, created by the installer and enforced at startup: `~/.latem` is `0700`; `config.toml`, `secrets.env`, and the token file are `0600`; `agent.sock` is `0600`. The agent refuses to start otherwise rather than warning, because a world-readable directory holding a machine token and every secret is the threat model failing silently.

**Install script** `curl -fsSL … | sh`: detect OS/arch, download the matching binary, verify checksum, place in `~/.local/bin`, run `latem doctor`, print the next command. Never run as root.

---

## 24. Testing requirements

**Unit** — registry parsing (valid, invalid, unknown-field); the ref validation table (§5 hostile inputs); **ref normalization and policy matching** as a table covering `main` vs `refs/heads/main` vs `refs/tags/main`, `release/*` vs `release/**`, the ambiguous heads-and-tags case, and `refs/pull/*` rejection; input coercion and bounds; redactor correctness including encoded forms **and multi-line values**; `$LATEM_ENV` parsing including malformed lines, secret-name collisions, and `LATEM_*` collisions; state machine transition legality including both TTL expiries; cache name derivation; log sequence assignment and resume arithmetic.

**Integration** (`test/integration`, build-tagged, needs Docker) — full run against a fixture repo; **`git rev-parse HEAD` and `git describe` succeed inside the container**, which is the regression test for §12; a failing step produces `failed` with the right exit code; job timeout produces `timed_out` and tears down; a step timeout produces `E_STEP_TIMEOUT` and still collects artifacts; shell state does *not* leak between steps while filesystem state does; a cancel mid-run terminates within the grace window and still collects; agent killed mid-run leaves nothing orphaned after restart; concurrent runs on one runner do not corrupt a shared cache; artifact glob escaping is rejected; a broker disconnect mid-run produces no duplicated and no missing log frames after reconnect.

**Invariant tests** (`invariants_test.go`) — one per rule in §5, named `TestINV1_NoInboundListeners` and so on, so a failure names the rule it broke. `TestINV4_SecretsNeverLeave` MUST include a multi-line PEM fixture; a version that only covers single-line secrets passes while the leak in §16 is live, which is how draft 01 shipped it.

**Shell checks** (`test/scripts`) — A37's rule, made mechanical rather than remembered. Every `.sh` file this repository owns is scanned: a script that enables `pipefail` may not pipe a producer into a stage that exits early — `grep -q`, `grep -m`, `grep -l`, `head` — because the producer then takes SIGPIPE and the pipeline reports failure on input that matched. The escape hatches are a `# pipefail-safe: <reason>` comment and an explicit `|| true`, both of which require somebody to have looked. Vendored and generated shell is excluded by path, because a check that flagged CocoaPods' scripts would be switched off within a week.

The test carries its own negative controls: a planted hazard it must find, the fixed form it must not flag, and a script without `pipefail` it must leave alone. It earned them immediately — the first pass at fixing the six findings replaced `tool | grep -q` with `printf … | grep -q`, which has the same defect for a large enough value, and the check refused it. The answer in bash is no pipeline at all: capture the output and use `[[ $out == *pattern* ]]`.

**Manual matrix** (documented in `docs/testing.md`; not automatable) — iOS build end to end on real hardware with real signing; Tart VM lifecycle including forced kill; GPU passthrough; reboot recovery on both platforms.

---

## 25. Work breakdown

Milestones are ordered by dependency and sized to be independently reviewable. Each is complete only when its acceptance criteria pass.

> **These milestones are acceptance gates, not an assignment plan.** They describe *what must be true* at each stage, not who builds it or in what order.

### M0 — Prove the hard part manually (no code)

Not an engineering task, but it gates everything after it. Run `eas build --platform ios --local` by hand on the Mac, with match, and get an `.ipa` to TestFlight. Do the Android equivalent.

It also carries the one measurement that can change an architecture rather than a constant: the §13.2 DerivedData-over-VirtioFS timing, three configurations, on this hardware. Doing it here costs an afternoon. Discovering it during M5 costs the tart executor.

*Acceptance:* a build lands in TestFlight and in Play internal testing; every command used is written down; the three timings are recorded and `derived_data` has a defensible default. Those commands become the `ci/` scripts in `examples/`.

### M1 — Skeleton and config

`cmd/latem`, cobra wiring, `config.toml` load with strict decode and validation, sqlite with migrations, `latem version`, `latem doctor` covering git/docker/disk.

*Acceptance:* `latem doctor` on a clean machine reports accurate findings and exits non-zero when a dependency is missing.

### M2 — Registry and admission

`.latem/jobs.yaml` parsing, strict validation, the §10 gate chain, the full error taxonomy, `${input.NAME}` substitution confined to `steps[].env`.

*Acceptance:* the hostile-input table in §5 passes; every gate returns its documented code; INV-2 and INV-3 tests are green.

### M3 — Docker executor and source

Mirror + hardlinked clone acquisition (§12), docker Prepare/Exec/Fetch/Close, container labelling, cache volume mounting, log streaming to file and terminal, `latem run --follow`.

*Acceptance:* `latem run api test` builds a real repo end to end with no broker configured, warm caches on the second run, a `git status`-clean workspace, and `git rev-parse HEAD` working **inside** the container.

### M4 — Agent daemon and local broker

Runner pool, concurrency limits, concurrency groups, run state machine, graceful shutdown, crash reconciliation, unix socket broker, service install for both platforms.

*Acceptance:* `kill -9` mid-run leaves zero orphaned containers, VMs, or workspaces after restart, and the run shows `interrupted`.

### M5 — macOS executors and the mobile pipeline

Tart executor, base image build script, native executor, keychain handling, host-directory caches, `examples/expo-app/` with working `ci/` scripts, `docs/ios.md`.

*Acceptance:* `latem run myapp ios-release` produces a TestFlight build on a clean checkout, and the second run is measurably faster from warm DerivedData.

### M6 — Remote broker

WebSocket client, reconnect with backoff and jitter, resume-and-replay, buffered log flush, heartbeat, `latem register`, `latem approve`.

*Acceptance:* the agent survives a 10-minute network partition mid-run, reports correct terminal state on reconnect, and continues serving the local broker with a revoked token.

### M7 — Operational polish

Cache pruning and disk guards, artifact retention, log rotation, `latem doctor --fix`, install script, release pipeline with signed binaries, README and quickstart.

*Acceptance:* a new machine goes from `curl | sh` to a green run in under five minutes, following only the README.

The release pipeline is `.github/workflows/release.yml`: four cross-compiled targets from one runner, a sha256 manifest, a minisign signature over that manifest, a smoke job that runs the built binary on a real machine of each of the two common platforms, and a publish step that is deliberately not idempotent. Two parts of it are worth stating as requirements rather than leaving in the file:

- **The tag's shape is a contract.** §20's version comparison is strict, so a tag it cannot parse — `v1.2`, a branch name, a bare SHA — produces a release no agent can ever update to, with the failure appearing days later in an unread log. The workflow refuses such a tag before it builds anything.
- **Cross-compiling is verified by running the result, not by reasoning about it.** All four targets come off one Linux runner, which is sound because nothing here needs cgo, and which stands on one measured fact: Go's internal linker ad-hoc signs `darwin/arm64` binaries (`codesign -dv` reports `adhoc,linker-signed`), and macOS refuses to execute an arm64 binary without at least that. A paragraph is not evidence, so a smoke job unpacks the artefact on a real Mac and a real Linux box and runs `version --short`, `init` and `doctor` on it — reading the platform off `uname` rather than from the matrix, because a runner label is a promise about a machine and only `uname` is the machine. The other two archives ship untested by that job, which is a stated gap rather than an oversight; the job prints which one it opened so the gap is visible in the log.

---

## 26. Open questions

Draft 01 carried six. Two are now decided and moved into the body: step-level timeouts are in §9 as `steps[].timeout`, and remote log volume is in §7.4 as `[agent] remote_logs`. Both were cheap to settle now and awkward to retrofit, which is what made them worth closing rather than tracking.

1. **Registry location for polyrepo setups.** `.latem/jobs.yaml` in the repo is right for the common case, but a monorepo may want several. Proposal: allow `.latem/jobs.d/*.yaml`, merged, with duplicate job names an error. Defer to post-M7.
2. **Matrix jobs.** Explicitly out of scope (N1), but multi-arch builds want them. Watch whether `examples/` ends up duplicating jobs; if so, reconsider with a strictly declarative, non-expression form.
3. **Windows.** No current need. The executor interface should not acquire assumptions that make it impossible, but no work until someone asks.
4. **Approval UX.** INV-5 requires local approval, which is safe but awkward when travelling — precisely the case the hosted layer exists for. A signed approval from an authenticated session may be acceptable, but it weakens the rule and needs a written threat model before it ships. Note that granting approval remotely would require a third inbound command kind, which §7.1 declares closed; that is the size of the change, and it is the right size.
5. **Non-Expo mobile projects.** §18 is written entirely around `eas build --local`, while the product describes itself as building iOS and Android applications generally. Nothing in the agent is Expo-specific — steps are shell — but `examples/` currently would be. A bare `xcodebuild` + Gradle example is cheap insurance against the wedge being narrower than the pitch. Decide by M5.
6. ~~**Control plane licence.**~~ **Closed.** The hosted layer now lives in its own repository. This one is Apache-2.0 throughout, and the hosted layer's licence is that repository's decision rather than a question this one has to hold open. The worry recorded here — "a public repo with an implied answer" — is exactly what a commercial layer sitting inside an Apache-2.0 tree would have produced, so the split is the answer rather than a deferral of it.

   One consequence is worth naming: the two halves can now drift. `pkg/protocol` and [`docs/PROTOCOL.md`](./PROTOCOL.md) are the contract between them, and `docs/PROTOCOL.md` is descriptive of `internal/broker/remote` rather than aspirational — where it and this document disagree, the Go side is what ships.

---

## 27. Changes from draft 01

Fourteen findings, all resolved in the text above. Recorded here because several are corrections to statements draft 01 made confidently, and a reader who learned the old version needs to know what moved.

| # | Section | Draft 01 said | Draft 02 says | Why it mattered |
|---|---|---|---|---|
| F1 | §9, §13.1 | `cd` and `export` persist across steps | Each step is a new process; only the filesystem persists. `$LATEM_ENV` is the sanctioned channel between steps | False as written. `docker exec`, `ssh`, and `bash -lc` all start fresh processes |
| F2 | §12 | Mirror + `git worktree add` | Mirror + `git clone --local` (hardlinked) | A worktree's `.git` is a file pointing outside the mount; every `git` call inside a container failed |
| F3 | §5, §16 | Redact each secret value | Also redact every **line** of a multi-line value, and the whitespace-stripped form | `APP_STORE_CONNECT_KEY_P8` is a PEM. Per-line redaction of a whole-value match redacts nothing |
| F4 | §7 | `Runs()` yields `RunRequest`; `cancelled` reachable by "broker cancellation" | `Commands()` yields a closed two-kind union: `run` and `cancel` | The protocol could not express the Stop button the product promises |
| F5 | §5, §10 | `allowed_refs` "glob, matched after resolution" | Normalize → match policy → resolve, with expansion rules stated exactly (reordered to normalize → resolve → match by A27; the match is still against the name) | Globbing against a SHA is meaningless; short vs fully-qualified spellings had no defined answer |
| F6 | §7, §16, §22 | Logs are an `io.WriteCloser`; tolerate replays | `LogSink` with per-run monotonic `Seq` and a persisted `last_acked_seq` | State was idempotent; log lines were idempotent by nothing, so every partition duplicated output |
| F7 | §5, §11, §21 | `awaiting_approval` with no exit | `approval_ttl` and `queue_ttl` → `rejected`, swept daily and at startup | Unapproved runs accumulated forever inside the partial index that exists to stay small |
| F8 | §4, §7 | Wire types declared in `package broker` | All wire types in `pkg/protocol`; `internal/broker` holds only the interface | §4 already said protocol was the shared package; §7 contradicted it |
| F9 | §8 | Two machine "roles" implying two agents | One agent, several labelled runners. `LATEM_CONFIG_DIR` for the rare exception | The architecture overview and the spec described different deployment models |
| F10 | §15 | Secrets "zeroed where the language allows" | `[]byte` inside `internal/secrets`, converted to `string` once at env-build | Go strings cannot be zeroed; the guarantee was unenforceable |
| F11 | §12, §14.4 | `fetch --prune` on mirrors | Plus `gc.auto 0`, with explicit gc from the daily ticker | Automatic gc could collect the run's commit mid-build after a force-push |
| F12 | §8, §13, §14.4 | Global `min_free_gb = 20` | Per-runner floors: docker 40, native 60, tart 150 — **tart revised to 200 by A31**, once the image was measured rather than estimated | A Tart Xcode image was believed to be ~90 GB (it is 140 GB provisioned); 20 admitted builds the machine could not finish |
| F13 | §19, §23 | LaunchAgent + auto-login | Plus FileVault status and the two options it forces | FileVault stops an unattended reboot before login, so "survives reboots" was false on any encrypted machine |
| F14 | §8, §13.2 | Tart `concurrency = 1` "see §13.3" | Ceiling is 2 (§13.2); §13.3 is the native executor's separate constraint | Wrong cross-reference on a licence-derived limit |

Two open questions from draft 01 were closed into the body rather than carried: step-level timeouts (§9) and remote log volume (§7.4). Two were added: non-Expo mobile projects, and the control plane licence.

### Amendments since draft 02 was written

Implementing WS-00 surfaced four gaps between the §7 snippets and code that compiles. Each is recorded here rather than silently corrected, because `pkg/protocol` is frozen and the whole value of freezing it is that changes are visible.

| Amendment | Change | Reason |
|---|---|---|
| A1 | `Origin.RequestAt` → `Origin.RequestedAt` | Grammatical slip in the Go identifier. The JSON tag `requested_at` was already correct and is unchanged, so this is not a wire change. |
| A2 | `Update.ErrorCode` typed as `ErrorCode`, not `string` | The §21 table already defines a closed set with a retryability column. Typing the field lets `Retryable()` and `TerminalState()` derive from one table instead of two switch statements that drift. |
| A3 | `Resume` and `RunResume` added | §7.4 describes the resume frame in prose — "for every non-terminal run it owns, `{run_id, state, last_acked_seq}`" — but no §7.2 type expressed it. It crosses the wire, so per F8 it belongs in `pkg/protocol`. |
| A4 | `protocol.Version` constant added | The spec calls the protocol independently versionable without giving it a version. Because inbound decoding is strict, even an additive change is visible to an older agent as a decode failure; a version makes that diagnosable rather than mysterious. |
| A5 | `protocol.Duration` accepts a leading day component | §8 and §9 write retention as `log_retention = "14d"` and `retain: 7d`, and Go's `time.ParseDuration` rejects the unit outright — both would have failed at load. Only a leading component is accepted, so `1d12h` parses and `12h1d` does not. |
| A6 | **`RunRequest.ID` and `CancelRequest.ID` are validated** (INV-3 above) | Found by `gosec` G304 during WS-02 and confirmed by hand: a control plane sending `{"id": "../../../../tmp/x"}` made the agent open a log file outside `~/.latem` entirely. INV-3 enumerated `repo`, `ref`, and `inputs` and never mentioned `id`, while `id` is the field every path is built from. |

A1, A4, and A5 are not wire changes. A2 and A6 narrow a field's legal values, and A3 formalises a frame the spec already required. **A6 is a security fix, not a tidy-up** — it closes a path traversal reachable from a compromised or buggy control plane, which is precisely the threat §5 exists to bound.

### Amendments from Wave 1

Seven implementations built simultaneously against the frozen contracts, each pushing on a different part of the spec. A7 through A16 are what they found. None was discovered by re-reading the document; every one came from something refusing to compile, refusing to run, or refusing to be expressible.

| # | Section | Change | Why it surfaced |
|---|---|---|---|
| A7 | §5 (INV-3), §12 | Options terminator is `--` **or** `--end-of-options`; §12's `checkout` and `rev-parse` lines corrected; LFS gains a workspace-side fetch | §12's own shell sketch does not run. `git checkout --detach -- $SHA` fails outright, because to git `--` means "a pathspec follows". Verified on git 2.50.1 |
| A8 | §7.2, §22 | `Update` gains `Runner`, `ExpiresAt`, `CancelReason` | Three columns of §22's schema were unreachable through the interface that owns them. `Transition` could clear an expiry but never set one |
| A9 | §13 | `Env.Exec` takes separate stdout and stderr; `Env` gains `EnvFile()`; `Step.EnvFile` removed | One writer made `LogFrame.Stream` unfillable. And only the executor knows the `$LATEM_ENV` path, so the caller filling it forced the orchestrator to import a concrete executor |
| A10 | §9, §13 | `Spec` gains `Secrets []string` | §9 requires `$LATEM_ENV` to ignore "a declared secret name", and nothing told the executor which keys those were. It had to protect the entire injected environment — safe, but it silently refused legal overrides of ordinary variables |
| A11 | §14 | `cache.Manager.Acquire` takes `Request{Name, Target}`, not names | Mount paths live in the registry, which the cache manager cannot read. It returned mounts with an empty target — silent at every layer, and the only symptom is a build that stays cold forever |
| A12 | §22 | `ClaimNext` documented as claiming *unpinned or pinned-to-me*, not label-matched | The old wording contradicted INV-5: a job's `runs_on` lives in `jobs.yaml` at the resolved commit, which is not on disk until after the claim |
| A13 | §16 | `TooShort` moved beside the `Registrar` interface | It sat in the implementation file while the constant it uses sat in the seam, so `internal/secrets` reached past the interface it was given |
| A14 | §8, §11 | `[runner.native]` block added; `preparing → queued` documented as legal | §13.3 required a native disk floor the schema could not express. Gate 16 requires a state transition the diagram does not draw |
| A15 | §15 | `[secrets.commands]` are argv arrays; `provider` key removed | The spec wrote commands as strings, INV-3 demands argv, and nothing said how one becomes the other. `provider` had exactly one legal value |
| A16 | §21 | `E_SECRET_BACKEND` added, retryable | Every secrets failure collapsed to a non-retryable `E_MISSING_SECRET`, including a locked keychain after a reboot — a machine condition that resolves on its own |

Three of these (A7, A9, A11) describe things that could not have worked as written. Two (A10, A12) are places the spec asked for a guarantee it gave no way to implement. That is the return on freezing contracts first and building against them in parallel: the defects surfaced in a week of implementation rather than in month three of integration.

### Amendments from Wave 2

Four more implementations — the orchestrator, the local broker, the CLI, and the native executor — against the same contracts. A28 is the odd one out and came later, from the first integration in which both transports were live at the same time; it is the only amendment so far that changes the §22 schema of an already-shipped table.

| # | Section | Change | Why it surfaced |
|---|---|---|---|
| A17 | §10, §11 | `preparing → awaiting_approval` is legal (its account of gate 4 is superseded by A27, which stopped gate 4 parking anything) | **Gate 8 could not park a run.** §11 draws "gate 4 / 8 ──▶ awaiting_approval" as though both gates sat in one place; they do not. Gate 4 runs before a row exists, so `Admit` creates it parked. Gate 8 is the *authoritative* INV-5 check, runs during `preparing` because it needs a fetched mirror, and had no edge out. A ref gate 4 admitted optimistically — a tag under a branch policy — could never be parked by the check that decides |
| A18 | §7 | `protocol.Ack` added | `Command` was a one-way vocabulary, so every transport had to invent its own "accepted, run id r_123". The local broker did; the WebSocket broker would have invented a different one |
| A19 | §7 | `Broker` gains `Close()` | Both transports need shutdown, so the agent had to hold a concrete type — undercutting the one property the interface exists for |
| A20 | §7.3, §21 | `protocol.ErrNoAgent` sentinel added | Two packages had each defined their own value for "nothing is listening". The exit code stays in the CLI: §7.3 fixes it at 3, but an exit status is a property of a process, not of the wire |
| A21 | §7.2, §22 | `Update` gains `RefNormalized` and `SHA` | Both are decided at gates 7 and 9, long after `Create`. §22 has columns for exactly this pair and nothing could write them, so the audit trail INV-5 exists to leave was permanently NULL |
| A22 | §15, §21 | `secrets` distinguishes `E_SECRET_BACKEND` from `E_MISSING_SECRET` | A16 added the code; the package predated it and collapsed both, telling an operator to fix a commit when the answer was to unlock a keychain |
| A23 | §6, §13 | `Executor` gains `Reap` | §6 step 5 says reconciliation is not optional, and each executor had grown its own shape — docker `Discover`+`Remove`, native `Reap` — so the agent would have type-switched on a concrete executor to clean up after a crash |
| A24 | §14 | `cache.Options` gains `WorkDir`/`MirrorDir` | `work/` was derived from the cache root while `agent.work_dir` is relocatable, so a relocated tree made `FreeGB` measure the wrong filesystem — silently, on exactly the gate that exists to stop a build dying at 80% with a full disk |
| A25 | §5 (INV-1) | The INV-1 test recipe carries the Darwin caveat | `SO_ACCEPTCONN` returns `ENOPROTOOPT` on macOS for a real listening socket, so the obvious detector reports the invariant clean having enumerated nothing |
| A26 | §19 | `config.DerivePaths` exported | `doctor` exists for the machine whose config does not parse, and `Load` returns nil on failure — taking the paths with it |
| A27 | §5 (INV-5), §10, §11, §22 | **Approval is implemented, and bound to a commit.** `runs` gains `approved_at` and `approved_sha`; `store.Store` gains `Approve`; `registry.AdmitRef` gains an approval input; gates 8 and 9 swap so the SHA is resolved before the policy decision; gate 4 no longer parks | **`latem approve` did nothing.** It moved a parked run to `queued`; the run was claimed, prepared, and parked again by the same gate 8, having paid for another mirror fetch and clone. Nothing recorded that a human had said yes — §22 had no column for it and `AdmitRef` had no input for it — so the loop never terminated and INV-5's second sentence was unimplemented. The fix could not be "approved runs skip gate 8": approval had to name a *commit*, because INV-5 exists precisely because a `ref` can point at a branch a contributor controls, and a ref-scoped approval is spent by force-pushing after it is granted. Pinning the commit forced the rest: the SHA must exist before a run is parked (so 9 moves ahead of 8), gate 4 cannot park at all (nothing is fetched yet, so it has no commit), gate 10 must check out the SHA gate 8 admitted rather than resolving again, and parking must clear any earlier approval |

| A28 | §7, §7.2, §7.4, §16, §22 | **Reporting state becomes per transport.** `runs.reported_state` and `runs.last_acked_seq` are replaced by a `broker_state` table keyed `(run_id, broker)`; migration `0003` moves the old values under `broker = 'local'` and drops the columns. `store.Store`'s `Unreported`, `MarkReported` and `SetLastAckedSeq` each take a broker key, `LastAckedSeq` is added so a transport can read its own position back, and `store.Run` loses both fields. `internal/run` takes `Brokers []broker.Broker` instead of one, and the agent consumes commands from every transport. Separately, §16's machine token is now actually registered with the redactor | **Two transports existed and the schema had room for one.** The two mean different things by both words. `internal/broker/local` marks `reported_state` the moment a transition is persisted — the honest local meaning of "durably accepted", where the far side is the same disk — and advances `last_acked_seq` on every batch it writes. `internal/broker/remote` needs both to mean "what the control plane has accepted", which a partition leaves arbitrarily far behind. With both live they collided in the direction that loses data: `store.Unreported` came back empty for the remote transport on every machine that had a local broker, so a run that finished during an outage was never reported to anybody; and the resume frame claimed the control plane held frames it had never seen, so ingestion restarted **past the gap**. The lost region is precisely the output covering the outage — the failure `LogSink` and finding F6 exist to prevent, reintroduced by the schema. The agent's consequence was visible in the code: it did not read the remote command stream and did not fan reports or log frames out to it, because accepting a run from a control plane it could not report back to would leave that run showing as queued forever. Per-transport columns were rejected as the fix: they do not scale past two and put the list of transports in the schema |

**A27 is a security fix, not a feature.** The half of INV-5 that worked — parking — is the half that costs nothing to get right. The half that did not is the one an operator relies on, and the obvious implementation of it, a boolean "approved" consulted by gate 8, would have been worse than none: it reads as a control, and it grants execution of whatever a branch points at whenever a runner gets round to the run. The whole design follows from refusing that. F5's ordering (normalize → match → resolve) is corrected to normalize → resolve → match; matching still happens against the ref *name*, so no policy answer changes.

### Amendment from the integration suite

| # | Section | Change | Why it surfaced |
|---|---|---|---|
| A29 | §13, §17 | **`executor.Env.Fetch` and `executor.File` removed** | `Fetch` had no production caller and never had one: artifacts are collected by `internal/artifact` against the host workspace path, and all three executors share that path — docker bind-mounts it, native uses the clone directly, and §13.2 shares it into the VM with `--dir`, which is why that section's own Fetch line says "rsync from the shared dir". So two implementations of glob resolution and symlink containment existed, ran never, and disagreed with the one that does run. Removing them deleted ~1,240 lines and resolved the §17 divergence below by construction |

| A30 | §17, §21 | **A literal artifact path naming a directory is `E_BAD_REGISTRY`**; a glob matching one collects nothing rather than expanding | `artifacts: [dist]` had no defensible silent meaning. Expanding is unbounded against a 50 GB budget evicted oldest-first, so it deletes other runs' artifacts; matching nothing tells an operator a release was collected when it was not. Removing the expansion also made `*` stop crossing a `/`, which §17 had claimed all along and this package had never done |

A29 is worth stating as a rule rather than an incident: **an abstraction with no caller is not "ready for the third implementation", it is untested code that has already drifted.** Both copies had containment checks written for a workspace sitting next to `~/.ssh` and `~/Library/Keychains`, and neither was the code protecting anything.

A29 left one question behind — what a bare directory in `artifacts:` *should* mean — and **A30 answers it: nothing, loudly.** The two are worth reading together. A29 removed the disagreement between implementations; A30 chose the behaviour. Neither would have been safe alone: settling the policy while two globbers still existed would have settled it in only one of them.

Two further items are recorded but **not** amended, because neither is a contract defect:

- **`$LATEM_ENV` parsing, artifact containment, and the error triple are duplicated verbatim across the docker and native executors**, and will be a third copy with Tart — while §9 requires the step contract be identical across all three. The shared code has nowhere to live because `internal/executor` is frozen. This is tech debt with a known trigger point (the Tart executor), not a gap, and refactoring two working packages now buys nothing.
- ~~**Artifact glob semantics diverge**~~ — **resolved by A29.** The divergence existed because two glob implementations lived in the executors and never ran. With `Env.Fetch` removed there is one, so `artifacts: [dist]` has one meaning. What §17 still owes an answer to is whether that meaning is the right one; see A29.

### Amendment from deployment

| # | Section | Change | Why it surfaced |
|---|---|---|---|
| A31 | §8, §13, §18.1 | **Native becomes the default macOS executor; Tart becomes the isolation upgrade.** New §13.0 states the choice and the criterion that decides it. §8's reference config ships the native block enabled and the tart block commented. §13.2's disk figures are replaced with measured ones and `min_free_gb` goes 150 → 200. §13.2's DerivedData measurement is taken off M5's critical path and given a decision threshold from the measured native baseline | **The default cost 69 GB of download and 140 GB of disk to defend a boundary the first user does not have.** Draft 02 made Tart the default because it is the stronger configuration, which is true and was the wrong basis for the decision: a self-hosted runner's first deployment builds the operator's own repositories, where the threat Tart defends against — a contributor moving a ref you do not control — does not yet exist. Nothing in the code had to change; the native executor was already built, tested, and at 90.5% coverage, which is itself the evidence that the ordering was a documentation decision rather than an engineering one. The consequence that made it worth doing now is scheduling: §13.2's VirtioFS measurement was the last open question capable of changing an architecture, and it is a question only Tart asks — native has no VirtioFS, the workspace is the clone and DerivedData is on local APFS. Making native the default retires it from the critical path, unblocks M5, and moves WS-13 to v2 |

**Promoting the block found it was broken.** §8's `[runner.native]` carried `shell = "bash"`, and no runner schema has ever had a `shell` key — decoding is strict, so the reference config a new operator is told to start from would have been rejected at load, with an unknown-key error whose remedy text points back at §8. `machine.id = "mch_01H..."` failed the ID pattern for the same reason. Both had been wrong since draft 02 and neither could be caught, because no test had ever read the spec. `TestSpecReferenceConfigParses` (`internal/config/spec_test.go`) now extracts §8's fenced TOML and puts it through `config.Parse`, and additionally asserts *which* executor is enabled — "the reference config parses" would also hold for a file where the wrong block had been commented out, and which executor an operator gets by default is the substance of A31 rather than a detail of it. `native.Options.Shell` remains an internal seam with no config surface, which is what it always was.

### Amendments from the first real iOS build

| # | Section | Change | Why it surfaced |
|---|---|---|---|
| A32 | §18, §18.3 | **§18.3 is rewritten and §18 gains a second, Expo-free reference example** (`examples/ios-native/`, verified building). §18.1's runner line and cache list are split by example | **The first attempt to run §18.1 never reached a compiler.** `eas build --local` requires an Expo account — a hosted dependency in the critical path of every iOS build, on a runner that advertises working with no account — and §18.3's argument for depending on the flag had bounded the wrong risk: it reasoned about Expo *changing* the flag, not about what invoking it already requires. Separately, Expo SDK 57.0.12 does not compile against Swift 6.2 (Xcode 26.0.1) — 31 errors in `expo-modules-jsi` across three classes of Swift 6 strictness, of which mechanical patches fixed 30. Neither is a defect in this project, and that is the point: §18 had staked the wedge on a dependency whose failure modes it did not enumerate |
| A33 | §9, §13.3, §20 | **A step's environment must set `LANG`**, and `latem doctor` checks it on macOS runners | `pod install` aborts with `Unicode Normalization not appropriate for ASCII-8BIT`, which names Unicode rather than the locale. §9 says a step is a new process with the environment the agent hands it, and that environment had no `LANG` — correct per §9 and wrong in practice, because the interactive shell every operator tests in has one. The class is larger than CocoaPods: Ruby, Python, and Java tools all read it. It belongs in the executor's base environment rather than in each `ci/` script, which is where it currently is |
| A34 | §12, §20 | **A committed shared Xcode scheme is a hard requirement**, checked by `latem doctor` for macOS runners | `xcodebuild -scheme X` fails with "scheme not found" on a fresh clone unless the `.xcscheme` is committed under `xcodeproj/xcshareddata/xcschemes/`. Xcode auto-creates schemes into `xcuserdata/`, which nobody commits — so this works on every developer's machine and fails on every runner, and §12 guarantees a fresh clone every run, which guarantees the failure. It is the cheapest possible check and it fails a 90-minute job in the first ten seconds |

| A35 | §19, §20 | **`latem doctor` gains credential-agreement checks**, not just presence checks | Every value in an App Store Connect key can be individually valid while the set is unusable: the JWT's `kid` names one key and the signature comes from another, and Apple answers 401 without naming either. Verified 2026-08-11 — four `AuthKey_*.p8` files on one laptop, all 257 bytes, all valid, one of them the right one. `doctor` must check *relationships* (does this key id match this private key? does this key see this bundle id?) and not merely that each variable is set. `examples/ios-native/ci/ios/check-credentials.sh` is the working implementation; it answers in a second where the build answers in two minutes |
| A36 | §18.1, §22 | **A release job must derive its build number from the run, not the project** | App Store Connect rejects a build number it has already accepted for a marketing version, so a project with `CURRENT_PROJECT_VERSION` pinned uploads exactly once and then fails forever — after archiving, exporting, and uploading. The example now takes `LATEM_BUILD_NUMBER`, defaulting to Unix time. The agent should supply the run counter instead: §22 already stores it, and a TestFlight build traceable to the run that produced it is the whole point of an audit trail |

| A37 | §24, §25 | **A shell assertion must not put a producer into `grep -q` under `pipefail`**, and `latem doctor`'s own checks are covered by the same rule | Measured, not theorised: `unzip -l file \| grep -q PATTERN` under `set -o pipefail` returned a false negative **17 times in 200 runs**. `grep -q` exits at the first match, the producer takes SIGPIPE, and the pipeline reports failure on input that matched. It is timing-dependent, so it passes every hand test and fails on a loaded machine — which is exactly what a runner is. The project already had one instance, fixed, with an explanatory comment; the comment did not stop the identical bug being written again two hours later in a sibling script. Capture to a variable, or use a tool that answers the question directly (`jarsigner -verify` rather than grepping a zip listing) |

| A38 | §9, §13.3, §14.2 | **A leading `~` in `caches[].path` is expanded to the executing environment's home**, per executor, before the absolute-path check | Found by the first run an *agent* ever performed, in under a second: `E_CACHE: cache "derived-data" target "~/Library/Developer/Xcode/DerivedData" is not an absolute path`. Both reference registries used `~`, §9's own example used `~`, and no executor accepted it — so the documented form had never worked. It cannot simply be banned either: a committed `jobs.yaml` runs on every machine (§12), so an absolute `/Users/<someone>/Library/...` is correct on exactly one. Expansion is per-executor rather than central because `~` denotes a different directory in each — the agent user's home natively, the container's `runner` home under docker (§13.1) — and resolving it once would have to pick one and be wrong elsewhere |

| A39 | §8, §23 | **The service definition's log paths follow `--config-dir`, and `service install` creates the log directory** | Two defects in one plist, both found by rendering it before installing. (1) It carried `LATEM_CONFIG_DIR=<relocated>` while pointing `StandardOutPath` at `~/.latem/logs` — two config trees in one file, when §8 says `--config-dir` relocates the lock, socket, sqlite, work dir and caches *together*. (2) Nothing created the log directory, and **launchd does not create the parent of `StandardOutPath`**: when it is missing the job fails to spawn and the diagnostic is written to the file it could not open. On any machine that has not yet run `latem agent` — which is every machine at first install, and the Mac mini this targets — `service install` therefore produced a service that silently never started |

| A40 | §5 (INV-3), §12, §20 | **`git checkout --detach` takes no options terminator**, and INV-3's argv rule is satisfied for a resolved object name by *validation* rather than by punctuation | `git checkout --detach --end-of-options <sha>` works on git 2.50.1 and **fails on 2.43.0**: `--detach does not take a path argument '--end-of-options'`. checkout's own path/revision disambiguation did not learn the terminator until somewhere between those releases. A7 recorded the terminator as required having verified it on 2.50.1 *alone*, so every runner with an older git could fetch and then fail to check anything out — the first job on a fresh Mac mini died there. The value at that argv is not a broker-supplied string: `validateSHA` has already required `^[0-9a-f]{40}$` or `^[0-9a-f]{64}$`, so it provably cannot begin with a dash and cannot be read as an option however the argv is arranged. Both INV-3 argv tests now exempt values by that *shape*, and nothing else: a ref, a URL and a path are strings whose characters an attacker picks, and all three still fail without a terminator |

### Amendment from the release pipeline

| # | Section | Change | Why it surfaced |
|---|---|---|---|
| A41 | §20, §25 | **§20's self-update gains the five requirements its one sentence assumed**, and the `auto_update` load error narrows from "not implemented" to "this build trusts no signing key" | Written while building the thing, and every one of the five is a decision the original sentence leaves to the implementer with a wrong default available. *What gets signed* determines whether the release feed is part of the trust boundary — sign the manifest and it is not, sign nothing and the feed is the whole of it. *Strictly newer* is the difference between an update and a replay. *Asking the binary its version* catches the failure no signature can, because a release built from the wrong tag is signed as faithfully as a right one. And *"when no run is active"* is not a condition that can be observed — checking it and then acting is a race whose loss orphans a build, so the pool has to stop claiming first, with a pause that is reversible and bounded. The load error narrowed rather than disappearing because the key is embedded at build time: a hand-built binary still cannot verify anything, and an agent declining every release into an unread log is precisely the silent no-op the original refusal existed to prevent |

**The two copies of one key are held level by a test, not by a comment.** The signing key appears twice by necessity — `internal/selfupdate/trusted.pub`, embedded in the agent, and `PUBKEY` in `install.sh`, which is POSIX sh and cannot read Go. A populated `trusted.pub` beside an empty `PUBKEY` ships an installer that cannot check what the agent it installs will insist on, and the reverse ships an agent that refuses what the installer accepted. `test/scripts/install_test.go` requires them to be equal or both empty. That is A37's rule applied to a convention: a hazard that can recur needs something mechanical.

**The installer's checksum was overclaiming, and the fix was a paragraph rather than code.** `install.sh` said a checksum mismatch covers "a corrupted or substituted archive". It does not cover substitution: the checksums file arrives from the same server as the archive, so a match proves the download is intact and says nothing about who served it. The signature is what says that, and `minisign` is not installed on most machines — so the script verifies it when it can, says plainly what it did not check when it cannot, and no longer claims the stronger property. An installer that overstates its guarantees is worse than one that states a weaker guarantee accurately, for the same reason a control that does nothing is worse than an absent one.

**A40 corrects A7 rather than replacing it.** A7's finding stands — `git checkout --detach -- $SHA` really does fail, because to git `--` means a pathspec follows — and its correction to §12's `rev-parse` line is untouched, since `rev-parse` accepts the terminator on every version and carries a genuinely attacker-influenced ref. What A7 got wrong was scope: it verified one git version and wrote the result as a rule. **A version-dependent behaviour confirmed on one machine is a fact about that machine.** `latem doctor` already checks git ≥ 2.24 for `--end-of-options` support generally, and that check passed on 2.43 while the specific invocation did not work — a floor that is necessary and not sufficient.

**A39 is the failure mode §23 exists to prevent, in §23's own implementation.** The section's argument is that a runner which does not come back after a reboot is worse than one that was never installed, because nothing reports it. A plist whose log destination cannot be opened is exactly that: `launchctl list` shows the job, `service status` reports installed and loaded, and the agent is not running. It was caught only because `--print` renders the plist without installing it — a flag that exists so an operator can read what they are about to be given, and which turned out to be the review step that mattered.

**A38 is what the M0 → agent transition was for.** Eleven human-run invocations of the same scripts never touched this code path, because a person types `./ci/...` from a shell and never asks the agent to mount a cache. The defect sat in the two files most likely to be copied by a new user, in the form the spec itself recommends. It survived the entire manual baseline and died to the first automated run.

**A37 generalises past shell.** The rule is that *a fix recorded only as a comment is not a fix* — if a hazard can recur it needs something mechanical: a shared helper, a lint rule, or a test. The same reasoning is why §24's invariant suite exists rather than a document describing the invariants.

**A32 is the "Expo coupling" risk the build plan named, arriving on schedule**, not a surprise. That row said the response was to add a bare `xcodebuild` + Gradle example in WS-09 before M5; the row was right, and the example now exists and builds. What the row underestimated is that the coupling is not only about how *narrow* the wedge is — an Expo-only mobile story also inherits an account requirement that contradicts the project's own first sentence.

**A33 and A34 are the same shape and worth stating as a rule:** *the runner's environment is not the operator's shell, and its workspace is not the operator's checkout.* Both failures are invisible locally and certain on a runner, both were found within an hour of running a real build, and neither would ever have been found by testing the agent against itself. That is the argument for M0 existing.

**A31 is a decision about who the first user is, not about which executor is better.** Tart is better. It is also a boundary you buy before you need it, and defaults are what an operator gets when they have not yet formed an opinion — so the default should be the configuration that is correct for the deployment they actually have, with the stronger one one uncomment away and a stated criterion for when to reach for it. §13.0 is that criterion, and it is a question about provenance ("whose code runs here?") rather than about scale or hardware, because provenance is what actually changes the answer.

The invariants are untouched. INV-5 in particular means exactly what it did: unapproved code does not run. What native changes is not whether the invariant holds but what it costs when an approval is granted wrongly — under Tart, a VM that is deleted; under native, the operator's keychain. That is stated in §13.0 rather than left for an operator to infer, because it is the only part of this amendment that can hurt someone.

### Amendment from the Actions-compatibility decision

Every amendment above was forced by something refusing to compile, refusing to run, or
refusing to be expressible. **A42 is the first that is not.** It is a product decision,
recorded here because it reverses a non-goal and moves the trust boundary, and a reader
who finds N1 contradicted by the code deserves to find the reason next to it rather than
in a commit message.

| # | Section | Change | Why |
|---|---|---|---|
| A42 | §1 (N1), §5 (INV-3, INV-4, INV-5), §9, §13.0 | **N1 is reversed. Latem interprets GitHub Actions workflow syntax** — `.github/workflows/*.yml` as a job registry, `uses:`, composite actions, and the expression language — with `.latem/jobs.yaml` retained as the native format | Adoption, and the measurement that settles it. The user latem most needs to reach already has CI and wants to move one job — the expensive, macOS-bound one — onto hardware they own, without touching what already works. Asking them to re-author that job in a second format is the friction that ends the conversation, and it is a format cost, not a capability cost. A format that accepts only `run:` does not avoid this: **this repository's own two workflows are 22 `uses:` steps against 20 `run:`**, so a `uses:`-rejecting subset would refuse to load the workflows of the project shipping it. Half-compatibility is the one option that buys nothing |

**What A42 costs, stated now rather than discovered in an incident.** Each of these is a
claim currently made in §5 or the README that becomes narrower the moment a workflow names
a third-party action, and each needs its wording corrected in the same change that ships
the feature — an invariant that quietly stops holding is worse than one never claimed.

- **INV-5 no longer yields reproducibility from the commit alone.** `uses: actions/checkout@v5`
  is a *mutable tag on a repository that is not yours*, resolved at run time. The registry
  still comes from your disk at the resolved commit, which is the part INV-5 actually
  guarantees, but "a run is reproducible from the commit alone" is false for any workflow
  with an unpinned `uses:`. Reproducibility becomes a property of SHA-pinned references and
  must be described as one.
- **INV-4 holds for `run:` steps and not for actions.** Redaction at the point of emission
  keeps a secret out of a log. It does nothing about an action that receives the secret as
  an input and opens a socket. "Secrets never leave the machine" is true of the log
  pipeline, not of third-party code, and the distinction has to be in the sentence.
- **INV-3 inherits a known vulnerability class.** `${{ }}` substituted into `run:` is the
  script injection GitHub itself issues advisories about — `github.event.*` is attacker-authored
  text landing in a shell. The rule that keeps INV-3 true is therefore normative for the
  evaluator: **a context value reaches a step as an environment variable, never as
  substituted characters in a command string.** A37's principle applies — this needs a test,
  not a comment.
- **N3 is hit sideways.** `actions/cache` and `actions/upload-artifact` do not manipulate
  files; they call GitHub backend APIs. Supporting the two actions that appear in nearly
  every workflow means implementing those services locally.
- **§13.0's default inverts, and G6 pays for it.** "Your own repositories, native is right"
  was sound while the only code that ran was yours. A marketplace action runs third-party
  code as the agent user, with the login keychain unlocked and the signing identities
  present — which is the precise scenario §13.0 gives Tart as the answer to. For any
  workflow containing an action that is not vendored into the repository, **Tart is the
  floor and not the upgrade**, and a machine that has not pulled the image is a 69 GB
  download away from its first green build. G6's five minutes survives only on the
  `.latem/jobs.yaml` path and for workflows whose steps are all `run:`.

**Order of work, chosen so the trust boundary moves last.**

| Phase | Work | Boundary |
|---|---|---|
| 1 | The workflow parser and the expression evaluator, `uses:` a named error | none |
| 2 | Workflow commands: `$GITHUB_ENV`, `$GITHUB_OUTPUT`, `$GITHUB_PATH`, `$GITHUB_STEP_SUMMARY`, `::add-mask::`, `::group::`; step `id:`; the `steps` context | none |
| 3 | `if:` against run-time status, which needs a condition on the registry's step | none — a model change |
| 4 | `uses: ./path` and `uses: docker://` | INV-5 intact: both resolve at the commit the registry did |
| 5 | The marketplace, behind SHA-pinning and an operator allowlist, with the §13.0 inversion in the same change | **here** |
| 6 | The cache and artifact services | N3 |

Nothing in phase 5 ships before the §5 wording in phases 1–4 is already correct.

**This table replaces a four-item list written before any of it was built, and the
correction is the point of recording it.** The original had today's phase 4 as phase 2 and
went straight to the marketplace, because it was drawn from what *changes the trust
boundary* rather than from what *has to exist first*. Phase 1 surfaced the ordering it
actually has: `$GITHUB_OUTPUT` is what makes `steps.*.outputs` resolvable, and the compiler
refuses that context today — so a composite action, which communicates through exactly that
channel, cannot be implemented before the commands are. Phase 3 is separate from it because
`if: always()` is not an expression problem at all: `registry.Step` has no condition field,
so a cleanup step that must run after a failure cannot be compiled to one at any level of
expression support.

Phase 5 is larger than 1, 2, 4 and 6 together, and should be planned as such: resolving and
caching action repositories, parsing `action.yml`, three action kinds (node20/node24, Docker,
composite) and the pre/main/post hooks around each.

**Two constructs are deliberately unscheduled**, because neither is blocked on effort:

- **`strategy` / matrix** expands one job into several, and §11's run state machine has one
  run per job. Scheduling it means deciding what a matrix leg *is* — a run, a step, or
  something new — and that is a model question, not a parser one.
- **`needs`** is N2. A42 reversed N1 and said nothing about N2, so a workflow with a job
  graph stays refused until someone decides to reverse that too. It is listed here so the
  refusal reads as a decision rather than an omission.

### Amendment from wiring phase 2

| # | Section | Change | Why |
|---|---|---|---|
| A43 | §9, §13, §26 | **`$LATEM_ENV` is also `$GITHUB_ENV`, one file read by one parser, and the format gains the `KEY<<DELIMITER` heredoc** | The alias was the cheap part: a workflow written for GitHub appends to `$GITHUB_ENV`, and exporting the existing file under that name costs one line per executor. What it exposed was that the two files had two parsers — the executors' `parseEnvFile` and the workflow package's `ParseKeyValueFile` — and only the second spoke the heredoc form. Aliasing without unifying would have given a multi-line value written the documented GitHub way a "malformed line" warning and no variable, from a file the workflow was told it could write to. Both executors now read `$LATEM_ENV` through the shared parser, whose cap, key grammar and never-quote-a-value rules are §9's already. The native format acquires heredocs as a consequence, which is recorded here because it is a §9 change, however small. §26 had already named the verbatim duplication of `$LATEM_ENV` parsing across the two executors as debt with the Tart executor as its trigger; this retires it earlier, and the Tart executor will have one parser to call rather than a third copy to make |

### Amendment from designing phase 4

Recorded before the build, as A42 was, because it changes the registry's model and adds an
admission gate, and both should be findable next to the reason.

| # | Section | Change | Why |
|---|---|---|---|
| A44 | §9, §10 (gate 13), §13.1 | **Local actions (`uses: ./path`) and `uses: docker://` are compiled into the registry. A step may be a container instead of a command, and a job may require an executor** | The two were designed together because they are the two halves of one question — what a `uses:` step *becomes* — and because only one of them needs the model to change. Both resolve at the commit the registry did, read through the same `os.Root`, so INV-5 holds for each without a new argument |

**A composite action changes no model at all.** Its `runs.steps` are inlined as ordinary
steps. Three rewrites make that exact rather than approximate, and all three are rewrites of
an expression's *tree*, not of its value:

- `inputs.x` inside the action is replaced by the expression the caller wrote in `with: x:`,
  or the input's default. Because it is the tree that is substituted, a caller's
  `${{ steps.prev.outputs.sha }}` makes the action's own reference late through the
  classifier that already exists. There is no second late-binding mechanism.
- Inner step ids are namespaced `<outer id>/<inner id>`, so the registry's ordering check
  sees real ids and a collision with the job's own ids is impossible by construction.
- An action's `outputs.<name>.value` has its step references namespaced the same way, and a
  later `steps.<outer>.outputs.<name>` is replaced by it. Composite outputs therefore ride
  the inner step's `$GITHUB_OUTPUT` and need no support at run time.

The outer step's `if:` is and-ed into each inner condition; its `env:` lies under theirs. A
nested `./` action is allowed, with a depth cap and a cycle check. A nested remote `uses:`
is phase 5. Composite `run` steps name a `shell:`, and one that is not bash or sh is refused.

**`docker://` changes two things, both small.**

- *The registry.* `Step` gains `Container{Image, Entrypoint, Args}`, exclusive with `run` —
  validation requires exactly one. `with.args` is a list and each element is one argv entry,
  so a value containing `; rm -rf /` is an argument: INV-3's argv discipline, with nothing
  added. Written as GitHub's single string it is split into words the way a shell would
  split it, and then never seen by a shell — with one deliberate difference from GitHub,
  found while building it. GitHub substitutes expressions and *then* splits, so a value
  containing a space becomes several arguments and a pull request titled
  `x --config=/etc/passwd` becomes a flag. Here an expression is atomic: it is kept whole
  inside its word and evaluated there, so whatever it evaluates to is part of exactly one
  argument. `with:` inputs arrive as `INPUT_<NAME>`. The field is excluded from decoding. A
  native `container:` step is plausibly a real §9 feature and should arrive as one, not as a
  side effect of this.
- *Admission.* A job with a container step requires the docker executor, and gate 13 checks
  the runner's executor as well as its labels. A machine with only a native runner learns at
  admission — `E_NO_RUNNER`, naming the step — rather than in a build. The native and Tart
  executors still refuse a container step if handed one; that is a backstop, not the gate.

**In the docker executor, the four files move onto a per-run volume.** Today `$LATEM_ENV`,
`$GITHUB_OUTPUT`, `$GITHUB_PATH` and `$GITHUB_STEP_SUMMARY` are written into the run
container's own filesystem, where a second container cannot see them. They move to a named
volume mounted at the same path in the run container and in each container step, which is
then: create from the action's image with the run's workspace mount, caches, volume,
resource limits and labels — so §6's crash reaping collects it like the run container —
start, stream, wait, remove. Cancellation kills it.

**A tag is mutable, and that is said rather than discovered.** `docker://alpine:3.19` is the
same class of reference as a marketplace action at a tag: INV-5 yields reproducibility only
for a digest. The policy is permissive and honest rather than strict and surprising — the
reference is allowed, the run log records the digest `ImageInspect` reports so the run says
what it ran, and an operator option, `require_pinned_images`, refuses a non-digest reference
before anything is pulled — at the step rather than at `Prepare`, because `Prepare` is handed
the run's own image and not its steps', and the run's own image is the operator's choice in
config, which this does not second-guess. It ships off. That mirrors phase 5's SHA-pinning gate without pre-empting it.
`image: Dockerfile` in a local action is refused: building images is its own surface, and it
belongs with phase 5.

Of the action kinds, this is the safest. It is third-party code, but in a container of its
own with the workspace and one volume mounted — narrower than a marketplace JavaScript
action running natively with the login keychain unlocked, which is the whole of phase 5's
problem.

**Order.** The expression rewrites; the registry's container step and executor requirement;
runner selection by executor; the `action.yml` parser and the inliner; the run volume, then
the container step. Composite actions are usable after the fourth with no executor touched.

### Amendment from a step timeout on the docker executor

| # | Section | Change | Why |
|---|---|---|---|
| A45 | §9, §13.1 | **On the docker executor a step that exceeds its timeout has its process group stopped in place — SIGTERM, the grace the native executor gives, SIGKILL — and the run container is left standing** | An interaction between two things that were each right alone. The Docker API cannot signal an exec, so a timed-out step had always taken the run container with it; and A42 phase 3 made a step timeout a failure like any other, after which later steps consult their conditions. Together, an `if: always()` cleanup after a timeout ran against a container that was already dead — on docker only, which §9 forbids: the step contract is the same on every executor. It surfaced while building A44, was written down as a known defect rather than shipped quietly, and the cheaper fix — treat a timeout as ending the run on docker — was declined in favour of this one |

**The mechanism, and the measurement it rests on.** Every step now runs under a constant
wrapper, `echo "$$" > "$0"; exec "$@"`, which records the shell's pid on the per-run volume
and then becomes the step. No part of a step is ever written into the wrapper — `run` is
still one argv element, whole and untouched, so INV-3 is as it was. Stopping a step is a
second exec, as root, that signals the negative of that pid. That is correct only if the
recorded pid is the id of a process group containing the step and nothing else, which is
true because the daemon starts every exec as the leader of a session and group of its own.
That was measured against a daemon rather than recalled, and an integration test asserts it
on every run, because if it ever stops being true the stop would signal the wrong processes.

**Two details that exist because the obvious spelling is wrong.** There is no `--` before the
negative pid: busybox's `kill` rejects it, and an explicit signal is what keeps a negative
number from being read as one. And the pid is validated before it is used — digits only,
never 0 or 1 — because the file lives on a volume the step can write to, and `kill -TERM -1`
as root signals every process in the container. A step that forges the file can get its own
processes signalled, which it could do itself; it cannot turn the stop into the
container-wide kill this amendment exists to avoid.

**When a step cannot be stopped in place** — no pid file, a forged one, an image with no
`sh` — the container is killed after all, and the step's log says that later steps cannot
run and why. A step that cannot be stopped must not be left running against the job's
clock, and losing the later steps is the smaller of the two failures.

**What it does not do**, and neither does the native executor: a process that has left the
group — a daemon that called `setsid` — is not signalled. On docker it dies with the
container at the end of the run.

### Amendment from a flaky test that was not a flaky test

| # | Section | Change | Why |
|---|---|---|---|
| A46 | §7.2, §11, §22 | **A broker's report persists only while the run is still in the state the report describes.** `store.ReapplyIfCurrent` is the guarded form, and the local broker's `Report` uses it | `TestARefOutsidePolicyIsParkedAndRunsOnceApproved` had been failing intermittently on loaded Linux runners for weeks and was carried as a flake. It was not one. It was reproducible with `GOMAXPROCS=1 -race`, at about one run in twenty, and the instrumentation added for it — printing the approval columns on failure — named the mechanism on the first capture |

**The defect.** §11 has the agent write local truth before it reports, and the local broker's
`Report` applied the transition again, reasoning that this made it "a self-transition — legal
and idempotent by construction (§7.2)". The word carrying too much weight was *construction*.
Reports are queued per run and delivered on a goroutine, so between the agent's write and the
broker's re-application the run can legitimately move — and the run that moves furthest is
precisely the one this test exercises. A run parked at gate 8; a human approves it, which pins
the commit and re-queues it (A27); the pool claims it; and only then does the park's own report
arrive. Re-applying it rewound the row to `awaiting_approval`, and A27's rule that entering that
state clears the approval did exactly what it should to a row nobody had re-parked. The
operator's decision was gone, the in-flight execution was left transitioning from a state the
row no longer held, and the run sat waiting to be approved a second time.

**Why the state machine could not catch it.** `preparing → awaiting_approval` is a real edge —
it is how a park happens — so a rewind along it is indistinguishable from a park by legality
alone. Nothing in an `Update` says which moment it describes, and the states either side of the
rewind are both legitimate, so the graph has no opinion.

**The guard, and what it cost.** `ReapplyIfCurrent` writes only while the run is still in
`u.State`; otherwise it writes nothing and says so, because a late report is a true statement
about the past rather than a fault. That makes §11's ordering a *precondition* of `Report`
rather than an expectation of it, which is the contract change: the previous doc comment claimed
`Report` was "correct for a caller that has not already written", and it cannot be, because such
a caller is indistinguishable from a stale one. Four tests in `internal/broker/local` relied on
that claim and now persist first, as §11 requires of the agent — and so did one caller that was
not a test: `Agent.report`, which reconciliation uses to mark a crashed run interrupted, leaned
on `Report` to do the persisting. It now writes local truth and then reports, which is the
ordering §11 always specified and the reason that caller was the one to notice. A report is still marked
reported when it is skipped: what a transport has seen is a fact about delivery, not about the
row, and leaving it unmarked would keep the update in `store.Unreported`'s replay set forever.

**What this says about the other flakes.** The instrumentation that solved this was added by the
commit that gave up on solving it, and it worked on the first failure it saw. An intermittent
test that has been rationalised as a flake is an unread bug report; the cheap move is not to
re-run it but to make its next failure explain itself.

### Amendment from saying what the project is

| # | Section | Change | Why |
|---|---|---|---|
| A47 | §1, §13.0, §13.2, §13.3 | **Docker and native are the two shipping executors, and native's target is every OS an operator runs — Linux and macOS today, Windows named as unimplemented. Tart is backlogged and nothing depends on it** | A positioning correction, and it is recorded because the documents had drifted into describing a narrower product than the one being built. §13.0 opened "Linux is not a choice: it is Docker", which was true when the native executor was written for macOS and is not true now — native builds and runs on Linux. The README had drifted further, opening on iOS. The thesis is owning the machines your CI runs on, whatever you build; an iOS release is the hardest case it has to pass, not its subject |

**What this settles about isolation, which phase 5 needs.** Docker isolates: a job reaches the
workspace, the caches it declared, and nothing else. Native does not, on any OS — a step runs as
the agent's user with everything that user can reach, and that is a property of running on the
host rather than a gap waiting on a VM. A42's §13.0 inversion was written as "Tart becomes the
floor" for a workflow containing third-party actions; with Tart backlogged that sentence has no
referent. It is replaced by: such a job requires an isolating executor, docker is the one, and
on native it is a per-machine opt-in that ships off (`[actions] allow_unisolated`). Refusing
outright is not available, because native is a first-class target rather than a stopgap.

**Windows native is a goal and is unimplemented.** A Windows host can run Docker jobs today; it
cannot run native ones. The executor is Unix-only by construction rather than by omission, and
the list is what a separate workstream has to answer:

- `internal/executor/native/proc_unix.go` is `//go:build unix`, and `proc_other.go` is a stub.
  The process-group contract §9 requires — SIGTERM, a grace, SIGKILL, to a step's whole tree —
  has no direct Windows equivalent; a job object is the usual answer.
- Every step runs `bash -lc` (§9, A33). Windows needs a shell decision that is not a
  translation: PowerShell and `cmd` differ from a login shell in what they inherit, and §9's
  "each step is a new process, the filesystem persists" has to survive whichever is chosen.
- `$LATEM_ENV` and the four GitHub files are appended to with shell redirection in every example
  and in §9's own. The channel is a file and survives; the idiom in the docs does not.
- Paths, the agent's own service installation (§23), and `doctor`'s machine checks are all
  written against Unix shapes.

None of that is on phase 5's path, and phase 5 does not wait for it.

### Amendment from shimming the action everything starts with

| # | Section | Change | Why |
|---|---|---|---|
| A48 | §9, §12, §13 | **`uses: actions/checkout` is satisfied by the checkout gate 10 already performed, needs no allowlisting, and is refused by name when it asks for something that checkout did not do.** A fetched action's own files are reachable through a new per-executor path, `latem.action_dir` | Almost every workflow opens with it, so a marketplace implementation that could not answer it would be a marketplace implementation nobody could use. And running the real one is not merely wasteful: it re-resolves the ref, which is a second answer in exactly the window INV-5's approval exists to close |

**The shim is asked before the allowlist**, and that ordering is the decision. Nothing is
fetched and nothing third-party executes, so there is no judgement for an operator to make —
whereas requiring `actions/checkout` to be allowlisted would fail every workflow on a machine
that had deliberately opted into no actions at all, for a step that does nothing.

*A53 supersedes this paragraph's `fetch-depth` answer and the next paragraph's `ref:` answer:
the workspace was never one commit, and a checkout may now move to a commit the policy admits.*

**What it will stand in for is enumerated, and everything else is refused by name.**
`persist-credentials` either way, because nothing is left in the workspace; `clean`, because
§12 gives every run a fresh clone; `fetch-depth: 1`, because one commit is what §12 checks
out; `submodules: false`; `path` at the root. The case that decides the shape is
`fetch-depth: 0`: it asks for full history, the workspace has one commit, and a build that
silently got a shallow tree fails later inside `git describe` or a version derived from tags —
a failure nobody traces back to a checkout step that appeared to succeed. Refusing it names
the input.

**`ref:` gets its own answer**, because the honest one differs. A checkout at a ref other than
the one the run named would resolve it a second time, which is the window INV-5 closes, so the
fix is to name the ref when the run starts — `latem run <repo> <job> --ref <ref>` — rather
than to re-check-out inside it. This repository's own `release.yml` is the worked example: it
passes `ref: ${{ github.event.inputs.ref || github.ref }}`, and the refusal says what that is
here.

**`github.action_path` becomes late for a fetched action.** A local action's path is relative
to the workspace, which is where a step starts, so it stays a literal. A fetched action lives
in a cache that is at one absolute path on the host and another inside a container — and which
of those applies is not known until a runner is chosen at gate 13, after the job was compiled
at gate 11. So it resolves at step time against `latem.action_dir`, an agent-owned context
filled from the executor's own answer, through the late-binding machinery A44 already built.
The docker executor bind-mounts the cache read-only, because a fetched action is shared between
runs and keyed on the commit it came from: a step that could write there would be editing what
every later run believes that commit contains.

**Still refused, by name:** a JavaScript action, which waits on the runtime question and on
phase 6; and a fetched composite action's own `uses: ./path`, which is relative to *its*
repository rather than to the checkout and needs a loader rooted at the fetched tree.

### Amendment from phase 6: the cache and artifact services, as shims

| # | Section | Change | Why |
|---|---|---|---|
| A49 | §14, §17, A42 | **`actions/cache` compiles into a mounted cache and `upload-artifact`/`download-artifact` into a step the run loop performs itself. GitHub's cache and artifact services are not implemented** | Phase 6 was scheduled as "the cache and artifact services" and was built as two shims instead, because the services cost more than the protocol: GitHub's current actions speak an RPC API at `ACTIONS_RESULTS_URL`, upload to a signed blob-storage URL it hands back, and authenticate with a runtime token the client decodes — and a container cannot reach the host's loopback, where INV-1 permits a listener, without either binding a non-loopback interface (INV-1) or host networking (the isolation phase 5 relies on). Both protocols also changed within the last two years with the old versions switched off. §14 already said caches are mounted rather than archived; the shim takes it at its word |

**The cache.** The `path:` becomes a cache declaration mounted for the whole job, named from
the path alone so that every job on a runner caching `~/.npm` shares one mount — §14.1's own
reasoning. `actions/cache/restore` and `/save` are the same shim. `cache-hit` is `'false'`,
substituted at compile time through A44's output machinery, so a condition gated on it is
answered at admission; `false` is the safe answer, because the install it gates runs fast
against a warm mount where `true` would let a stale mount skip needed work.

The semantic difference is recorded rather than hidden: a keyed cache replaces, a mount
accumulates. `key` and `restore-keys` change nothing about what is mounted — `runner.os` in a
key is free, since §14 scopes per runner, and a lockfile hash is the part whose meaning
changes. For content-addressed stores accumulating is what was wanted. Refused at compile
time, naming the line: a path inside the workspace (it would shadow the checkout), a pattern,
an exclusion, an environment variable, a path only known once the job is running, and
`fail-on-cache-miss` or `lookup-only`, which ask for exact-key semantics a mount cannot give.

**Artifacts.** A registry step may be an `ArtifactStep`, exclusive with a command and a
container, performed by the run loop rather than an executor — both ends of the copy are host
paths the agent owns, and a container reaching the artifact store would cross the boundary §17
keeps. It is a step rather than an end-of-job declaration because its `if:` decides whether it
happens. A literal naming a directory is collected recursively, decided at collection time
since at compile time it usually does not exist yet; this does not contradict A30, which
refuses a bare directory only because the author's intent is unknowable, whereas
upload-artifact's documentation defines it. Hidden files are excluded unless
`include-hidden-files` is set, matching upload-artifact v4.4's default — §17's globs match a
leading dot, so the default has to be written down. Download restores within the run; another
run's artifacts, and a selection across a matrix or job graph (N2), are refused by name.

**What this does to JavaScript actions.** Actions such as `setup-node` with `cache: npm`, and
`setup-go`, call the cache service themselves rather than through a `uses:` the compiler can
see, so no shim reaches them. The expectation — to be verified against the toolkit's source
when JavaScript actions are built — is that the toolkit finds no service configured and skips
caching with a warning rather than failing, in which case a latem mount on the same directory
does the job. The services remain possible later, under the same action names, if a workflow
needs exact-key semantics; native could have them cheaply on loopback, and docker would need a
relay inside the container's network namespace.

### Amendment from running a workflow the way GitHub runs it

| # | Section | Change | Why |
|---|---|---|---|
| A50 | §9, §15, A42, A47 | **A step compiled from a workflow runs under GitHub's contract rather than §9's: GitHub's shells, GitHub's environment variables, a per-run temporary directory, the repository as `owner/name`, `defaults.run.working-directory`, the `env` context, a 360-minute default, step `continue-on-error`, `runs-on` aliases, and `${{ secrets.* }}` and `${{ vars.* }}` resolved on the machine. `.latem/jobs.yaml` is unchanged** | A42 made a workflow file a registry and then ran its steps the way §9 runs a jobs.yaml step. The two contracts differ in places a workflow depends on without saying so. The one that decided this amendment: §9 hands `run` to `bash -c` (`-lc` natively), and GitHub hands it to `bash -e`, so a multi-line step whose middle line fails *succeeds* on latem and fails on GitHub: `bun test` followed by `bun run lint` in one `run:` reports whatever lint reports. Surveying the workflows of the first fleet this is meant to carry (79 files, 237 jobs) found the rest: `RUNNER_TEMP` read 96 times and never set, `defaults.run.working-directory` in 32 jobs and ignored, `${{ env.X }}` evaluating to "" although the job declares X, `github.repository` naming the config key rather than the repository, and `${{ secrets.X }}` in 46 files with no way for a workflow job to receive a secret at all |

**Shells.** A compiled step carries one of GitHub's three shells, and the executor builds the
argv from it: no `shell:` is `bash -e`, `shell: bash` is `bash --noprofile --norc -eo pipefail`,
and `shell: sh` is `sh -e`. `run` is still the last element of the argv, whole (INV-3). None of
them is a login shell, which is GitHub's behaviour and a departure from §13.3's `bash -lc`: on
the native executor a workflow step sees the agent's own environment — the service's PATH,
which `latem service install` writes with Homebrew's prefix in it — and not the operator's
profile. A step that needs a tool the profile would have put on PATH gets it the way it would
on GitHub, from a setup action or `$GITHUB_PATH`. A jobs.yaml step keeps `bash -lc`; §9's
contract for it is unchanged.

**The environment.** Every executor sets, beside the four `GITHUB_*` file paths it already
sets, `GITHUB_WORKSPACE` (the workspace as the step sees it) and `RUNNER_TEMP` (a directory per
run, emptied with the run, outside the workspace so that it never reaches `git status` or a
collected artifact). `runner.temp` reads the same directory. The run adds, for a workflow job
only: `CI` and `GITHUB_ACTIONS` set to `true`; `GITHUB_REPOSITORY`, `GITHUB_REPOSITORY_OWNER`,
`GITHUB_SHA`, `GITHUB_REF`, `GITHUB_REF_NAME`, `GITHUB_REF_TYPE`, `GITHUB_EVENT_NAME`,
`GITHUB_ACTOR`, `GITHUB_JOB`, `GITHUB_WORKFLOW`, `GITHUB_RUN_ID`, `GITHUB_RUN_NUMBER`,
`GITHUB_RUN_ATTEMPT`, `GITHUB_SERVER_URL`, `GITHUB_API_URL`, `GITHUB_GRAPHQL_URL`; and
`RUNNER_OS`, `RUNNER_ARCH` and `RUNNER_NAME`, from the runner as `runner.*` already is and in
the same spelling (`Linux`, `macOS`, `X64`, `ARM64`).
`GITHUB_ACTIONS=true` is a statement about the contract, not about the host: workflows and the
tools they call test it to decide whether they are in CI, and a workflow adopted onto latem is.
The agent's own `LATEM_*` names still win over all of these.

**The repository.** `github.repository` and `GITHUB_REPOSITORY` are `owner/name`. They come from
`[repos.<key>] github`, when set, and otherwise from the repository URL when it names a GitHub
host; a URL that names neither (a local path) falls back to the config key, as before, and the
run log says so. The config key remains what `latem run` takes.

**Defaults, `env`, timeouts.** `defaults.run.working-directory` and `defaults.run.shell` apply
at workflow and job level, a step's own setting winning. The `env` context holds the workflow's
and job's `env:` while the job compiles and adds the step's own for that step, which is what
GitHub evaluates `${{ env.X }}` against. A workflow job with no `timeout-minutes` gets GitHub's
360 minutes rather than §9's 30. A step with `continue-on-error: true` that fails lets the job
carry on: its `outcome` is `failure`, its `conclusion` `success`, and `failure()` in a later
step stays false, as on GitHub. `continue-on-error` on a job, and `if:` on a job, remain refused
here: both are about the job's place in a workflow run, which arrives with workflow runs
(they are N2's business, not this amendment's).

**`runs-on`.** A label passes through `[label_aliases]` before it is matched, each alias
replaced by the labels it names. GitHub's hosted labels map by default — `ubuntu-latest` and
`ubuntu-<version>` to `linux`, `macos-latest` and `macos-<version>` to `macos` — and a
`[label_aliases]` entry overrides or adds to that, which is where a vendor's labels such as
`blacksmith-4vcpu-ubuntu-2404` go. An alias may not name another alias. A `windows-*` label is
refused by name, since no runner can carry it (A47).

**Secrets and variables.** `${{ secrets.X }}` compiles into a late binding to the *name* X and
adds X to the job's secrets, so the value is resolved by §15's chain and registered with the
redactor before the first step, exactly as for a jobs.yaml job, and the compiled registry
holds the name and never the value — which is what A42's refusal protected. The binding is
evaluated at the step, so a value reaches a step only as an environment variable (A42's rule)
and is never written into `run`. A secret that resolves nowhere evaluates to the empty string
and the run log names it, which is GitHub's behaviour and what a workflow testing
`secrets.X != ''` relies on; §15's refusal of an unresolvable name still applies to jobs.yaml.
Resolution is scoped: a job naming an `environment` looks for `<repo>__<environment>__X`, then
`<repo>__X`, then `X`; any other job for `<repo>__X`, then `X` — so two repositories on one
machine can each have their own `CLOUDFLARE_API_TOKEN`. Two underscores, not a slash, because
the scoped name has to work as an environment variable and a `secrets.env` key as well as a
Keychain account; a character a name cannot hold becomes an underscore. A workflow job's
secrets are not put in the run's environment the way a jobs.yaml job's are: each reaches only
the step whose expression names it, which is GitHub's behaviour and keeps a credential away
from steps — and third-party actions — that did not ask for it. `${{ vars.X }}` reads `[repos.<key>.vars]`,
overridden by `[repos.<key>.environments.<environment>.vars]`; variables are not secrets, are
answered at admission, and may appear in the compiled registry. `GITHUB_TOKEN` is neither: it
arrives with workflow runs and the GitHub App, and until then `secrets.GITHUB_TOKEN` resolves
like any other name.

**What still is not GitHub's.** `permissions:` and `concurrency:` are parsed and not acted on;
the run log now says so for each rather than dropping them silently. An event payload and
`GITHUB_EVENT_PATH` (a made-up payload would mislead the actions that read it), `GITHUB_TOKEN`,
`needs`, matrices and reusable workflows wait for workflow runs.

### Amendment from running JavaScript actions

| # | Section | Change | Why |
|---|---|---|---|
| A51 | §9, §13, §14, A42, A48 | **A `runs.using: node*` action runs. Its `main`, and its `pre` and `post` when it has them, compile into ordinary steps whose `run` latem writes and whose inputs, paths and state arrive as environment variables; a persistent tool cache is the executor's, exported as `RUNNER_TOOL_CACHE`** | `oven-sh/setup-bun` is in 164 of the surveyed fleet's 237 jobs, and with `setup-node`, `wrangler-action` and `paths-filter` it is most of why A50's workflows still could not run unedited. A42 put JavaScript actions in phase 5, which shipped composite actions, the gate and the fetch without them; the remaining question was the runtime, and the answer is that there is nothing to build — the action is a file and `node` is a program, and both are already reachable from a step |

**One step per entry point.** `main` becomes a step that keeps the `uses:` step's id, name and
condition, so `steps.<id>.outputs` reads what the action wrote to `$GITHUB_OUTPUT` with nothing
declared — as on GitHub, where a JavaScript action's outputs need no `value:`. Its `run` is
latem's and constant, `exec node "$GHA_ACTION_DIR/$GHA_ACTION_ENTRY"`; the action's directory
and entry file are environment values, so INV-3 is untouched by what an action's metadata
says. The directory is a fetched action's late `latem.action_dir` path (A42 phase 5) or a
local action's path in the workspace, and `GITHUB_ACTION_PATH`, `GITHUB_ACTION_REPOSITORY` and
`GITHUB_ACTION_REF` are set beside it. `node` is the one on the step's PATH: the image's in a
container, the service's natively. The runtime's major version is not enforced; an action
built for node20 runs on a newer one, as GitHub itself now runs them.

**Inputs.** Each declared input, and each `with:` key, reaches the step as `INPUT_<NAME>` —
upper-cased with spaces made underscores and hyphens kept, which is GitHub's spelling and what
`@actions/core` reads. That is the one environment key a step may carry that is not an
identifier; the registry accepts `INPUT_` followed by letters, digits, `_`, `.` and `-`. A
default is an expression like any other value, so a secret or a step output in one is late
(A50). An undeclared `with:` key is passed and noted, as GitHub warns and passes it.

**`pre` and `post`.** `pre` runs at the start of the job, in the order of the steps that
declared it, under `pre-if` (default `always()`) and the step's own condition. `post` runs at
the end, in reverse order, under `post-if` (default `always()`) and only if the main step ran —
its outcome is not `skipped` — which is when GitHub registers a post step at all. State is
GitHub's: a step writes `name=value` to `$GITHUB_STATE`, a fifth file beside the four, and every
later step of the same use of the action receives it as `STATE_name`.

**The tool cache.** `setup-node`, `setup-python` and their kind install into `$RUNNER_TOOL_CACHE`
and expect to find the result there next time. It is a directory per runner that outlives
runs: on the native executor a directory under the agent's own state, in a container a named
volume `latem-toolcache-<runner>` mounted at `/opt/hostedtoolcache`, GitHub's path. Two runs on
one runner writing the same tool rely on the toolkit's own extract-then-mark-complete; a runner
at concurrency 1, which is every native runner, never has two. `AGENT_TOOLSDIRECTORY` names
the same directory, for the actions that read that instead.

**Policy is unchanged and now matters.** An action is fetched only when `[actions] allowed`
names it. A tag is accepted only with `require_pinned = false`, and the commit it resolved to
is written to the run log, so a moved tag is visible after the fact. A job that runs one needs
an isolating runner unless `allow_unisolated = true` — which a Mac running native Apple builds
will set, deciding that the owners in `allowed` are trusted with the agent's user.

### Amendment from running workflow runs

| # | Section | Change | Why |
|---|---|---|---|
| A52 | §1 N2, §5 INV-2 and G5, §7, §11, §12, A42 | **A GitHub event becomes a workflow run: the machine plans it from its own disk, the control plane schedules the plan's jobs across machines, and each job is one run on one machine, as before. The inbound command set grows to `plan`, `run` and `cancel`; `run` gains the event, its job's needs, and a GitHub installation token minted by the control plane. Protocol 2.0** | A42 made a workflow a registry and ran one of its jobs at a time, chosen by name. In the fleet this is for, 130 of 237 jobs use `needs:`, 87 an `if:` on the job, 25 files gate merges on pull-request checks, 20 use `GITHUB_TOKEN`, and every template repository has a job named `web` in three files — so "a job, by name" is ambiguous before it is anything else. N2's "fan-out is a v2 concern" is that concern arriving |

**The model.** An event (push, pull request, release, repository dispatch, or a dispatch or
schedule the control plane makes) produces zero or more workflow runs — one per workflow whose
`on:` it matches — and each workflow run is a graph of jobs. Every job is still exactly one
run on exactly one machine (N2 holds per job); a workflow run spans as many machines as its
jobs land on. A machine never coordinates with another: everything between jobs goes through
the control plane, outbound, as everything already does (INV-1).

**Planning is the machine's (INV-5).** The control plane holds no workflow definitions and does
not parse workflow files. It sends a `plan` command to one machine that has the repository;
that machine fetches, checks the event's commit out of its own mirror, reads
`.github/workflows` there, and answers with a plan: for each workflow the event triggers, its
file, name, evaluated `run-name`, evaluated `concurrency`, and its jobs — each a key, a display
name, `needs`, `runs-on` after aliases (A50), environment, `permissions`, job-level
`concurrency`, `continue-on-error` and `timeout-minutes`. Structure and names, never steps.
Triggering is evaluated as GitHub evaluates it: the event's name and, for pull requests,
`types` (default opened, synchronize, reopened); `branches`/`branches-ignore`, `tags`/
`tags-ignore`; `paths`/`paths-ignore` against the event's diff, computed in the mirror — a push's `before`
against its commit, or the commit against its first parent for a new branch, and a pull
request's merge commit against its first parent, which is the base branch's tip;
`workflow_dispatch` inputs validated against their declarations. A workflow that cannot be
parsed is in the plan as an error, so it becomes a failed check rather than a silent absence.
One parser, Go's, serves planning and execution, so the plan and the job that runs cannot
disagree about what the file says.

**Scheduling is the control plane's.** It keeps the workflow run and dispatches a job when every
job it needs has finished, to a machine that is online, has the repository, and advertises a
runner whose labels cover the job's. A job no online machine can take stays queued and says
which labels it is waiting for. Workflow- and job-level `concurrency` is enforced here, with
`cancel-in-progress` cancelling the older run, because no single machine sees both. Machines
advertise their repositories in their heartbeat — each one's config key beside its `owner/name`
(A50's `github`) — which is how an installation's repository finds its machines, and which key
to name it by in `plan` and `run`.

**`run` in 2.0.** Beside `id`, `repo`, `job`, `ref`, `inputs` and `origin`, a run of a workflow job
carries: `workflow`, the file, constrained to `.github/workflows/<name>.y(a)ml`; `sha`, the
commit to build, which must be reachable from `ref` — the run builds what the event named, not
whatever the branch says by the time the job starts; `event`, the event's name, action, refs,
pull-request number and payload (capped, and data: it reaches a step only as an environment
value or through `GITHUB_EVENT_PATH`, A42's rule); `needs`, each needed job's result and outputs;
`workflow_run_id`, `run_number` and `run_attempt`, the workflow run's, shared by its jobs, which
`github.run_id`, `github.run_number` and `GITHUB_RUN_NUMBER` now read ($LATEM_BUILD_NUMBER keeps
the machine's own, A36); and `token`. These fields are all set or all absent.
Still no field can express a command, an image, an executor, a timeout or a label: the job is
compiled from the agent's disk at `sha`, as every job is.

**A job's place in the run.** The job's `if:` is evaluated on the machine as the run starts, with
`needs` and the event in scope; false ends the run in a new terminal state, `skipped`, which
is neither success nor failure, as on GitHub. The job's `outputs:` are evaluated as it ends and
reported in its final update; an output containing a value the redactor holds is dropped and
named in the log, as GitHub drops one carrying a secret. `continue-on-error` on a job is the
control plane's to honour, in the workflow run's conclusion. A job whose needs did not all
succeed is skipped by the control plane without asking a machine — GitHub's implicit
`success()` — unless its `if:` calls a status function (`always()`, `failure()`,
`cancelled()`, `success()`), which the plan reports per job; then the machine evaluates it.
A plan's own expressions — `run-name`, `concurrency`, a job's `name` and `environment` — read
`github`, `inputs` and `vars` only; `github.run_id`, `run_number` and `job` are not known yet
and are refused by name rather than read as empty, since a concurrency group that silently
lost its run id would put every run in one group. GitHub stops matching `paths` after 300
changed files; the planner does not.

**Checks.** The control plane creates a check run per job when the plan arrives — queued — and
moves it with the job: in progress, then completed as success, failure, cancelled, timed out
or skipped. Its name is the job's display name, which is what branch protection already
requires, so cutting a repository over changes which app reports the check and not what it is
called. Its details link to the run on app.latem.dev.

**`GITHUB_TOKEN`, and the narrowing of G5 and §12.** The GitHub App's key is the control plane's
(the operator's decision for this fleet). It mints an installation token per job, scoped to
the repository and to the job's `permissions` (the workflow's when the job has none, GitHub's
restricted default when neither does), and sends it in `run.token`. §12 said the agent never
receives a credential and G5 that it is safe to point at an untrusted control plane; both are
narrowed, deliberately and only this far: the agent may receive one short-lived token for one
job. It is held in memory only — stripped from the run request before the request is made
durable, registered with the redactor before the first step — and offered to the job as
`secrets.GITHUB_TOKEN` and `github.token`, and to the checkout as its credential. A control
plane that turns hostile can mint tokens with the app's permissions, which it can do with or
without latem; it still cannot make a machine run anything not in the repository at the
commit, nor read a secret the machine holds (INV-4 is unchanged). A token is valid for an hour;
a job that outlives it is on its own until refresh exists. An event whose sender is the app
itself is ignored on arrival, which is GitHub's rule that a push made with `GITHUB_TOKEN`
triggers nothing, and what the fleet's version-stamp jobs rely on.

**INV-2 in 2.0.** The inbound set is `plan`, `run`, `cancel`, and no more. `plan` carries an id, a
repository key, the commit and the event: names and data. Nothing inbound can name a path
other than a workflow file, and that only by pattern.

**What 2.0 costs.** Strict decoding at both ends (PROTOCOL.md §2.3) means a 1.0 agent refuses a
2.0 frame, loudly; the control plane speaks the version the agent sends at the handshake, so a
machine is upgraded before it is pointed at workflow runs, and a 1.0 agent keeps working for
the hooks it has.

**Not in this amendment.** Matrices, reusable workflows and `schedule` (the planner reports them
as unsupported in the plan, by name); artifacts across machines; reviewer approval on an
environment. Each has a place in this model — a matrix leg is a job key, a called workflow's
jobs are planned into the caller's graph, a schedule is a cron the plan of the default branch
reports — and each arrives as its own amendment.

### Amendment from deploying with a workflow

| # | Section | Change | Why |
|---|---|---|---|
| A53 | §5 INV-5, §9, §12, §13, A48, A52 | **`uses: actions/checkout` is a step the agent performs. `ref:` moves the job's working tree — never its definition — to another commit of the same repository, resolved as the step runs against a freshly fetched mirror, and only to a commit the repository's policy already admits. `fetch-depth` and `fetch-tags` are satisfied by §12's clone, which was always complete. The job's token is the checkout's credential for the rest of the job, kept outside the workspace, unless `persist-credentials: false`. `executor.Env` gains `PutTemp`** | The fleet's deploy workflows stamp a version in one job — `checkout` with `token:`, `git commit`, `git push` — and deploy it in the next, which checks out `ref: main` to get the commit the first one pushed. Production rolls back with `ref: ${{ inputs.ref \|\| 'main' }}`. A48 refused all three, so no deploy workflow ran unedited. Its reason for refusing `fetch-depth: 0`, "the workspace has one commit", was never true: `git clone --local` hard-links the mirror's whole object store and copies every branch and tag |

**What still comes from the pinned commit.** Everything gate 11 compiles: the job, its steps,
its expressions, its local actions' metadata. `github.sha`, `GITHUB_SHA` and `LATEM_SHA` stay
the run's commit, which is also what GitHub does. A checkout changes only what is on disk, so
two things now differ from the commit the run names, and both are stated rather than left to be
discovered: a `run: ./script` reads the checked-out tree, and so do a local action's files,
whose metadata was compiled from the run's commit.

**Which commits a checkout may reach.** The pinned commit, or a commit reachable from the tip of
a ref the repository's `allowed_refs` admits, in the mirror as just fetched. Reachability rather
than a name match is what lets the deploy workflows through — `needs.bump.outputs.sha` is a
commit on `main`, and a rollback tag is a tag whose commit is on `main` — and it is what keeps
INV-5's meaning: a checkout target's code runs with the job's secrets (`bun install`,
`convex deploy`), so it may only be code the policy already trusts. Anything else fails the step
with `E_STEP`. It does not park: there is no `running → awaiting_approval` edge, and an approval
names only the run's own commit. `repository:` is accepted when it is the run's own repository;
any other is refused.

**Resolved as the step runs.** `ref:` is evaluated in the step's scope, so admission-time values
(`inputs`, `needs`, `github`) and late ones (`steps.*`) both work. The mirror is fetched first,
because `ref: main` exists to see a commit an earlier job of the same run pushed. A full hex
value is a commit; anything else is normalized as gate 7 normalizes a run's ref, ambiguity
included. A branch is checked out as a local branch at that commit with `origin/<branch>` and
an upstream set, so a bare `git push` works, as after GitHub's checkout; a tag or a commit
detaches. Gate 10's own checkout is unchanged: a job without a checkout step still starts
detached at the pinned commit.

**What is recorded.** The run log names the ref as written, the ref it normalized to, the commit
checked out, the run's commit beside it, and the policy ref whose history admitted it. A run is
reproducible from its commit and the commits its log names.

**Where it runs.** The agent runs git only in the mirror, which it owns: the ref's resolution,
the reachability question, and the pack below. Everything that touches the workspace's `.git`
runs in the job's environment as the step's own process — fixed text, `sh -e`, handed the commit,
the branch, and the paths of the pack and the credential file through its environment (INV-3).
Every earlier step can rewrite the workspace's `.git`, through docker's bind mount as much as
natively, and a git the agent ran there on the host would execute what it found: a hook, an
fsmonitor, a filter driver, a `url.insteadOf` that turns the mirror's path into an ssh command.
That is a container step running code as the agent. Run as the step, the same git is the job
acting on itself, which it always could. The environment therefore needs git, as GitHub's
checkout needs it in a container job.

**History.** Nothing is built. The workspace already holds every object, branch and tag the
mirror had when gate 10 cloned it. For a commit the mirror has fetched since — the one an
earlier job pushed — the agent packs, in the mirror, the objects that commit's history holds and
the run's commit's does not, hands the pack over through `PutTemp`, and the switch indexes it
before it checks out: no credential, no network, and refused past 256 MB, because a checkout is
not a second clone. `fetch-depth` of any value and `fetch-tags` are accepted and answered by
that. A tool that runs `git fetch --unshallow` fails, as it does on GitHub with
`fetch-depth: 0`.

**The credential.** A52 offered the job's token "to the checkout as its credential"; this is
where it is taken. Unless `persist-credentials: false`, the token — the job's, or `token:`'s
value, which must be an expression; a literal token in a workflow is refused — is written as
an `http.https://github.com/.extraheader` into a file in the job's temp directory through
`executor.Env.PutTemp`, and the workspace's `.git/config` only includes that file. So the token
is never in the workspace (an upload of `.` cannot carry it), never in a step's environment
(A50's rule), and never in a git argv (`ps`, and `gitError`, which quotes argv); it is gone when
the environment is closed or reaped. The token and its base64 form are registered with the
redactor before the file is written. This narrows §12's "the agent never receives, stores, or
forwards git credentials" by exactly this much: a job token, for one job, used by that job's
own git. Any later step of the job can use it, third-party actions included — GitHub's exposure,
taken on purpose so the fleet's workflows run unedited.

**The push goes out as the App.** latem-cloud ignores an event whose sender is the App itself
(A52), which is what keeps a version-stamp push from triggering the workflow that made it. On
the native executor the operator's `~/.gitconfig` could route a push elsewhere — an SSH
`insteadOf`, a keychain credential helper — and send it as a person, which would re-trigger the
workflow on every push. So `origin` and `remote.origin.pushurl` are set to
`https://github.com/<repository>.git`, a self-mapping `url.<that>.insteadOf` outranks the
operator's prefix, the credential helpers are emptied for the workspace, and the step fails —
taking the credential's include back out — if `git remote get-url --push origin` then answers
anything else. An operator rule for the very same URL, read first, wins a tie, which is why the
check is there.

**Still refused, by name.** Another repository; `submodules` and `lfs` other than false (§12
already handles both for the run's own commit); `sparse-checkout`, `filter`, and the `ssh-*`
inputs. A48's other answers stand.

### Amendment from planning a matrix

| # | Section | Change | Why |
|---|---|---|---|
| A55 | §5 INV-2 and INV-5, A42, A52 | **`strategy.matrix` is planned. The machine expands a matrix into legs, by GitHub's rules, and each leg is a planned job: the job's key, its own name, labels, environment and concurrency, and its matrix values. The control plane schedules legs as jobs, and a job that needs a matrix waits for every leg. `run` gains `matrix`, which the machine accepts only if it is one of the legs it expands from the pinned commit. `runs-on` may be an expression the plan answers. Protocol 2.1** | A52 left matrices out, and the planner has reported them as unsupported since. Nine jobs in the fleet are matrices — latem's own `check` among them, one job on Linux and macOS through `runs-on: ${{ matrix.os }}` — and a matrix job cannot be run at all, because its name, its runs-on and its environment all read the matrix: there is no un-expanded version of it to run |

**Expansion.** GitHub's, in its order. The matrix's keys, as written, are the dimensions; their
cross product, first key outermost, is the base. `exclude` is applied first: an entry removes
every combination whose values match all of the entry's keys. `include` is applied next, in
order: an entry extends each combination whose original values it matches — adding its other
keys, which may overwrite a value an earlier `include` added and never an original one — and an
entry that matches no combination is a combination of its own. A value may be an expression,
and so may the whole matrix, `include` or `exclude`, if the plan's scope answers it (`github`,
`inputs`, `vars`); one that reads `needs` — a matrix computed by an earlier job — is not planned
yet, and says so. A dimension with no values, a matrix with no combinations, and more than 256
legs are the workflow's errors, as on GitHub. Equality is a value's: `14` is not `"14"`.

**Key order is the file's.** The parser reads YAML into maps, which forgets order, and a leg's
name and position depend on it: `{os: [a], node: [1]}` is `build (a, 1)` on GitHub, and branch
protection names checks by that string. So the order of a matrix's keys, and of each `include`
entry's, is read from the file itself.

**A leg.** The job's key; its name — the job's `name:` evaluated with the leg's matrix, or the
key, followed by ` (v1, v2, …)`, the leg's values in key order, unless the name reads `matrix`;
and its labels, environment, concurrency, `continue-on-error` and `timeout-minutes`, each
evaluated with the leg's matrix. `runs-on` may therefore be an expression, here and in a job
without a matrix, if the plan's scope answers it; a label it yields is checked as a written one
is. A leg carries its values as `matrix`, and `strategy`: its index, the number of legs,
`fail-fast` (default true) and `max-parallel` (default unbounded). The job's `if:` cannot read
`matrix`, as on GitHub: it is answered once, for every leg.

**Scheduling is the control plane's.** Legs are jobs. A job that needs a matrix waits for every
leg; `needs.<key>.result` is `failure` if a leg failed, else `cancelled` if one was cancelled,
else `skipped` if every leg was skipped, else `success`; `needs.<key>.outputs` are the legs'
outputs merged in the order the legs ended, last writer winning, as GitHub documents.
`fail-fast` cancels a failed leg's queued and running siblings, unless the failed leg continues
on error. `max-parallel` caps how many legs run at once.

**A leg is the file's, not the control plane's.** `run.matrix` is data, as every inbound field is
(INV-2): it reaches a step only through an expression, which A42 binds to an environment
variable. And the machine does not take a leg on trust. Compiling a matrix job, it expands the
matrix itself, from the pinned commit (INV-5), and requires the run's values to be exactly one
of the legs — so a control plane cannot invent a leg the file does not declare, and cannot use
one to reach a runner the file does not name. That expansion is also where `strategy.job-index`
and `strategy.job-total` come from. A matrix job run without a leg, or a leg sent for a job with
no matrix, is `E_BAD_REGISTRY`.

**Protocol 2.1.** `run.matrix` and the plan's `jobs[].matrix` and `jobs[].strategy` are
additive, and §2.3's strictness makes every additive change a refusal at the other end: a 2.0
agent refuses a run carrying `matrix`, and a control plane that does not know the plan's new
fields refuses the plan. So the control plane is upgraded first, and sends a leg only to a
machine whose frames say 2.1 or later.

### Amendment from making a machine easy to adopt

| # | Section | Change | Why |
|---|---|---|---|
| A54 | §8, §15, §19, §23 | **Onboarding is commands, not hand-written TOML. `latem service reload` and a wired SIGHUP make a config edit take effect without a restart, and the list of what reloads is narrowed to what the agent actually re-reads** | Bringing the first real machine onto Path B took a hand-written `config.toml` (repo URLs, `refs/pull/*/merge`, `[label_aliases]` for Blacksmith labels, an `[actions]` allowlist), a machine token pasted into `register`, and a restart after every edit, because SIGHUP was documented and nothing handled it. Go's default action for an unhandled SIGHUP is to exit, so the documented way to reload was a way to stop the machine |

**Reload.** `latem agent` handles SIGHUP. The agent re-reads `config.toml` through
`config.Reloader`; a file that does not load is refused and the agent keeps the config it had,
because an operator mid-edit is not a reason to kill builds in flight. What a reload applies is
what the agent reads afresh: the repo allowlist with each repo's refs, vars and environments,
`[label_aliases]`, `[actions]`, and `log_retention`. It reaches the next admission, the next
plan, and the next heartbeat's advertised repositories; a run already admitted keeps what it was
admitted under. Everything consumed once at startup is restart-only and reported as such:
`[[runner]]`, `[machine] id`, `agent.work_dir`, `metrics_addr`, `[broker.local]`, and — new in
this amendment, because no reload ever reached them and §8 said they reloaded — `[broker.remote]`,
`[secrets]`, `queue_ttl`, `remote_logs`, `shutdown_grace` and `auto_update`. Rotating the machine
token's contents still needs nothing: the remote broker re-reads its file on every dial.

`latem service reload` checks the file first and sends nothing if it does not load. It signals
only the process holding the config directory's flock — the kernel's word that it is the agent,
which a pid file alone cannot give — and only when that process has written
`~/.latem/agent.reload` naming itself. The agent writes that file when it has started and after
every reload, with the reload count, the error if it refused one, and the changes it deferred;
the command waits for the count to move and prints the outcome. An agent built before this
amendment writes no such file, and is not signalled: SIGHUP would stop it.

**No repos is a legal config.** It is the state `latem setup` leaves a machine in — registered,
serving, and rejecting every run — until `latem repo add` gives it one; the loader used to
refuse it, which meant the agent could not start until a repo was written by hand. `latem
doctor` warns about it and names the fix.

**`latem repo add <owner/name>`** writes `[repos.<key>]` — the key is the repository's name,
lowercased into §8's key characters, or `--key` — with `url = "https://github.com/<owner>/<name>.git"`
and `allowed_refs` set to the repository's default branch (as `git ls-remote --symref` names it)
and `refs/pull/*/merge`. It first runs that `ls-remote` in the agent's git environment: prompting
off, the git state variables dropped as `internal/source` drops them, and no `SSH_AUTH_SOCK`,
since a service does not inherit one. A repository that answers only with the operator's ssh
agent is refused with that diagnosis, as `doctor`'s repo check gives it; `--no-verify` skips the
probe. `repo remove` deletes the table and its `vars` and `environments` subtables. Both edit the
file as text — the table found by its header, everything else byte for byte — and refuse an edit
whose result would not load when the original did, then ask the running agent to reload.

**`latem secrets set|list|rm [--repo R] [--env E] NAME`** stores a value under
`secrets.ScopedName(NAME, R[, E])` — exactly the name a workflow job's lookup tries first for that
scope, so the command cannot file a value under a name no run reads. The value comes from stdin
(a one-line value loses the one trailing newline `echo` adds; a multi-line value is kept as it is)
or from a prompt with echo off, and the command takes no argument that could carry it. On macOS it
goes to the Keychain through `security -i`, which reads `add-generic-password -U -s latem -a <name>
-X <hex>` from stdin: the value is never in a process's argv, and hex carries a PEM's newlines
without quoting. `security -i` runs at most 4095 bytes of a line and the rest as a command of its
own — a truncated value written over the good one, and the remainder echoed as an unknown command
— so a value whose line would pass 4000 bytes (about 1,950 bytes of value) is refused with
`--store file` named, and a run of hex in anything security prints is elided before it is shown.
The prompt reads with canonical mode off as well as echo, because the terminal's line discipline
holds 1,024 bytes of a line on macOS and drops the rest. Two `[repos.*]` keys that fold to the same
scoped prefix — `my-app` and `my_app` are both `my_app__` — would each read the other's secrets, so
the config that has both does not load. Elsewhere, and with `--store file`, it is a line of `secrets.env` in the double
quoting the file provider reads back exactly, every other line kept, written through a 0600
temporary and a rename; a file that does not parse is refused rather than rewritten. `list` reads
names only — the Keychain's from `security dump-keychain` without `-d`, the file's from its keys,
and `[secrets.commands]` from config — and decodes each back into its scope. `set` warns when
`secrets.env` holds the same name, because the file is read before the Keychain.

**`latem vars set|list|rm --repo R [--env E] NAME [VALUE]`** edits `[repos.R.vars]` or
`[repos.R.environments.E.vars]` as text, creating the table beside its repository, and reloads.

**`latem workflows check [path]`** runs `workflow.PlanEvent` over `os.DirFS`-style access to a
local checkout — through an `os.Root`, as the agent reads a workspace — for a push to the
default branch (the repo's first plain `allowed_refs` entry, or `--branch`), a pull request into
it, and a `workflow_dispatch` of every file declaring one, with each required input that has no
default given its first option, `false`, `0` or a placeholder. A paths filter is matched against
every file in the checkout. Each planned job is then compiled with `CompileWith` as a workflow
run's job, its `if:` blanked — the question is whether this machine could run it, not whether
this event would — with every need succeeded and the machine's `[label_aliases]` and vars. Remote
actions are compiled against a permissive policy and an offline fetcher that records each
reference and answers with a placeholder JavaScript action; the machine's own `[actions]` policy
is applied to that record afterwards, so one refused action neither stops the compile nor hides
the next. Nothing is fetched and nothing runs. Per job it reports: the runner gate 13 would pick
(labels, a docker runner for container steps, an isolating runner for remote actions unless
`allow_unisolated`); a `runs-on` label no runner carries, with a `[label_aliases]` line mapping it
to `linux` or `macos` when its name says which; an action outside the allowlist or not pinned;
secrets the compiled job declares (less `GITHUB_TOKEN`, which the control plane mints) that no
store holds under any scope the run looks in, from names alone; and the compiler's own refusals.
A matrix job (A55) is compiled once per leg the plan made, with that leg as `Options.Matrix` —
exactly the `run.matrix` a run of it would carry — and reported per leg, because `runs-on` may
read the matrix. Alias suggestions therefore come from the compiled labels: one the compiler
produced itself (`linux`, `macos`, `arm64`, or an existing alias's target) is a platform this
machine lacks; any other is a vendor's label as written, and an alias is the fix.
Vars referenced in the file with no value are a warning, as they read as empty. `--fix` writes
the alias and allowlist entries, through the same text edit and load gate as the other commands,
and reloads; it never changes `require_pinned` or stores a secret. The checkout is matched to a
`[repos.*]` entry by its `origin` remote or `--repo`.

**`latem register` with no token** runs RFC 8628's device flow against the control plane behind
`--url`, which now defaults to the hosted one, `wss://gw.latem.dev/agent`: `POST <base>/claim` with
the machine's name, OS, architecture, labels and version, then `POST <base>/claim/token` every
`interval` seconds — slower by 5 s after each `slow_down` — until the person who opened the
printed link approves it, declines it, or it expires. The contract is PROTOCOL.md §1's. What
follows is unchanged and is the point: the claimed token goes through `remote.Verify` before
`remote.WriteToken` and `writeRegistration`, so a token that came from a browser is held to the
same rule as one pasted in — proven, then written, and nothing written on any failure. No token
is ever printed, and the device code is not either.

**`latem setup`** is the guided path for a fresh machine, and each step skips what is already
done, so running it again is safe. (1) With no `config.toml` it writes `init`'s template with no
repositories: a native `macos` runner on darwin, plus a `linux` docker runner when a Docker
daemon answers a ping — on Linux, always — whose `default_image` is `--image` or the template's
placeholder until the published `latem-ubuntu` digest replaces it. A config that exists is left
alone; one that does not load stops setup, because the agent would not start on it. (2) Unless
the machine already has `[broker.remote]` and a readable token, or `--no-register`, it runs
`latem register`'s device flow. (3) It installs the user service (§23), or restarts it when steps 1
or 2 changed anything or it is installed and not running. Registration comes before the service
rather than after because `[broker.remote]` is restart-only: the other order starts the agent
twice. (4) It runs doctor, and exits with doctor's verdict. `install.sh` now ends by naming it.

### Amendment from updating by hand

| # | Section | Change | Why |
|---|---|---|---|
| A56 | §19, §20, A41, A54 | **`latem update` installs the latest release, verified exactly as §20's self-update verifies one, and SIGUSR2 moves the running agent onto it at its next idle moment, in place. Every other command says once a day, on a terminal, when a newer release exists. A version `git describe` stamped — a build from commits after a tag — compares as newer than that tag** | A machine moved onto a new latem only by someone building it and restarting its service: `auto_update` is off by default, daily, and silent. And the comparison §20 rests on was wrong for every build from source. The Makefile stamps `git describe --tags --dirty`, so a machine built 69 commits after v0.1.2 is `v0.1.2-69-gbf886f2`, which semver reads as a prerelease of v0.1.2 and sorts before it — so the self-update offered that machine v0.1.2, the downgrade item 2 refuses, arriving through the parser |

**The command.** `latem update [--check]`. It is A41's chain, unchanged: the feed supplies a
version and nothing else, the signed manifest binds every archive to its version's name, the
release must be strictly newer than this binary, and the downloaded binary is asked its version
before it replaces anything. What it replaces is the file the command is running from, symlinks
resolved, staged beside it and renamed over it. It refuses a `dev` build, which has no version to
compare, and a build that trusts no key, which can verify nothing. `--check` reports and changes
nothing. Up to date, it still asks the running agent to move onto this binary, which is how an
agent left on an older file — replaced by hand, or restarted before a move it was waiting on —
catches up.

**Moving the agent: SIGUSR2.** The signal names no file and no version. The agent asks the binary
at its own path what it is (`version --short`), and if that is what it runs, the answer is
`current` and nothing pauses. Otherwise it does what §20 item 5 does: stop claiming, wait for the
runs in flight, re-exec in place. The pid survives the exec, so launchd and systemd see a service
that never stopped, and an agent run in a terminal moves too. Two choices differ from the daily
self-update, both because a person asked for this one. The wait is `shutdown_grace`, the time this
operator has already said runs may need to finish, not ten minutes. And running out of it is not
"tomorrow": the agent takes work again and moves at the first moment nothing is running, checked
every 30 seconds. It never cancels a run to move. While a move is pending the daily self-update
skips its check, so the two never pause and resume claims over each other.

**Who may be signalled.** As A54 decided for SIGHUP: only the process holding the config
directory's flock, and only when its status file says it handles the signal. Go's default action
for an unhandled SIGUSR2 is to exit, so signalling an older agent would stop a build machine to
update it. A54's status file gains `version`, `binary` and `switch` (the answer to the last
SIGUSR2: `current`, `waiting` with the number of runs, `switching`, or `failed` with the reason).
An agent that writes a version handles SIGUSR2; one that writes none predates this amendment, and
the command says `latem service restart` instead. The command waits up to 30 seconds for the
answer: the new image's first status with the same pid, or `waiting`, `current` or `failed`.

**The notice.** After every command except `agent`, `update` and shell completion — and only when
stderr is a terminal, `CI` is unset and `LATEM_NO_UPDATE_NOTIFIER` is unset — the command prints
three lines to stderr when the feed's latest release is newer than it: the two versions, `To
upgrade, run: latem update`, and the release's page. The answer is remembered in
`<user cache dir>/latem/release.json` for 24 hours. A stale one is refreshed in the background
while the command runs, and a finished command waits at most one second more for it; an answer
that arrives later is still remembered for the next command. A failed check is remembered too, so
an offline machine, or one reading a feed that is not public, asks once a day and prints nothing.
The read needs no key, because nothing it returns reaches disk. It never fails a command. One
request a day per user is far inside api.github.com's 60 an hour per address.

**Versions past a tag.** A version ending in what `git describe --tags --dirty` appends —
`-<commits>-g<abbreviated commit>`, `-dirty`, or both — is past that tag. It is newer than the tag
and than the tag's prereleases, and older than anything newer than the tag. Two builds past the
same tag compare as the same, so neither is offered the other. Release tags never have this
shape.

### Amendment from reading a repository without a checkout

| # | Section | Change | Why |
|---|---|---|---|
| A57 | §12, §19, A52, A54 | **A command reads a repository's files without a checkout: the agent's mirror, read and never written, or a temporary blobless clone of the default branch. `latem workflows check --repo R` with no path checks a repository that way, `.latem/jobs.yaml` included, and `latem setup` gains a repositories step that adds one, checks it, and offers what its workflows need** | Onboarding asked for a checkout before it could say anything about a repository — `latem workflows check <checkout>` — on a build machine, which is the one place a checkout is not lying around. And everything the guided-setup work wants to know next (what kinds of builds a machine will run, what it is missing for them) starts from reading the repositories' workflows on the machine, from the command line, beside a running agent |

**Reading.** `source.NewReader` is a manager that only reads: it makes no directory, checks no
filesystem, and refuses `Fetch` and `Checkout`. `Manager.Open` finds the agent's mirror of a
repository without fetching, or says there is none yet (the agent makes one on its first plan
or run). `NewSnapshot` makes a bare, one-commit, blobless clone (`--depth=1 --filter=blob:none
--single-branch`) in a directory the caller owns and removes. `Mirror.Tree` reads a commit's
regular files as an `fs.FS`: the listing at once — without `ls-tree -l`, because in a blobless
clone a blob's size is a fetch — and each file when it is opened, under a cap. A symlink or a
submodule is not there at all, as A52's `ReadDir` leaves them out.

**A command never writes a mirror.** Fetches into one bare repository race, and the lock that
serialises them (§12) lives inside the agent's process, so a command typed beside a running agent
reads, by object id, from objects that are immutable, and fetches nothing into it. A repository
the agent has no mirror of is read from its own remote instead, into a directory of the command's.

**`workflows check --repo R` with no path** reads R's default branch from its remote, through the
blobless clone — only the files the check reads are downloaded — or with `--offline` from the
agent's mirror. `hashFiles` in a repository read this way resolves against an empty directory:
a check needs the job to compile, not the hash. `.latem/jobs.yaml`, when the repository has one,
is checked beside the workflows; its `runs_on` labels are matched as written, since
`[label_aliases]` maps a workflow's `runs-on` and not these. `--json` prints the report for a
script, with the same exit code.

**`latem setup`'s repositories step**, second of five. It adds the repositories `--repo` names, or —
on a terminal, with none configured — asks which, one at a time. Each is added as `latem repo add`
adds it, then read without a checkout (the mirror if there is one, else the remote) and checked:
how many jobs this machine can run, why not the rest, and the `[label_aliases]` and `[actions]
allowed` entries that would let more run. On a terminal it asks before writing them; with `--repo`
it says how to (`latem workflows check --repo R --fix`). A repository that cannot be read is
reported and does not stop setup. Off a terminal with no `--repo`, the step asks nothing.

**Found while building it.** The cap on git's output that A52's `ReadDir` and A53's checkout pack
rely on (`cappedBuffer`) was never consulted: it embedded `bytes.Buffer`, whose promoted `ReadFrom`
`io.Copy` prefers to `Write`, so a blob or a pack of any size was read whole. `ReadDir` hid it by
checking sizes from `ls-tree -l` first; A53's 256 MB limit on a checkout's pack did not hold. The
buffer is now a field, and a test copies past the cap.

### Amendment from running latem's own CI on latem

| # | Section | Change | Why |
|---|---|---|---|
| A58 | §13.0, A42, A47 | **A `uses:` step whose `if:` reads only `runner.os` and the matrix leg, through functions that depend on their arguments alone, and is false on the platform the job's labels settle, is decided at compile: its action is never fetched and asks for no isolation. The step stays, with its id, name and `if:`, and is skipped when reached** | latem's own `check` job runs `actions/setup-go` on Linux only (`if: runner.os == 'Linux'`) and its matrix runs the job on macOS too. On a native macOS runner the job was refused for using a remote action (A47) — an action that step would never have run there. A workflow written for both platforms should run on both, without its author choosing between isolation and a second job |

**When it is decided.** The platform is `runner.os` as the job's labels settle it: `macos` alone means
`macOS`, `linux` alone means `Linux` — labels the compiler produces itself for GitHub's hosted
labels and that an executor carries only on its own platform — and anything else, including both,
settles nothing. A step is decided only when its condition reads no context but `runner` (and of
it only `os`) and `matrix`, and calls no function but `contains`, `startsWith`, `endsWith`,
`format`, `join`, `toJSON` and `fromJSON`. A status function, `hashFiles`, `env`, `steps`, `needs`
or anything else leaves it to the run. Only a job's own steps are decided, not a composite
action's.

**Why the answer is the run's.** GitHub evaluates the condition when the step is reached. It cannot
answer differently: a job's runner — and with it `runner.os` — does not change during the job, and
a condition with no status function is `success() && it`, which a false `it` makes false. The step
is kept as a no-op with its `if:`, so it is reported skipped, its `steps.<id>` reads as a skipped
step's does, and a log reads as GitHub's would.

### Amendment from working out what a machine will build

| # | Section | Change | Why |
|---|---|---|---|
| A59 | §5 INV-5, §19, A57 | **What a job needs of a machine — Xcode and its version, an iOS SDK or simulator, a macOS app's signing and notarization, a JDK and an Android SDK, a Docker daemon, the language runtimes — is read from the job as written, and `latem workflows check` says it for every job, with the evidence on `-v`** | Onboarding never learned what a machine would build. A config is written by operating system, doctor checks every mobile toolchain on every machine as a warning because it "cannot know what jobs a repo declares", and the answer was in the repositories all along. Since A57 a command can read a repository's files without a checkout |

**What is read, and what is not.** A workflow job's leg — its steps, with `uses:` and `with:` as
written, the labels the plan resolved for it and its matrix values — or a `.latem/jobs.yaml` job;
the scripts its steps run, followed to a depth of two, 32 files and 64 KiB each; `package.json`
scripts a step runs through `bun run`, `npm run` or `pnpm`; and a few repository facts:
`.xcode-version`, the Android SDK and JDK an `app/build.gradle(.kts)` names, and whether a
Worker's wrangler config declares `containers`. Never a compiled job: compiling refuses
`services:`, `container:` and reusable workflows — the jobs most worth explaining — and rewrites
`run:` text and `with:`. Nothing is run and nothing is fetched.

**Commands, not words.** A hand-written reader finds the words in command position: comments,
heredoc bodies and quoted strings are not commands, `$(…)` and backticks are commands of their
own, and `sudo`, `env`, `bundle exec`, `bunx` and their like are seen past. That is what keeps
"ios" in an echo, "codesign" in a comment and `eas build` — which builds on Expo's machines —
from reading as needs; `eas build --local` does build here. A step whose `if:` is false on the
leg's platform, decided as A58 decides it, is not read.

**Evidence and strength.** Every requirement carries where it was read from and how directly:
**declared** (a version pinned: `DEVELOPER_DIR`, an `xcode-select -s` path, a `setup-*` action's
version input, `.xcode-version`, `compileSdk`), **invoked** (a step runs the command), or
**inferred** (a convention: `find-xcode.sh 27`). A version's strength is kept apart from the need's:
an `xcodebuild` invokes Xcode, and `find-xcode.sh 27` beside it only infers which. An action
that installs a tool — `setup-node`, `setup-java`, `setup-bun` — provides it, and a requirement
it provides asks nothing of the machine but what the action itself needs. What cannot be read —
a remote action's contents, a script not in the repository, an `eval`, a reusable workflow — is
reported as not read, never guessed at.

**Kinds.** Each job gets one, from its platform and the most specific thing it needs: iOS, a macOS
app, Xcode, Android, Linux with a Docker daemon, Linux, macOS, or not known. The platform is
read from labels that name one, vendors' included (`blacksmith-…-ubuntu-2404`): looser than A58's,
because this explains and does not decide. `workflows check` prints each job's kind and
requirements, counts the kinds at the end — "kinds of build: 7 Linux, 2 iOS — Xcode 27" — and
`latem setup` prints the same count for each repository it adds. A checkout with only
`.latem/jobs.yaml` is checked too.

**What it is not.** It decides nothing: a job's needs never route it, never refuse it, and never
reach admission (INV-5 is unchanged). A control plane may be told a job's kind and requirement
keys and versions — a closed vocabulary, never steps or evidence — and that is a later protocol
change.

**What the machine has.** `system.Toolchains` probes it as a step would find it: every
`Xcode*.app` under `/Applications` and `~/Applications` with the version its `Info.plist` names (read
with `plutil`, not by running each Xcode), the iOS simulator runtimes, the JDKs (`java_home -V`,
Homebrew's `openjdk@N`, `/usr/lib/jvm`), the Android SDK where a build would find it and its
platforms and emulator, the tools a job's needs ask about on the PATH a step gets — the
service's, not a shell's, plus the per-user directories bun, cargo and pipx install into — and
whether `sudo -n true` succeeds. It reports and judges nothing.

**Gaps.** `needs.Gaps` compares one job's needs with that, for the executor of the runner that
would take it. A native runner's needs are the host's. A docker runner's are its image's, which
this does not open, except what no image can give: a Docker daemon (the docker runner gives a
step none) and `services:`/`container:` (A44 refuses them). A tart runner's are in its VM and
not known here. Something an action provides (`setup-node`), or that runs through `bundle exec`,
`bunx` or `npx`, asks the machine for nothing. Each gap names the fix: a command (`brew install
xcodegen`, `xcodebuild -downloadPlatform iOS`, `sdkmanager "platforms;android-36"`) or a step for
a person (install Xcode 27 beside the one there; a step selects it with `DEVELOPER_DIR`).

**`latem doctor`** gains a section, "What jobs need". It reads each configured repository from the
agent's mirror — never fetching: the agent owns its mirrors — plans and classifies its workflows
as `workflows check` does, and for every job a runner here would take, reports the gaps, with
their fixes; a repository with no mirror yet is skipped until the agent has one. These are
warnings: doctor's exit code is a CI gate, and a reading of a workflow is not yet trusted to fail
one. The toolchain checks that existed before (`ruby.*`, `java.version`, `android.sdk`,
`xcode.simruntime`) are now decided by need: skipped when every repository was read and no job
here needs them, and naming the jobs that do when they run. Until every repository has been
read, all of them run, as before.

**Labels that promise a toolchain.** A native runner labelled `xcode`, `ios` or `android` is
where a job asking for that label goes, and `latem init` writes `xcode` on every Mac. Doctor
warns, under Executors, when a step on that runner would find no Xcode, no iOS simulator
runtime, or no Android SDK: the job would fail at its first build step.

**A run's note.** The agent probes its toolchains in the background when it starts, and again
every fifteen minutes, after a config reload, and on a SIGUSR2 that finds the binary on disk
already running (A56): Xcode changes under a running agent — the App Store replaces it, an operator
installs another — and a probe that ran only at start went on describing the Xcode the machine
used to have. A run whose job needs what its runner lacks says so in its log before the first
step — "this job needs Xcode 27; this runner has Xcode 26.0.1" — and runs exactly as it would have.
A run before the first probe lands, or on an agent given none, writes no note.

### Amendment from installing what a machine is missing

| # | Section | Change | Why |
|---|---|---|---|
| A60 | §8, §13.3, §19, A57, A59 | **`latem setup` gains a toolchains step: what the jobs this machine takes need that it lacks — or, with no repository, what it says it will build — and, on a terminal, each install a command can do, offered one at a time and run as an argv. A native runner gains `[runner.native] env` and `path_prepend`, which setup writes for a keg-only install** | A59 made doctor say what is missing and how to get it, and left every install to the operator, one copied command at a time. And what doctor said to install, a step could not always find: `brew install openjdk@17` is keg-only, so the JDK lands on no PATH launchd gives the agent, and a build finds it only through `JAVA_HOME` |

**What it offers.** Every gap A59 names, for each job a runner here takes, read from the agent's
mirror or else a temporary clone (A57); and each docker runner's image, when Docker does not have
it yet, so a first job does not wait on the pull. A job a native runner refuses only because its
remote actions want isolation counts as that runner's: `[actions] allow_unisolated` is a setting
away, and what the job needs is the same either way. A repository none of whose jobs comes here is
said, not passed over. A gap's fix is now an argv —
`["brew", "install", "xcodegen"]`, never a line for a shell — and doctor prints it as a line to
type. A fix whose program is not installed (no Homebrew) is said and not offered — looked for on the
shell's PATH and then the agent's, which an account made for the agent may have and its login
shell not — and so is a
Homebrew install where this account cannot write the Cellar: one Homebrew serves a Mac, owned by
the user who installed it, and an account made for the agent can run what it holds and install
nothing into it. Setup names the owner. A tool whose only installer is a pipe into a shell
(`curl … | bash`) is a step for a person.

**How it runs one.** Asked, `[y/N]`, one at a time, and only on a terminal: off one, setup prints
the fixes and installs nothing. Each runs with stdin closed and in a session of its own, so
nothing it starts can open the terminal to ask for a password — `sudo` fails instead of
prompting. latem never runs `sudo`, accepts a licence (`xcodebuild -license`, `sdkmanager
--licenses`) or installs Xcode: those are said, for a person. Homebrew runs with
`HOMEBREW_NO_AUTO_UPDATE` and `HOMEBREW_NO_INSTALLED_DEPENDENTS_CHECK`, so installing one tool
cannot upgrade the JDK or Ruby a working build relies on. After an install the machine is probed
again, so a gap the install closed is not offered twice.

**Runner env.** A native runner's `env` is set for every step beneath the job's own env — a job
or step that sets a name wins — and its `path_prepend` goes in front of the PATH a step inherits
from the agent. Names a run sets for itself (`GITHUB_*`, `RUNNER_*`, `LATEM_*`) are refused, as is
a directory that is not absolute or holds a `:`. It is for where a toolchain lives, never for a
secret, which §15 keeps out of config. After a keg-only install, setup writes what a step needs:
`openjdk@N`'s `bin` on `path_prepend` and its home as `JAVA_HOME`; Ruby's `bin`;
`android-commandlinetools`' root as `ANDROID_HOME`. A name the runner's env already sets is left
as it is, and said. Doctor's probe and the agent's (A59) look on the same PATH a step gets, the
runner's `path_prepend` included.

**Checked again.** The repositories step adds the `[label_aliases]` and `[actions] allowed`
entries a repository's workflows need (A57) and then checks it again, at most three times: a job
refused for its labels is checked for nothing else until a runner carries them, so the reason it
fails next is only known after. What still stops a job is said, and so are the settings that would
let it run which setup never writes for you — `allow_unisolated`, `require_pinned = false`, an
alias for a label no runner here carries.

**With no repository.** Setup asks what the machine will build — iOS apps, macOS apps, Android
apps, Linux builds — and checks what each needs of the runner here that would take it: Xcode and a
simulator runtime for iOS, Xcode for a macOS app, JDK 17 and an Android SDK for Android.

### Amendment from telling the control plane what jobs need

| # | Section | Change | Why |
|---|---|---|---|
| A61 | §5 INV-5, §7, §20, A52, A59, A60 | **Protocol 2.2. A planned job carries `requires` — what it needs of a machine that nothing in it provides, as closed keys and versions — and a heartbeat carries `toolchains` — what the machine has, as versions and names. Both are sent only to a control plane whose hello says 2.2, and both only explain** | A59 put what a job needs in a run's log and doctor's report, on the machine. The person looking at a queued or failing job is looking at the control plane's page, which knew neither what the job needed nor what the machine had, and said "waiting for an online machine with R and a runner labelled L" whatever the cause |

**What is sent.** `requires` is each requirement of the job that no action provides, as A59
reads it — the scripts its steps run included, through the mirror's tree at the planned commit —
with a version only when it is digits and dots: `[{"key":"xcode","version":"27"},
{"key":"ios-simulator"}]`. At most 32. Never the evidence, a path or a step: the control plane
gets a closed vocabulary, which is what INV-5's "the job definition comes from the pinned commit"
leaves it able to hold. `toolchains` is the agent's probe (A59, with A60's `path_prepend`):
Xcode versions, an iOS simulator runtime, JDK majors, the Android SDK and its platforms and
emulator, the probed commands a step's PATH finds, and passwordless sudo. Never a path.

**To whom.** The agent records the protocol version of the control plane's last hello. It
computes `requires` only for a control plane that said 2.2 or later, and the remote broker strips
both fields from what it sends to one that did not, as a second guard: a 2.1 control plane
decodes a plan strictly and would refuse the whole of it over one job's field. So the control
plane is upgraded first, as for 2.1, and an agent on 2.2 talks to a 2.1 control plane exactly as
a 2.1 agent does.

**What it is for.** The control plane shows what a job needs, and — once the job is on a machine
whose runners covering it are native — what that machine likely lacks: "likely to fail on mac
mini, which lacks Xcode 27". For a docker runner it says only what no image gives: a Docker
daemon, `services:`, `container:`. It never routes a job, never holds one back, and admission
never reads either field: INV-5 is unchanged, and the machine decides what it runs from its
own config and the pinned commit, as before.

### Amendment from running a recorder's tests on the Mac mini

| # | Section | Change | Why |
|---|---|---|---|
| A62 | §23, §19 | **The LaunchAgent's `ProcessType` is `Interactive`, as GitHub's own runner has it, not `Standard`. Every unit and plist latem writes carries a revision; `latem setup` rewrites an installed definition of an older revision, and `latem doctor` names one, with `latem service install` as the fix** | take-one's recorder tests write frames in real time. In the agent they wrote 11 to 14 frames a second where 30 were due, and a recording cap stopped 100 ms late. The same two tests passed over ssh on the same Mac, under the same load, and on Blacksmith. launchd.plist(5) gives a `Standard` job, like one with no `ProcessType`, "light resource limits" on CPU and disk I/O. The plist's own comment said `Standard` was the unthrottled choice |

**Revisions.** Nothing rewrote an installed definition. `latem setup` restarts a service it finds
installed, and a restart keeps the plist launchd already holds, so a template fix reached only a
machine set up after it. A unit or plist now says which revision it is, in a comment
(`latem service definition, revision 2`); one with no such line is revision 1. Setup rewrites an
older one, which restarts the agent. Doctor names it as a warning, because the agent still works,
and the fix restarts the agent, so doctor says to run it while nothing is building. A definition
that cannot be read is not called old.

**Reinstalling a running agent.** `launchctl bootout` returns before the job has gone: the agent
takes SIGTERM and exits, and until launchd has reaped it, a `bootstrap` of the same label fails
with "Bootstrap failed: 5: Input/output error". The first rollout of revision 2 hit this, and left
the Mac mini and the MacBook with no agent until each was loaded by hand. `service install` now
waits, up to two minutes, for `launchctl print` to stop finding the old job before it loads the new
one. An agent finishing a run takes longer than that, and the install then says so rather than
loading over it.

### Amendment from sizing runners to the machine

| # | Section | Change | Why |
|---|---|---|---|
| A63 | §8, §13, §20, A59, A61 | **latem measures the machine and recommends each runner's `concurrency`. Setup writes the recommendation for the docker runner it adds, doctor warns about a runner set above it, and the heartbeat carries it (protocol 2.3) for the control plane to show. A machine has at most one native runner. An install that runs lifecycle scripts needs Node** | Setup wrote `concurrency = 2` for every docker runner, whatever the machine. On an 8 GB Mac mini, Docker Desktop's VM holds one CI-sized job. Nothing said so until jobs failed under load in ways that read as flaky tests. A second native `[[runner]]` block ran two builds on one host, which §13.3's concurrency of 1 exists to stop. take-one's `bun install` ran a workspace's `wxt prepare` on a Mac whose only Node was too old for it, and no needs said Node |

**The recommendation.** A job is sized at 2 CPUs and 4 GB, what GitHub's own Linux runners give a
job at the low end and what the fleet's bun, Go and Convex builds peak at.
- **Native:** 1, always (§13.3).
- **Docker:** the smaller of CPUs ÷ 2 and GB ÷ 4, never below 1. It is counted from what the
  daemon says it has (Docker Desktop's VM on a Mac), or else the host. Docker runners on one
  machine share it.
- **Tart:** as many macOS VMs as fit after the host keeps 8 GB, at 8 GB each, at least 1 and at
  most Apple's 2. Below 16 GB, the one VM is said to swap.

Each recommendation carries a sentence naming the numbers it came from, so a person can check it.
It never changes a config. A runner set above it runs what it is given, and doctor warns,
because the failures it invites never name the machine.

**Where it shows.**
- `latem setup` writes the docker runner's `concurrency` from it, and says so.
- `latem doctor` has a Capacity section: a pass at or under the recommendation, saying the room;
  a warning above it, with the line to change.
- The agent measures the machine beside its toolchains (A59), at start and every fifteen
  minutes, and sends `capacity` in its heartbeat to a control plane that said 2.3 (PROTOCOL.md
  §4.5).

**One native runner.** The config check refuses a second `executor = "native"` block. Native jobs
share the host, so a machine has one native runner, and it takes one job at a time.

**Installs.** `bun install`, `npm install`, `npm ci`, `pnpm install` and `yarn install` run each
package's `preinstall`, `install`, `postinstall` and `prepare` scripts, the root's and its
workspaces'. The CLIs those scripts name start with `#!/usr/bin/env node`. A job whose install
finds any such script needs Node, inferred, with the package and the script as its evidence.


### Amendment from living with automatic updates

| # | Section | Change | Why |
|---|---|---|---|
| A64 | §10, §13.3, §20, A52, A59 | **A native runner on a Mac is unavailable while the Xcode its steps use is missing or half-installed, wants its licence accepted, or wants its first launch, and the reason names the command that clears it. The agent fingerprints the machine's Xcodes every 30 seconds and checks its runners at once when the fingerprint moves. A job whose machine's Xcode changed while it ran says so in its log. A workflow run's job whose pull request ref is gone builds the commit its event named, fetched by its name when no ref here reaches it** | Macs update Xcode from the App Store, and macOS, by themselves, and latem should not ask anyone to turn that off. An update replaces Xcode in place: for minutes every tool behind /usr/bin's shims fails, and afterwards a licence or a first launch can stand between the machine and any build. Jobs failed one after another meanwhile, naming a framework or a licence prompt on stderr. Separately, GitHub's Re-run on a merged pull request's check sent a machine to refs/pull/16/merge, which GitHub had deleted, and the job was refused with E_INVALID_REF |

**What makes the runner unavailable.** On a Mac, a native runner's preflight finds the developer
directory a step would use: the runner's `DEVELOPER_DIR` from `[runner.native] env`, else
`xcode-select -p`. When that is inside an Xcode.app it checks, in order:
- the bundle's Info.plist and its `xcodebuild` exist, or Xcode is mid-update;
- `xcodebuild -license check`, or `sudo xcodebuild -license accept` is the fix;
- `xcodebuild -checkFirstLaunchStatus`, or `sudo xcodebuild -runFirstLaunch` is.

An `xcodebuild` probe that does not answer in time is no verdict, and the check moves on (A67).

The Command Line Tools, or no developer directory, is doctor's to report, not preflight's. latem
runs neither fix: both need an administrator, and a licence is the operator's (A60). The runner
takes work again on the first preflight that passes.

**Noticing.** `HostFingerprint` is a few stats: every Xcode*.app's Info.plist in /Applications and
~/Applications, where /var/db/xcode_select_link points, and
/Library/Preferences/com.apple.dt.Xcode.plist, which records accepted licences. When it moves the
agent runs preflight and the toolchain probe (A59) at once and rings, so a runner goes dark within
half a minute of an update and comes back as soon as Xcode is whole, rather than up to five
minutes either side (§20).

**The note.** A job on a native runner records the machine's Xcodes as it starts. As it ends, if
the toolchain probe now says otherwise, its log says "this machine's Xcode changed while the job
ran (27.2 → 27.3)". An update in a job's last half minute can go unsaid.

**Re-runs.** A52's pin accepts a pull request's merge commit without asking that the ref reach it.
When the ref itself is gone, because the pull request merged or closed, gate 7 no longer refuses a
workflow run's job that names a commit: there is no tip to resolve, and pin goes by the commit
alone. When the mirror lacks that commit, it is fetched by its name, as actions/checkout fetches
it; GitHub serves a commit no ref reaches until its own gc. A branch's commit is never fetched by
name, because one the branch's fetch did not bring is not on the branch.


### Amendment from moving an app off EAS

| # | Section | Change | Why |
|---|---|---|---|
| A65 | §19, A35, A50, A57 | **`latem store check` says whether a preview would reach testers from this machine: the secrets by name, the signing store, the App Store Connect key, the app record, the App Store profile and its certificate, the upload key, the Play service account, and the last upload on each store. It only reads. Nothing it does can create or change anything at Apple, at Google or in the signing store, and it never opens a Play edit** | Moving one Expo app off EAS meant nine secrets set by hand, and each mistake surfaced as a failed upload after a build of six to eleven minutes. `doctor --secrets` asks whether a name resolves and whether Apple takes the key (A35); it does not know which app a machine is for, so it could not say whether that app has a record, a profile, or a certificate about to expire. Everything that decides those can be read in a few seconds |

**What it reads.** An Expo project's `app.json`, found below the path when a monorepo keeps it in a
package, for the bundle ID and the package, and `fastlane/Matchfile` for the signing store and its
branch. A `Matchfile` is read for its strings and not run, and an `app.config.ts` is not run at all:
it is code, and a check does not execute a project to read a value out of it, so `--bundle-id` and
`--package` stand in. With `--repo` and no path the tree is read as in A57, and the agent's mirror
is never written. A project with both flags need not be an Expo project.

Secrets are looked up as a run looks them up (A50). A value is resolved only to be used or measured,
and zeroed after. Reports carry names, ids, dates and Apple's and Google's own messages, never a
value (INV-4); an alias and keytool's output count as values, and neither reaches a report.

**What it cannot do.** Every request to App Store Connect is a GET, through one function that takes
no method. Reads that would return a profile's or a certificate's contents do not ask for them. The
one POST anywhere is the assertion traded for an access token at
`https://oauth2.googleapis.com/token`, which is the only address it goes to: a key file's
`token_uri` is not read. Play is read through `applications.tracks.releases.list`, which has no edit
in its path, because a new edit invalidates the same user's open one and a timed check could
otherwise kill a running `fastlane supply`. The signing store is asked `git ls-remote`, for a
reachable branch, and is not cloned. keytool reads a keystore only from a file, so the keystore is on
disk for as long as keytool runs, at mode 0600 in a directory of its own, as a build's script has it;
the password reaches keytool through the environment, and the alias is matched here rather than
passed.

A `Matchfile`'s URL is passed to git only if it is https or ssh. It is a file in a repository, and
INV-3's reasoning applies to what a repository can make a machine run.

An https remote that finds no credential in a Mac session with no GUI is a warning with no fix, here
and in `doctor`. ssh cannot open the login keychain, so git's words there do not mean the machine has
no login, and `gh auth login` from that session may write the token to a plain file. The first run
against a real app, from the Mac that uploads it, failed on exactly this.

**Levels.** As `doctor`'s, and for what actually breaks. A date warns inside 30 days and fails inside
7: an expired certificate stops new uploads and leaves live apps alone, and a TestFlight build stops
opening after 90 days. Missing secrets, keys a store refuses, a profile that is not `ACTIVE` and a
rejected upload fail. An app with nothing uploaded yet warns, because a new app has no reason to
have a build. `IOS_DISTRIBUTION` and `DISTRIBUTION` both pass, and the report shows which one this
is: EAS's certificate carries the older type and the older name, and both sign for the App Store.

An agreement that has lapsed shows only as Apple's refusal of a read, because Apple's API cannot list
them. The check says so and names the Account Holder, who alone can accept one. latem never does
(A60). Every fix a check names for signing says to run it at a terminal: a runner consumes signing
material and never creates it (§18.1, `docs/ios.md`).

**What it leaves out.** Anything that writes, any change to the protocol, and anything in a control
plane. Whether a workflow is switched on, and whether a push reaches a machine, are the control
plane's to know. `MATCH_PASSWORD` and `ANDROID_KEY_PASSWORD` are checked by name: proving either
would mean decrypting the signing store or exporting a key.

| # | Section | Change | Why |
|---|---|---|---|
| A66 | §19, §23, A63 | **On Linux, doctor asks whether the running agent can reach Docker, not only whether doctor's own shell can: it reads the agent's credentials from /proc and compares them with the socket's owner, group and mode. When they do not let it in, doctor fails with the fix, which is a reboot** | A machine set up by installing Docker, adding its account to the group, enabling linger, logging out and back in, and running `latem setup` had a docker runner that was unavailable from its first heartbeat, with every doctor check green. The new login had the group. The agent did not: it is a child of the user manager, which had started before the group was added and which linger keeps through a logout. `newgrp` and `latem service restart` do not reach it either, and doctor's own fix said to log out and back in |

**What it can't see.** The check goes by mode bits, so it would say no to a socket that lets the agent in
through an ACL, which is not how Docker sets one up. It looks only at the default socket, because that
is where the agent looks: the unit sets no DOCKER_HOST. A Docker the shell reaches some other way, such
as rootless Docker with DOCKER_HOST in a profile, gives no verdict rather than a false one, and the
runner's own unavailable reason remains the answer there.

### Amendment from a test that timed out on a busy Mac

| # | Section | Change | Why |
|---|---|---|---|
| A67 | §13.3, §20, A64 | **An `xcodebuild` probe in native preflight that does not answer in time says nothing about Xcode, so the runner stays available** | A64 refused such a runner as possibly mid-update. A cold `xcodebuild` on a loaded 8 GB Mac took longer than `ProbeTimeout`, so a machine with a whole Xcode went dark until the next preflight, five minutes off, for being busy. A latem test failed this way on 2026-10-31 |

**What still makes the runner unavailable.** Everything in A64 that is an answer: missing files, or
`xcodebuild` refusing for a licence or a first launch. Being half-installed is what the file checks
find, and they need no process to answer.

**What this gives up.** An Xcode that hangs rather than refuses keeps its runner, and its jobs fail
on their own with what they saw. That is rarer than a busy Mac, and costs one job rather than five
minutes of every job.

### Amendment from a third machine joining a repository

| # | Section | Change | Why |
|---|---|---|---|
| A68 | §5 INV-5, §7, §20, A47, A50, A52, A61 | **Protocol 2.4. A machine joining a repository does not change where another machine's jobs go or whether they run, unless it can take them and agrees with the fleet about them. A plan says each job's `runs_on` before any machine's aliases, and which variables decide its place. A heartbeat says the machine's aliases and a digest of each variable. The control plane reads a job's labels on each machine with that machine's aliases, asks the machine that stands for a repository to plan it, and keeps a job off a machine that would answer its variables differently from that one, saying so from that machine's first heartbeat** | A plan was one machine's reading, and every other machine lived with it. `requestPlan` asked the online machine whose id sorted first, and a job's `labels` carried that machine's `[label_aliases]`. On 2026-10-31 the Linux desktop joined Tsedal, its id sorted first, and it had no Blacksmith alias, so PR #5's `web` job kept the raw label `blacksmith-4vcpu-ubuntu-2404` and waited, though the mini's own alias maps it to `linux`. Reading the code found a quieter case. A job's `if:` is answered not by the plan but by the machine that takes the job, from its own `[repos.<key>.vars]`, so `if: vars.LATEM == 'true'` on a machine without the variable skips the job with no error |

**Labels are read on each machine.** `runs_on` is each label as written, lowercased, without
`self-hosted`, with `hosted`: what it stands for on a machine with no alias for it. For each
machine the control plane takes that machine's alias for the label, else `hosted`, else the label.
That is `compileLabels`' own order, so placement now asks what the machine will ask when it
compiles the job (A50), and a job cannot reach a machine that would refuse it because the planner's
alias covered it. A plan's `labels` stays, for an older control plane or machine, and for `requires`,
which is read from it and only explains (A61).

**Who stands for a repository.** Its default machine, then the machine that has advertised it
longest, and never the machine id. `repos[].since` is stamped by the control plane the first time a
machine advertises a repository and kept while it stays advertised; a row from before it exists
reads as the machine's age. A machine that joins comes last, so it plans nothing while another
machine can. Planning is not held for the one that stands: a laptop with its lid shut must not stop
a repository's events, so the next machine in that order plans.

**Variables.** `reads_vars` names what decides whether a job runs, where, and as what: its `if:`,
`runs-on`, `environment`, `strategy`, `name`, `concurrency` and `continue-on-error`, and its
workflow's `concurrency`. A step's reads are not there, because they change what a job does on a
machine and not whether it does it, and a value that differs per machine for a step is fair use of a
machine's config. The names are read from the file, so they are the commit's and no machine's;
`"*"` says the job reads `vars` in a way no list can describe, and every variable is compared.

Among the machines with a runner for the job, online or not, the one that stands gives the answer,
and a machine that differs from it on a name the job reads is not given the job. An unset variable
is an answer. A machine that has not reported its variables is not compared. With the machine that
stands offline, and the others differing from it, the job waits and names the variable and both
machines rather than run on another answer.

This does not decide which value is right. There is no authority to ask: GitHub holds no latem
variables, and Tsedal's `LATEM` is set only in latem on purpose. It keeps the answer the repository
was already getting, which is what makes joining safe, and tells a person where the machines
differ. A heartbeat carries the first 8 bytes of each value's SHA-256 and not the value: comparing
needs no more, and a machine's config stays on the machine.

**What the app says.** Each plan records, per workflow, what its variable-reading jobs ask of a
machine. Overview's Needs you names each machine that differs from the one that stands, with the
jobs and the variables, from the first heartbeat that says so and before any event. A job held by a
difference shows as unplaceable, with the variable in its reason.

**What a machine joining can still change.** A machine that can take a job and agrees about it
shares it: that is what adding one is for, and a repository's default machine still pins its jobs.
A machine that joins can plan an event nothing else can, which would otherwise have waited, and then
what only a plan answers is its answer. Whatever it differs on is already named in Needs you, except
`run-name`, which only labels a run and is not compared. A `windows-*` label is refused by the
planner unless the planner has an alias for it, as A47 had it: an alias on another machine does not
rescue the job, and it fails with that error. Until a workflow has been planned once, nothing is
known of its jobs, and a difference first shows on the job.

**To whom.** All four fields go only to a control plane whose hello says 2.4, and the remote broker
strips them otherwise, because a 2.3 control plane decodes plans and heartbeats strictly. So the
control plane is upgraded first: Convex, then the gateway, then agents. A job with no `runs_on` is
placed by `labels`, and a machine that speaks less than 2.4 reports no aliases and is placed by
`labels` too, so an agent already running behaves exactly as it did.

**INV-5** is unchanged. Labels and variable names are the pinned commit's, read by the agent, and the
control plane holds them to place a job as it already held `labels`. No step reaches it, and the
machine that takes a job still compiles it from the commit and its own config, and refuses what it
would have refused.

**What was not done.** Preferring a planner whose config covers a repository's workflows would leave
one machine's config deciding, and only move which. Sending variable values through the control
plane so that every machine answers alike would need a place where the true value lives, and there
is none. Holding a job whenever machines differ would let a newcomer stop jobs that ran yesterday.

### Amendment from a Linux desktop that should build Android

| # | Section | Change | Why |
|---|---|---|---|
| A69 | §3, §8, §19, A59, A60 | **`latem setup`'s toolchains step becomes a screen: every kind of build in the machine's repositories, whichever machine their jobs ask for today, whether this operating system could do it, and what is in the way. It finds what is installed, says each distribution's commands, sets what a step needs, and turns a kind on with a label once a check of it passes. `internal/readiness` is the one answer behind the screen, `--json` and a log. `latem setup android-sdk` installs Google's command-line tools for one user** | A desktop got JDK 27 and nothing said what to do next. The step only looked at jobs whose labels already reached a runner here, so a machine could not be told what it might take: Android asked for `[macos, xcode]`, so to Linux it did not exist. Every fix was Homebrew's. A JDK was looked for only in `/usr/lib/jvm`, and any folder there counted, JRE or not. Nothing set `JAVA_HOME` unless Homebrew had installed the JDK, and on Linux a second JDK is reached no other way |

**One answer.** `readiness.Build` takes the machine and returns a plan, and does nothing else. Each
kind is ready, missing something, off, or impossible here, with its runner, the label that switches
it, and what is in the way. An item says what closes it: commands latem runs once asked (A60),
commands that are a person's to type, changes to `config.toml`, or words. The classifier's reading
is its input, so what it gets wrong the plan gets wrong: building this found a Gradle build behind
`env "ORG_GRADLE_PROJECT_android.injected.…=…"`, a name with dots in it, read as a command of that
name, and a script named from where a sourced script had `cd`'d to. Both are fixed.

**Needs still do not route.** The screen reads every job in the repositories, whatever its labels,
but a kind goes on only as a label on a runner, in `config.toml`, which a person confirms. A job
comes here when its `runs-on` asks for that label, and the screen says when a workflow asks for
something else: that is a change to the repository. INV-5 is unchanged, and so is placement.

**Off a terminal** nothing changes in what setup does: it says the fixes, prints the plan and goes
on to the next step, so `install.sh` and a provisioning script still run to the end. `--plain` asks
one question at a time on a terminal, as A60 did. `--json` is the plan, and writes nothing.

**Found before installed.** A JDK is a folder with `bin/javac`, in `/usr/lib/jvm`, `/usr/java`,
`/opt`, SDKMAN's, mise's, asdf's, IntelliJ's and Gradle's folders, or Android Studio's runtime. Its
version is the `release` file's, because a folder's name is not always a version:
`temurin-17-jdk-amd64` and `zulu17-ca-amd64` are 17 too. A JDK on the machine and not what a step
finds is a setting, not an install: `JAVA_HOME` on the runner. That is an item only where it can
matter: where jobs already run here it is shown and not counted, because their scripts found the
JDK themselves. An Android job that names no JDK wants 17, which is what AGP 9 takes.

**What a machine is told to do.** `/etc/os-release` gives the Linux family: Arch, Debian or Fedora,
through `ID_LIKE` for a derivative. Each has its own package names, and each line wants `sudo`, so
it is a person's to type (A60). The screen opens their own shell with the line above the prompt, and
looks again when they leave. It does not type for them: a command that latem starts cannot be told
from one latem ran. Doctor says the same lines. Homebrew stays the Mac's.

**The Android SDK** is built up a step at a time, each needing the last: the command-line tools,
the licence, platform-tools, then a platform. `sdkmanager` is on no PATH, and runs on a JDK.
`latem setup android-sdk` puts the tools in `$ANDROID_HOME` or `~/Android/Sdk`: no package gives
these on every distribution, and those that do install where only root can add to them. It reads
Google's manifest rather than pinning a version, which would go stale, downloads what that names
over HTTPS, checks the size and the SHA-1 the manifest publishes, and unpacks as the user, refusing
a path outside itself and anything that is not a file or a folder. Nothing lands at `latest` until
all of it has. The licence stays the person's: `sdkmanager --licenses`, in the shell the screen
opens.

**Settings.** The screen offers what setup left to a person until now: `allow_unisolated`,
`require_pinned = false` and an `[actions] allowed` entry, and a native runner on a machine that
has none. It says what the first two give up before asking, and the answer defaults to no. Asked
one at a time by name, on a terminal, this is the operator's decision being made by the operator,
which A60 kept it. Off a terminal they are said and never written.

**Switching a kind on.** Android is the label `android`, and a Linux job that talks to a Docker
daemon is `docker-host`. A kind goes on only with nothing in the way, and after its proof: its tools
run in a step's environment, which is the service's PATH and the runner's env and nothing from the
shell setup was started in. A proof is a check that the tools run, not a build: a real one takes
minutes, and the first job is that. Switching off takes the label away, with a yes.

**The screen** is `stty(1)` and escape codes, like `readHidden`: no dependency joins §3's list. A
read comes back within a tenth of a second with or without a key, so nothing is left blocked in a
read to steal a key from a shell the terminal has been handed to. All of it but that loop is a
model tested without a terminal: a key changes a cursor or calls a hook, and a view is drawn from
what is true now.

**What was not done.** The app did not show this: a heartbeat carried what a machine has, not what
its repositories ask of it. A70 adds that. A step's `ANDROID_HOME` from a runner's env is read by doctor and
setup and not by the agent's probe, which looks where an SDK usually is: a machine whose SDK lives
elsewhere reports none in the app, and builds.

| A70 | §7, §20, INV-2, INV-5, A61, A63, A69 | **Protocol 2.5. A heartbeat says what `latem setup` says of the machine, so the app can show what each machine can build and what is in the way, with no way to act on it.** |

**What is sent.** `readiness.Advert` copies a plan field by field into a heartbeat's `readiness`:
each kind, where it stands, its runner, its repositories' keys, how many jobs it has and how many
of those ask for another machine's labels, and the short names of what is in the way, with what the
machine has instead. It is a copy and not a filter because most of a plan belongs to the machine:
an item's commands and config edits, a JDK's home, an SDK's root.

**What is not, and why each.** Commands and config edits, so the far side holds nothing it could
ask for: `latem setup` on the machine stays the only thing that closes an item (A69), and latem
works with no account. Paths, as `toolchains` has none (A61). Which actions a job uses and what its
image is, because the control plane is never told a job's steps (INV-5): an action not allowed is
counted, and tag-named actions are said without their names. Secrets' names, which INV-2 would
allow and a page has no use for: the item says how many are not set. A job's name is a count too.

**The last check.** An item's words are fixed phrases and a version, which a workflow can write any
way it likes, so a word with `/` or `\` in it, or over 120 characters, or not on one line, is
counted in `more` and not sent. The control plane refuses the same on decoding, and the whole of
`readiness` with it; the rest of the beat is written, as for `capacity`.

**When.** Beside the toolchain probe, every 15 minutes and whenever that probe is asked for: a
reload, SIGUSR2 and an Xcode that changed. It probes again and reads every repository, which no
heartbeat should. It has two minutes, so a Docker daemon that does not answer costs the next probe
no more than that. A look that fails, or panics on a repository's files, is logged and keeps the
last answer: this only explains, and nothing it does may stop a build.

**From where.** Only the agent's own mirrors, `HEAD`, never fetched and never cloned. A repository
with no mirror yet is `unread`, and a mirror is as new as its last plan or run, so the page can be
behind a push by as long as the machine has been idle.

**To whom.** Only a control plane whose hello says 2.5. A 2.4 one decodes a heartbeat strictly and
would refuse the beat, so the control plane is upgraded first. It explains: nothing routes on it,
and the agent's silence means an agent that has not looked or predates this.

| A71 | A59, A69, A70 | **A kind of build is read job by job, on the runner each job goes to, and leaves out what a machine's variables stop.** |

The desktop's card said Linux with Docker wanted a Docker daemon while `integration-host` passed on
its native runner. A kind was read on one runner, the first job's, and that job was `integration`,
which goes to the docker runner and which `if: vars.LATEM != 'true'` skips there every time.

**A kind's runner** is one of its jobs' that suits it: native for what runs on the host, docker for
what runs in a container. With none, it is the first of its jobs', and with no jobs what could take
it, as before.

**Each job is read on its own runner.** One that a label sends elsewhere is compared with that
runner, and an item it raises says which: `a Docker daemon`, on the linux runner. That is true of
`images.yml`, whose jobs a machine's aliases can send to a container.

**A job this machine's variables stop is not counted**, in the plan or in doctor: its `if:` reads
`vars` and is false on every event the file was planned for. An `if:` that reads no variable may be
true on an event no check tries, so it counts. So does one that reads a variable and is false only
for that reason: a line too many costs less than the job a person would miss.

**An item says how many jobs it holds up** when it is some of a kind's and not all, in the words a
heartbeat already carries: `3 not set, 1 of 17 jobs`. A kind is flagged over one job, and a page
should say so. No field is added, so the protocol stays 2.5.

### Amendment from a desktop that rebooted before anyone logged in

| # | Section | Change | Why |
|---|---|---|---|
| A72 | §5 G5, §12, §19, A52 | **A plan and a job bring a read-only token, and the agent's own fetches use it, so a machine needs no GitHub login to build its repositories. Protocol 2.6.** | The Arch desktop's agent starts at boot, as §23 asks, and its login was in a keyring that opens at login. Two jobs reached it 44 seconds after boot and failed in 650ms with `could not read Username`, and the guide's own `gh auth login` had put it there. The alternative is a whole-account token in a plain file that every native job can read |

**The token.** Minted by the control plane when it sends a `plan` or a `run`, in `fetch_token`, for
one repository with `contents: read`, and gone after that frame, as a job's token is. Not the job's
own: that is cut down to the job's `permissions`, which may not include reading the code, and a plan
has no job.

**How it is used.** An http header for the remote's host, in the environment of one git process: not
on a command line, which any process can read, and not in a file. The mirror's config never holds it.
Gate 6, a checkout's second fetch, a plan's fetch, a commit fetched by name and `git lfs fetch` use
it. It reaches no step and is registered with the redactor, and a run holds it as it holds its job
token, so a restart loses it.

**Only to GitHub.** It goes only to a repository whose configured URL is on github.com. A `github =`
key that names a repository on GitHub does not send it to a URL somewhere else, and an ssh remote
takes no header.

**The machine's credential stays the fallback.** With no token, or a token that fails, git runs as it
always has, so a token that expires under a long job, a control plane with no GitHub App and a
repository elsewhere all behave as before. When both fail, the token's error is the one reported.
The agent's log says which it used, and warns of a token that failed, because a working login would
otherwise hide a broken token until the next reboot.

**G5 is narrowed once more, and not far.** §12 already let the agent receive a token for a job. A
hostile control plane gains nothing here: the URL comes from config, the commit is still checked, and
the token is one it could have sent in `run.token`.

**What still needs a login.** A token covers one repository. Private submodules, private actions from
another repository and `latem run` use the machine's credential, so doctor keeps asking about it: a
repository it cannot reach is a warning when jobs bring their own way in, and a failure otherwise.

