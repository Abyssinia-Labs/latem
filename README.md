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
# .latem/jobs.yaml — declared once at the top level
caches:
  - name: pnpm-store
    path: /home/runner/.local/share/pnpm/store

jobs:
  test:
    caches: [pnpm-store]      # jobs reference it by name
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
| **INV-5** | The job registry comes from your disk at the resolved commit. `jobs.yaml` is read from the checked-out workspace, never fetched from a broker, so a run is reproducible from the commit alone. Refs outside your allowlist are rejected outright; set `require_approval_otherwise = true` to park them for a local `latem approve` instead, and what that approves is one commit — if the branch moves before the run starts, it is parked again rather than built. **Parking is opt-in: `latem init` leaves the setting commented out, so the default is rejection.** |

Each has a test named after it. A pull request that weakens one is rejected
regardless of its other merits. The full statement is
[`docs/RUNNER_SPEC.md` §5](docs/RUNNER_SPEC.md).

---

## Install

```console
$ curl -fsSL https://raw.githubusercontent.com/Abyssinia-Labs/latem/main/install.sh | sh
```

Never as root. The script detects your OS and architecture, verifies the
download, installs to `~/.local/bin`, and runs `latem doctor`.

**v0.1.0 is released, and this repository is private**, so that line returns
404 today — for the script itself, for the release assets, and for anything
else anonymous. It is one line and it works; there is simply nobody it works
*for* until the repository is public. With a token, `gh release download v0.1.0`
gets the same archives, and their signature is checked the same way (below).

Otherwise, build from source with Go 1.25 or newer:

```console
$ git clone https://github.com/Abyssinia-Labs/latem && cd latem
$ make latem && ./bin/latem doctor
```

### What "verifies the download" means

Two checks, and they prove different things:

- The **sha256** against the release's checksums manifest. Not optional, and
  there is no flag to skip it. It proves the file arrived intact.
- The **minisign signature** over that manifest, when `minisign` is installed.
  This is the one that proves who produced it — the manifest comes from the
  same server as the binary, so a checksum match alone says nothing about who
  served either. When the tool is missing the script says so and what it did
  not check, rather than implying more than it did.

An agent with `[agent] auto_update = true` performs the same two checks for
itself, daily, against a key compiled into the binary, and replaces itself only
while no build is in flight (RUNNER_SPEC §20). A binary built from a tree with
no signing key in it refuses that setting at load, because an agent that
declined every release into a log nobody reads would be worse than one that
never offered to update.

The same privacy caveat applies: the release feed is `api.github.com`, the agent
sends no token, and a private repository answers 404. `latem doctor` says so
rather than leaving it to a daily line in a log.

Then, either way:

```console
$ latem init --repo myapp=git@github.com:me/myapp.git   # write a config.toml
$ latem doctor            # check the machine; every failure names its fix
$ latem service install   # run the agent at boot
$ latem run myapp test    # trigger a build
```

Two things that sequence assumes, neither of which it creates. Both are one-time
and both are quick, but a run fails without them.

### Give the machine a git credential

Latem mirrors your repository over the URL in `config.toml`, so **the machine
needs its own credential** — a fresh account has none, and `latem init`'s example
is an ssh URL. Without one, `doctor` fails the repo check and every run fails
`E_SOURCE` with `Permission denied (publickey)`.

```console
$ gh auth login --git-protocol ssh      # or: ssh-keygen -t ed25519, then add the
                                        # public half as a deploy key on the repo
$ latem doctor                          # the repo check should now be [ ok ]
```

A **deploy key in `~/.ssh` with an `ssh config` entry** is the form that works
from both an interactive shell and an agent started by launchd or systemd. A
forwarded agent works from neither, because the service does not inherit your
interactive `ssh-agent`. An https URL with a credential helper works too, as long
as the helper can answer without a terminal.

Verify with the same command the agent will run:

```console
$ git ls-remote git@github.com:me/myapp.git
```

### Give the repository a job

**`latem run myapp test` runs the job named `test` from `.latem/jobs.yaml` in
your repository**, read at the commit being built — not from this machine. If the
file is not there, the run fails immediately:

```console
latem: r_… failed (E_NO_REGISTRY): .latem/jobs.yaml is not present at this commit
```

Most repositories do not have one yet, so **write it, commit it, and push it**.
A working tree change is not enough: latem mirrors from the remote and checks out
the resolved commit (INV-5). The smallest file that runs is:

```yaml
version: 1

jobs:
  test:
    runs_on: [macos]        # must be a subset of a runner's labels
    steps:
      - name: test
        run: echo hello
```

`version`, `runs_on` and `steps` are the only required keys, and the job name has
to match the one you pass to `latem run`. On macOS `latem init` generates the
labels `["macos", "arm64", "xcode"]`, so `runs_on` must be a subset of those.
[Configuration](#configuration) has the full schema.

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

### Where secret values live

A job's `secrets: [NAME, …]` names what to resolve. The values are on the
machine, never in the repository, and are looked up in a fixed order — the first
that answers wins:

| | Source | How to set it |
|---|---|---|
| 1 | Environment | `LATEM_SECRET_<NAME>` in the agent's environment — the override, useful for `LATEM_SECRET_X=… latem run` to answer "is it the secret or is it the build?" |
| 2 | File | a `NAME=value` line in `~/.latem/secrets.env`, mode `0600` |
| 3 | Keychain | `security add-generic-password -s latem -a <NAME> -w …` |
| 4 | Command | a `[secrets.commands]` entry in `config.toml` that prints the value |

**`secrets.env` is line-based and cannot hold a multi-line value.** This matters
because the secret INV-4 is built around — `APP_STORE_CONNECT_KEY_P8`, a PEM
block — is multi-line by definition. A quoted block spanning lines is a parse
error, and the obvious workaround of collapsing it with `\n` escapes is worse
than an error: the escapes are **not** expanded, so the value resolves, `doctor`
reports it `[ ok ]`, and the build gets one line containing literal backslash-n.
That is not a usable key, and nothing says so until `fastlane` fails.

**Put multi-line secrets in the Keychain**, which holds them intact:

```console
$ security add-generic-password -s latem -a APP_STORE_CONNECT_KEY_P8 \
      -w "$(cat AuthKey_ABC123.p8)"
```

> **On v0.1.0 this silently produced a corrupt key.** `security` prints any
> value containing a newline as a hex string, and v0.1.0 passed that through
> unchanged, so a PEM resolved to `2d2d2d2d2d424547…` — and reported `[ ok ]`.
> Fixed after v0.1.0; on v0.1.0 use `[secrets.commands]` below instead.

**Or name a command**, which works equally well and is where a secret lives when
it comes from a password manager rather than this machine:

```toml
[secrets.commands]
APP_STORE_CONNECT_KEY_P8 = "cat /Users/admin/.config/latem/AuthKey_ABC123.p8"
MATCH_PASSWORD           = "op read op://Private/match/password"
```

The string is tokenised with shell *quoting* rules and no shell *semantics*
(INV-3): whitespace separates words, quotes group, and a backslash escapes one
character. A pipe, `$`, a backtick or a redirect is rejected rather than
interpreted — so put anything that needs a shell in a script and name the script.

Redaction does not depend on which source a value came from, and multi-line
values are redacted line by line (INV-4).

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

`require_approval_otherwise = true` is shown here because it is the setting most
worth knowing about, and **`latem init` writes it commented out**. Without it, a
ref outside `allowed_refs` is rejected rather than parked:

```console
latem: r_… rejected (E_INVALID_REF): ref "refs/heads/feature/x" is outside allowed_refs for "myapp" and approval is disabled
```

**To add a second repository, edit `config.toml` by hand** and add another
`[repos.<name>]` block. `latem init` refuses to run against an existing config,
and its `--force` flag *overwrites* the file rather than merging into it — it
would drop the repos already there. `latem doctor` validates what you added.

Every rule is validated at load and fatal there: a runner that starts on a
broken config does not fail at startup, it fails forty minutes into somebody's
build. Disk floors are per runner — 40 GB for docker, 60 for native, 200 for
tart — because a global 20 GB admits an iOS build the machine cannot finish.

Jobs live in `.latem/jobs.yaml` **in your own repository**, committed and
pushed — see [Give the repository a job](#give-the-repository-a-job) above.

**Caches are declared once at the top level, and jobs reference them by name.**
The declaration carries the name and the mount path; the job carries a list of
names and nothing else. Writing the name-and-path mapping inside a job is the
most common mistake, and it is rejected at parse time.

```yaml
version: 1

# Top-level: declare each cache once, with the path it mounts at.
caches:
  - name: pods
    path: /Users/admin/Library/Caches/CocoaPods
  - name: pnpm-store
    path: /Users/admin/.local/share/pnpm/store

jobs:
  ios-release:
    runs_on: [macos, xcode, ios]
    timeout: 90m
    caches: [pods, pnpm-store]      # names only — the paths are declared above
    secrets: [MATCH_PASSWORD, APP_STORE_CONNECT_KEY_P8]
    artifacts:
      - path: build/*.ipa
        retain: 30d
    steps:
      - name: install
        run: pnpm install --frozen-lockfile
      - name: build
        run: ci/ios/build.sh
```

`runs_on` is matched against a runner's `labels`. **`latem init` generates
`["macos", "arm64", "xcode"]` — not `ios`**, so this job would find no runner on
a freshly initialised machine until you add `ios` to the runner's `labels`, as
the `config.toml` above does. A job whose labels are not a subset of some
runner's never starts.

The schema is strict: an unknown key is an error rather than a warning, so a
typo fails the run that introduced it instead of silently doing something else.
**Required are `version: 1`, at least one job, and per job `runs_on` and
`steps`.** Everything else is optional — `timeout` defaults to 30m, the shell
defaults to `bash`.

`artifacts` is a list of globs relative to the workspace root, each with an
optional `retain`. Collected files land in `~/.latem/artifacts/<run-id>/`;
there is no subcommand to fetch them yet.

## Commands

```
latem init --repo <name>=<url>       Write a starting config.toml for this machine
latem agent                          Run the daemon (foreground; see `latem service install`)
latem run <repo> <job> [--ref R] [--input k=v] [--follow] [--wait]
latem list [--all] [--state S] [--repo R] [--limit N]
latem logs <run-id> [-f]
latem cancel <run-id> [--reason R]
latem approve <run-id>               Local only; pins that run's commit (INV-5)
latem register --url U --token T
latem doctor [--secrets] [--fix]
latem cache ls | prune | rm <name>
latem service install [--print] | uninstall | status
latem version
```

**`latem list` shows only runs that have not finished.** A run that has already
succeeded or failed needs `--all`, which is how you confirm a build worked:

```console
$ latem list --all
RUN ID                      STATE      REPO   JOB   REF              STARTED   DURATION
r_a0d1c0b8aae59b84287b3729  succeeded  myapp  test  refs/heads/main  just now  1s
```

`--limit` caps the number printed and defaults to 50.

### Run states

`--state S` takes one of these. Three of them are worth knowing about because
they are **not** `failed`, and a script that filters for `failed` alone misses
every one:

| State | Means |
|---|---|
| `queued` | admitted, waiting for a runner |
| `awaiting_approval` | outside `allowed_refs`; needs `latem approve` (INV-5) |
| `claimed` · `preparing` · `running` · `collecting` | in flight |
| `succeeded` | every step exited 0 |
| `failed` | a step exited non-zero |
| `timed_out` | exceeded the job or step budget — **not** `failed` |
| `cancelled` | `latem cancel` — **not** `failed` |
| `rejected` | refused at admission, e.g. `E_INVALID_REF` — **not** `failed` |
| `interrupted` | the agent stopped mid-run |

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

**`latem run` on its own exits `0` as soon as the run is accepted**, before it
has done anything — it prints a run id and returns. That `0` means "queued", not
"succeeded", and a run that fails a second later has already exited `0`. **Use
`--wait` or `--follow` in anything that checks the result**; only those make the
exit code describe the build:

```console
$ latem run myapp test --wait
latem: r_90782d84ba6689ec1739424c failed (E_NO_REGISTRY): …
$ echo $?
1
```

`latem logs` also exits `0` for a run that failed — it succeeded in printing the
logs. The state is in the output, not the status.

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
the agent and its local broker, the remote broker, and the release pipeline —
four cross-compiled targets, a signed checksums manifest, a smoke job that runs
the result on a real machine, and self-update that replaces the binary only
between builds. All five invariants have tests named after them and all five
are green.

Not built, and honestly named:

- **A public release.** v0.1.0 **is** tagged, and the signed archives are on it
  — the pipeline, the signing key (`D73938C642AD64D3`, which `latem version`
  reports), and the verification the installer and the agent both perform are
  all in place and exercised. What has not happened is making the repository
  public, so the installer above has nothing it can fetch anonymously and no
  stranger has yet installed this.
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
  §5's invariants and the forty-one amendments the implementation waves
  produced.
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
