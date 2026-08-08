# Android on a self-hosted runner

> **Status: UNVERIFIED.** Nothing here has been executed. It is written from
> [`RUNNER_SPEC.md`](./RUNNER_SPEC.md) §18.2 and from Google's documented
> behaviour, as scaffolding for the M0 manual baseline. Correct it from
> [`manual-baseline.md`](./manual-baseline.md) §7 once real builds have run.

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

Two rules that will otherwise cost you a scheduling surprise:

- **The first upload of an app must be manual.** The Play API refuses a package
  it has never seen. Upload one `.aab` by hand in the console, then `supply`
  works from then on. Plan for this rather than discovering it as a failure.
- `supply` pushes to a **track**. It does not promote to production, and the job
  in `examples/expo-app/.latem/jobs.yaml` deliberately offers only
  `internal`/`alpha`/`beta` as enum values. A production rollout is a human
  decision made in the console, not a CI input.

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

Everything here is *(inferred)* — no Android build has been run. Replace rows
with observed symptoms as they occur.

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

---

## See also

- [`manual-baseline.md`](./manual-baseline.md) — the M0 runbook
- [`ios.md`](./ios.md) — the harder platform
- [`testing.md`](./testing.md) — the manual matrix
- `examples/expo-app/ci/android/build.sh` — the (unverified) script
