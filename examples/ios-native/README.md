# `ios-native` — the Xcode example that builds

A plain SwiftUI iOS app with an Xcode project, built by `xcodebuild` and shipped
by `fastlane`. No Expo, no Node, no CocoaPods.

**Bring a new Mac runner up on this one.** It isolates the question "is this
machine configured correctly?" from every other variable, and it needs no Apple
Developer account, no certificate, and no provisioning profile to answer it.

```bash
./ci/ios/build-simulator.sh
```

Verified 2026-08-11 on Xcode 26.0.1 / macOS 26.5.2 / M4 Max: exit 0, **20.2 s**
cold, produces `build/dd/Build/Products/Release-iphonesimulator/LatemExample.app`
and zips it to `build/app-sim.zip`. Also passes under
`env -i HOME=$HOME PATH=/usr/bin:/bin:/usr/sbin:/sbin`, which is the assertion
that matters — it does not depend on an interactive shell, and a runner step
does not have one.

## Why this exists

`examples/expo-app/` is the reference the spec was written around, and on this
hardware it does not build. Two independent reasons, both recorded as amendment
A32 in `../../docs/RUNNER_SPEC.md`:

- `eas build --local` requires an **Expo account**. The build is local; the
  authorisation is not. That is a hosted dependency in the critical path of
  every iOS build, on a runner whose first claim is that it needs no account.
- Expo SDK 57.0.12 does not compile against Swift 6.2 (Xcode 26.0.1) — 31 errors
  inside `expo-modules-jsi`, across three distinct classes of Swift 6
  strictness.

Neither is a defect in `latem`, and neither makes Expo a wrong choice for a repo
that already uses it. Both mean the mobile story should not rest on Expo alone.

## What is verified, and what is not

| Step | State |
|---|---|
| `xcodebuild build` for the simulator | **verified** — 20.2 s, exit 0 |
| `xcodebuild archive` for a device | **verified** — arm64, `LC_BUILD_VERSION` platform 2, `CFBundleSupportedPlatforms [iPhoneOS]` |
| `xcodebuild -exportArchive` → `.ipa` | **blocked** — `error: exportArchive No Team Found in Archive` (exit 70). Needs a distribution certificate and a team |
| `fastlane match` | **never run** — needs a private certs repo that does not exist yet |
| `fastlane pilot upload` → TestFlight | **never run** — needs an App Store Connect API key |

Scripts say which they are, at the top, in the same words.

## Layout

```
LatemExample.xcodeproj/
  project.pbxproj                     objectVersion 77, synchronized file groups
  xcshareddata/xcschemes/*.xcscheme   COMMITTED — see below
LatemExample/                         sources; the sync group picks them up
ci/
  install.sh                          bundle install; there is nothing else to install
  ios/build-simulator.sh              VERIFIED. No credentials.
  ios/build.sh                        archive → export → upload. Verified to `archive`.
  ios/unlock-keychain.sh              unverified
  ios/match.sh                        unverified
fastlane/{Appfile,Matchfile}          account detail read from the environment
.latem/jobs.yaml                      ios-smoke (runs) and ios-release (blocked)
```

**The shared scheme is committed on purpose.** Xcode auto-creates schemes into
`xcuserdata/`, which nobody commits, so `xcodebuild -scheme` works on every
developer's machine and fails on every runner — and §12 hands each run a fresh
clone, which makes that failure certain rather than likely. Amendment A34.

## Before this can reach TestFlight

Membership is **paid and confirmed**, and `DEVELOPMENT_TEAM = C8M8H6M43X` is set
in the project and confirmed by Xcode's own behaviour — it accepts the team and
now fails only on the missing profile. So the list below is what remains, and
none of it can be done from here: every step needs an Apple login, and steps 3
and 4 produce a private key and a password that should never pass through a
terminal transcript.

**1 — Register the bundle id.** developer.apple.com/account → Identifiers → `+`.

```
com.abyssinialabs.latemexample
```

Change it if you would rather own a different one. It appears in exactly three
places: `PRODUCT_BUNDLE_IDENTIFIER` (twice, Debug and Release) in
`LatemExample.xcodeproj/project.pbxproj`, `fastlane/Appfile`, and
`fastlane/Matchfile`.

**2 — Create the app record.** appstoreconnect.apple.com → Apps → `+` → New App,
pointing at that bundle id. TestFlight needs the record to exist before an
upload will be accepted, and the error if it does not is unhelpful.

**3 — Create an App Store Connect API key.** App Store Connect → Users and
Access → Integrations → App Store Connect API → `+`. Give it the **App Manager**
role — Developer is not enough to upload builds.

You get a `.p8` file that downloads exactly once. Three values go into the
runner's secret store, never into this repo:

| Secret | Where it comes from |
|---|---|
| `APP_STORE_CONNECT_KEY_ID` | the key's 10-character id, shown in the table |
| `APP_STORE_CONNECT_ISSUER_ID` | the UUID at the top of the same page |
| `APP_STORE_CONNECT_KEY_P8` | the **contents** of the `.p8`, multi-line PEM and all |

`APP_STORE_CONNECT_KEY_P8` being multi-line is the reason §16's redactor
registers each line of a multi-line secret separately. Load it with
`latem secrets set` or your chosen §15 backend; do not paste it into a shell.

**4 — Create the `match` certificates repo.** An empty **private** git repo —
its whole contents will be encrypted certificates. Then, once, from a machine
signed in to your Apple account:

```bash
export MATCH_GIT_URL=git@github.com:you/certificates.git
bundle exec fastlane match appstore          # NOT --readonly, this first time only
```

That creates the distribution certificate and the App Store provisioning
profile, encrypts them, and pushes them. It prompts for a passphrase — that
becomes `MATCH_PASSWORD`. Every later run, including every CI run, uses
`--readonly`, which `ci/ios/match.sh` already does: without it `match` will
create *new* certificates, and an account has a hard cap on how many can exist.
A runner that makes one per build exhausts the cap and then breaks Xcode for
every developer on the team.

**5 — Then the full job runs.**

```bash
./ci/ios/build.sh
```

`../../docs/manual-baseline.md` §2 has the lead times for anything above that
involves waiting on Apple.
