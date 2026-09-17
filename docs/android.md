# Android on a self-hosted runner

> **Status: partly verified, and the split matters.** On **2026-08-11** a signed
> `.aab` from `examples/android-native/` reached **Play internal testing**
> (21:07:34), so §1, §2 and the upload path have been run end to end and their
> failure rows in §5 are real error text —
> [`manual-baseline.md`](./manual-baseline.md) §7.3 has the session.
>
> **That run was Gradle on macOS, by hand, not the container this document
> describes.** Everything specific to the Linux docker runner — the container
> image, `/dev/kvm`, emulator tests — remains written from
> [`RUNNER_SPEC.md`](./RUNNER_SPEC.md) §18.2 and Google's documentation, and has
> never been executed. Correct it from a real run rather than from here.
>
> Two things in §2 are worth reading before you trust any other guide: Play
> Console's API-access flow has been restructured and most instructions for it
> are stale, and a service account key may be blocked by an organization policy
> that has nothing to do with Play.

Android is the easier platform, and it is worth being clear about why: it runs
in a **container** on the Linux runner (`runs_on: [linux, docker]`), so there is
no keychain, no user session, no auto-login, no FileVault, and no LaunchAgent.
Everything that makes [`ios.md`](./ios.md) long is absent here.

What Android has instead is one credential you can permanently lose, and one
capability the hosted services mostly do not offer.

---

## Table of contents

- [1. The keystore](#1-the-keystore)
- [2. The Play service account](#2-the-play-service-account)
- [3. `/dev/kvm` and emulator tests](#3-devkvm-and-emulator-tests)
- [4. The Gradle cache](#4-the-gradle-cache)
- [5. Failure table](#5-failure-table)

---

## 1. The keystore

**Losing the release keystore means you can never ship an update to that app
again, under that package name.** Not "it is inconvenient" — Play identifies an
app by its signing certificate, and there is no recovery path that does not
involve publishing a new app and asking every user to reinstall it.

This is the single most consequential artefact in the Android pipeline, and it
is a 2 KB file that is easy to leave on one laptop.

```bash
keytool -genkey -v -keystore release.jks -alias upload \
  -keyalg RSA -keysize 2048 -validity 10000
```

- [ ] Backed up somewhere that survives the machine, the laptop, and the person.
- [ ] The password is in a password manager, not in anyone's shell history.
- [ ] It is **not** in the repo. Check `.gitignore` before the first commit, not
      after.

> **Play App Signing** changes this calculus and is worth enabling. Google holds
> the app signing key; you hold an *upload* key, and an upload key can be reset
> by support if you lose it. It converts an unrecoverable loss into a support
> ticket. If you have not enabled it, the keystore above is genuinely
> irreplaceable.

### Getting it to the runner

The keystore is binary, and the secrets chain (§15) carries strings, so it is
base64 in transit:

```bash
base64 -i release.jks | tr -d '\n' > keystore.b64
```

Store as `ANDROID_KEYSTORE_B64` via §15 — the macOS Keychain or a
mode-`0600` `~/.latem/secrets.env`. `ci/android/build.sh` decodes it into a
mode-`0700` temp directory and removes it on every exit path.

Required by the job, per §18.2:

| Secret | What it is |
|---|---|
| `ANDROID_KEYSTORE_B64` | base64 of the `.jks` |
| `ANDROID_KEYSTORE_PASSWORD` | keystore password |
| `ANDROID_KEY_ALIAS` | key alias inside the keystore |
| `ANDROID_KEY_PASSWORD` | password for that alias |
| `PLAY_SERVICE_ACCOUNT_JSON` | Play Developer API service account key |

Verify the decode before spending 25 minutes on a build that cannot be signed —
a base64 copy-paste that lost a trailing newline is the common case:

```bash
keytool -list -keystore release.jks -storepass "$PW" -alias upload
```

---

## 2. The Play service account

Two-sided, and the second side is the one everyone forgets.

**Google Cloud side.** Create a service account, create a JSON key, download it.
This part gives you a file and a feeling of completion.

**Play console side.** Users and permissions → invite the service account's
email address → grant **Release manager** (or at minimum "Manage testing track
releases") for the app. Without this, the key authenticates fine and every API
call returns a permission error, which reads like a broken key and is not one.

```bash
bundle exec fastlane run validate_play_store_json_key \
  json_key:~/keys/play-service-account.json
```

Four things that will otherwise cost you a scheduling surprise, two of them
contradicting advice you will find repeated everywhere:

- ~~**The first upload of an app must be manual.**~~ **Not observed.** The
  widely-repeated rule is that the Play API refuses a package it has never seen.
  On 2026-08-11 the *first* `.aab` for a Draft app uploaded through the API with
  no manual bundle first. Plan for the manual upload anyway — it costs nothing
  if it turns out to be unnecessary — but do not treat a successful first API
  upload as evidence that something is misconfigured.
- `supply` pushes to a **track**. It does not promote to production, and the job
  in `examples/android-native/.latem/jobs.yaml` deliberately offers only
  `internal`/`alpha`/`beta` as enum values. A production rollout is a human
  decision made in the console, not a CI input.
- **A service account key may be refused by policy, not by Play.**
  `An Organization Policy that blocks service accounts key creation has been
  enforced` comes from Google Cloud's `iam.disableServiceAccountKeyCreation`,
  which is enforced by default on new organizations. Override it per project,
  create the key, then re-enable it. Nothing about the message mentions Play,
  and it is the first wall a new org hits.
- **Play Console's API-access flow has been restructured, so most instructions
  for it are stale** — including, for a while, this document's. There is no "API
  access" page and `/developers/<id>/api-access` redirects to the app list. The
  flow verified on **2026-08-11**: create any Cloud project, enable the Google
  Play Android Developer API, create a service account, download a JSON key,
  then **Play Console → Users and permissions → Invite new users** with that
  service account's address. Anything here reproducing a vendor's web UI needs
  the date it was checked beside it.

The JSON key is multi-line. Same warning as the iOS `.p8`: §16 registers every
individual line of a multi-line secret with the redactor, because registering
only the whole value redacts nothing when the value never appears on one line.

---

## 3. `/dev/kvm` and emulator tests

An Android emulator without KVM falls back to software emulation and is roughly
an order of magnitude slower — slow enough that a test suite which takes four
minutes locally will exceed its step timeout in CI, and the failure looks like a
flaky test rather than a missing device node.

§13.1 requires the Docker executor to pass `--device /dev/kvm` for emulator
jobs, and §19 requires `latem doctor` to confirm the device exists and is
readable by the daemon's user — at startup, not mid-build.

```bash
ls -l /dev/kvm
# crw-rw---- 1 root kvm 10, 232 … /dev/kvm

# readable by this user?
[ -r /dev/kvm ] && [ -w /dev/kvm ] && echo ok

# if not:
sudo usermod -aG kvm "$USER"    # then log out and back in
```

Inside a container:

```bash
docker run --rm --device /dev/kvm my-android-image \
  sh -c '[ -r /dev/kvm ] && echo kvm ok'
```

Absent `/dev/kvm` usually means: not a bare-metal Linux host (a VM without
nested virtualisation), virtualisation disabled in the BIOS, or the user is not
in the `kvm` group.

> **This is a real differentiator and §18.2 says to call it out.** Most hosted
> CI does not give you nested virtualisation, so Android emulator tests there
> are either unavailable or run in software emulation. A self-hosted box with
> `/dev/kvm` runs them at native speed. It is one of the few places where owning
> the hardware is not merely cheaper but *capable of something the hosted
> option is not*.

**macOS note:** there is no `/dev/kvm`. Emulator jobs belong on the Linux
runner. The Mac mini's container runner is arm64 Linux under Docker Desktop and
does not have it either.

---

## 4. The Gradle cache

The largest single win on this platform (§18.2), and unambiguously so — unlike
iOS DerivedData, there is no VirtioFS question here because the Linux runner
mounts a Docker volume, not a network filesystem.

Caches are **mounted, not archived** (§14). No save step, no restore step, no
tarball, no upload, no size ceiling in the job. The declaration in
`.latem/jobs.yaml` is the whole of it:

```yaml
caches:
  - name: gradle
    path: /home/runner/.gradle
```

Point Gradle at the mount explicitly. Its default is `$HOME/.gradle`, and if
`$HOME` in the image is not what the cache path says, the cache is mounted and
silently unused — a slow build with no error:

```bash
export GRADLE_USER_HOME=/home/runner/.gradle
```

This depends on §13.1's base image contract: uid 1000 named `runner`, with a
writable `$HOME` that **exists**. Passing `--user` for a uid with no
`/etc/passwd` entry leaves `$HOME` unset, and the resulting Gradle, npm, and
CocoaPods failures look like tooling bugs. `latem doctor` checks this by running
the image once.

**Concurrency.** Two runs writing one `~/.gradle` corrupt each other, and the
resulting failures are intermittent and confusing. §14.3 requires per-cache
`flock` when a runner's `concurrency > 1`, held from `preparing` to
`collecting`, with a 10-minute acquisition timeout that fails `E_CACHE` rather
than hanging. Cache names embed the runner, so runners never share.

**The Android SDK is baked into the image, not cached** (§18.2). It is large,
it changes rarely, and a cache that is only ever read is a base-image layer
wearing a costume.

---

## 5. Failure table

Rows marked **(observed)** carry real error text from the 2026-08-11 session;
[`manual-baseline.md`](./manual-baseline.md) §7.5 has the verbatim output and
what fixed each one. The rest are still *(inferred)* — replace them with
observed symptoms as they occur.

| Symptom | Cause | Fix |
|---|---|---|
| `supply` fails: package not found | Play has never seen this package | Upload one `.aab` manually in the console first (§2) |
| API auth succeeds, every call is `403` | Service account not granted access **in the Play console** | §2, Play console side |
| Play rejects the `.aab`: wrong certificate fingerprint | Signed with a debug key, or the wrong keystore | `jarsigner -verify -verbose:summary app.aab` before uploading |
| `keytool` fails on the decoded keystore | base64 lost a newline, or the wrong variable was set | Re-encode with `base64 -i … \| tr -d '\n'` |
| Version code already exists | Play version codes must strictly increase | Derive from `git rev-list --count HEAD`, passed forward via `$LATEM_ENV` |
| Emulator tests time out | No `/dev/kvm`; software emulation | §3 |
| Gradle re-downloads everything every run | `GRADLE_USER_HOME` is not the mounted cache path | §4 |
| Gradle daemon OOM-killed | Container memory limit below the daemon's heap | Raise `memory` in `[runner.docker]`, or set `org.gradle.jvmargs` |
| Intermittent, irreproducible Gradle corruption | Two runs sharing one cache without a lock | §14.3; check the flock is actually being taken |
| Build works locally, fails in the container | `$HOME` unset or unwritable for uid 1000 | §13.1 base image contract |
| **(observed)** `The operation couldn't be completed. Unable to locate a Java Runtime.` from `keytool` or `java` | `/usr/bin/java` and `/usr/bin/keytool` are Apple stubs. A keg-only openjdk is installed but invisible — so `java -version` succeeding proves nothing about what Gradle will use | Pin `JAVA_HOME` explicitly; `examples/android-native/ci/lib/env.sh` finds it |
| **(observed)** Gradle selects JDK 26 as the daemon JVM and AGP rejects it | `brew install gradle` pulls the *current* openjdk as a dependency, so installing Gradle can break an Android build that worked before | Pin `JAVA_HOME` to 17 in the job's environment, not in the machine's |
| **(observed)** `The 'org.jetbrains.kotlin.android' plugin is no longer required for Kotlin support since AGP 9.0.` | AGP 9 has Kotlin built in; applying both is a hard error | Remove the Kotlin plugin |
| **(observed)** `SDK location not found. Define a valid SDK location with an ANDROID_HOME environment variable or by setting the sdk.dir path in ... local.properties` | `ANDROID_HOME` unset. The suggested fix is the wrong one — `local.properties` is per-machine and must never be committed, so following it makes the runner *less* reproducible | Set `ANDROID_HOME` in the job's environment |
| **(observed)** `An Organization Policy that blocks service accounts key creation has been enforced` | Google Cloud's `iam.disableServiceAccountKeyCreation`, on by default for new orgs. Nothing to do with Play | §2 |
| **(observed)** `ERROR: bundle carries no signature block` on a correctly signed `.aab`, **intermittently** | The assertion, not the bundle: `unzip -l \| grep -q` under `set -o pipefail`. grep exits at the first match, unzip takes SIGPIPE, and the pipeline reports failure on input that matched. **Measured at 17 false negatives in 200 runs** | `jarsigner -verify` with its output captured to a variable; never a producer piped into `grep -q` (A37) |

---

## See also

- [`manual-baseline.md`](./manual-baseline.md) — the M0 runbook
- [`ios.md`](./ios.md) — the harder platform
- [`testing.md`](./testing.md) — the manual matrix
- `examples/expo-app/ci/android/build.sh` — the (unverified) script
