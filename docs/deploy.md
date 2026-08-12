# Bringing up a Mac runner

> **Status.** Every command here was run on a MacBook Pro (M4 Max, macOS 26.5.2,
> Xcode 26.0.1) on 2026-08-11, most of them more than once and several of them
> wrong the first time. What has **not** been done is [§9](#9-surviving-a-reboot):
> no machine has yet built after a reboot with nobody logged in. That step is
> marked, and it is the only one written from the spec rather than from a
> terminal.
>
> The failures behind each instruction are in
> [`manual-baseline.md`](./manual-baseline.md) §7.5. If something here goes
> wrong, that table is where the verbatim error text lives.

The order is not arbitrary. Each section can be verified before the next one
starts, and the expensive dependencies — Apple, Google, other humans — are
pulled as early as they can be. **The single most useful property of this
sequence is that the machine is proved before any credential exists**, so when
a signing job later fails you already know it is not the machine.

---

## Table of contents

1. [Before you touch the machine](#1-before-you-touch-the-machine)
2. [The toolchain](#2-the-toolchain)
3. [The binary](#3-the-binary)
4. [The job repository](#4-the-job-repository)
5. [Configure the agent](#5-configure-the-agent)
6. [Checkpoint: doctor](#6-checkpoint-doctor)
7. [Checkpoint: a build with no credentials](#7-checkpoint-a-build-with-no-credentials)
8. [Credentials](#8-credentials)
9. [Surviving a reboot](#9-surviving-a-reboot)
10. [When something breaks](#10-when-something-breaks)

---

## 1. Before you touch the machine

Two of these have lead times measured in days and neither is under your
control, so file them first and do §2 while you wait.

| What | Who you wait on | Realistic |
|---|---|---|
| Apple Developer Program | Apple; plus finance for an org account | 1–7 days |
| Google Play Developer account | Google | 1–3 days |
| App Store Connect API key | whoever holds Account Holder | hours |
| A private git repo for `match` certificates | whoever can create one | hours |

**Decide now whose code this machine will build.** It is the only architectural
question in this document. If the answer is "repositories my team controls",
the native executor is right and the rest of this applies as written. If it
includes a fork, a pull request, or any ref a contributor can move, read
[`RUNNER_SPEC.md`](./RUNNER_SPEC.md) §13.0 first: the native executor runs steps
as the logged-in user, with that user's keychain and signing certificates, and
nothing here changes that.

---

## 2. The toolchain

Everything in this section is checked by `latem doctor` later, so you do not
have to get it perfect now. What you cannot skip is the *full* Xcode.

```bash
xcode-select -p          # want /Applications/Xcode.app/Contents/Developer
sudo xcodebuild -license accept
sudo xcodebuild -runFirstLaunch
```

The Command Line Tools have no iOS SDK. Selecting them produces
`xcrun: error: SDK "iphoneos" cannot be located` much later, from inside a
build, where it reads as a project problem.

**Homebrew Ruby, for fastlane.** macOS ships 2.6 and modern fastlane will not
install against it:

```bash
brew install ruby
```

Do **not** assume that put it on `PATH`. Homebrew's ruby is keg-only, so
`which ruby` still answers `/usr/bin/ruby`. `doctor` reports this precisely
— *"PATH gives ruby 2.6.10 …, but ruby 3.4.6 … is installed"* — and the ci/
scripts in both examples find it themselves.

**For Android only:**

```bash
brew install openjdk@17
```

17 specifically. A newer JDK is rejected by AGP 9, not tolerated, and
`brew install gradle` pulls the current one in as a dependency — so installing
Gradle can break an Android build that worked the day before. Gradle itself is
not needed: `./gradlew` bootstraps the version the repo pins.

The Android SDK comes from Android Studio or
`brew install --cask android-commandlinetools`. Set `ANDROID_HOME`; do not
follow Gradle's advice to write `local.properties`, which is per-machine and
cannot be committed.

---

## 3. The binary

Go is needed only to build it. Building on the mini is simplest:

```bash
brew install go
git clone <your latem remote> ~/src/latem
cd ~/src/latem && go build -o ~/bin/latem ./cmd/latem
sudo mv ~/bin/latem /usr/local/bin/latem
```

`/usr/local/bin` is root-owned, hence the `sudo mv` rather than building
straight into it as root.

**Put it in its final location before §9.** `latem service install` records the
resolved path in the LaunchAgent, so a binary that later moves leaves a service
pointing at nothing.

---

## 4. The job repository

This is the step most likely to be misunderstood. **`latem` is what you build;
your app repo is what it builds from.** They are different repositories.

The agent clones your app repo and reads `.latem/jobs.yaml` **from its root**,
at the resolved commit. That file is never fetched from a broker (INV-5), so
what runs is decided entirely by code you can read at that SHA.

Copy a starting point from `examples/ios-native/` or
`examples/android-native/` — both have run end to end — and commit
`.latem/` and `ci/` into your own repository. Both examples also ship a
credential-free smoke job, which §7 depends on.

**The mini must be able to clone it.** It has none of your credentials. A read
-only deploy key is the right answer:

```bash
ssh-keygen -t ed25519 -f ~/.ssh/latem-runner -C "latem runner"
cat ~/.ssh/latem-runner.pub    # add as a deploy key on the repo
```

```
# ~/.ssh/config
Host github.com
  IdentityFile ~/.ssh/latem-runner
  IdentitiesOnly yes
```

A deploy key is scoped to one repository and revocable on its own; a personal
token carries every repo you can see.

---

## 5. Configure the agent

```bash
latem init --repo myapp=git@github.com:me/myapp.git
```

Writes `~/.latem/config.toml`, picks the executor from the platform, sets
labels that match the reference jobs, creates the directory `0700` and the file
`0600`, and parses back what it wrote before reporting success.

Add `--allowed-refs main,release/*` if `main` alone is too narrow. Everything
else — approval policy, a control plane, secrets provider — is commented in the
file it writes, and the full schema is §8.

You do not need `latem register`. That is for a control plane; the local broker
works without one, and `latem run` on this machine needs nothing else.

---

## 6. Checkpoint: doctor

```bash
latem doctor
```

Read every line. It names the fix for each failure, and the checks exist
because each one has cost somebody a morning.

At this point expect these to be unresolved, and that is correct:

- **auto-login** and **FileVault** — §9 decides those
- **agent service** not installed — §9
- **repositories** — passes only once §4's deploy key works, which is the point
  of checking it here rather than discovering it inside a build

Anything else red, fix before continuing. `doctor` exits non-zero when a check
fails, so it is usable in a script.

---

## 7. Checkpoint: a build with no credentials

**Do not skip this.** It is the cheapest thing in the document and it partitions
every later failure into "the machine" or "the credentials".

Start the agent in a terminal:

```bash
latem agent
```

and in another:

```bash
latem run myapp ios-smoke --follow
```

`ios-smoke` builds for the simulator, which needs no Apple account, no
certificate, no provisioning profile, and no API key. `android-smoke` is the
same for a debug APK. Both were verified taking 20–30 s.

If this fails, the problem is this machine — Xcode, Ruby, the SDK, the clone.
If it passes and §8 later fails, the problem is a credential. That distinction
is worth more than it sounds.

---

## 8. Credentials

Now, and not before.

**`match` certificates.** Once, from a machine that can sign in to Apple — this
prompts for your Apple ID and 2FA, so it is not something a runner does:

```bash
export MATCH_GIT_URL=git@github.com:you/certificates.git
bundle exec fastlane match appstore          # NOT --readonly, the first time only
```

It asks you to invent a passphrase. **Write it down.** Every later run,
including every CI run, uses `--readonly`, which the example's `ci/ios/match.sh`
already does — without it `match` creates new certificates, and an account has
a hard cap.

> A trap worth knowing: `match` caches that passphrase in the login keychain as
> an *internet* password named `match_login.keychain`. A machine that has run
> `match` interactively then works whether or not `MATCH_PASSWORD` is correct —
> so a wrong value is invisible until you deploy to a machine that has no cache.
> This mini is that machine.

**Secrets.** `~/.latem/secrets.env`, mode `0600`, `KEY=value` per line. For iOS
there is a generator, because the multi-line PEM has to become one
double-quoted value with `\n` escapes and hand-writing that is easy to get
subtly wrong:

```bash
./ci/ios/write-secrets-env.sh
```

It prompts for the two passwords, reads the `.p8` from disk, and derives the key
id from its filename — Apple names every download `AuthKey_<KEY_ID>.p8`, and
typing that id separately is how a JWT ends up claiming one key while being
signed by another.

Jobs declare secrets by **name** and never by value (INV-2), and every resolved
value is registered with the log redactor before the first step runs (INV-4 —
verified against a real signing run: nothing reached the logs or the store).

**Verify the App Store Connect key before building anything:**

```bash
bundle exec fastlane run validate_play_store_json_key json_key:<file>   # Android
./ci/ios/check-credentials.sh                                           # iOS
```

The iOS one isolates which of the three values Apple rejected, in about a
second, against roughly two minutes per guess through a full build.

Then the real thing:

```bash
latem run myapp ios-release --follow
```

---

## 9. Surviving a reboot

**This section is unverified.** Everything above ran from an interactive shell,
which has a live user session and an unlocked keychain. What follows is what
§13.3 and §23 require, and it is what a Mac mini in a cupboard actually needs.

```bash
latem service install --print    # read it first
latem service install
```

`--print` renders the LaunchAgent without installing it. Read it: that flag is
what caught a plist pointing its log destination at a directory that did not
exist, which launchd reports by failing to start and writing the reason to the
file it could not open.

It must be a **LaunchAgent**, never a LaunchDaemon. `codesign` needs a real user
session with keychain access, and a daemon context has none; the failure is
`User interaction is not allowed`, which names neither.

Then the two settings that decide whether the machine comes back on its own:

- **Auto-login** must be enabled, or no session exists after a reboot and the
  LaunchAgent never loads.
- **FileVault** defeats auto-login: an encrypted disk stops at a password prompt
  before login. Either turn it off, or keep it and reboot only with
  `sudo fdesetup authrestart`, which unlocks the disk for exactly one boot.

Both are security settings on a machine that holds your signing certificates.
`doctor` reports them and deliberately does not change them — that decision is
yours, and [`ios.md`](./ios.md) §4 lays out the trade honestly.

Verify by actually rebooting and running a job without logging in. Until you
have, treat this section as a plan.

---

## 10. When something breaks

Triage in this order. It is cheapest-first, and each step rules out everything
above it:

1. `latem doctor` — thirty-odd checks, each naming its fix
2. `latem run myapp ios-smoke` — machine, or credentials?
3. `./ci/ios/check-credentials.sh` — which credential, specifically
4. `latem logs <run-id>` — the run's own output
5. `~/.latem/logs/agent.err.log` — the agent's, when a run never started

[`ios.md`](./ios.md) §1 is a failure table keyed by verbatim symptom; rows
marked **VERIFIED** were observed rather than reasoned. `manual-baseline.md`
§7.5 has the full list of what went wrong the first time and what fixed it.

**One class of failure deserves naming here**, because it wastes the most time:
a tool that is installed, on `PATH`, and the wrong one. `/usr/bin/ruby` is 2.6
while Homebrew's 3.4 sits keg-only. `/usr/bin/java` and `/usr/bin/keytool` are
stubs that print *"Unable to locate a Java Runtime"* on a machine with a perfect
JDK. `bundle` may belong to a different Ruby than the one that will run. In each
case the naive check passes and the build fails, and `doctor`'s Mobile toolchain
section exists to tell them apart.
