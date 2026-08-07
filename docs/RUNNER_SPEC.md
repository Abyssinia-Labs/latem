# Latem Runner — Technical Specification

**Status:** Draft 01 · implementation-ready
**Scope:** the open-source runner stack only (agent, job registry, executors, cache, mobile pipeline, local trigger)
**Out of scope:** the hosted control plane (auth, Convex schema, web app). It is referenced here only through the `Broker` interface in §7.

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

**Language: Go 1.22+.** Single static binary, trivial cross-compilation to `darwin/arm64` and `linux/amd64`, first-class process and signal handling, mature Docker SDK. No runtime to install on the target machine, which matters for G6.

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
│   ├── source/                 # mirror clones, worktrees (§12)
│   ├── cache/                  # volume management, locks, prune (§14)
│   ├── secrets/                # provider chain (§15)
│   ├── logstream/              # framing, redaction, sinks (§16)
│   ├── artifact/               # collection, retention (§17)
│   ├── config/                 # config.toml
│   ├── store/                  # sqlite; migrations embedded
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

---

## 5. Security invariants (normative)

These are requirements, not guidance. Each has a test in `test/integration/invariants_test.go` and a review checklist item. A pull request that weakens one is rejected regardless of its other merits.

### INV-1 — The agent only makes outbound connections

The agent MUST NOT bind a TCP listener on any non-loopback interface. The local broker listens on a Unix domain socket at `~/.latem/agent.sock`, mode `0600`. There is no HTTP server, no port to forward, no inbound firewall rule.

*Test:* start an agent, enumerate listening sockets for the process, assert every entry is a Unix socket or bound to `127.0.0.1`.

### INV-2 — The broker supplies names, never definitions

A run request carries `{repo, job, ref, inputs}`. It MUST NOT carry commands, scripts, images, environment definitions, file paths, executor selection, or cache configuration. `RunRequest` (§7) has no field capable of expressing any of those, and adding one is a §5 change.

The agent resolves `job` against the registry it read from disk. An unknown name is rejected with `E_UNKNOWN_JOB` before any process starts.

*Test:* fuzz `RunRequest` with hostile values in every field; assert no execution occurs outside the registry-declared command set.

### INV-3 — Broker-supplied values are never interpolated into a shell

`ref` and `inputs` originate outside the trust boundary. They are passed to the executor as environment variables only. The agent MUST NOT build a command string by concatenating them, and MUST NOT pass them to `sh -c`.

Validation, applied before use:

- `repo` — must match a `[repos.*]` key present in local config. Exact string match against a map. No path construction from this value.
- `ref` — must match `^[A-Za-z0-9._/-]{1,255}$`, must not begin with `-`, must not contain `..`, must not be `HEAD@{…}`. This blocks git argument injection (`--upload-pack=`, `--exec=`).
- `inputs` — every key must be declared in the job's `inputs:` block. Values are type-checked per declaration. Undeclared keys are a hard rejection, not a warning.

All git invocations use `--` before user-influenced arguments and `exec.Command` with an argv slice. Never `exec.Command("sh", "-c", …)`.

*Test:* table-driven, with `--upload-pack=/bin/sh`, `; rm -rf /`, `$(id)`, `../../etc/passwd`, `refs/heads/x\nrefs/heads/y`, 10 KB refs, and unicode homoglyphs.

### INV-4 — Secrets never leave the machine

Secret values MUST NOT appear in any structure sent to a broker, including run metadata and error messages. Every log line passes the redactor (§16) before reaching any sink. The redactor is applied at the point of emission, not at the sink, so a sink cannot be added that bypasses it.

*Test:* run a job that echoes every loaded secret; assert none appears in the broker transcript, the persisted log file, or the run's error field.

### INV-5 — The registry comes from disk, at the pinned commit

`jobs.yaml` is read from the checked-out worktree after checkout, never fetched from the broker. Jobs are resolved against the registry at the resolved commit, so a run is reproducible from the commit alone.

Because a `ref` can point at a branch a contributor controls, config gates which refs may run without approval:

```toml
[repos.myapp]
allowed_refs = ["main", "develop", "release/*"]   # glob, matched after resolution
require_approval_otherwise = true
```

Runs on non-matching refs enter `awaiting_approval` and require a local `latem approve <run-id>`. Approval is a local action only; a broker cannot grant it.

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
  │       marked `interrupted`; orphaned containers/VMs/worktrees are reaped
  ├─ 6. start runner pool (one goroutine per runner, bounded by concurrency)
  ├─ 7. connect brokers; local always, remote if configured
  ├─ 8. start heartbeat ticker
  └─ 9. block until SIGINT/SIGTERM → graceful shutdown
```

**Graceful shutdown.** Stop accepting new runs, mark the machine `draining`, allow in-flight runs to finish up to `shutdown_grace` (default 30m, since an iOS build is long), then send the run's process group SIGTERM, wait 30s, SIGKILL. Always tear down containers and VMs, even on the kill path — leaked Tart VMs consume disk fast. Release the lock last.

**Crash recovery.** Step 5 is not optional. On startup, list containers with label `latem.run-id`, list Tart VMs matching `latem-run-*`, list worktrees under `~/.latem/work/`, and delete anything whose run id is not in a non-terminal state in sqlite.

---

## 7. Broker interface

The single seam between the runner stack and any control plane. Both implementations satisfy it; the agent contains no branch on which is in use.

```go
package broker

type Broker interface {
    // Runs yields run requests. It closes only on ctx cancellation.
    // Implementations reconnect internally; a dropped connection is not
    // an error the caller sees.
    Runs(ctx context.Context) (<-chan RunRequest, error)

    // Claim attempts exclusive ownership. Returns ErrAlreadyClaimed if
    // another runner won. MUST be atomic at the broker.
    Claim(ctx context.Context, runID string) error

    // Report publishes a state transition. Idempotent by (runID, state).
    Report(ctx context.Context, runID string, u Update) error

    // Logs returns a writer for streamed output. Batched by the caller.
    Logs(ctx context.Context, runID string) (io.WriteCloser, error)

    // Heartbeat publishes liveness and capacity.
    Heartbeat(ctx context.Context, h MachineHealth) error
}

// RunRequest is the complete set of data crossing the trust boundary
// inbound. Adding a field here is a security review.
type RunRequest struct {
    ID     string            `json:"id"`
    Repo   string            `json:"repo"`             // must exist in local config
    Job    string            `json:"job"`              // must exist in registry
    Ref    string            `json:"ref"`              // validated, INV-3
    Inputs map[string]string `json:"inputs,omitempty"` // declared inputs only
    Origin Origin            `json:"origin"`           // for audit only
}

type Origin struct {
    Kind      string    `json:"kind"` // "manual" | "webhook" | "schedule" | "cli"
    Actor     string    `json:"actor,omitempty"`
    RequestAt time.Time `json:"requested_at"`
}

type Update struct {
    State      RunState  `json:"state"`
    ExitCode   *int      `json:"exit_code,omitempty"`
    ErrorCode  string    `json:"error_code,omitempty"`
    ErrorMsg   string    `json:"error_message,omitempty"` // redacted
    StartedAt  *time.Time `json:"started_at,omitempty"`
    FinishedAt *time.Time `json:"finished_at,omitempty"`
    Artifacts  []ArtifactRef `json:"artifacts,omitempty"` // names+sizes, not content
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

### Local broker

Unix socket, newline-delimited JSON, plus a sqlite-backed queue so `latem run` works when the agent is momentarily busy. Claim is a conditional `UPDATE … WHERE state='queued'` and relies on sqlite's write serialization.

### Remote broker

WebSocket to the control plane. `Authorization: Bearer <machine-token>`.

- Reconnect with exponential backoff: 1s → 2s → 4s … capped at 60s, ±20% jitter.
- Resume by sending the last acknowledged run id; the server replays anything missed. The agent MUST tolerate replays — every handler is idempotent by run id.
- Log writes buffer to disk while disconnected and flush on reconnect, bounded at 50 MB per run; beyond that, drop with a marker line.
- If the token is rejected (`401`), stop retrying, log loudly, and continue serving the local broker. A revoked token must never disable local operation.

---

## 8. Configuration

`~/.latem/config.toml`, mode `0600`. Reloaded on `SIGHUP`; changes to `[[runner]]` require a restart.

```toml
schema_version = 1

[machine]
name  = "mac-mini"                 # display only
id    = "mch_01H..."               # assigned at register; stable
labels = ["macos", "arm64", "xcode-16", "ios"]

[agent]
shutdown_grace = "30m"
work_dir       = "~/.latem/work"
log_retention  = "14d"

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
allowed_refs = ["main", "release/*"]
require_approval_otherwise = true

[repos.api]
url          = "ssh://git@forgejo.internal/me/api.git"
allowed_refs = ["main"]

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

[[runner]]
name        = "gpu"
executor    = "docker"
labels      = ["linux", "amd64", "gpu", "cuda"]
concurrency = 1

  [runner.docker]
  default_image = "nvidia/cuda:12.4.0-devel-ubuntu22.04"
  gpu           = true
  memory        = "32g"

[[runner]]
name        = "ios"
executor    = "tart"
labels      = ["macos", "arm64", "xcode", "ios"]
concurrency = 1                     # non-negotiable; see §13.3

  [runner.tart]
  base_image  = "latem-xcode-16"
  cpus        = 6
  memory_gb   = 16
  ssh_user    = "admin"
  boot_timeout = "3m"
```

**Validation at load.** Unknown keys are an error, not a warning (strict decode). `schema_version` mismatch is fatal with a migration hint. Every `[[runner]]` name must be unique. Every `allowed_refs` glob must compile. `token_file` must be `0600` or the agent refuses to start.

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
      - name: Test
        run: ./ci/test.sh

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
      - name: Sync certificates
        run: ./ci/ios/match.sh
      - name: Build and upload
        run: ./ci/ios/build.sh
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

### Interpolation rules

`${input.NAME}` is the **only** interpolation syntax, and it is legal **only** inside `steps[].env` values. It is substituted into the env map after type-checking, never into `run`. There is no `${{ }}`, no expressions, no conditionals, no `if:`. Branching belongs in the shell script.

Secrets are never interpolated. They arrive in the environment by name because they were declared in `secrets:`.

---

## 10. Job resolution and admission

Ordered. Every gate before "execute" must pass, and each failure is a distinct error code (§21).

```
1.  repo lookup            → E_UNKNOWN_REPO      (map hit in config; no path building)
2.  ref validation         → E_INVALID_REF       (regex + prefix + ".." checks, INV-3)
3.  ref policy             → awaiting_approval   (allowed_refs glob; INV-5)
4.  fetch + resolve ref    → E_SOURCE            (to an immutable SHA)
5.  worktree checkout      → E_SOURCE
6.  read .latem/jobs.yaml  → E_NO_REGISTRY / E_BAD_REGISTRY
7.  job lookup             → E_UNKNOWN_JOB
8.  label match            → E_NO_RUNNER         (runner ⊇ job.runs_on)
9.  input validation       → E_BAD_INPUT         (declared, typed, bounded)
10. secret resolution      → E_MISSING_SECRET    (all declared names resolvable)
11. concurrency group      → queued              (wait, do not fail)
12. cache mount prep       → E_CACHE
13. execute
```

Gates 1–3 run **before** any network or filesystem activity, so a hostile request is cheap to reject. Gate 4 is the first action that touches the outside world.

---

## 11. Run state machine

```
                    ┌──────────────────┐
                    │ awaiting_approval│──(latem approve)─┐
                    └──────────────────┘                  │
                             ▲                            ▼
  queued ──claim──▶ claimed ─┴─ preparing ──▶ running ──▶ collecting ──▶ succeeded
     │                 │           │             │            │              
     │                 │           │             │            └──▶ failed
     └──▶ rejected     │           └──▶ failed   ├──▶ failed
                       │                         ├──▶ timed_out
                       └──▶ cancelled            ├──▶ cancelled
                                                 └──▶ interrupted
```

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
| `rejected` | yes | Admission gate failure (§10) |
| `interrupted` | yes | Agent died mid-run; set during startup reconciliation |

**Rules.**

- Transitions are persisted to sqlite **before** being reported to the broker. A broker failure never loses local truth.
- `Report` is retried with backoff; the run does not block on it.
- `collecting` always runs, including after `failed` and `timed_out`, so artifacts and logs from a broken build survive. Only `cancelled` may skip artifact collection.
- Teardown is `defer`-registered at resource creation, so no path leaks a container or VM.

---

## 12. Source acquisition

**Mirror plus worktree.** A persistent bare mirror per repo, a disposable worktree per run. Fetches are incremental; checkouts are clean; parallel runs do not share a working directory.

```
~/.latem/
  mirrors/<repo-key>.git/      # bare, persistent, --mirror
  work/<run-id>/               # worktree, deleted after collecting
```

```bash
# once
git clone --mirror -- "$URL" "$MIRRORS/$KEY.git"

# per run
git -C "$MIRRORS/$KEY.git" fetch --prune --tags -- origin
SHA=$(git -C "$MIRRORS/$KEY.git" rev-parse --verify "${REF}^{commit}")
git -C "$MIRRORS/$KEY.git" worktree add --detach "$WORK/$RUN_ID" "$SHA"
```

Notes:

- `${REF}^{commit}` fails loudly on annotated tags and non-commit refs rather than checking out something surprising.
- Every invocation uses `--` before the ref and passes argv as a slice (INV-3).
- Submodules: `git submodule update --init --recursive --depth 1` inside the worktree, only when `.gitmodules` exists.
- LFS: `git lfs fetch --all` in the mirror, `git lfs checkout` in the worktree, only when `.gitattributes` mentions LFS.
- A per-repo mutex serialises fetches into one mirror. Concurrent `fetch` into a bare repo is not reliably safe.
- Credentials are the machine's own — SSH agent or a credential helper. The agent never receives, stores, or forwards git credentials.
- `git worktree prune` on the mirror during startup reconciliation.

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
    Workspace  string            // host path to the worktree
    Image      string
    Env        map[string]string // includes redacted-at-log secrets
    Caches     []CacheMount
    Labels     map[string]string // includes latem.run-id for reaping
    Resources  Resources
}
```

### 13.1 Docker executor

Target: Linux x86_64 (desktop) and Linux arm64 (Mac mini container role).

- Workspace bind-mounted at `/workspace`, `rw`. Container `WORKDIR` is `/workspace`.
- Cache volumes mounted at the paths from `caches[].path` (§14).
- Label every container `latem.run-id=<id>` and `latem.managed=true`. Startup reconciliation depends on this.
- `--rm` is **not** used; the agent removes containers explicitly after `Fetch`, so artifacts remain retrievable if a step fails.
- Non-root user inside the container. Provide `runner` (uid 1000) in the base image; bind-mount ownership is the usual friction point — document it, and on Linux pass `--user $(id -u):$(id -g)`.
- Resources: `--cpus`, `--memory`, `--pids-limit 4096`.
- GPU: when `runner.docker.gpu = true`, add `DeviceRequests` for all GPUs (equivalent to `--gpus all`). Preflight must verify NVIDIA Container Toolkit by running `nvidia-smi` in a throwaway container; fail the runner at startup, not mid-build.
- Each step is `docker exec`-equivalent into the same container, so `cd`, `export`, and installed tools persist across steps within a run.

### 13.2 Tart executor (macOS, ephemeral)

Target: any job touching Xcode. This is the isolation story for the Mac mini.

```
Prepare:
  tart clone <base_image> latem-run-<run-id>
  tart set latem-run-<run-id> --cpu N --memory M
  tart run --no-graphics --dir=workspace:<host-worktree> latem-run-<run-id> &
  poll `tart ip latem-run-<run-id>` until routable or boot_timeout
  ssh -o StrictHostKeyChecking=accept-new <ssh_user>@<ip> true   # readiness

Exec:
  ssh … 'bash -lc <step>'    # step passed as a single argv element

Fetch:
  rsync from the shared dir, or scp by glob

Close:
  tart stop  → wait → tart delete   (both idempotent; delete even if stop fails)
```

- Apple's licence permits **two** guest VMs per host. `concurrency` for a tart runner MUST be `<= 2`, and the config loader enforces it.
- Caches cannot be Docker volumes here. Use host directories shared with `--dir`, one per cache name (§14.2).
- The base image (`latem-xcode-*`) is built once by a documented Packer-style script in `examples/`: Xcode, command-line tools, Homebrew, Node, fastlane, CocoaPods. Rebuilding on Xcode upgrades is a documented operator task.
- Boot cost is real (30–60s). Acceptable against a 20-minute iOS build; do not use tart for short jobs.

### 13.3 Native executor (macOS, no VM)

Fallback when Tart is unavailable or the base image is not built.

- Runs directly on the host as the logged-in user. **No isolation.** Document this plainly; it is a convenience mode.
- `concurrency` MUST be 1. Parallel Xcode builds contend on DerivedData and the keychain and produce failures that look like code bugs.
- Requires a **LaunchAgent under an auto-login user**, not a LaunchDaemon. `codesign` needs a real user session with keychain access; daemon contexts fail with errors that point nowhere useful. This is the single most common install failure — `latem doctor` must detect and name it.
- Every step runs `bash -lc` so the login environment (Homebrew paths, rbenv, nvm) is present.
- Workspace is the worktree directly; no copy.

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
- Worktrees under `work/` older than 24h with no live run: delete, then `git worktree prune`.
- Mirrors untouched for 90 days: report in `latem doctor`, never auto-delete (a mirror may be the only local copy).
- When free disk drops below `min_free_gb` (default 20), prune aggressively and refuse new runs with `E_DISK` rather than failing halfway through a build.

---

## 15. Secrets

Resolution chain, first hit wins. Only names declared in the job's `secrets:` block are looked up.

1. `LATEM_SECRET_<NAME>` in the agent's own environment
2. `~/.latem/secrets.env` — `KEY=value`, mode `0600` enforced, agent refuses to start otherwise
3. macOS Keychain — `security find-generic-password -s latem -a <NAME> -w`
4. `pass` / `gopass`, if configured
5. A shell command per secret, defined in config

```toml
[secrets]
provider = "chain"                                # env → file → keychain → command
[secrets.commands]
MATCH_PASSWORD = "op read op://Private/match/password"
```

Rules:

- Values are held in memory only, zeroed on run completion where the language allows.
- Registered with the redactor (§16) **before** the first step starts.
- Never written to sqlite, never included in `Update.ErrorMsg`, never sent to a broker (INV-4).
- A declared secret that resolves empty is `E_MISSING_SECRET` at admission, not a runtime surprise 40 minutes in.
- `latem doctor --secrets` reports resolvable/unresolvable **names only**, never values.

---

## 16. Log pipeline and redaction

```
step stdout/stderr
   → line splitter (64 KB max line; longer lines truncated with a marker)
   → REDACTOR  ← every secret value, every token, every matched pattern
   → fan-out ─┬─ local file  ~/.latem/logs/<run-id>.log
              ├─ broker writer (batched: 100 lines or 250 ms)
              └─ `latem logs -f` subscribers (ring buffer, 1000 lines)
```

Redaction is applied **once, at the point of emission**, before fan-out. A new sink therefore cannot bypass it (INV-4).

**Redactor contents:**

- Every resolved secret value, and its base64 and URL-encoded forms — CI tooling re-encodes secrets constantly, and a base64'd token in a log is still a leaked token.
- The machine token.
- Pattern matches, always on: AWS keys `AKIA[0-9A-Z]{16}`, private key blocks `-----BEGIN [A-Z ]*PRIVATE KEY-----`, GitHub tokens `gh[pousr]_[A-Za-z0-9]{36,}`, Slack tokens `xox[baprs]-`, JWTs `eyJ[A-Za-z0-9_-]{10,}\.`, Apple `.p8` contents.
- Replacement is `***` plus a stable 6-char hash of the value, so `***a3f9c1` lets an operator correlate occurrences without learning the secret.
- Values shorter than 6 characters are not redacted — matching them produces log soup. Document this; warn at load when a secret is short.

Retention: `agent.log_retention` (default 14d), enforced by the daily ticker. Logs are gzipped after 24h.

---

## 17. Artifacts

- Globs are relative to the workspace root and MUST NOT escape it. Resolve, then verify the result is within the workspace; reject symlinks pointing outside (`E_ARTIFACT`).
- Copied to `~/.latem/artifacts/<run-id>/`, preserving relative paths.
- Retention per declaration, enforced daily.
- Only `ArtifactRef{name, size, sha256}` is reported to a broker. Content never crosses the boundary (§5).
- Large-artifact hygiene: `.ipa` and `.aab` files are 50–300 MB. Default total artifact budget 50 GB, evicted oldest-first, surfaced in `latem doctor`.

---

## 18. Mobile build pipeline

The wedge feature. Everything here lives in the repo's own `ci/` directory — the agent has no mobile-specific code. `examples/expo-app/` is the reference implementation and should be treated as part of the deliverable.

### 18.1 iOS

Runner: `ios` (tart, concurrency 1).

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

Caches: `cocoapods`, `derived-data`, `pnpm`. DerivedData is the largest single win and the reason not to use a fully clean VM without shared cache dirs.

**Known failure modes to document in `docs/ios.md`:**

| Symptom | Cause |
|---|---|
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

### 18.3 Why `--local` is safe to depend on

`eas build --local` is a supported Expo flag running the same pipeline on the local machine. The runner invokes it as an ordinary command; there is no reverse engineering and no private API. If Expo changes the flag, the blast radius is one shell script in `ci/`, not the agent.

---

## 19. Local trigger and CLI

```
latem agent                          Run the daemon (foreground; service wrappers in §23)
latem run <repo> <job> [--ref R] [--input k=v] [--follow] [--wait]
latem list [--state S] [--repo R]
latem logs <run-id> [-f]
latem cancel <run-id>
latem approve <run-id>               Local-only; satisfies INV-5
latem register --url U --token T     Write config + machine token, verify connectivity
latem doctor [--secrets] [--fix]     Preflight, diagnose, name the fix
latem cache ls | prune | rm <name>
latem version
```

**`latem doctor` is a first-class deliverable, not a nice-to-have.** It is what makes G6 achievable and what turns the failure table in §18.1 into a self-service fix. It must check, and for each failure print the exact command to run:

- git, docker, tart availability and versions
- Docker daemon reachable; current user in the `docker` group
- NVIDIA toolkit functional when `gpu = true`
- macOS: LaunchAgent vs LaunchDaemon context, auto-login enabled, keychain unlockable, Xcode selected, licence accepted
- Every configured repo reachable with current credentials
- Every declared secret resolvable (names only)
- Disk headroom against `min_free_gb`
- Token file permissions `0600`
- Clock skew against the broker (token validation fails confusingly otherwise)

---

## 20. Health, heartbeat, self-update

**Heartbeat** every 15s carrying `MachineHealth`. The control plane marks a machine offline after 3 missed beats (45s). Heartbeats continue while draining, with `Draining: true`.

**Preflight** runs at startup, per executor, and every 5 minutes thereafter. A runner failing preflight is marked unavailable and stops accepting runs; recovery is automatic when the check passes again. Never accept a run onto a runner that cannot execute it.

**Self-update** is opt-in and off by default (`[agent] auto_update = false`). When enabled: check a release feed daily, verify a minisign signature, download to a temp path, atomic rename, re-exec only when no run is active. Never update mid-run.

---

## 21. Error taxonomy

Stable codes; the web app maps them to human copy, and they appear in `Update.ErrorCode`.

| Code | State | Retryable | Meaning |
|---|---|---|---|
| `E_UNKNOWN_REPO` | rejected | no | Repo not in local config allowlist |
| `E_INVALID_REF` | rejected | no | Ref failed validation (INV-3) |
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
| `E_TIMEOUT` | timed_out | no | Wall clock exceeded |
| `E_ARTIFACT` | failed | no | Glob escaped the workspace, or collection failed |
| `E_INTERRUPTED` | interrupted | yes | Agent died mid-run; set at reconciliation |

Retryable errors are safe for the control plane to offer as a one-click retry. Non-retryable ones need a human or a commit.

---

## 22. Persistence

sqlite at `~/.latem/state.db`, WAL mode, embedded migrations run at startup.

```sql
CREATE TABLE runs (
  id                TEXT PRIMARY KEY,
  repo              TEXT NOT NULL,
  job               TEXT NOT NULL,
  ref               TEXT NOT NULL,
  sha               TEXT,
  runner            TEXT,
  state             TEXT NOT NULL,
  exit_code         INTEGER,
  error_code        TEXT,
  error_message     TEXT,          -- redacted
  origin_kind       TEXT NOT NULL,
  origin_actor      TEXT,
  inputs_json       TEXT NOT NULL DEFAULT '{}',
  created_at        INTEGER NOT NULL,
  started_at        INTEGER,
  finished_at       INTEGER,
  reported_state    TEXT,          -- last state acked by the broker
  UNIQUE(id)
);
CREATE INDEX runs_state_idx ON runs(state) WHERE state NOT IN
  ('succeeded','failed','cancelled','rejected','timed_out','interrupted');
CREATE INDEX runs_repo_created_idx ON runs(repo, created_at DESC);

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

`reported_state` is what makes broker reporting recoverable: on reconnect, replay every run where `reported_state != state`.

`inputs_json` MUST be scrubbed of anything matching the redactor before persistence.

---

## 23. Installation and service management

**Linux — systemd user unit** at `~/.config/systemd/user/latem.service`, with `loginctl enable-linger $USER` so it survives logout.

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

**macOS — LaunchAgent** at `~/Library/LaunchAgents/com.latem.agent.plist`, `RunAtLoad`, `KeepAlive`.

Two things `latem doctor` must verify and explain, because getting either wrong produces opaque signing failures (§13.3):

1. This is a LaunchAgent, not a LaunchDaemon.
2. Auto-login is enabled for the user, so a session exists after reboot.

**Install script** `curl -fsSL … | sh`: detect OS/arch, download the matching binary, verify checksum, place in `~/.local/bin`, run `latem doctor`, print the next command. Never run as root.

---

## 24. Testing requirements

**Unit** — registry parsing (valid, invalid, unknown-field), ref validation table (§5 hostile inputs), input type checking, redactor correctness including encoded forms, state machine transition legality, cache name derivation.

**Integration** (`test/integration`, build-tagged, needs Docker) — full run against a fixture repo; failing step produces `failed` with the right exit code; timeout produces `timed_out` and tears down; agent killed mid-run leaves nothing orphaned after restart; concurrent runs on one runner do not corrupt a shared cache; artifact glob escaping is rejected.

**Invariant tests** (`invariants_test.go`) — one per rule in §5, named `TestINV1_NoInboundListeners` and so on, so a failure names the rule it broke.

**Manual matrix** (documented in `docs/testing.md`; not automatable) — iOS build end to end on real hardware with real signing; Tart VM lifecycle including forced kill; GPU passthrough; reboot recovery on both platforms.

---

## 25. Work breakdown

Milestones are ordered by dependency and sized to be independently reviewable. Each is complete only when its acceptance criteria pass.

### M0 — Prove the hard part manually (no code)

Not an engineering task, but it gates everything after it. Run `eas build --platform ios --local` by hand on the Mac mini, with match, and get an `.ipa` to TestFlight. Do the Android equivalent.

*Acceptance:* a build lands in TestFlight and in Play internal testing, and every command used is written down in `docs/manual-baseline.md`. Those commands become `examples/expo-app/ci/`.

### M1 — Skeleton and config

`cmd/latem`, cobra wiring, `config.toml` load with strict decode and validation, sqlite with migrations, `latem version`, `latem doctor` covering git/docker/disk.

*Acceptance:* `latem doctor` on a clean machine reports accurate findings and exits non-zero when a dependency is missing.

### M2 — Registry and admission

`.latem/jobs.yaml` parsing, strict validation, the §10 gate chain, the full error taxonomy, `${input.NAME}` substitution confined to `steps[].env`.

*Acceptance:* the hostile-input table in §5 passes; every gate returns its documented code; INV-2 and INV-3 tests are green.

### M3 — Docker executor and source

Mirror + worktree acquisition, docker Prepare/Exec/Fetch/Close, container labelling, cache volume mounting, log streaming to file and terminal, `latem run --follow`.

*Acceptance:* `latem run api test` builds a real repo end to end with no broker configured, warm caches on the second run, and a `git status`-clean worktree.

### M4 — Agent daemon and local broker

Runner pool, concurrency limits, concurrency groups, run state machine, graceful shutdown, crash reconciliation, unix socket broker, service install for both platforms.

*Acceptance:* `kill -9` mid-run leaves zero orphaned containers or worktrees after restart, and the run shows `interrupted`.

### M5 — macOS executors and the mobile pipeline

Tart executor, base image build script, native executor, keychain handling, host-directory caches, `examples/expo-app/` with working `ci/` scripts, `docs/ios.md`.

*Acceptance:* `latem run myapp ios-release` produces a TestFlight build on a clean checkout, and the second run is measurably faster from warm DerivedData.

### M6 — Remote broker

WebSocket client, reconnect with backoff and jitter, resume-and-replay, buffered log flush, heartbeat, `latem register`, `latem approve`.

*Acceptance:* the agent survives a 10-minute network partition mid-run, reports correct terminal state on reconnect, and continues serving the local broker with a revoked token.

### M7 — Operational polish

Cache pruning and disk guards, artifact retention, log rotation, `latem doctor --fix`, install script, release pipeline with signed binaries, README and quickstart.

*Acceptance:* a new machine goes from `curl | sh` to a green run in under five minutes, following only the README.

---

## 26. Open questions

1. **Registry location for polyrepo setups.** `.latem/jobs.yaml` in the repo is right for the common case, but a monorepo may want several. Proposal: allow `.latem/jobs.d/*.yaml`, merged, with duplicate job names an error. Defer to post-M7.
2. **Step-level timeouts.** Currently job-level only. A 90-minute iOS job hiding a hung 5-minute step wastes an hour. Likely worth adding to §9 as `steps[].timeout` in M2 — cheap now, awkward later.
3. **Matrix jobs.** Explicitly out of scope (N1), but multi-arch builds want them. Watch whether `examples/` ends up duplicating jobs; if so, reconsider with a strictly declarative, non-expression form.
4. **Windows.** No current need. The executor interface should not acquire assumptions that make it impossible, but no work until someone asks.
5. **Log storage cost at the control plane.** Verbose Gradle output is large. Decide whether the agent should ship full logs remotely by default or only on failure; `[agent] remote_logs = "always" | "on_failure"` is the likely shape.
6. **Approval UX.** INV-5 requires local approval, which is safe but awkward when travelling — precisely the case the hosted layer exists for. A signed approval from an authenticated session may be acceptable, but it weakens the rule and needs a written threat model before it ships.
