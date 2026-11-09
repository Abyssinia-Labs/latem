# The Latem agent wire protocol

**Status: descriptive, not normative.** This document records the format as it
exists in `internal/broker/remote`, which is the implementation that ships.
Where this document and RUNNER_SPEC disagree, the Go side wins and the
disagreement is noted here. Where this document and the Go source disagree, the
Go source wins and this document is wrong.

## Why this file exists

`pkg/protocol` is the shared package and the only exported one. RUNNER_SPEC §4
and finding F8 put every type that crosses the wire in it, and it holds the
payloads — `Command`, `Update`, `LogFrame`, `MachineHealth`, `Resume`, `Ack` —
and **no envelope**. It has no way to say which payload a given message carries,
and no way to say which request a given `Ack` answers.

A Unix socket carrying one kind of message each way does not need an envelope,
which is why `internal/broker/local` never grew one. A WebSocket carrying eight
does. So `internal/broker/remote/wire.go` defines one, **unexported**, and its
own comment records that as a contract defect rather than smuggling it into
`protocol`:

> This is unexported, and that is a statement rather than a convenience […]
> `pkg/protocol` has no envelope: it has the payloads and no way to say which
> one a given message is.

The consequence is that the format has two implementations in two languages and
one written definition — the Go source — which the other implementation cannot
import. **That is the defect WS-14 reported and this document cannot fix.** The
fix is promoting the envelope into `pkg/protocol`, which is frozen. What this
document can do is make the drift visible, and it is written to be the evidence
for that promotion when it happens.

Alongside it, and doing more work than it does:

| Artifact | What it is |
|---|---|
| `fixtures/frames.jsonl` | 28 frames encoded by Go itself, from real values, round-tripped through the agent's own strict decoder before being written. Replayed against this server in `tests/conformance.test.ts`. |
| `fixtures/server-frames.jsonl` | Every frame this server can send, encoded by `convex/lib/wire.ts`. |
| `fixtures/server-frames.verdicts.jsonl` | What the Go decoder said about each of those lines. All 14 accepted. |
| `fixtures/server-frames.rejected.{jsonl,verdicts.jsonl}` | Six frames the Go decoder refused, and why. The negative control: without it, "Go accepted everything" is indistinguishable from "the checker accepts anything". |
| `fixtures/e2e/*.jsonl` | Transcripts from the real `latem agent` binary connected to this server over a real socket, including a real mid-build partition. |

---

## 1. Transport

- **WebSocket.** `wss://` in anything but a test; the agent's config loader
  refuses plain `ws://` to a non-loopback host, because the machine token rides
  in a header.
- **The agent dials. Always.** INV-1. The control plane never initiates a
  connection to a machine and stores no address that would let it.
- **Text messages only.** The wire is JSON by specification. `readLoop` ignores
  a binary message from a control plane; this server ignores one from an agent.
- **One frame per message.** No framing, no length prefix, no batching of frames
  into one message beyond what `logs` already does.
- **1 MiB per message, either direction.** `remote.MaxFrameBytes = 1 << 20`,
  applied with `Conn.SetReadLimit` so an oversized message fails the read rather
  than being assembled first. This server applies it as `ws`'s `maxPayload` and
  checks it again on encode.
- **Compression negotiation is left on** by the agent's dialler; a build's log
  volume is exactly what it pays off on.

### Handshake

```
GET /agent HTTP/1.1
Upgrade: websocket
Authorization: Bearer <machine-token>
Latem-Protocol-Version: 2.6
```

`Latem-Protocol-Version` is set by `Broker.authHeader` and by `remote.Verify`.
It is **not** documented in RUNNER_SPEC §7.4, which mentions only the
`Authorization` header. This server reads it for diagnostics and does not
require it. Since 2.0 it is also how a control plane knows which frames a
machine can decode: `plan` commands and workflow-run fields go only to an agent
that said `2.0` here (A52).

**Status codes are load-bearing, and getting them wrong takes machines off the
network:**

| Status | What `internal/broker/remote` does |
|---|---|
| `101` | Session begins. |
| **`401` or `403`** | `ErrRevoked`. **The reconnect loop stops and stays stopped** until a new token is written and the agent restarts. It logs once, at error, naming the token file and the fix. It does **not** stop the agent: `latem run` and every configured runner keep working. |
| anything else | A partition. Retried with backoff 1s → 2s → 4s … capped at 60s, ±20% jitter, forever. |

So: answer `401` **only** for a credential that is genuinely bad or revoked.
Answer `503` when the database is unreachable, when a dependency times out, when
anything transient goes wrong. A `401` for a transient failure requires a human
to restart every affected agent by hand.

*(Verified: `tests/tokens.test.ts` covers the decision, and the gateway's
handshake was exercised by hand — no `Authorization` → 401, unreachable Convex →
503. See README.md.)*

### Registration: the claim endpoints (A54)

How a machine gets its token without anybody copying one: the device
authorization grant of RFC 8628, over two plain HTTPS endpoints beside the
WebSocket. Their base is the agent URL's HTTPS origin — `wss://gw.latem.dev/agent`
claims at `https://gw.latem.dev`; a trailing `/agent` is dropped and any path
before it kept. Plain `http` is used only for a loopback host. The agent does
not follow redirects on either.

```
POST <base>/claim
{"name":"mac-mini","os":"darwin","arch":"arm64","labels":["macos","arm64"],"agent_version":"v0.2.0"}

200 {"device_code":"…","user_code":"WDJB-MJHT",
     "verification_uri":"https://app.latem.dev/connect",
     "verification_uri_complete":"https://app.latem.dev/connect?code=WDJB-MJHT",
     "expires_in":600,"interval":5}
```

`labels` is always an array, `[]` when there are none; the agent sends its
runners' labels unless `--label` overrides them. `name`, `os`, `arch` and
`labels` are what the person approving sees, and display only.

```
POST <base>/claim/token
{"device_code":"…"}

200 {"machine_id":"mch_…","token":"…"}       exactly once; the claim is then spent
400 {"error":"authorization_pending"}       keep polling
400 {"error":"slow_down"}                   keep polling, 5 s slower from now on
400 {"error":"expired_token"}               stop: the code expired
400 {"error":"access_denied"}               stop: the person declined
```

The agent polls every `interval` seconds (5 when absent), adds 5 on every
`slow_down` and keeps the slower pace, and stops at `expires_in` (600 when
absent) without a further poll. A `503` (latem-cloud's
`temporarily_unavailable`, its database out of reach) or a `429` (which also
slows the poll by 5 s) is waited out like a dropped connection: up to five
polls in a row may fail before it gives up. Any other `400` error, or any
other status, ends the wait with the body's `error` printed. The token is then proven with the
handshake above, and only after that written to `agent.key` — the same order
as a token supplied by hand, so a claim that does not verify writes nothing.
`machine_id` is recorded when the `hello` carries none.

Outside the happy path, as latem-cloud answers (its `docs/protocol.md` §1
"Registering"):

| Answer | Where | What the agent does |
|---|---|---|
| `400 invalid_request` + `error_description` | `/claim` | Stops, printing both. The agent fits its claim to the limits first: `name` 1–64 bytes (`latem` when the hostname is not spellable), `os`, `arch`, `agent_version` ≤ 32, at most 32 labels of ≤ 64, no control characters |
| `429 slow_down` + `Retry-After: 600` | `/claim` | Stops and says when to try again: 20 claims from one network (IPv6 /64) in 10 minutes |
| `503 temporarily_unavailable` | either | Retried like a partition: up to 3 attempts at `/claim`, 5 polls in a row at `/claim/token` |
| `400 expired_token` | `/claim/token` | Stops. It also means a device code that is unknown or already spent |

The agent prints `user_code` and the link after removing control characters,
and opens `verification_uri_complete` (else `verification_uri`) in a browser
only when it is `https` (or loopback `http`) and stdout is a terminal.

### Where this endpoint lives

**Convex HTTP actions cannot accept a WebSocket upgrade.** They take a `Request`
and return a `Response` following the Fetch API; there is no upgrade primitive
and no way to hold a socket. `internal/broker/remote` speaks WebSocket and
nothing else.

So the endpoint is a small Node process, `gateway/`, that terminates the socket
and calls Convex functions. It holds no state that matters and can be restarted
at any moment — the agent's reconnect loop treats that as a partition, and
partitions end. Everything protocol-shaped lives in `convex/lib/`, shared by the
gateway and the Convex functions, so there is one implementation of the format
per language and not two.

---

## 2. The envelope

Mirrored from `internal/broker/remote/wire.go`'s unexported `frame`. Exactly one
payload field is populated, and it is the one matching `kind`.

| Field | JSON | Type | Direction | Required |
|---|---|---|---|---|
| Kind | `kind` | string, one of nine below | both | **yes** |
| ID | `id` | string | both | when a reply is wanted — see §2.1 |
| Version | `protocol_version` | string | both | stamped on every frame by `encode`; see §2.2 |
| Hello | `hello` | object | server → agent | with `kind: "hello"` |
| Command | `command` | object | server → agent | with `kind: "command"` |
| Ack | `ack` | object | both | with `kind: "ack"` |
| Resume | `resume` | object | agent → server | with `kind: "resume"` |
| Update | `update` | object | agent → server | with `kind: "update"` |
| Logs | `logs` | array | agent → server | with `kind: "logs"` |
| Heartbeat | `heartbeat` | object | agent → server | with `kind: "heartbeat"` |
| Claim | `claim` | object | agent → server | with `kind: "claim"` |
| Plan | `plan` | object | agent → server | with `kind: "plan"` — 2.0, A52 |

All payload fields are `omitempty`. Unset fields are **absent**, not `null` —
with the exceptions in §5.

### 2.1 `id` — correlation

The envelope carries the correlation identifier because `protocol.Ack` (added by
amendment A18) carries only `{ok, run_id, code, error}` — enough to answer "did
that work", not enough to answer "which of the three things outstanding for this
run does this refer to". A log batch and a state report for one run can be in
flight at once.

- The agent allocates ids as `strconv.FormatUint(n, 36)` — `"1"`, `"2"`, …,
  `"z"`, `"10"`. **Do not parse them.** They are opaque, per-session, and this
  server does not reuse the space; it uses Convex document ids for the commands
  it sends.
- A frame with no `id` gets **no answer**. `Broker.reply` refuses to send one and
  `resolve` discards one, because there is nothing to correlate it with.
- Which agent frames carry an `id`:

| Frame | `id`? | Because |
|---|---|---|
| `resume` | no | `sendResume` calls `send`, not `request`. No waiter is registered. **Do not ack it.** |
| `heartbeat` | no | Same. Fire and forget (§20). |
| `claim` | **yes** | `Claim` blocks on the answer. |
| `update` | **yes** | `Report` blocks on the answer. |
| `logs` | **yes** | The ack advances the sink's resume point. |
| `ack` | **yes** | Echoes the `id` of the `command` being answered. |
| `plan` | **yes** | `ReportPlan` blocks on the answer. Nothing replays it: the control plane, which is waiting for it, asks again. |

### 2.2 `protocol_version`

`encode` sets `f.Version = protocol.Version` unconditionally, so **every frame
in both directions carries it**. The current value is `"2.8"`.

**2.0 (amendment A52)** added the `plan` command and the `plan` frame that
answers it, the workflow-run fields of `run` (§3.2), `update.outputs` (§4.2),
`heartbeat.repos` (§4.5) and the terminal state `skipped`. Every one of those is
a field or a value a 1.0 decoder refuses, which is what §2.3 means by "there is no
additive change": a 1.0 agent receiving a 2.0 `plan` command refuses it, loudly,
and keeps its session. A control plane sends 2.0 frames only to an agent whose
frames say `"2.0"`.

**2.1 (amendment A55)** added `run.matrix` (§3.2) and a planned job's `matrix` and
`strategy` (§4.8): a matrix job is planned as one job per leg. The same rule
applies in both directions. A 2.0 agent refuses a run carrying `matrix`, so a
control plane sends a leg only to an agent whose frames say 2.1 or later; and a
control plane that does not know the plan's new fields refuses a plan that has
them, so the control plane is upgraded before the agents. A plan with no matrix
job carries neither field, and is the same at 2.0 and 2.1.

**2.2 (amendment A61)** added a planned job's `requires` (§4.8) and a
heartbeat's `toolchains` (§4.5): what a job needs of a machine, and what the
machine has. Both explain, and neither routes a job. The rule runs the other
way here: the agent sends either only to a control plane whose `hello` said
2.2 or later, because a 2.1 control plane decodes a plan strictly and would
refuse all of it over one job's `requires`. A control plane answers a 2.2
agent's hello with 2.2.

**2.3 (amendment A63)** added a heartbeat's `capacity` (§4.5): the machine's
size and the concurrency latem recommends for each runner. It explains, like
`toolchains`; the config decides. The agent sends it only to a control plane
whose `hello` said 2.3 or later, because a 2.2 control plane decodes a
heartbeat strictly. A control plane answers a 2.3 agent's hello with 2.3.

**2.4 (amendment A68)** added a planned job's `runs_on` and `reads_vars` (§4.8),
and a heartbeat's `label_aliases` and each repository's `vars` (§4.5). These are
the first of the additive fields that route: a control plane places a job with
them. The agent sends them only to a control plane whose `hello` said 2.4 or
later, because a 2.3 one decodes plans and heartbeats strictly, so the control
plane is upgraded first. A job from an older planner, or a machine on an older
agent, is placed by `labels` as before. A control plane answers a 2.4 agent's
hello with 2.4.

**2.5 (amendment A70)** added a heartbeat's `readiness` (§4.5): what `latem setup`
says of the machine. It explains, as `toolchains` does, and nothing routes on it.
The agent sends it only to a control plane whose `hello` said 2.5 or later,
because a 2.4 one decodes a heartbeat strictly. A control plane answers a 2.5
agent's hello with 2.5.

**2.6 (amendment A72)** added `fetch_token` to `run` (§3.1) and `plan` (§3.2): a
read-only GitHub token for the agent's own fetches, so a machine needs no login of
its own to build its repositories. A control plane sends it only to an agent that
said 2.6 or later, because an older one decodes inbound commands strictly and would
refuse the frame. Nothing about it is required: without one the agent fetches with
the machine's credential, as before. A control plane answers a 2.6 agent's hello
with 2.6.

**2.7 (amendment A75)** added `resume.runs[].needs_tokens` (§4.1): a workflow job
that came through an agent restart, still `queued` or just `claimed`, holding
neither its job token nor its fetch token, which live in the agent's memory only.
The control plane answers by sending the job's `run` command again, which brings
fresh ones. The agent sends it to any control plane: `resume` goes before `hello`
says what one speaks, and a decoder of `resume` reads the fields it knows. A
control plane answers a 2.7 agent's hello with 2.7.

**2.8 (amendment A82)** lets a key be a **job path**: a job of a workflow that a
job calls (`uses: ./.github/workflows/x.yml`), planned into the caller's
workflow run as `<caller>/<job>`, at most four ids, `release/package/pack`. It
appears in a plan's `jobs[].key` and `jobs[].needs` (§4.8), and in `run.job` and
the keys of `run.needs` (§3.2). No field is added; what changes is that a `/`
may appear where only a job's id could. A 2.7 control plane refuses a plan with
one, and the whole plan with it, so the agent plans calls only for a control
plane whose `hello` said 2.8, and before that plans each call as one job with an
`error`. A 2.7 agent refuses a run whose `job` holds a `/`, so a control plane
sends one only to an agent whose frames say 2.8. A control plane answers a 2.8
agent's hello with 2.8.

The agent's own test control plane fails a test if an inbound frame arrives
without it, so a server that omits it is non-conformant against the suite that
exists.

`hello` carries a **second, nested** `protocol_version` (§3.1). The agent reads
the nested one in `handleHello` and warns on a mismatch; it does not read the
envelope's own. Both are set to the same value by every implementation here.

This server refuses nothing over a version mismatch: it warns on a differing
major and continues. Refusing a session over a bump would make an additive
protocol change an outage rather than a log line.

### 2.3 Strictness

Both ends decode with `DisallowUnknownFields`, **at every level**, including
inside `protocol.Command`. An unknown field is a hard decode failure.

That costs forward compatibility, deliberately: `protocol.Command.UnmarshalJSON`
makes the same trade at the same boundary, because at a trust boundary a loud
refusal beats a quiet misinterpretation. Practically, for a control plane:

- **Adding a field to any frame breaks every older agent, loudly.** There is no
  additive change. A version bump is the only way.
- A malformed frame is **skipped**, not fatal. `readLoop` logs and continues,
  because WebSocket messages carry their own boundaries so the next one is still
  intelligible. This server does the same.

### 2.4 Frame kinds

```
server → agent:   hello   command   ack
agent → server:   resume  update    logs   heartbeat   claim   ack   plan
```

`frame.validate()` on the agent accepts **only** `hello`, `command`, and `ack`
from a control plane, and refuses everything else by name — "a control plane may
not send a %q frame". A server echoing back a heartbeat or a resume is "either
confused or probing, and neither deserves an answer". This server applies the
mirror image.

---

## 3. Server → agent

### 3.1 `hello`

The control plane's first message, and how a machine learns the identifier
assigned to it. `latem register` writes it into `[machine] id`.

```json
{
  "kind": "hello",
  "protocol_version": "2.6",
  "hello": {
    "machine_id": "mac-mini-01",
    "protocol_version": "2.6",
    "message": "connected as build-01"
  }
}
```

| Field | Type | Required | Notes |
|---|---|---|---|
| `machine_id` | string | no | Empty or absent is ignored. A different value from the agent's current one is accepted, with a warning. |
| `protocol_version` | string | no | The **control plane's** version. `handleHello` warns on a mismatch. |
| `message` | string | no | Display text, printed verbatim by `latem register`. Never interpolated. |

`pkg/protocol` has no type for this frame. `MachineHealth` carries a `MachineID`
the agent already knows; nothing carries one the server chose.

**Silence is success.** `remote.Verify` — the `latem register` path — reads once
with a timeout and treats a timeout as a verified token. A hello is not
required. Sending one immediately is nonetheless the difference between
registration reporting the assigned machine id and waiting fifteen seconds to
report nothing.

### 3.2 `command`

```json
{
  "kind": "command",
  "id": "j57abc",
  "protocol_version": "2.6",
  "command": {
    "kind": "run",
    "run": {
      "id": "r7k9m2p4q6r8s0t2u4v6w8x0",
      "repo": "myapp",
      "job": "ios-beta",
      "ref": "main",
      "inputs": { "lane": "beta" },
      "origin": {
        "kind": "manual",
        "actor": "michael@example.com",
        "requested_at": "2026-08-08T09:15:00.000Z"
      }
    }
  }
}
```

**The inbound command set is closed at three kinds, `run`, `cancel` and `plan`**
(INV-2; `plan` is 2.0's, A52). A fourth is a RUNNER_SPEC §5 change requiring a
written threat model, not a feature; the decoder rejects an unknown kind rather
than zero-valuing it. Our negative control confirms: `{"kind":"deploy"}` is
refused with *"the inbound set is closed to \"run\", \"cancel\" and \"plan\"
(INV-2)"*.

#### `run` — `protocol.RunRequest`

| Field | JSON | Type | Required | Notes |
|---|---|---|---|---|
| ID | `id` | string | **yes** | Must match `^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`. See below. |
| Repo | `repo` | string | **yes** | A key into the agent's local config. Not a URL, not a path. |
| Job | `job` | string | **yes** | A name in the registry the agent read from its own disk at the resolved commit. |
| Ref | `ref` | string | **yes** | Validated per INV-3 by the agent before use. |
| Inputs | `inputs` | object, string → string | no | Omitted when empty. Declared inputs only; coerced to the registry's declared types at gate 9. |
| Origin | `origin` | object | **yes** | `{kind, actor?, requested_at}`. Audit and display only. |

`origin.kind` is `"manual"`, `"webhook"`, `"schedule"`, or `"cli"`.
`origin.requested_at` is RFC 3339 and is **never trusted** — the agent stamps
its own times (§22).

**`id` is a security boundary, not a label** (amendment A6). The agent builds
`work/<id>`, `logs/<id>.log`, `artifacts/<id>/`, and the container and VM name
`latem-run-<id>` from it. `{"id": "../../../../tmp/x"}` writes wherever the
agent's user can write. The character class excludes every separator **and every
dot**. Our negative control confirms the frozen decoder refuses it — but a
control plane that relays an id from a webhook body has made itself the delivery
mechanism regardless, which is why ids here are minted, never accepted.

**There is no other field.** No command, script, image, environment, file path,
executor, cache configuration, timeout, or label override — the agent resolves
all of that from a registry on its own disk at the resolved commit (INV-5).
Adding a field is a security review. Our negative control confirms that a
smuggled `script` or `image` is a hard decode failure at the agent.

**A workflow run's job (2.0, A52)** adds the fields below, which are **all set or
all absent** — a run carrying any of them without `workflow` is refused. `job` is
then the job's id under `jobs:` in that file, which is what makes `web` in three
files three different jobs. Since 2.8 (A82) it may be a job path, `release/pack`:
the job `pack` of the workflow that the file's job `release` calls. The agent
finds each id in a file it reads at the pinned commit, through the `uses:` of
the one above, so no inbound field names the called file.

| Field | JSON | Type | Notes |
|---|---|---|---|
| Workflow | `workflow` | string | Must match `^\.github/workflows/[A-Za-z0-9_-][A-Za-z0-9._-]*\.ya?ml$` — the only path an inbound field may name. |
| SHA | `sha` | string | The commit to build, 40 or 64 hex. Must be reachable from `ref`: the job builds what the event named, not what the branch says by the time it starts. |
| Event | `event` | object | `protocol.Event`, §3.2's `plan` below. Data: the payload reaches a step only as `$GITHUB_EVENT_PATH` or an expression bound to an env var (A42). |
| Needs | `needs` | object, job id → `{result, outputs?}` | `result` is `success`, `failure`, `cancelled` or `skipped`. Outputs are strings. Keyed as the plan keyed the needs: since 2.8, a key may be a job path (§4.8). |
| WorkflowRunID | `workflow_run_id` | string | The workflow run's id, shared by its jobs: `github.run_id`. Same pattern as `id`. |
| RunNumber, RunAttempt | `run_number`, `run_attempt` | number | GitHub's per-workflow numbers. `$LATEM_BUILD_NUMBER` stays the machine's own (A36). |
| Token | `token` | string | A GitHub installation token for this job alone, scoped by its `permissions`. One printable word, at most 1024 bytes. **Held in memory only**: stripped before the run is made durable, registered with the redactor before the first step, dropped when the run is terminal. An agent that restarts with the job queued builds it without one. |
| FetchToken | `fetch_token` | string | 2.6, A72. A read-only GitHub token for this repository, for the agent's own fetches, and never given to a step. `token` is no use for that: it is cut down to the job's `permissions`, which may not include reading the code. One printable word, at most 1024 bytes. **Held in memory only**, as `token` is. Optional: without it the agent fetches with the machine's credential. |
| Matrix | `matrix` | object | 2.1, A55. A matrix leg's values, exactly as the plan's `jobs[].matrix` gave them; absent for a job without a matrix. At most 16 KB. The agent expands the job's matrix itself from the pinned commit and builds the leg **only if these values are exactly one of its legs** — `14` is not `"14"`, and a key too many is a different leg — else the run fails `E_BAD_REGISTRY`. A matrix job run without one fails the same way. |

#### `plan` — `protocol.PlanRequest` (2.0, A52)

```json
{ "kind": "plan", "plan": { "id": "p7k9m2p4", "repo": "myapp", "event": {
    "name": "push", "ref": "refs/heads/main",
    "sha": "3bf12613f4a2a8f9d24a5e0c1b7d9e3a5c8f0b21",
    "before": "1111111111111111111111111111111111111111",
    "actor": "octocat", "payload": { "ref": "refs/heads/main", "head_commit": { "message": "fix" } } } } }
```

`fetch_token` (2.6, A72) is optional, and is what the plan's fetch authenticates
with, as on a `run`. It is not in the example because it is never stored or logged.

Which workflow runs does this event start at this commit? The control plane holds
no workflow definitions and parses none (INV-5); the machine fetches, reads
`.github/workflows` at `event.sha` from its own mirror, and answers with a `plan`
frame (§4.8). Nothing runs.

| Field | Type | Required | Notes |
|---|---|---|---|
| `id` | string | **yes** | Same validation as `run.id`. Echoed in the answer. |
| `repo` | string | **yes** | A key into the agent's local config — the one the machine advertised beside the repository's `owner/name` (§4.5). |
| `event.name` | string | **yes** | `push`, `pull_request`, `pull_request_target`, `release`, `repository_dispatch` or `workflow_dispatch`. Any other is answered with an error, by name; `schedule` is not planned yet. |
| `event.action` | string | for pull requests, releases and repository dispatches | The activity type; a repository dispatch's `event_type`. |
| `event.ref` | string | **yes** | `refs/heads/…` or `refs/tags/…` for a push or dispatch; `refs/pull/N/merge` for a pull request. |
| `event.sha` | string | **yes** | 40 or 64 hex. A pull request's merge commit. |
| `event.before` | string | no | A push's previous tip, for `paths` filters. Absent for a new branch, where the agent diffs against the commit's first parent. A pull request's diff is its merge commit against its first parent. |
| `event.base_ref`, `event.head_ref` | string | for pull requests | Branch names. `branches` filters read `base_ref`. |
| `event.workflow` | string | for `workflow_dispatch` only | Same pattern as `run.workflow`. |
| `event.inputs` | object, string → string | `workflow_dispatch` only | Checked against the workflow's declarations; defaults are filled in. |
| `event.actor` | string | no | `github.actor`. |
| `event.payload` | object | no | The webhook body: `github.event`. A JSON object of at most 256 KiB. |

#### `cancel` — `protocol.CancelRequest`

```json
{ "kind": "cancel", "cancel": { "id": "r7k9…", "actor": "michael@example.com", "reason": "wrong branch" } }
```

| Field | Type | Required | Notes |
|---|---|---|---|
| `id` | string | **yes** | Same validation as `run.id`. |
| `actor` | string | no | Display. |
| `reason` | string | no | Display only. **Truncated to 200 runes on decode** (`utf8.RuneCountInString`, so an emoji is one). Never interpolated, never passed to a shell, redacted before it is logged or persisted. |

Semantics: **idempotent, and a no-op for an unknown, unclaimed, or already
terminal id.** A cancel during `preparing` or `running` cancels the run context.
A cancel during `collecting` is **ignored** — artifacts and logs from a build
that already ran are worth more than a few seconds of teardown latency.

A cancel is **durable nowhere on the agent**. No table remembers it and only the
goroutine holding the run's context can act on it, so the control plane is the
only thing that can re-offer one after a partition.

#### What the agent answers

`handleCommand` makes an inbound run durable **before** it acknowledges, then
answers, then wakes the runner pool. So `ok: true` means "this run will happen —
by this agent in a moment, or by the next agent to start if this one dies".

It is **idempotent by run id**: a run already queued is re-queued; a run that
already finished is refused rather than resurrected under its old identifier.
That is what makes replay after a resume safe.

For a `cancel`, the answer is delivery: `ok: true` if it reached the command
channel, `ok: false` with *"the agent is not reading commands; the cancel was not
delivered"* if the buffer was full.

A `plan` is answered the same way — delivery, not the plan. Fetching and reading
a repository takes longer than an acknowledgement should, so the plan itself
follows as a `plan` frame (§4.8). A plan is durable nowhere on the agent; if the
answer does not come, ask again, here or of another machine with the repository.

### 3.3 `ack` (server → agent)

```json
{ "kind": "ack", "id": "7", "protocol_version": "2.6", "ack": { "ok": true, "run_id": "r7k9…" } }
```

| Field | Type | Required | Notes |
|---|---|---|---|
| `ok` | bool | **yes** | No `omitempty` on the Go side, so `false` is present, not absent. |
| `run_id` | string | no | Echo, for logging. |
| `code` | string | no | One of §21's codes, set when `ok` is false. |
| `error` | string | no | Human-facing, already redacted. |

`ackReason` renders these as `error (code)`, `error`, `code`, or *"the control
plane refused without saying why"*.

Per frame kind:

- **`claim`** — `ok: false` means another runner won. The agent maps it to
  `broker.ErrAlreadyClaimed`, an ordinary outcome. A transport error is
  explicitly **not** a refusal: "reporting 'somebody else has it' for 'I could
  not ask' would silently idle a machine that could have taken the work."
- **`update`** — `ok: false` is the control plane's **final answer**. `Report`
  does not replay a refusal, ever, because that would loop on every reconnect
  for the life of the run. A transport error *is* replayed. Choose accordingly:
  refuse only what will never become acceptable.
- **`logs`** — a refusal is **treated as acceptance, with a warning**. The
  alternative is re-offering the same batch on every reconnect forever, and the
  lines are still in the run's log file on the machine. **The ack must not be
  sent until the frames are durable**, because `sink.ack` advances the resume
  point on receipt and drops everything at or below it from the buffer.

An `ack` for an unknown `id` is discarded silently by `resolve`. An `ack` with an
empty `id` is discarded.

---

## 4. Agent → server

### 4.1 `resume` — the first frame of every session

```json
{
  "kind": "resume",
  "protocol_version": "2.7",
  "resume": {
    "machine_id": "mac-mini-01",
    "runs": [{ "run_id": "r-alpha", "state": "running", "last_acked_seq": 4200 }]
  }
}
```

Sent immediately on connect, **before anything else**, carrying
`{run_id, state, last_acked_seq}` for every non-terminal run the machine owns.
No `id`; **no ack is expected or wanted**.

| Field | Type | Notes |
|---|---|---|
| `machine_id` | string | May be `""` on a machine that has never completed registration. |
| `runs` | array (never `null`) | See §5. |
| `runs[].run_id` | string | |
| `runs[].state` | string | One of §11's thirteen (2.0 added `skipped`). |
| `runs[].last_acked_seq` | number (uint64) | **This transport's own** position, never the machine's. |
| `runs[].needs_tokens` | boolean, omitted when false | A workflow job, `queued` or `claimed`, that lost its tokens to a restart (2.7, A75). |

The server owes two things in reply:

1. **Replay the commands the agent missed.** Every handler is idempotent by run
   id.
2. **Resume log ingestion from `last_acked_seq + 1`.**

`last_acked_seq` is the live sink's position when there is one, and
`broker_state` under `broker = 'remote'` otherwise (amendment A28). It is
**never** the machine's high-water mark, which a local broker writing to the same
disk keeps far ahead of any network. Zero is the safe direction and the agent
sends it when it cannot read the real value: the server re-sends from 1 and its
`(run_id, seq)` dedupe discards what it already has.

**What to replay, and what not to** — this server's rules, in
`convex/lib/ingest.ts`:

| Server's view | Agent holds it? | Action |
|---|---|---|
| `queued`, unclaimed | no | Replay the `run` command. |
| past `queued` | no | **Do not replay.** Mark unaccounted and wait. |
| any non-terminal | yes | Adopt the agent's state; replay a pending `cancel`; resume logs at `last_acked_seq + 1`. |
| non-terminal, and the agent says `needs_tokens` | yes, `queued` or `claimed` | Replay the `run` command: its delivery mints fresh tokens. A run the agent has claimed takes them and stays claimed (A75). |
| terminal | either | Nothing. |
| unknown to this server | yes | Report it; do not adopt. Nothing here can invent an intent. |

The second row is the expensive one to get wrong. `claimed`, `preparing`,
`running` and `collecting` all mean a build started; a `run` command for one asks
a Mac to check out, sign, and possibly upload the same commit again. The
agent settles it instead: §6 step 5 reconciles an abandoned run to `interrupted`
and reports that through its own replay.

### 4.2 `update`

```json
{
  "kind": "update",
  "id": "9",
  "protocol_version": "2.6",
  "update": { "run_id": "r-alpha", "update": { "state": "running", "started_at": "…" } }
}
```

**`update` is the one frame whose envelope adds a field to the payload.**
`protocol.Update` has **no run id** — `Broker.Report` takes it as a separate
argument — while `protocol.LogFrame` does have one. A transport putting an
update on a wire has to pair them itself, which is why `updateFrame` exists. This
is the second recorded contract defect.

`update.update` is `protocol.Update`:

| Field | JSON | Type | Notes |
|---|---|---|---|
| State | `state` | string | **Required.** One of §11's thirteen. `skipped` (2.0, A52) is terminal: a workflow job whose `if:` was false as it started — neither success nor failure. |
| ExitCode | `exit_code` | number \| absent | Pointer on the Go side. A real run sends it only on failure; see §5. |
| ErrorCode | `error_code` | string | One of §21's codes. |
| ErrorMsg | `error_message` | string | Already redacted. |
| StartedAt | `started_at` | RFC 3339 | |
| FinishedAt | `finished_at` | RFC 3339 | |
| Artifacts | `artifacts` | array | §4.4. |
| Runner | `runner` | string | A8. The slot that claimed the run; not known at create time. |
| ExpiresAt | `expires_at` | RFC 3339 | A8. The `approval_ttl` / `queue_ttl` deadline. Set on entering a waiting state; **a transition out of one clears it.** |
| CancelReason | `cancel_reason` | string | A8. Echo of `CancelRequest.Reason`, redacted and truncated. |
| RefNormalized | `ref_normalized` | string | A21. The fully-qualified ref policy matched (INV-5). |
| SHA | `sha` | string | A21. The commit it resolved to. |
| Outputs | `outputs` | object, string → string | 2.0, A52. A workflow job's `outputs:`, evaluated as it ended, on its **terminal** update only. One holding a value the redactor knows is dropped and named in the run's log. |

`ref_normalized` and `sha` together are the answer to the only question anyone
asks about a surprising build: "requested as `main`, built `refs/heads/main` at
`3bf1261`".

**Idempotent by `(run_id, state)`.** A broker receiving the same transition twice
must treat the second as a no-op. This is not defensive: the agent replays
unacknowledged transitions on reconnect *and* re-sends anything
`store.Unreported` still holds after a restart, and **both fire on the same
reconnect** — we observed a terminal `succeeded` arrive twice from one healed
partition (`tests/e2e.test.ts`).

Two further properties this server implements and recommends:

- **A terminal run does not leave its terminal state.** Replay order is not
  guaranteed across a restart — `store.Unreported` returns rows, not a stream —
  so `running` can arrive after `succeeded`. Letting it win shows a finished
  build as running forever.
- **The late transition is still recorded**, for the audit trail.

### 4.3 `logs`

```json
{
  "kind": "logs",
  "id": "15",
  "protocol_version": "2.6",
  "logs": [
    { "run_id": "r-alpha", "seq": 1, "stream": "agent", "at": "2026-08-08T09:15:00Z", "line": "[latem] preparing" }
  ]
}
```

| Field | Type | Notes |
|---|---|---|
| `run_id` | string | Every frame in a batch names the same run; a sink refuses a foreign frame. |
| `seq` | number (uint64) | **Per run, monotonic from 1**, assigned by the emitter after redaction and before fan-out (§16). |
| `stream` | string | `"stdout"`, `"stderr"`, or `"agent"`. |
| `at` | RFC 3339 | The machine's clock. Display only — **order by `seq`**. |
| `line` | string | **Already redacted** (INV-4). ≤64 KB; longer lines are truncated with a marker upstream. |

Batching is 100 frames or 250 ms (§16), chunked to 256 KiB per message on the
way out so a reconnect flush advances the resume point while it is still running.

**The dedupe rule, and it is the whole contract:**

> The server keeps the **first** frame it sees for a given `(run_id, seq)` and
> discards later ones.

Keep the *first*, not the last. A re-offered frame is normally byte-identical, so
which wins looks like it cannot matter; it matters when they are not identical,
and a second or confused agent process must not be able to rewrite log history
that has already been read.

**Acknowledge the batch's own high sequence, including frames refused as
duplicates.** The agent advances `sink.acked` to the high sequence of the batch
it sent; a server that acked only as far as what it stored would rewind the
agent's resume point and make it re-offer the same batch on every reconnect for
the life of the run.

**A gap marker** is an ordinary frame on `stream: "agent"` whose `line` begins
`[latem] N log lines were dropped while the control plane was unreachable`. It
takes the sequence of the **first** frame it stands in for, so the stream stays
contiguous at the point the resume frame named, and it dedupes like anything
else. Sequences keep incrementing across the gap — the server sees 1…1000, a
marker, then 15001 — so the hole is visible rather than silent.

### 4.4 Artifacts — inside `update.artifacts`

```json
{ "path": "build/app.ipa", "size_bytes": 84233216, "sha256": "9f2c1a…", "expires_at": "…" }
```

`path`, `size_bytes`, `sha256` are required; `expires_at` is optional.

**Content never crosses the boundary** (§17, INV-4). An `.ipa` is 50–300 MB and
belongs on the machine that signed it, or in TestFlight. There is no field here
that could carry one and no table in `convex/schema.ts` that could store one.

### 4.5 `heartbeat`

```json
{
  "kind": "heartbeat",
  "protocol_version": "2.6",
  "heartbeat": {
    "machine_id": "mac-mini-01", "agent_version": "0.4.1", "os": "darwin", "arch": "arm64",
    "runners": [{ "name": "ios", "executor": "tart", "labels": ["ios"], "concurrency": 1, "active": 1, "available": true }],
    "disk_free_gb": 412.5, "draining": false,
    "repos": [
      { "key": "myapp", "github": "Abyssinia-Labs/myapp", "vars": { "LATEM": "b5bea41b6c623f7c" } },
      { "key": "scratch" }
    ],
    "label_aliases": { "blacksmith-4vcpu-ubuntu-2404": ["linux"] }
  }
}
```

`repos` (2.0, A52) is how a GitHub event finds the machines that can plan and run
it: an installation's repository `owner/name` matched against `repos[].github`,
and `repos[].key` sent back as `plan.repo` and `run.repo`. A repository that is
not on GitHub has a key and no `github`. Absent on a machine with no repositories.

`repos[].vars` (2.4, A68) is a digest of each of the repository's variables, by
name as `[repos.<key>.vars]` writes it: the first 8 bytes of the value's SHA-256,
in hex. An unset variable is an absent name; an environment's are not sent. It
tells two machines' answers apart and carries no value. Sent only to a control
plane that said 2.4, which keeps a machine out of a job it would answer
differently from the machine that stands for the repository.

`label_aliases` (2.4, A68) is the machine's `[label_aliases]`, which is how it
will compile a job's `runs-on`. Absent on a machine with none, which a control
plane must read as none: a 2.4 agent's silence is an answer, as an older one's is
not. Sent only to a control plane that said 2.4.

`toolchains` (2.2, A61) is what the machine has for builds, as a step on its
native runners would find it: `xcode` (versions, `"26.0.1"`), `ios_simulator`,
`jdk` (majors), `android_sdk`, `android_platforms`, `android_emulator`, `tools`
(the probed commands a step's PATH finds: `xcodegen`, `docker`) and
`passwordless_sudo`. Versions and names, never a path. Sent only to a control
plane that said 2.2, and absent until the agent's probe has run.

`capacity` (2.3, A63) is the machine's size and what each runner's `concurrency`
should be: `cpus`, `memory_gb`, `docker` (`cpus`, `memory_gb`: what the Docker
daemon is given, Docker Desktop's VM on a Mac; absent when no daemon answered)
and `runners` (`name`, `recommended`, `why`, a sentence saying what the number
comes from). A native runner is recommended 1; a docker runner one job per 2
CPUs and 4 GB of what Docker has, shared between docker runners; a tart runner as
many macOS VMs as fit in memory, at most two. Sent only to a control plane that
said 2.3, measured beside the toolchains and as often.

`readiness` (2.5, A70) is each kind of build in the machine's repositories, where
it stands here, and what is in the way, in a person's words. `kinds` is at most
16: `kind` (`android`), `title` (`Android`), `state` (`ready`, `missing`, `off` or
`impossible`), and when they have something to say `why`, `runner`, `repos` (keys,
at most 64), `jobs`, `elsewhere` (jobs whose workflows still ask for another
machine's labels), `items` (at most 32: `title`, `have`, `soft`) and `more` (what is
in the way and not in `items`). `unread` counts the repositories it could not read
and `unclassified` the jobs whose kind it could not tell. Words are at most 120
characters on one line, with no `/` or `\`. There are no commands in it and no
config edits, so nothing on the far side can act on it, and no path, action
or image: those are counted. A secret's name is said, never its value (A73):
a secrets item's `have` is `A, B (staging) not set`, ending `and N more` when
they do not fit. Sent only to a control plane that said
2.5, and absent until the agent has looked, which it does beside the toolchains
and as often, from its own mirrors.

```json
"readiness": {
  "kinds": [
    { "kind": "android", "title": "Android", "state": "off", "runner": "host", "repos": ["tsedal"], "jobs": 2, "elsewhere": 2,
      "items": [{ "title": "JDK 17", "have": "JDK 27" }, { "title": "tsedal's secrets set here", "have": "ANDROID_KEYSTORE_B64 (preview), PLAY_SERVICE_ACCOUNT_JSON (preview) not set" }] },
    { "kind": "ios", "title": "iOS", "state": "impossible", "why": "needs a Mac" }
  ],
  "unread": 1
}
```

```json
"capacity": {
  "cpus": 8, "memory_gb": 8,
  "docker": { "cpus": 6, "memory_gb": 3.8 },
  "runners": [
    { "name": "macos", "recommended": 1, "why": "native runs one job at a time: …" },
    { "name": "linux", "recommended": 1, "why": "Docker has 6 CPUs and 3.8 GB, and a job takes about 2 CPUs and 4 GB" }
  ]
}
```

No `id`; no ack expected. Every 15 seconds (§20). **Three missed beats — 45
seconds — is offline.**

`runner.available` is false when preflight failed: Docker unreachable, no GPU
toolkit, a missing Tart image, disk below the runner's floor.
`unavailable_reason` carries the words for a human. Recovery is automatic.

`draining` means the agent is shutting down: in-flight runs continue, no new ones
are accepted, **and heartbeats continue** — "a machine that stops beating and a
machine that is draining need to look different to whoever is watching".

**`MachineHealth` carries no timestamp**, and that is right: liveness is stamped
by the server on arrival. §22 and §7.1 are explicit that another machine's clock
is audit and display only, and a machine with a fast clock must not be able to
report itself online forever.

One more thing the agent does that a naive server will get wrong: on every
reconnect it publishes its **last known health immediately**, before the next
15-second tick, so a machine away for ten minutes is current the moment it
returns.

### 4.6 `claim`

```json
{ "kind": "claim", "id": "7", "protocol_version": "2.6", "claim": { "run_id": "r-alpha" } }
```

An identifier and nothing else: "the control plane already knows everything about
the run, and a claim that could assert anything would be an inbound verb pointed
backwards."

**It must be atomic at the control plane.** The agent's own comment: the
atomicity "belongs to the far side here, not to this process: the control plane
is the only thing that can see every machine racing for the same run, which is
the whole reason the method exists." Convex mutations are serializable
transactions, so the read and the write are one.

### 4.7 `ack` (agent → server)

The agent's answer to a `command`, echoing the envelope `id` the command carried.
Same payload shape as §3.3.

### 4.8 `plan` (2.0, A52)

```json
{ "kind": "plan", "id": "12", "protocol_version": "2.6", "plan": {
    "id": "p7k9m2p4",
    "workflows": [{
      "file": ".github/workflows/checks.yml", "name": "Checks",
      "concurrency": { "group": "checks-refs/heads/main", "cancel_in_progress": true },
      "jobs": [
        { "key": "lint", "name": "lint", "labels": ["linux"], "permissions": { "contents": "read" }, "timeout_minutes": 360 },
        { "key": "web", "name": "Web build", "needs": ["lint"], "labels": ["linux"], "environment": "staging",
          "permissions": {}, "if_checks_status": true, "timeout_minutes": 30 }
      ] }] } }
```

The answer to a `plan` command, correlated by `plan.id`, and acked like an
`update`. Structure and names, never steps.

| Field | Type | Notes |
|---|---|---|
| `id` | string | The plan command's id. |
| `workflows` | array \| null | One entry per workflow the event triggers, in file order. **`[]` when the event triggers nothing; `null` when there is no plan** — read `error`. |
| `error_code`, `error` | string | Why the request could not be planned at all: an unknown repository, a fetch that failed, a dispatch the workflow refuses (unknown input, missing required one, no `workflow_dispatch` trigger). |
| `workflows[].file`, `.name` | string | The path, and `name:` or the path when there is none. |
| `workflows[].run_name` | string | `run-name:` evaluated; absent when the workflow has none. |
| `workflows[].concurrency` | `{group, cancel_in_progress?}` | Evaluated. **Enforced by the control plane**, which alone sees every run in a group. |
| `workflows[].inputs` | object | A dispatch's inputs with defaults filled in; give every job of the run these. |
| `workflows[].error` | string | Why this workflow could not be planned — it cannot be parsed, its `on:` or `needs:` is invalid, its `concurrency` reads something unknown when planning. **Report it as a failed run**, as GitHub reports an invalid workflow file. |
| `jobs[]` | array | Ordered so each job comes after every job it needs. |
| `jobs[].key`, `.name` | string | The id under `jobs:` (send it as `run.job`), and the display name a check run is named by. 2.8, A82: a job of a called workflow is keyed by its **job path**, the caller's id and its own joined by `/` (`release/pack`, at most four ids, each a job id), and named as GitHub names it, `release / pack`. Schedule it as any job; send it only to an agent that said 2.8. The caller itself is not in the plan: no machine runs it. |
| `jobs[].needs` | array | Keys of jobs in the same workflow run, job paths included. A job that needs a call needs every job of it; a job of a call needs its caller's needs. |
| `jobs[].labels` | array | `runs-on` after the **planning** machine's aliases and GitHub's hosted labels. Dispatch to a runner whose labels cover these only when `runs_on` is absent or the machine is too old to report its aliases. |
| `jobs[].runs_on` | `[{label, hosted?}]` | 2.4, A68. `runs-on` before any machine's aliases: each `label` lowercased, without `self-hosted`, and `hosted`, what it stands for on a machine with no alias for it when that is not itself. **Read it per machine**: the machine's alias for `label`, else `hosted`, else `label` itself, which is how that machine will compile the job. Sent only to a control plane that said 2.4. |
| `jobs[].reads_vars` | array | 2.4, A68. The variables that decide whether the job runs, where, and as what: everything of it but its steps. Names as the file writes them, read without case, or `"*"` when no list can say. Never a value. **Do not send the job to a machine whose `repos[].vars` differ on these from the machine that stands for the repository.** Sent only to a control plane that said 2.4. |
| `jobs[].environment` | string | The environment's name. |
| `jobs[].permissions` | object | The token's scopes in the workflow's vocabulary (`pull-requests`, not `pull_requests`), `read` or `write`; a scope absent has none. Always present: the job's, else the workflow's, else GitHub's restricted default (`contents` and `packages` read). |
| `jobs[].concurrency` | object | Job-level, evaluated, enforced like the workflow's. |
| `jobs[].if_checks_status` | bool | The job's `if:` calls `success()`, `failure()`, `always()` or `cancelled()`. **Only then** dispatch it after a need did not succeed — the machine evaluates the condition. Otherwise skip it without asking: GitHub's implicit `success()`. |
| `jobs[].continue_on_error` | bool | A failure of this job does not fail the workflow run. |
| `jobs[].timeout_minutes` | number | 360 unless the job says. |
| `jobs[].matrix` | object | 2.1, A55. Set on each **leg** of a matrix job: its values. A matrix job is planned as one entry per leg, all with the job's `key`, each with its own `name` (GitHub's: `check (linux, 22)`), `labels`, `environment` and `concurrency`. **Send it back as `run.matrix`.** |
| `jobs[].strategy` | `{job_index, job_total, fail_fast, max_parallel?}` | 2.1, A55. Set on each leg. Legs are jobs to schedule, with three rules the control plane enforces: **a job that needs `key` waits for every leg**, and reads `needs.<key>.result` as `failure` if a leg failed, else `cancelled` if one was, else `skipped` if all were, else `success`, and `needs.<key>.outputs` as the legs' outputs merged in the order they ended; **`fail_fast`** cancels a failed leg's queued and running siblings, unless that leg is `continue_on_error`; **`max_parallel`**, when set, caps how many legs run at once. |
| `jobs[].error` | string | This job cannot run — a matrix computed from another job's outputs, a workflow of another repository, a Windows label. It fails alone; jobs that need it are skipped as after any failure. |
| `jobs[].requires` | `[{key, version?}]` | 2.2, A61. What the job needs of a machine that nothing in it provides, read from the commit: `xcode` at `27`, `ios-simulator`, `jdk` at `17`, `docker-daemon`. Keys from a closed vocabulary that may grow (a control plane keeps a key it has no words for), versions only digits and dots, at most 32. **Never route on it**: it is for saying a job is likely to fail on the machine it went to. Sent only to a control plane that said 2.2. |

---

## 5. Encoding traps

Things a hand-written mirror gets wrong. Every one below is in
`fixtures/frames.jsonl` and covered in `tests/conformance.test.ts`.

1. **`resume.runs` is always an array, never `null`.** `protocol.Resume.Runs`
   has no `omitempty`, so a *nil* slice would encode as `"runs": null` — but
   `conn.go` always builds it with `make([]protocol.RunResume, 0, len(runs))`,
   including on the error path, so the nil case is unreachable. A machine's very
   first connection sends `"runs": []`.

   This entry previously claimed the opposite and called `null` the common case.
   It was wrong, and wrong in the direction that matters: a reader written to
   that claim would carry a branch it can never exercise, and would look
   defensive while being untested. The corpus records the real shape. Per §26.6
   this document is descriptive of the Go side, so the document was the defect.

2. **`heartbeat.runners` and `runners[].labels` can be `null`,** for the same
   reason.

3. **`ack.ok` is always present.** No `omitempty` on a Go bool means `false` is
   written, not omitted.

4. **RFC 3339 with variable fractional precision.** Go writes
   `2026-08-08T09:15:03.5Z` — one digit. Comparing timestamps as strings sorts
   that before `…03.50Z`. Parse to epoch milliseconds and compare numbers.

5. **`inputs` is absent, not `{}`,** when empty. Likewise `origin.actor`,
   `cancel.actor`, `cancel.reason`, and every `omitempty` field. Emitting `{}`
   decodes fine; matching Go's own encoding keeps a fixture comparison exact.

6. **`exit_code` is a pointer, and a real run never sends `0`.** Absent means
   "no exit code"; do not coalesce absent to zero, because the two are different
   facts and a control plane that conflates them will report a green build as
   having exited 0 when the agent never said so.

   In practice only a *failing* step sets it. `internal/run` returns success
   with a nil exit code, so `exit_code: 0` is reachable only from a hand-built
   `Update` — it appears in the fixture corpus for exactly that reason, since a
   reader must still handle a present zero if the agent ever starts sending one.
   Whether it *should* — a successful step did exit 0, and saying so is more
   informative than silence — is an open question for §7.2 rather than a defect
   in either side.

7. **`error_code: ""` never appears** — it is `omitempty` — but treat an empty
   string as absent anyway.

8. **`seq` is a `uint64`.** JSON numbers are IEEE 754 doubles in every JavaScript
   runtime, so sequences above 2^53 lose precision silently. A run producing nine
   quadrillion log lines is not a real concern; a `uint64` on the wire with no
   agreed encoding is a real defect. Recorded in §7.

---

## 6. Session order

What the agent does, in this order, on every connection (`runSession` →
`onConnect`):

1. Start reading. **Before** sending anything, because the acknowledgements for
   the replay and the flush arrive on that goroutine.
2. Send `resume`.
3. Send `heartbeat` with the last known health, if there is any.
4. Replay undelivered `update`s — first from the transport's own memory, then
   from `store.Unreported` under `broker = 'remote'`. **Both fire.**
5. Flush every sink's unacknowledged log frames.

A session that got as far as sending its resume frame resets the backoff to one
second. A server that accepts a connection and immediately hangs up would
otherwise hold the agent at the base delay indefinitely.

On disconnect the agent re-offers every batch that was written but never
acknowledged. "Without it the frames covering the outage would be marked sent,
never acknowledged, and never seen again."

---

## 7. Defects found, and not fixed here

Reported, per the brief. None is fixed in `pkg/protocol`, which is frozen.

**D1 — There is no envelope type, and now two implementations of one.**
Previously recorded by WS-14 as a Go-side observation; this document is the
consequence made concrete. `internal/broker/remote/wire.go` defines `frame`
unexported; `convex/lib/wire.ts` defines it again, in another language, from
reading that source. There is no shared definition, no test that both agree, and
no version on the envelope's own shape. The fix is promoting `frame`,
`helloFrame`, `updateFrame` and `claimRequest` into `pkg/protocol`. This document
plus `fixtures/` is the evidence for that promotion.

**D2 — `protocol.Update` has no run id.** `Broker.Report` takes it as a separate
argument, so every transport pairs them itself. `protocol.LogFrame` has a
`RunID`; `Update` does not. Already recorded in `remote.go`'s package comment.
Confirmed from this side: `updateFrame` is a wrapper that exists purely to carry
a field the payload should own.

**D3 — `protocol.Ack` has no correlation identifier.** Amendment A18 added the
type, not a request/response discipline, so `frame.ID` carries it. Already
recorded. Confirmed.

**D4 — `Latem-Protocol-Version` is undocumented.** `Broker.authHeader` and
`remote.Verify` both set this handshake header. RUNNER_SPEC §7.4 documents only
`Authorization`. A conformant control plane written from the spec would not know
it exists; one written from the Go source would. *New.*

**D5 — `LogFrame.Seq` is a `uint64` with no agreed JSON encoding.** Every
JavaScript control plane — including the one the spec's own §21 assumes, "the web
app maps these to human copy" — parses it as a double, so values above 2^53 are
silently wrong. Either bound it in the spec or encode it as a string. *New.*

**D6 — the `hello` frame is not in RUNNER_SPEC at all.** §7.4 describes the
resume frame in prose and A3 formalised `Resume` for exactly that reason. `hello`
is the mechanism by which a machine learns its own id — the thing `latem
register` writes into `[machine] id` — and it exists only in
`internal/broker/remote/wire.go`. It should be a type in `pkg/protocol` and a
paragraph in §7.4. *New.*

**D7 — `RUNNER_SPEC` §7.2's `LogFrame.Stream` is `string`; the code's is a typed
`Stream`.** The spec snippet writes `Stream string` with a comment listing three
values; `pkg/protocol` declares `type Stream string` with three constants. Not a
wire difference, and the code is right. Noted so a reader of §7.2 alone does not
implement an open set. *New, cosmetic.*

**D8 — nothing defines what a server should do with a `resume` frame naming a run
it has never heard of.** §7.4 says the server "replays commands the agent missed"
and is silent on the reverse. This server reports it and adopts nothing, because
nothing here can invent an intent and an intent is what INV-2 says a run is — but
the choice is ours, not the spec's, and two control planes could reasonably
differ. *New, a specification gap rather than a defect.*

---

## 8. Regenerating the corpus

`fixtures/frames.jsonl` and the verdict files need a Go toolchain and the Latem
source. The generator is a throwaway module outside the repository that imports
`pkg/protocol` — the only exported package — and mirrors `wire.go`'s envelope
verbatim.

```console
$ mkdir /tmp/gofix && cd /tmp/gofix
$ cat > go.mod <<'EOF'
module latemfix
go 1.24
require github.com/Abyssinia-Labs/latem v0.0.0
replace github.com/Abyssinia-Labs/latem => /path/to/latem
EOF
$ # main.go: copy `frame`, `helloFrame`, `updateFrame`, `claimRequest`, `encode`,
$ # `decode`, `validate` verbatim from internal/broker/remote/wire.go, plus
$ # `decodeAny` from helpers_test.go. Add a `gen` mode that emits the corpus and
$ # a `check` mode that decodes stdin line by line and prints a verdict per line.
$ go run . gen  > frames.jsonl                    # what the agent emits
$ go run . check < server-frames.jsonl           # what a server may send back
```

**This section describes a cross-check that no longer runs here.** It was built
when the control plane lived in this repository: frames from each side were fed
through the other's decoder, and that caught real encoding mistakes in both.
The hosted layer is now a separate repository, so nothing in this tree exercises
the far end of the wire.

What remains on this side is the strictness of `protocol.Command.UnmarshalJSON`
— a closed command set, unknown fields refused at every level — which stops a
malformed or hostile frame being *accepted*, but says nothing about whether the
frames this agent *emits* are the ones a server expects.

Restoring the check means committing the generated corpus here and replaying it
in `test/integration`, so a change to `wire.go` fails a test in this repository
rather than surfacing as a decode error in another one. That belongs to WS-16
and is recorded with the build plan.
