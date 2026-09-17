# Latem

**Self-hosted CI that treats a Mac mini as a first-class build machine.**

Buildkite, Woodpecker, Drone, Forgejo Actions, and Actions Runner Controller
all run jobs on hardware you own. Latem exists for the three things they leave
to you.

### 1. Xcode is an executor, not a shell script

An iOS build gets a real user session, an unlocked keychain, and code signing
that works. Not a `runs-on: macos` label pointing at a machine somebody set up
by hand, and not a container pretending Xcode fits in one.

Latem ships three executors behind one interface: Docker for Linux, and two for
macOS — **native**, which runs on the Mac's own OS, and **Tart**, which gives
each build an ephemeral VM (`tart clone`, boot, run, delete).

**Native is the default**, because on a Mac you already own it needs nothing
installed and no disk: your Xcode, your keychain, your simulators. Tart is one
uncommented config block away, and it costs a 69 GB image pull and 140 GB of
disk. The question that decides which you want is not how big your team is —
it is **whose code runs on this machine**. Your own repositories, native is
right. Anything a contributor outside the team can influence, you want the VM,
because native runs steps as you, with your keychain and your signing
certificates. `docs/RUNNER_SPEC.md` §13.0 is the long version, and
[`docs/deploy.md`](docs/deploy.md) is the ordered runbook for bringing up a Mac
from scratch.

A job picks a runner by label, not by executor, so switching a machine between
the two is a config edit and touches no repository. The step contract is
identical across all three — each step is a new process, the filesystem
persists, `$LATEM_ENV` carries variables between them — so a job that works on
one works on the others.

### 2. Caches are mounted, not archived

Hosted CI needs save-and-restore because its machines are ephemeral. Yours are
not.

```yaml
caches:
  - name: pnpm-store
    path: /home/runner/.local/share/pnpm/store
```

That is a Docker named volume, or a host directory shared into the VM. There is
no tarball, no upload, no size ceiling, no cache key to get wrong, and no cache
step in the job. `~/.gradle` is simply still there. A cold `pod install` and a
warm one differ by the thing that actually differs.

Caches are scoped per runner, never shared across runners, and a runner at
concurrency > 1 takes a per-cache lock. Two runs writing one `~/.gradle`
corrupt each other in ways that surface as intermittent failures nobody
attributes to caching, so that is prevented structurally rather than
documented.

### 3. Five security invariants, written down and tested

A self-hosted runner is a machine on your network holding your signing
certificates and running code a webhook asked it to run. Latem's threat model
is a document with tests attached, not a paragraph in a README.

| | |
|---|---|
| **INV-1** | The agent makes only outbound connections. No listener on any non-routable interface, no port to forward, no inbound firewall rule. The local CLI talks over a `0600` Unix socket. |
| **INV-2** | The control plane supplies names, never definitions. The inbound command set is closed at two kinds — `run` and `cancel`. A `run` carries `{id, repo, job, ref, inputs, origin}` and has no field able to express a command, an image, a path, an executor, or a timeout. |
| **INV-3** | Nothing from outside the trust boundary reaches a shell. Every git invocation passes argv as a slice with an options terminator, so a ref named `--upload-pack=/bin/sh` is a ref. |
| **INV-4** | Secrets never leave the machine. Redaction happens at the point of emission, so no sink can be added that bypasses it — including multi-line values, line by line, because `APP_STORE_CONNECT_KEY_P8` is a PEM block and redacting only the whole value redacts nothing. |
| **INV-5** | The job registry comes from your disk at the resolved commit. `jobs.yaml` is read from the checked-out workspace, never fetched from a broker, so a run is reproducible from the commit alone. Refs outside your allowlist need a local `latem approve`, and what that approves is one commit — if the branch moves before the run starts, it is parked again rather than built. |

Each has a test named after it. A pull request that weakens one is rejected
regardless of its other merits. The full statement is
[`docs/RUNNER_SPEC.md` §5](docs/RUNNER_SPEC.md).

---

## Install

**No release is tagged yet, so the installer has nothing to download** — it
fails on the checksum fetch rather than installing a broken binary. The release
pipeline is the last open item before v0.1 (see [Status](#status)). Until it
lands, build from source with Go 1.25 or newer:

```console
$ git clone https://github.com/Abyssinia-Labs/latem && cd latem
$ make latem && ./bin/latem doctor
```

Once a release exists, installing is one line:

```console
$ curl -fsSL https://raw.githubusercontent.com/Abyssinia-Labs/latem/main/install.sh | sh
```

Never as root. The script detects your OS and architecture, verifies a
checksum, installs to `~/.local/bin`, and runs `latem doctor`.

Then, either way:

```console
$ latem init --repo myapp=git@github.com:me/myapp.git   # write a config.toml
$ latem doctor            # check the machine; every failure names its fix
$ latem service install   # run the agent at boot
$ latem run myapp test    # trigger a build
```

## `latem doctor`

The reason a clean machine reaches a green build in under five minutes is that
almost every failure mode of a self-hosted runner is environmental, and almost
none of them produce an error that names the environment. `doctor` checks
thirty-odd things and prints the exact command that repairs each one.

```console
$ latem doctor

Tooling
  [ ok ] git                     git version 2.50.1 (Apple Git-155)
  [ ok ] docker daemon           reachable
  [skip] tart                    not installed, and no runner uses it

Disk
  [ ok ] free disk / ios         394.7 GB free, floor 60 GB (native)
  [ ok ] mirrors and work_dir    one filesystem, so clones hardlink

Executors
  [FAIL] base image / linux-x64  ghcr.io/me/ci-base:latest does not satisfy the §13.1 contract
         user-1000: no /etc/passwd entry for uid 1000; add one named runner with gid 1000
         tool-git: git is missing
         fix: add to your image: RUN useradd -u 1000 -m runner && apt-get install -y bash git

macOS session
  [warn] FileVault               on — an unattended reboot stops before login
         fix: sudo fdesetup authrestart
         fix: sudo fdesetup disable

18 passed, 2 warning(s), 4 failed, 3 skipped
```

Among the checks worth knowing about:

- **Mirrors and `work_dir` on one filesystem.** Latem clones each workspace
  from a bare mirror with `git clone --local`, which hardlinks the object
  store. Across filesystems, git silently copies it instead — no error, no
  warning, just a multi-gigabyte copy per run and a machine that got slower for
  reasons nobody attributes correctly.
- **The Docker base image contract.** `doctor` runs your image once and checks
  that uid 1000 exists, `$HOME` is writable, and `bash` and `git` are present.
  An image missing them fails forty minutes into a build as npm `EACCES` or a
  Gradle daemon that will not start — errors that look like tooling bugs.
- **FileVault**, which quietly falsifies "survives reboots". See below.
- **Every configured repo, reachable with the credentials this machine has.**
  An agent started by launchd does not inherit your interactive ssh agent.
- **Clock skew against the broker**, because token validation fails
  confusingly otherwise.
- **Whether an App Store Connect credential set agrees with itself**, under
  `--secrets`. Not whether each value is present — whether the key id names the
  key that signs the token, and whether Apple accepts it. Every way of getting
  that wrong produces the same 401 from `fastlane pilot upload`, two minutes
  in, after a full archive and export. Verified on a laptop holding four
  `AuthKey_*.p8` files, all valid, one of them the right one.
- **The locale a step's shell actually resolves to.** The agent sets
  `LANG=en_US.UTF-8`, and a login shell's profile can still overrule it, so
  `doctor` asks the shell rather than trusting the executor. Without it
  `pod install` aborts naming Unicode instead of the locale.
- **A committed Xcode scheme**, read from the repo's bare mirror. Xcode writes
  schemes to `xcuserdata/` by default and nobody commits that, so this works on
  every developer's machine and fails on every runner — where every run is a
  fresh clone.

`latem doctor --secrets` also resolves every locally-known secret name — names
only, never values — and warns about any short enough that the redactor will
skip it. `latem doctor --fix` prints a consolidated list of the remediation
commands; it never runs them.

## Running a Mac mini headless

Three things have to be true for a macOS machine to come back on its own after
a reboot, and `doctor` checks all three.

**It must be a LaunchAgent, not a LaunchDaemon.** `codesign` needs a real user
session with keychain access. In a daemon context it fails with errors that
point nowhere useful, and this is the single most common install failure.
`latem service install` writes `~/Library/LaunchAgents/com.latem.agent.plist`
and never touches `/Library/LaunchDaemons`.

**Auto-login must be enabled** for the account the agent runs as, so a session
exists after a reboot.

**FileVault is a decision you have to make.** With it on, an unattended reboot
stops at the pre-boot unlock screen: no login, no session, no LaunchAgent, no
builds — until somebody walks over to the machine and types a password. There
is nothing in any log, because the agent never started to write one. The
symptom is "my machine went offline overnight and nothing explains it".

Two valid answers, and Latem picks neither:

- **Disable FileVault**, accepting that a stolen machine yields its signing
  certificates and keystores.
- **Keep it and reboot only via `sudo fdesetup authrestart`**, which unlocks
  the disk for exactly the next boot and works over ssh. A kernel panic or a
  power cut still parks the machine at the unlock screen.

A machine holding signing certificates is a reasonable thing to want
encrypted. The point is that it should be a decision rather than a surprise.

## Configuration

`~/.latem/config.toml`, mode `0600`. One agent owns a config directory, a
machine identity, and a set of labelled runners — "two roles on one box" means
two `[[runner]]` blocks, not two agents.

```toml
schema_version = 1

[machine]
name   = "mac-mini"
labels = ["macos", "arm64", "xcode-26", "ios"]

[agent]
shutdown_grace = "30m"           # an iOS build is long
metrics_addr   = "127.0.0.1:9838"  # loopback only; "" disables (INV-1)

[repos.myapp]
url          = "git@github.com:me/myapp.git"
allowed_refs = ["main", "release/*"]     # expands to refs/heads/* (INV-5)
require_approval_otherwise = true

[[runner]]
name        = "ios"
executor    = "native"           # the default; Tart is the isolation upgrade
labels      = ["macos", "arm64", "xcode", "ios"]
concurrency = 1                  # enforced — parallel Xcode builds contend

  [runner.native]
  min_free_gb = 60
```

Every rule is validated at load and fatal there: a runner that starts on a
broken config does not fail at startup, it fails forty minutes into somebody's
build. Disk floors are per runner — 40 GB for docker, 60 for native, 200 for
tart — because a global 20 GB admits an iOS build the machine cannot finish.

Jobs live in `.latem/jobs.yaml` in your own repository:

```yaml
version: 1

jobs:
  ios-release:
    runs_on: [macos, xcode, ios]
    timeout: 90m
    caches:
      - { name: pods,        path: /Users/admin/Library/Caches/CocoaPods }
      - { name: pnpm-store,  path: /Users/admin/.local/share/pnpm/store }
    secrets: [MATCH_PASSWORD, APP_STORE_CONNECT_KEY_P8]
    steps:
      - name: install
        run: pnpm install --frozen-lockfile
      - name: build
        run: ci/ios/build.sh
```

## Commands

```
latem init --repo <name>=<url>       Write a starting config.toml for this machine
latem agent                          Run the daemon (foreground; see `latem service install`)
latem run <repo> <job> [--ref R] [--input k=v] [--follow] [--wait]
latem list [--state S] [--repo R]
latem logs <run-id> [-f]
latem cancel <run-id> [--reason R]
latem approve <run-id>               Local only; pins that run's commit (INV-5)
latem register --url U --token T
latem doctor [--secrets] [--fix]
latem cache ls | prune | rm <name>
latem service install [--print] | uninstall | status
latem version
```

`--print` on `service install` renders the unit or plist to stdout without
installing it. Read it before you install it: that flag is what caught a
LaunchAgent pointing its log destination at a directory nothing created, which
launchd reports by failing to start and writing the reason to the file it could
not open.

Exit codes are contract:

| | |
|---|---|
| `0` | success |
| `1` | failure — a check failed, a run failed, an operation did not complete |
| `2` | usage — an unknown flag, a missing argument, an unknown subcommand |
| `3` | no agent is listening (`E_AGENT_DOWN`) |

Three is the interesting one. `latem run --wait` finding nothing to wait for is
not the same event as a build that failed, and a script has to be able to tell
them apart without parsing English.

## Metrics

`metrics_addr` serves Prometheus text at `/metrics` and a JSON snapshot at
`/debug/vars`, on loopback and nowhere else. INV-1 forbids a routable listener;
it does not forbid this one. Anyone wanting the numbers off-machine points a
local Prometheus at it or tunnels — the agent does not grow a remote surface
for observability.

Exposed: runs by terminal state, run duration, queue depth, cache sizes, disk
free per runner, runner availability, broker connection state, and dropped log
frames.

## Status

Under active development toward v0.1. Every command above is wired and does
what it says.

The claim this project exists to make has been tested rather than argued: on
2026-08-11 the agent, started by launchd rather than by a shell, took
`ios-release` from a bare clone to a signed build in TestFlight, with `codesign`
reaching the login keychain from a launchd-spawned process. INV-4 was then
checked against that run and not against a fixture — neither signing password,
nor the issuer id, nor any interior line of the App Store Connect PEM appears
in the run log, in any other log, or in the sqlite store. The same sequence has
since been run on a second machine that had never seen the software
([`docs/deploy.md`](docs/deploy.md)), which found three defects the first could
not.

Built: the Docker and native executors, mirror-and-hardlink source acquisition,
the registry and its admission gates, commit-pinned approval, the secrets chain
and the redactor, mounted caches and artifact collection, the run state machine,
the agent and its local broker, and the remote broker. All five invariants have
tests named after them and all five are green.

Not built, and honestly named:

- **The release pipeline** — signed binaries and minisign self-update. It is
  why the installer above has nothing to fetch, and it is the last thing
  between here and v0.1.
- **The Tart executor**, deferred to v2 by amendment A31. Native is the default
  because the first deployment builds repositories its operator controls; Tart
  is the isolation upgrade for when that stops being true, and `RUNNER_SPEC`
  §13.0 is the criterion rather than a preference.
- **A reboot with nobody logged in.** [`docs/deploy.md`](docs/deploy.md) §1–§8
  have been followed end to end on two machines; §9 is the one section still
  written from the spec rather than from a terminal, because a LaunchAgent
  needs a user session and auto-login and FileVault decide whether one exists.
- **The far end of the remote broker.** The control plane lives in its own
  repository now, so nothing here exercises a real server. What pins the wire
  is [`docs/PROTOCOL.md`](docs/PROTOCOL.md) and `test/conformance`, which
  replays recorded frames in both directions against committed fixtures.

Where to read further, in the order a new operator needs them:

- [`docs/RUNNER_SPEC.md`](docs/RUNNER_SPEC.md) — the specification, including
  §5's invariants and the forty amendments the implementation waves produced.
  The amendments are the interesting part: each one names the defect that
  forced it.
- [`docs/PLAN.md`](docs/PLAN.md) — workstreams, file ownership, and where each
  one stands.
- [`docs/deploy.md`](docs/deploy.md) — the ordered runbook for bringing up a
  Mac runner, written from two machines rather than from the spec.
- [`docs/manual-baseline.md`](docs/manual-baseline.md) — every command that
  actually worked, and the twenty-five defects found on the way.
- [`docs/ios.md`](docs/ios.md), [`docs/android.md`](docs/android.md) — the
  mobile pipelines, including the failure table `doctor` turns into a
  self-service fix.

Apache-2.0.
