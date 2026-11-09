# Why latem

What latem is for, what it does differently, and how far it has got. The
[README](../README.md) is the quickstart.

## Why this exists

You already own machines that can build your software. The laptop on your desk,
the box under it, the server you are paying for anyway. They are faster than
what you are renting by the minute, they are already configured the way your
project needs, and they are sitting idle while a queue somewhere else bills you
for a slower one.

So the compute is not the missing piece. The plumbing is.

**Hosted CI rents you a machine and then throws it away.** That single fact is
where the cost and most of the ritual come from. Caching exists as a save step,
a restore step, a key to get wrong and a ceiling to hit, because the machine
that warmed the cache will not be there next time. Anything that takes real
setup — a toolchain, a device, a signing identity, a licence — either gets
rebuilt every run or gets handed to somebody else's computer to keep.

**The self-hosted runners already run on hardware you own.** Buildkite,
Woodpecker, Drone, Forgejo Actions, Actions Runner Controller — that is the
right idea, and it is why latem does not try to replace them at that layer.
What they leave you is everything between "I have a machine" and "it builds my
software unattended":

- **The machine itself.** A label pointing at a box somebody set up by hand.
  Nothing in the tool knows whether the toolchain is the one the project needs,
  whether the credential works from a service rather than from your shell, or
  whether the thing will come back after a reboot — and none of those fail with
  an error that names itself.
- **The cache**, still modelled as though the machine were disposable, when
  yours is the same machine every time.
- **The threat model.** A box on your network, holding your credentials,
  running what a webhook asked it to run, usually gets a paragraph.

Those three are what the rest of this page is about.

**The hardest version of this is a Mac**, and latem is built to pass it: an iOS
release needs a real user session, an unlocked keychain, signing identities and
a simulator runtime that matches the SDK, and it is the job hosted CI charges
the most for. If a runner can do that unattended it can do the rest. That is why
iOS shows up throughout these docs as evidence — not because latem is an iOS
tool.

## Two ways to take it

**Beside what you already have.** Leave your pipeline where it is and move one
job — the long, expensive, awkward one — onto a machine you own. Nothing is
ripped out and nothing is migrated. A hook binds one repository and one ref
pattern to one job on one machine; the git host supplies the push, never the
work; your existing CI never learns about it.
[`app.latem.dev`](https://app.latem.dev) is the control plane that holds those
hooks, and it only ever sends names — §5's INV-2 below is what makes pointing a
machine that holds your credentials at somebody else's server a reasonable
thing to do.

**Instead of what you already have.** Or take all of it. `latem run` needs no
account, no hook and no network: the local broker is the default and the hosted
one is opt-in (`RUNNER_SPEC` G2). A machine that never registers is a supported
machine, not a trial.

It is the same binary either way, and going from the first to the second
installs nothing new.

---

Latem exists for the three things the other runners leave to you.

### 1. Your machine is the build machine

Two executors, behind one interface.

**Docker** runs a job in a container, on any host with a daemon. It is the
isolating one: a build reaches the workspace, the caches it declared, and
nothing else — including the daemon itself, so a step that runs `docker build`
or `wrangler containers build` has nothing to talk to and needs a native runner
on a Linux host. Its image is
[`latem-ubuntu`](../images/ubuntu/README.md), which stands in for GitHub's
`ubuntu-latest`.

**Native** runs a job on the host's own OS, as the agent's user. It needs
nothing installed and no disk, and it is how you use a machine that is already
set up — the Xcode you have, the keychain you have, the devices plugged into
it, the licence tied to it. A container cannot borrow any of that.

The question that decides between them is not how big your team is. It is
**whose code runs on this machine.** Your own repositories, native is the
direct answer. Anything a contributor outside the team can influence, or a
third-party action you did not write, wants the container — because native runs
steps as you, with everything your user can reach.
[`docs/RUNNER_SPEC.md`](RUNNER_SPEC.md) §13.0 is the long version, and
[`docs/deploy.md`](deploy.md) is the ordered runbook for bringing a machine
up from scratch.

Native runs on Linux and macOS today. **Windows is a goal and is not
implemented** — the executor is Unix-only by construction, and a Windows host
needs its own answer for process groups, shells and the between-step channel
(`RUNNER_SPEC` A47). A Windows machine can still run Docker jobs. Tart, which
would give a macOS build an ephemeral VM, is specified in §13.2 and backlogged;
nothing depends on it.

A job picks a runner by label, not by executor, so moving a machine between the
two is a config edit and touches no repository. The step contract is identical
across both — each step is a new process, the filesystem persists, `$LATEM_ENV`
carries variables between them — so a job that works on one works on the other.

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


**A workflow's `uses: actions/cache` becomes one of these**, with no edit to the
workflow, and latem performs `actions/checkout`, `upload-artifact` and
`download-artifact` itself. [Steps latem performs](jobs.md#steps-latem-performs)
says how each behaves.

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
[`docs/RUNNER_SPEC.md` §5](RUNNER_SPEC.md).

## Status

Every command in the [README](../README.md#commands) is wired and does what it
says. The [latest release](https://github.com/Abyssinia-Labs/latem/releases/latest)
is below 1.0 because the surface is still moving, not because the parts
described here are sketches.

The claim this project exists to make has been tested rather than argued: on
2026-08-11 the agent, started by launchd rather than by a shell, took
`ios-release` from a bare clone to a signed build in TestFlight, with `codesign`
reaching the login keychain from a launchd-spawned process. INV-4 was then
checked against that run and not against a fixture — neither signing password,
nor the issuer id, nor any interior line of the App Store Connect PEM appears
in the run log, in any other log, or in the sqlite store. The same sequence has
since been run on a second machine that had never seen the software
([`docs/deploy.md`](deploy.md)), which found three defects the first could
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

- **A 1.0.** The surface described here is stable enough to build on and not
  yet frozen: a flag may be renamed, and a config key may grow a better
  spelling, between now and a 1.0. Releases are signed and self-update refuses
  anything the key in your binary does not vouch for, so upgrading is not the
  risk; a rename you have to notice is.
- **The Tart executor**, deferred to v2 by amendment A31. Native is the default
  because the first deployment builds repositories its operator controls; Tart
  is the isolation upgrade for when that stops being true, and `RUNNER_SPEC`
  §13.0 is the criterion rather than a preference.
- **A reboot with nobody logged in.** [`docs/deploy.md`](deploy.md) §1–§8
  have been followed end to end on two machines; §9 is the one section still
  written from the spec rather than from a terminal, because a LaunchAgent
  needs a user session and auto-login and FileVault decide whether one exists.
- **The far end of the remote broker.** The control plane lives in its own
  repository now, so nothing here exercises a real server. What pins the wire
  is [`docs/PROTOCOL.md`](PROTOCOL.md) and `test/conformance`, which
  replays recorded frames in both directions against committed fixtures.

Where to read further, in the order a new operator needs them:

- [`docs/RUNNER_SPEC.md`](RUNNER_SPEC.md) — the specification, including
  §5's invariants and the amendments the implementation produced.
  The amendments are the interesting part: each one names the defect that
  forced it.
- [`docs/deploy.md`](deploy.md) — the ordered runbook for bringing up a
  Mac runner, written from two machines rather than from the spec.
- [`docs/ios.md`](ios.md), [`docs/android.md`](android.md) — the
  mobile pipelines, including the failure table `doctor` turns into a
  self-service fix.
