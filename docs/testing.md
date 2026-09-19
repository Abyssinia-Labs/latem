# Manual test matrix

> **Status: mostly unverified, and this document is unusual in that it will
> stay that way.** These are the tests that *cannot* be automated, so a full
> pass will always be the most recent operator's word rather than a green CI
> job. Two cases have now been run for real — **M1.1** on 2026-08-11 (the agent
> under launchd took `ios-release` to TestFlight) and its redaction check
> against that run's log — and they are in the [results log](#results-log).
> Everything under **M2** needs Tart, which is not installed. Everything under
> **M4** is still drafted, and **M4.3/M4.4 are the gap that matters**: no
> machine has yet built after a reboot with nobody logged in
> ([`deploy.md`](./deploy.md) §9).
>
> The procedures are drafted from [`RUNNER_SPEC.md`](./RUNNER_SPEC.md) §24 and
> the sections they exercise. Correct them as you run them, and record results
> in the tables.

§24 splits testing four ways: unit, integration, invariant, and this. The first
three live in Go and run on every commit. This one exists because four things
about the system are only true on real hardware, in ways no CI job can assert:

| Why it cannot be automated | Cases |
|---|---|
| Needs credentials that must not live in a test | iOS end-to-end with real signing |
| Needs a hypervisor and a 140 GB image | Tart VM lifecycle |
| Needs physical hardware | GPU passthrough |
| Needs the machine to actually stop and start | Reboot recovery |

**When to run this matrix:** before any release, after a macOS or Xcode upgrade,
after any change to the executors or to `examples/*/ci/**`, and after
touching the LaunchAgent or the install script. It is not a per-commit gate. It
is the gate on the things that break silently between commits.

Each case below is: **preconditions → steps → expected → what a failure means.**
"Expected" is what should happen; where it is a guess, it says so.

---

## Table of contents

- [M1. iOS end to end with real signing](#m1-ios-end-to-end-with-real-signing)
- [M2. Tart VM lifecycle](#m2-tart-vm-lifecycle)
- [M3. GPU passthrough](#m3-gpu-passthrough)
- [M4. Reboot recovery](#m4-reboot-recovery)
- [Results log](#results-log)

---

## M1. iOS end to end with real signing

The one §24 names first, and the one that cannot be faked: a self-signed
certificate and a fake upload endpoint would exercise the plumbing and prove
nothing about the part that actually breaks.

**Preconditions:** credentials provisioned;
Tart base image built; the `ios` runner configured; `latem doctor` clean.

### M1.1 — Happy path

1. `latem run myapp ios-release --input channel=internal --follow`
2. Watch the run through `queued → preparing → running → collecting → succeeded`.

**Expected:** a signed `.ipa` in `~/.latem/artifacts/<run-id>/`, a build visible
in TestFlight, and — the part worth checking deliberately — **no secret value
anywhere in `~/.latem/logs/<run-id>.log`**.

```bash
grep -c 'BEGIN PRIVATE KEY' ~/.latem/logs/<run-id>.log     # want 0
grep -c "$(printf '%s' "$MATCH_PASSWORD")" ~/.latem/logs/<run-id>.log   # want 0
grep -c '\*\*\*' ~/.latem/logs/<run-id>.log                # want > 0
```

The third check matters as much as the first two: zero redaction markers means
the redactor is not running, not that there was nothing to redact.

**Failure means:** if the log contains key material, INV-4 is broken in
production even if `TestINV4_SecretsNeverLeave` passes — which is exactly the
draft-01 failure §16 describes, where a whole-value-only redactor let every
base64 line of a PEM through. Stop and fix before anything else.

### M1.2 — Multi-line secret in the log (deliberate leak attempt)

1. Temporarily add a step to a scratch job: `echo "$APP_STORE_CONNECT_KEY_P8"`.
2. Run it. Read the log.

**Expected:** every line redacted to `***<hash>`. Not just the
`-----BEGIN-----` delimiter — **every base64 body line**.

**Failure means:** the redactor is registering only whole values. §16's rules 2
and 3 are missing. This is the single highest-severity finding in this document.

*(Remove the scratch job afterwards. Obviously.)*

### M1.3 — Step timeout mid-build

1. Set `steps[].timeout: 1m` on the build step. Run.

**Expected:** `E_STEP_TIMEOUT`, the step's **process group** killed (no orphaned
`xcodebuild` — check `ps aux | grep xcodebuild`), `collecting` still runs, and
partial logs are retained. A hung step's partial logs are the diagnostic.

### M1.4 — Cancel mid-build

1. Start a run. At ~5 minutes, `latem cancel <run-id>`.

**Expected:** terminates within the grace window, still collects artifacts, and
the VM is deleted. `tart list` shows no `latem-run-*` left behind.

### M1.5 — Concurrency group

1. Start two `ios-release` runs within a few seconds of each other.

**Expected:** the second **queues**, it does not fail. `concurrency_group: ios`
serialises across jobs, not merely across runners (§9). They must not run
concurrently: parallel Xcode builds contend on DerivedData and the keychain and
produce failures that look like code bugs.

---

## M2. Tart VM lifecycle

`Close` must be idempotent and must leave nothing behind — §13.2 requires
`tart delete` to run even when `tart stop` fails. A leaked VM grows toward its
image's provisioned 140 GB, which §14.4 names as the fastest way to fill a Mac
mini.

### M2.1 — Clean lifecycle

1. Run any macOS job. Watch `tart list` throughout.

**Expected:** `latem-run-<id>` appears at `preparing`, gets an IP within
`boot_timeout` (3m default; real boot is 30–60s per §13.2), disappears at
`collecting`. `tart list` is empty afterwards.

### M2.2 — Forced kill of the VM

Named explicitly in §24.

1. Start a run. Mid-build: `tart stop <vm> --timeout 0`, or kill the `tart run`
   process outright.

**Expected:** the run fails with a sensible error rather than hanging until the
job timeout; the VM is deleted anyway; the agent stays healthy and accepts the
next run.

**Failure means:** `Close` is not idempotent, or delete is conditional on stop
succeeding. Both leak 90 GB per occurrence.

### M2.3 — Forced kill of the agent

1. Start a run. `kill -9` the agent.
2. Restart it.

**Expected:** startup reconciliation deletes the orphaned VM and marks the run
failed. Nothing orphaned after restart — the same guarantee §24's integration
suite asserts for Docker containers, here for VMs.

### M2.4 — Two VMs, and the licence ceiling

1. Set a tart runner's `concurrency = 2`. Run two jobs.
2. Then try `concurrency = 3`.

**Expected:** two run concurrently. Three is **rejected by the config loader at
load**, not discovered at runtime — Apple's licence permits two guest VMs per
host (§13.2), and §8 enforces it in the parser.

### M2.5 — Disk floor

1. Fill the disk until free space is below `min_free_gb` (150 for tart).

**Expected:** new runs refused with `E_DISK` **before** starting, not failed
halfway through a 70-minute build.

### M2.6 — Cache mounts

1. Run a job with caches. Inspect `~/.latem/caches/<runner>/<cache>/` on the
   host.

**Expected:** populated after the run, and reused (visibly faster) on the
second. If `derived_data = "vm"`, DerivedData is **not** in this directory —
that is the measurement's decision taking effect (§13.2).

---

## M3. GPU passthrough

**Preconditions:** the Linux box with an NVIDIA card, NVIDIA Container Toolkit
installed, `[runner.docker] gpu = true`.

### M3.1 — Preflight detects a working GPU

1. `latem doctor`

**Expected:** runs `nvidia-smi` in a throwaway container and reports success.
§13.1 requires this to fail the runner **at startup, not mid-build**.

### M3.2 — Preflight detects a broken GPU

1. Break it deliberately: uninstall the toolkit, or point the runtime at a
   missing binary.

**Expected:** the runner is marked unavailable and stops accepting runs. Runs
already queued for it do not fail with a confusing CUDA error 20 minutes in.
Recovery is automatic when the check passes again (§20).

### M3.3 — A GPU job sees the GPU

1. Run a job with `runs_on: [linux, gpu]` executing `nvidia-smi`.

**Expected:** all GPUs visible.

### M3.4 — `/dev/kvm` for emulator jobs

Related capability, same class of check (§18.2, [`android.md`](./android.md) §3).

1. Configure an emulator job. `latem doctor`.

**Expected:** confirms `/dev/kvm` exists and is readable by the daemon's user.
Then run the job and confirm the emulator uses hardware acceleration — a
software-emulated boot is roughly an order of magnitude slower and looks like a
flaky test rather than a missing device.

---

## M4. Reboot recovery

§24 says "both platforms", and the two platforms fail in genuinely different
ways: Linux is a systemd question, macOS is a *did the machine even finish
booting* question.

### M4.1 — Linux, clean reboot

**Preconditions:** systemd user unit installed, `loginctl enable-linger $USER`.

1. `sudo reboot`. Do not log in.

**Expected:** the agent is running and the machine is online within a minute or
two. Lingering is what makes this work without a login; without it the unit
waits for a session that an unattended reboot never creates.

### M4.2 — Linux, reboot mid-run

1. Start a long run. `sudo reboot` mid-build.

**Expected:** after boot, startup reconciliation finds the orphaned container,
marks the run failed with an accurate reason, and removes it. No orphaned
`latem.managed=true` containers.

### M4.3 — macOS, clean reboot with FileVault OFF

1. Confirm `fdesetup status` says Off and auto-login is enabled.
2. `sudo reboot`. Do not touch the machine.

**Expected:** auto-login → session → LaunchAgent → agent online, unattended.
Then confirm the *session* is right, not merely that the process is up:

```bash
launchctl managername       # from the agent's context: Aqua
```

An agent that is running but cannot sign is worse than one that is down, because
monitoring says it is healthy.

### M4.4 — macOS, clean reboot with FileVault ON

**This is the case §23 says draft 01 missed, and it is the reason this row
exists.**

1. Confirm `fdesetup status` says On.
2. `sudo reboot`. Do not touch the machine. Wait 10 minutes.

**Expected — and this is a *pass*, not a bug:** the machine does **not** come
back. It is sitting at the pre-boot unlock screen. No login, no session, no
LaunchAgent, no agent, and **no log line explaining any of it**, because the
agent never started and it is the agent that writes the logs.

**What you are verifying** is that the operator understands this, not that the
software handles it — it cannot be handled. Confirm:

- [ ] The FileVault decision in [`ios.md`](./ios.md) §4 has been **made** and
      recorded, with a name attached.
- [ ] `latem doctor` reports FileVault status and names both options without
      picking one.
- [ ] If Option B was chosen, on-call knows that `sudo reboot` is the command
      that takes the runner offline for a day, and that
      `sudo fdesetup authrestart` is the one to use instead.

### M4.5 — macOS, `fdesetup authrestart`

1. With FileVault on: `sudo fdesetup authrestart`.

**Expected:** the machine reboots, unlocks for **exactly this one boot**, and
comes all the way up to a session and a running agent, unattended. Then confirm
it re-armed — a second plain `sudo reboot` should park at pre-boot again.

### M4.6 — macOS, reboot mid-run

1. Start an iOS run. Reboot mid-build.

**Expected:** after recovery, the orphaned `latem-run-*` VM is deleted and the
run is marked failed. Check `tart list` and `df -h` — this is where 90 GB goes
missing.

---

## Results log

Fill in per execution of the matrix. A row with no date has not been run.

| Case | Date | Result | Notes |
|---|---|---|---|
| M1.1 iOS happy path | 2026-08-11 | **pass** | Agent started by launchd (`managername` = `Aqua`), `ios-release` → TestFlight build 1786501043, `codesign` reaching the login keychain from a launchd-spawned process. |
| M1.2 multi-line secret redaction | 2026-08-11 | **pass, by absence** | Checked against that real run rather than by a deliberate leak step: neither signing password, nor the issuer id, nor any of three interior lines of the App Store Connect PEM appears in the 148-line run log, in any other log, or in the sqlite store. The deliberate-leak form is automated in `test/invariants` against a PEM fixture; this row is the version with a real credential |
| M1.3 step timeout | | | |
| M1.4 cancel mid-build | | | |
| M1.5 concurrency group | | | |
| M2.1 clean VM lifecycle | | | |
| M2.2 forced VM kill | | | |
| M2.3 forced agent kill | | | |
| M2.4 two VMs / licence ceiling | | | |
| M2.5 disk floor | | | |
| M2.6 cache mounts | | | |
| M3.1 GPU preflight OK | | | |
| M3.2 GPU preflight broken | | | |
| M3.3 GPU visible in job | | | |
| M3.4 `/dev/kvm` | | | |
| M4.1 Linux clean reboot | | | |
| M4.2 Linux reboot mid-run | | | |
| M4.3 macOS reboot, FileVault off | | | |
| M4.4 macOS reboot, FileVault on | | | |
| M4.5 `fdesetup authrestart` | | | |
| M4.6 macOS reboot mid-run | | | |

**Environment for this execution:**

| | |
|---|---|
| Date | |
| Agent version | |
| macOS / Xcode | |
| Tart version / base image | |
| Run by | |

---

## See also

- [`ios.md`](./ios.md) — failure table, FileVault, LaunchAgent
- [`android.md`](./android.md) — keystore, Play, `/dev/kvm`
