# Documentation

Sorted by who it is for, because this directory mixes three audiences and a
visitor should not have to guess which one a file is addressing.

## If you are running latem on a machine

| | |
|---|---|
| [`deploy.md`](deploy.md) | The ordered runbook for bringing up a Mac from scratch. Start here if you have hardware and want it building |
| [`ios.md`](ios.md) | The iOS pipeline: signing, the keychain under launchd, App Store Connect, and the failure table for each |
| [`android.md`](android.md) | The Android pipeline: the JDK, the SDK, and signing for Play |

The [README](../README.md) covers install, the first run, and writing a
pipeline. [`examples/`](../examples/) has two projects that have been taken to
a real store listing.

## If you want to know how it works, or why

| | |
|---|---|
| [`RUNNER_SPEC.md`](RUNNER_SPEC.md) | The specification. Every invariant, every gate, every error code, and an amendment log recording what changed and what forced it |
| [`PROTOCOL.md`](PROTOCOL.md) | The wire protocol between an agent and a control plane. `test/conformance` is its executable half |
| [`architecture.html`](architecture.html) | The same shape as a diagram. Open it in a browser |

## If you are working on latem

| | |
|---|---|
| [`testing.md`](testing.md) | The matrix of tests that cannot be automated, and the log of who ran which, when |
| [`release.md`](release.md) | Cutting a release: the signing key, the pipeline, and what to do when it goes wrong |
| [`PLAN.md`](PLAN.md) | How the work was decomposed, and §0 is the record of what landed |

## How we know it works

Kept because a project that claims things should be able to show its working,
and because the failures are the interesting part.

| | |
|---|---|
| [`acceptance.md`](acceptance.md) | The M7 acceptance test: the instrument, written before the measurement, with predictions recorded in advance so the result could refute them. §7 is the verdict |
| [`acceptance-pass1-findings.md`](acceptance-pass1-findings.md) | Twenty findings from the first pass, verbatim, with the error text |
| [`acceptance-pass2-findings.md`](acceptance-pass2-findings.md) | The timed pass: six more findings, and a decomposition of where the five minutes went |
| [`acceptance-real-repo.md`](acceptance-real-repo.md) | The same agent pointed at a 26 MB private monorepo, running that repo's own PR gate |
| [`manual-baseline.md`](manual-baseline.md) | The M0 baseline: every command that actually worked, by hand, before any of it was automated |
