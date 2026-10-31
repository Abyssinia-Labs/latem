# iOS on a self-hosted Mac

> **Status: PARTLY VERIFIED, 2026-08-11.** A full pipeline — archive, sign,
> upload, TestFlight — ran on a MacBook Pro M4 Max with Xcode 26.0.1, first by
> hand and then under a `latem` agent. Rows marked **VERIFIED** carry the
> literal error text observed; rows marked *(inferred)* are still reasoned from
> [`RUNNER_SPEC.md`](./RUNNER_SPEC.md) §18.1 and from Apple's and fastlane's
> documented behaviour, and are the ones most likely to be wrong.
>
> **§2 and §3 are now verified; §4 is not, and it is the one that matters.**
> The build did not only run from an interactive shell: launchd started the
> agent (`launchctl managername` = `Aqua`, parent PID 1) and `codesign` reached
> the login keychain from that context, which is §13.3's central claim tested
> rather than argued. What remains unproven is narrower and harder — **a reboot
> with nobody logged in.** A LaunchAgent needs a user session, so auto-login
> and FileVault (§4) decide whether the machine comes back on its own, and no
> machine has yet been rebooted to find out. `doctor` reports both as failing,
> correctly. [`deploy.md`](./deploy.md) §9 is the procedure, and it is the only
> section of that runbook still written from the spec.
>
> Correct these rows as real failures happen — a symptom string you can paste
> into a search engine is the whole value of this page.

Three things make iOS CI on your own hardware different from every other
platform, and all three are about **the machine being a real logged-in Mac**
rather than a container:

1. `codesign` needs a keychain, and a keychain needs a user session.
2. A user session needs auto-login, and auto-login is defeated by FileVault.
3. Signing material is a credential you can permanently lose, so the runner is
   only ever allowed to consume it.

Everything below follows from those.

---

## Table of contents

- [1. Failure table](#1-failure-table)
- [2. LaunchAgent vs LaunchDaemon](#2-launchagent-vs-launchdaemon)
- [3. Keychain unlocking](#3-keychain-unlocking)
- [4. FileVault — an operator decision](#4-filevault--an-operator-decision)
- [5. Signing material](#5-signing-material)
- [6. Xcode selection and upgrades](#6-xcode-selection-and-upgrades)
- [7. Triage order](#7-triage-order)

---

## 1. Failure table

### 1.1 From RUNNER_SPEC §18.1, verbatim

| Symptom | Cause |
|---|---|
| `User interaction is not allowed` | Keychain locked, or running under a LaunchDaemon rather than a LaunchAgent |
| `No signing certificate "iOS Distribution" found` | `match` not run, or wrong `MATCH_GIT_URL` |
| Build hangs at the end | `--non-interactive` missing; EAS is waiting on a prompt |
| `xcrun: error: SDK "iphoneos" cannot be located` | Xcode CLT selected instead of full Xcode; `xcode-select -s` |

### 1.2 Expanded — session, keychain, and signing

| Symptom | Cause | Fix |
|---|---|---|
| `User interaction is not allowed` | Keychain locked, **or** a LaunchDaemon context with no user session | `security unlock-keychain`; check `launchctl managername` returns `Aqua`. §2, §3 |
| `errSecInternalComponent` from codesign *(inferred)* | Same root cause wearing a different hat — the keychain is unreachable rather than locked | As above. Do not chase the error code; it is not diagnostic |
| `No signing certificate "iOS Distribution" found` | `match` not run, or wrong `MATCH_GIT_URL` | `bundle exec fastlane match appstore --readonly`, then `security find-identity -v -p codesigning` |
| `match` succeeds but no identity appears *(inferred)* | Keychain was locked when `match` imported, so the import silently no-op'd | Unlock **before** `match`. The step order in `jobs.yaml` is deliberate |
| Signing works interactively, fails under the agent *(inferred)* | The agent is a LaunchDaemon, or under a different user than the one holding the certificates | §2 |
| A codesign dialog is waiting on the console *(inferred)* | The keychain ACL does not trust `codesign` non-interactively | `security set-key-partition-list -S apple-tool:,apple: -s -k "$PW" login.keychain-db` after import |
| `Authority=Apple Development` in the built `.ipa` | A development certificate was used for a distribution build | Wrong `match` type; use `match appstore` and check the EAS profile |

### 1.3 Expanded — EAS and the build itself

> Rows marked **VERIFIED** were reproduced by hand on 2026-08-11 (Xcode 26.0.1,
> macOS 26.5.2, M4 Max) and the symptom column is the literal error text. Rows
> marked *(inferred)* are still read off the spec and have not been seen.

| Symptom | Cause | Fix |
|---|---|---|
| **VERIFIED** `EAS project not configured. Must configure EAS project by running 'eas init' before this command can be run in non-interactive mode.` | `eas build --local` requires an **Expo account**. The build is local; the authorisation is not | None that keeps "no account required". Build with `xcodebuild` directly — `examples/ios-native/`. See §18.3 / amendment A32 |
| **VERIFIED** `'weak' must be a mutable variable, because it may change at runtime` — 31 of them, all in `expo-modules-jsi` | Expo SDK 57.0.12 does not compile against Swift 6.2 (Xcode 26.0.1) | `weak let` → `weak var` via a committed `pnpm patch` clears all 31, then two further classes of Swift 6 strictness appear. Not fully solved; use `examples/ios-native/` or pin an older Xcode |
| **VERIFIED** `Unicode Normalization not appropriate for ASCII-8BIT (Encoding::CompatibilityError)` from `pod install` | `LANG` is unset. A step is a new process with only the environment the agent gives it (§9); CocoaPods aborts on a non-UTF-8 locale. The error names Unicode, not the locale, so it reads as a corrupt-file problem | Nothing, on a runner: amendment A33 is implemented and the native executor sets `LANG=en_US.UTF-8` in every step's environment (§9, §13.3). `latem doctor` reports the locale the step's own login shell resolves to, which is what catches an `export LC_ALL=C` in a profile overriding it. The `export` in `ci/lib/env.sh` remains only for running these scripts by hand |
| *(inferred)* `The build number must be higher than the previously uploaded build` from `pilot upload` | The app already carries a larger build number — usually a time-based one from before the agent supplied `LATEM_BUILD_NUMBER`. Amendment A36 makes the number the run's own counter, which is traceable and *smaller* than a timestamp | Bump the marketing version, or set the build number in the job's `steps[].env` for that app until its version changes. `latem list`'s `#` column is the same number, so a build in TestFlight can be traced back to the run |
| **VERIFIED** `xcodebuild: error: The project named "X" does not contain a scheme named "X"` on the runner, works locally | The scheme lives in `xcuserdata/` and was never committed. §12 gives every run a fresh clone, so the runner never has it | Commit the scheme under `<project>.xcodeproj/xcshareddata/xcschemes/`. Amendment A34; `latem doctor` checks it per repo, reading the bare mirror, and skips a repo it has not fetched yet |
| **VERIFIED** `Invalid password passed via 'MATCH_PASSWORD'` — while `match` with the variable *unset* succeeds | `MATCH_PASSWORD` outranks match's keychain cache (`fetch_password!` reads ENV first). A machine that has run match interactively has the passphrase cached as an **internet** password `match_login.keychain`, masking a wrong variable | Get the real passphrase — Keychain Access → login → `match_login.keychain` → Show password — or re-key with `fastlane match change_password`. A LaunchAgent-started runner has no cache, so the variable must be right |
| **VERIFIED** `error: exportArchive No Team Found in Archive` (exit 70) | Archive built unsigned, or `DEVELOPMENT_TEAM` empty | Set `DEVELOPMENT_TEAM` on the `archive` invocation, not only on the export |
| Archive uploads, then App Store Connect rejects it hours later | The archive contains a **simulator** binary. `xcodebuild` exits 0 either way | `otool -l …/LatemExample \| grep -A2 LC_BUILD_VERSION` — platform 2 is a device, 7 is the simulator. Assert it in the build script |
| Build hangs at the end, no output | `--non-interactive` missing; EAS is waiting on a prompt nobody can see | Add it. On a headless runner a prompt is an invisible hang until the step timeout |
| Hangs even **with** `--non-interactive` *(inferred)* | EAS is trying to manage credentials itself and reaching for an Expo login | Configure the project so credentials are local; do not let EAS fetch its own — it races `match` |
| `xcrun: error: SDK "iphoneos" cannot be located` | Xcode CLT selected instead of full Xcode | `sudo xcode-select -s /Applications/Xcode.app/Contents/Developer` |
| Xcode licence prompt in the log *(inferred)* | Licence not accepted after an Xcode upgrade | `sudo xcodebuild -license accept && sudo xcodebuild -runFirstLaunch`. Bake into the Tart image |
| `pod install` fails on native extensions *(inferred)* | System Ruby 2.6 | Homebrew Ruby, first on `PATH` |
| Build much slower in the VM than on the host *(inferred, and the point of the measurement)* | DerivedData over VirtioFS | This is the §13.2 question. Measure it; set `derived_data = "vm"` if the share loses |
| Disk full mid-build | Base image 140 GB provisioned, DerivedData tens of GB more | `min_free_gb = 200` for tart, 60 for the default native runner. Check for leaked `latem-run-*` VMs |
| Two iOS builds fail intermittently and differently *(inferred)* | Parallel Xcode builds contending on DerivedData and the keychain | `concurrency = 1` and `concurrency_group: ios` |

### 1.4 Expanded — upload

| Symptom | Cause | Fix |
|---|---|---|
| `pilot` rejects the build: app not found *(inferred)* | No app record in App Store Connect for that bundle id | Create it. `pilot` uploads; it does not create apps |
| Upload succeeds, build never appears | Apple-side processing, 5–30 min | Wait. Not a failure |
| Build rejected for a missing export-compliance answer *(inferred)* | `ITSAppUsesNonExemptEncryption` unset in `Info.plist` | Set it, or answer once in the console |
| API key auth fails *(inferred)* | Key id / issuer id mismatch, or a mangled `.p8` | The `.p8` is multi-line; a `KEY=value` file will mangle it. Use the Keychain (§15) |

### 1.5 Expanded — machine lifecycle

| Symptom | Cause | Fix |
|---|---|---|
| **Machine offline after a reboot; nothing in the logs** | FileVault stopped the boot at the pre-boot unlock screen. No login → no session → no LaunchAgent → the agent never started, so it never logged anything | §4. This is the failure with no error message |
| Agent runs but every signing job fails | LaunchDaemon instead of LaunchAgent | §2 |
| Agent does not start after reboot, FileVault is off | Auto-login disabled | System Settings → Users & Groups → Automatic login |
| Machine came back but builds fail until someone logs in *(inferred)* | Auto-login raced the LaunchAgent, or the session is not `Aqua` | Check `launchctl managername` from the agent's own context |

---

## 2. LaunchAgent vs LaunchDaemon

**The agent must run as a LaunchAgent, under an auto-login user.** §13.3 calls
this the single most common install failure, and `latem doctor` is required to
detect it (§19).

| | LaunchAgent | LaunchDaemon |
|---|---|---|
| Location | `~/Library/LaunchAgents/` | `/Library/LaunchDaemons/` |
| Runs as | the logged-in user | root or a system user |
| Starts | at **login** | at **boot**, before any login |
| Keychain | the user's login keychain | none reachable |
| `codesign` | works | fails |

A daemon looks more correct — it starts at boot, it does not need anyone logged
in, it is how you would run any other server. That instinct is exactly the trap.
`codesign` reaches into the login keychain, and a daemon has no user session and
therefore no login keychain to reach into. The failure is
`User interaction is not allowed`, which sounds like something is trying to show
a dialog. Nothing is. The message means "this operation needs a session and
there is not one", and it sends people looking for a phantom prompt.

Install (§23):

```xml
<!-- ~/Library/LaunchAgents/com.latem.agent.plist -->
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN"
  "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key>              <string>com.latem.agent</string>
  <key>ProgramArguments</key>   <array>
    <string>/Users/YOURUSER/.local/bin/latem</string>
    <string>agent</string>
  </array>
  <key>RunAtLoad</key>          <true/>
  <key>KeepAlive</key>          <true/>
  <key>StandardOutPath</key>    <string>/Users/YOURUSER/.latem/agent.log</string>
  <key>StandardErrorPath</key>  <string>/Users/YOURUSER/.latem/agent.err</string>
</dict>
</plist>
```

```bash
launchctl bootstrap "gui/$(id -u)" ~/Library/LaunchAgents/com.latem.agent.plist
launchctl print "gui/$(id -u)/com.latem.agent" | head -20
```

Verify the context from inside the agent's own environment, not from your
terminal — your terminal is always `Aqua` and proves nothing:

```bash
launchctl managername      # Aqua = GUI session; Background/System = wrong
```

**Auto-login must be on**, or "starts at login" never happens on an unattended
machine: System Settings → Users & Groups → Automatic login. Which leads
directly to the next section.

---

## 3. Keychain unlocking

A login keychain unlocks when the user logs in. Auto-login does log in, so the
keychain is usually unlocked already — but it re-locks on sleep and on
inactivity timeouts, and a runner that only works until the machine idles is
worse than one that never works, because the failure is intermittent.

```bash
security unlock-keychain -p "$KEYCHAIN_PASSWORD" \
  "$HOME/Library/Keychains/login.keychain-db"
security set-keychain-settings -t 3600 -u \
  "$HOME/Library/Keychains/login.keychain-db"
security list-keychains -d user -s "$HOME/Library/Keychains/login.keychain-db"
```

- `-t 3600` — re-lock after an hour idle. Deliberately not "never": a build is
  well under an hour, and a permanently unlocked keychain on a machine holding
  distribution certificates is a standing risk for no gain.
- `-u` — lock on sleep.
- `list-keychains` — put the login keychain first in the search list. `match`
  imports into it, and a search list that omits it yields "no signing
  certificate found" while the certificate is demonstrably installed. That
  failure reads as a fastlane bug and is not one.

`-p` puts the password in `argv`, briefly visible in `ps`. The alternative is a
prompt, and a prompt on an unattended machine is a hang. The redactor covers
logs, not the process table; a shared machine is out of scope for this design.

**A dedicated CI keychain** is a reasonable hardening step — create one, import
only the signing material, leave the human's login keychain alone. It is more
moving parts, so it is not the default here, but if the Mac is also somebody's
desk machine, do it.

---

## 4. FileVault — an operator decision

This is the one §23 calls out as quietly falsifying "survives reboots", and it
is presented here as a decision rather than a recommendation because the
trade-off is genuinely the operator's.

### What happens

With FileVault on, the disk is encrypted at rest and the key is released by a
password typed at the **pre-boot** screen. That screen is before macOS starts.
So an unattended reboot — a kernel panic, a power cut, a `softwareupdate`
restart — stops there and waits.

No login. No user session. No LaunchAgent. No agent. **No log line explaining
any of it**, because the agent never started, and it is the agent that writes
the logs. The machine simply goes offline and stays offline until somebody
walks over and types a password.

A headless Mac in a cupboard is precisely the deployment this breaks, and
the symptom is *"my machine went offline overnight and nothing in the logs
explains it."* Nothing explains it because nothing ran.

```bash
fdesetup status
# FileVault is Off.
# FileVault is On.
```

### Option A — disable FileVault

```bash
sudo fdesetup disable      # decryption takes a while; the machine stays usable
```

- **Gain:** reboots are unattended and the machine always comes back. Auto-login
  works as advertised. Nothing else to think about.
- **Cost:** the disk is readable by anyone with physical access. That disk holds
  the login keychain with your **distribution certificates and private keys**,
  `~/.latem/secrets.env`, the Android keystore, and the machine token. A stolen
  machine yields your ability to sign releases under your own identity.
- **Reasonable when:** the machine is in a locked room or rack you control, and
  uptime matters more than physical-theft resistance.

### Option B — keep FileVault, reboot only via `authrestart`

```bash
sudo fdesetup authrestart
```

This unlocks the volume for **exactly the next boot** and then re-arms. The
machine reboots and comes all the way up to a session without anyone at the
console.

- **Gain:** the disk stays encrypted at rest. A stolen machine yields nothing.
- **Cost:** it only helps for reboots *you* initiate. An unexpected reboot — a
  panic, a power cut, an unattended OS update — still parks at pre-boot and the
  machine is offline until a human arrives. You have moved the problem from
  "every reboot" to "every reboot you did not plan", which is better but not
  solved.
- **Reasonable when:** the machine is somewhere less controlled, someone can
  reach it within a working day, and losing signing certificates to a theft is
  the worse outcome.

### There is no option C

Not honestly. FileVault can be unlocked at boot by a network-fetched key or a
TPM-like mechanism on some platforms; macOS does not offer this in a form that
survives the threat model, because any key material stored on the machine to
unlock the machine defeats the encryption. Anything promising otherwise is
storing the password somewhere and not telling you.

### Decide, and write it down

`latem doctor` reports FileVault status and names both options; it does not
pick one. Record the decision somewhere durable, including **who
made it**, so the next person to find the machine offline at 9am knows whether
that is a bug or the accepted cost.

> If you choose Option B, add the reboot procedure to whatever runbook your
> on-call uses. `sudo reboot` on a FileVault machine is the command that takes
> the runner offline for a day, and it is muscle memory for everyone.

---

## 5. Signing material

**The runner consumes signing material. It never creates it.**

`bundle exec fastlane match appstore --readonly` — the `--readonly` is the most
important flag in `ci/ios/`. Without it, `match` creates a new distribution
certificate when it cannot find one, which consumes one of the account's limited
slots and **revokes the certificate every other machine and developer is
using**. A revoked certificate is not a build failure; it is an outage for the
whole team, triggered by a runner that was trying to be helpful.

Populate the certs repo once, by hand, from a developer laptop, without
`--readonly`. Give the runner read-only access.

What crosses which boundary (INV-4):

| | Reaches the agent | Reaches a broker / control plane |
|---|---|---|
| `MATCH_PASSWORD` | yes, by name from §15 | **never** |
| Certificates, private keys, profiles | yes, into the keychain | **never** |
| App Store Connect `.p8` | yes | **never** |
| `.ipa` contents | yes | **never** — only `{name, size, sha256}` (§17) |

The `.p8` needs its own care. It is a multi-line PEM, and §16 registers **every
individual line** of a multi-line secret with the redactor for exactly this
reason: registering only the whole value redacts nothing, because the whole
value never appears on a single line. `echo "$APP_STORE_CONNECT_KEY_P8"` under
a whole-value-only redactor prints a usable key into the log file and ships it
onward with the INV-4 test still passing. Do not `echo` it. Do not add `set -x`
to `ci/ios/build.sh`.

---

## 6. Xcode selection and upgrades

```bash
xcode-select -p        # /Applications/Xcode.app/Contents/Developer
xcodebuild -version
```

An Xcode upgrade is an **operator task with a blast radius**, not a background
update:

- `xcode-select` can silently revert to the CLT.
- The licence must be re-accepted, or every build stops on a prompt that is
  invisible in a headless VM.
- The Tart base image is pinned to a major version by name (`latem-xcode-26`).
  Build `latem-xcode-27` alongside it, repoint `base_image`, and delete the old
  image only after a real build passes on the new one.
- **Automatic Xcode updates are survivable (A64).** The App Store replaces
  Xcode in place. Within half a minute the native runner reports itself
  unavailable: while Xcode is half-installed, then until a new licence is
  accepted or its first launch has run, naming the command that clears it.
  Jobs wait for it or go to another machine, and it takes work again once
  Xcode is whole. A job running when the update landed says so in its log.
  latem cannot run `sudo xcodebuild -license accept` or
  `sudo xcodebuild -runFirstLaunch` for you, nor fetch a simulator runtime a
  new Xcode wants (`xcodebuild -downloadPlatform iOS`).
- **Automatic macOS updates restart the machine.** Whether it comes back by
  itself is §4.

---

## 7. Triage order

When an iOS run fails and you do not yet know why, check in this order. It is
sorted by how often each is the answer, not by how interesting it is.

1. `fdesetup status` — is the machine even up because it rebooted? (§4)
2. `launchctl managername` from the agent's context — `Aqua`? (§2)
3. `security find-identity -v -p codesigning` — is there an
   `Apple Distribution` identity? (§3, §5)
4. `xcode-select -p` — full Xcode, not CLT? (§6)
5. `df -h /` — above `min_free_gb`? Any leaked `latem-run-*` VMs? (§13.2)
6. `tart list` — leaked clones diverging toward 140 GB each?
7. Only then read the build log.

`latem doctor` is meant to collapse steps 1–6 into one command (§19). Until it
exists, this is the list.

For a failed upload rather than a failed build, ask the store first:

```console
$ latem store check ~/src/myapp
```

It reads the app's bundle ID from `app.json` and asks Apple, read-only, for the
app record, the App Store profile and its certificate with their expiry, and the
last upload, after checking the secrets by name, the signing store and the key
(A35). It is safe beside a running build, and it creates nothing: the fixes it
names for signing are for a person at a terminal, not for a runner (§5).

---

## See also

- [`testing.md`](./testing.md) — the manual matrix
- [`android.md`](./android.md) — the other platform
- `examples/expo-app/ci/ios/` — the (unverified) scripts
