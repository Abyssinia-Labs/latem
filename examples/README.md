# Examples

Two complete projects and the job registries that build them. Both have run end
to end on a real machine and produced a real artefact — an app in TestFlight and
a signed bundle in Play internal testing — rather than parsing and being called
a reference.

| | What it is | What it produces |
|---|---|---|
| [`ios-native/`](ios-native/) | A plain Xcode application. No Expo, no hosted account, no framework in the critical path | `ios-smoke` builds a simulator `.app`; `ios-release` signs an `.ipa` and uploads it to TestFlight |
| [`android-native/`](android-native/) | A plain Gradle application | `android-smoke` builds a debug APK; `android-release` signs an `.aab` for Play |
| [`tart/`](tart/) | A script that builds a Tart base image | A VM image for the Tart executor, which is deferred to v2 |

## How to read one

Start with `.latem/jobs.yaml` — it is the whole pipeline, and it is the file you
would copy. Then follow the `run:` lines into `ci/`.

That split is not a stylistic preference. A step's `run` is literal and nothing
is interpolated into it, and each step is a new process, so anything
conditional, looping, or multi-command lives in a script the step calls. The
useful side effect is that every one of those scripts runs on a laptop, which is
where you want to debug a build.

The two registries share a shape worth stealing:

- **A smoke job that needs no credentials at all.** It builds for a simulator or
  a debug target and proves the machine and the toolchain before any signing
  identity is involved. It is the job to run first on a new runner.
- **A release job that declares its secrets by name.** The values live on the
  machine, resolved through the chain in [`docs/secrets.md`](../docs/secrets.md), and never appear in the
  registry or in a log.
- **Caches declared once at the top level** and referenced by name — CocoaPods
  and SPM for iOS, Gradle for Android — because the cost of a cold mobile build
  is mostly download.

## Adapting one

Change `runs_on` to labels your runner actually has (`latem init` writes them
into `config.toml`, and `latem doctor` lists them), point the `ci/` scripts at
your project, and adjust the timeouts — the ones here are measured on a
particular machine and are not a law.

[Writing a pipeline](../docs/jobs.md#writing-a-pipeline) in `docs/jobs.md` is
the shorter, non-mobile version of the same thing.
