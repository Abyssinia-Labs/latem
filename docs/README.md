# Documentation

Sorted by who it is for, because this directory mixes three audiences and a
visitor should not have to guess which one a file is addressing.

## If you are running latem on a machine

| | |
|---|---|
| [`install.md`](install.md) | Installing, `latem setup` step by step, checking a release by hand, updates, and a machine's git credential |
| [`jobs.md`](jobs.md) | GitHub workflows (`workflows check`, `latem try`, the steps latem performs itself) and `.latem/jobs.yaml` |
| [`configuration.md`](configuration.md) | `config.toml`, adding repositories, reloading the agent, metrics |
| [`secrets.md`](secrets.md) | Where secret values live, scopes, copying them between machines, the lookup order |
| [`doctor.md`](doctor.md) | What `latem doctor` checks, and running a Mac headless |
| [`commands.md`](commands.md) | `latem list`, run states, exit codes, and what `latem run` prints and exits with |
| [`deploy.md`](deploy.md) | The ordered runbook for bringing up a Mac from scratch. Start here if you have hardware and want it building |
| [`ios.md`](ios.md) | The iOS pipeline: signing, the keychain under launchd, App Store Connect, and the failure table for each |
| [`android.md`](android.md) | The Android pipeline: the JDK, the SDK, and signing for Play |

The [README](../README.md) is the quickstart, from install to a first green
run. [`examples/`](../examples/) has two projects that have been taken to a
real store listing.

## If you want to know how it works, or why

| | |
|---|---|
| [`why.md`](why.md) | Why latem exists, the two executors, mounted caches, the five invariants, and what is built |
| [`RUNNER_SPEC.md`](RUNNER_SPEC.md) | The specification. Every invariant, every gate, every error code, and an amendment log recording what changed and what forced it |
| [`PROTOCOL.md`](PROTOCOL.md) | The wire protocol between an agent and a control plane. `test/conformance` is its executable half |
| [`architecture.html`](architecture.html) | The same shape as a diagram. Open it in a browser |
| [`artifacts-across-jobs.md`](artifacts-across-jobs.md) | Why a job that downloads another job's artifact runs on the machine that made it, with the cost of the alternatives (A84) |

## If you are working on latem

| | |
|---|---|
| [`testing.md`](testing.md) | The matrix of tests that cannot be automated, and the log of who ran which, when |
| [`release.md`](release.md) | Cutting a release: the signing key, the pipeline, and what to do when it goes wrong |
