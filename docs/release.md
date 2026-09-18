# Cutting a release

The pipeline is [`.github/workflows/release.yml`](../.github/workflows/release.yml).
This document is the part of it that a person does.

RUNNER_SPEC [§25](./RUNNER_SPEC.md#m7--operational-polish) owns the milestone,
[§20](./RUNNER_SPEC.md#20-health-heartbeat-self-update) owns what a release has
to be for self-update to trust it, and amendment A41 records why each of those
requirements exists.

---

## 1. What a release is

Three kinds of file, and their names are a contract rather than a convention —
`install.sh` builds them in POSIX sh and `internal/selfupdate` builds them in
Go, and `test/scripts/install_test.go` fails if the two ever disagree.

```
latem_<version>_<os>_<arch>.tar.gz      the binary at the archive root, plus LICENSE and README.md
latem_<version>_checksums.txt           sha256 of every archive above
latem_<version>_checksums.txt.minisig   a minisign signature over that manifest
```

`<version>` has no leading `v`; the tag does. The difference is load-bearing:
`install.sh` strips it with `${VERSION#v}` and `selfupdate.AssetName` takes the
stripped form, so an asset published with the `v` in its name is an asset
nothing can find.

Four platforms: `darwin/arm64`, `darwin/amd64`, `linux/amd64`, `linux/arm64`.

**One signature, over the manifest, not over the binaries.** That is what keeps
the release feed out of the trust boundary: the feed supplies a version string,
and everything that reaches a machine's disk is covered by a signature from a
key compiled into the binary already running there. §20 has the full chain.

---

## 2. The signing key — do this once

No release can be cut until this is done, and nothing else in this document
works before it. Two minutes.

Generate a password-less keypair, on a machine you trust, not in CI:

```bash
minisign -G -p latem.pub -s latem.key -W
```

`-W` means no password, and that is deliberate rather than lazy: the secret key
is going into a GitHub Actions secret, and a password stored in the same secret
store as the key it protects adds a step and no security. (If you would rather
have one, drop `-W` and set `MINISIGN_PASSWORD` too; the workflow handles both.)

Then three places, all of which must agree:

1. **`internal/selfupdate/trusted.pub`** — replace the placeholder comment with
   the two lines of `latem.pub`. This is what every agent embeds and verifies
   its own updates against, and populating it is what makes
   `agent.auto_update = true` a config the loader accepts.
2. **`PUBKEY` in [`install.sh`](../install.sh)** — the single base64 line from
   `latem.pub` (the second line, not the comment). `install.sh` is POSIX sh and
   cannot read the embedded copy, so this is a second copy by necessity; the
   test named above is what keeps it honest.
3. **The `MINISIGN_SECRET_KEY` Actions secret** — the whole contents of
   `latem.key`.

```bash
gh secret set MINISIGN_SECRET_KEY < latem.key
```

Commit the two public copies. Keep `latem.key` somewhere you will still have it
in two years — losing it does not break installed agents, but it does mean
every future release is signed by a key they do not trust, and the only way out
is a new release that nobody with `auto_update` can reach automatically.

**Do not commit `latem.key`.** `.gitignore` does not know about it; nothing in
the tree should.

### Rotating it

Publish a release signed by the old key whose `trusted.pub` and `PUBKEY` carry
the new one, then sign everything afterwards with the new key. Agents update to
that release using the key they already trust, and arrive carrying the next
one. Skipping that step strands every machine on the last release its key can
verify.

---

## 3. Rehearsing, before there is anything to lose

`workflow_dispatch` builds all four targets, writes the manifest, runs the
smoke job on a real Mac and a real Linux box, and publishes and signs nothing.
It needs no secrets, so it works before §2 has been done.

```bash
gh workflow run release.yml
```

Watch it, and read the smoke job's output rather than only its colour: it is
the only thing in the pipeline that proves a cross-compiled `darwin/arm64`
binary executes on macOS at all.

---

## 4. Cutting one

```bash
git tag -a v0.1.0 -m "v0.1.0"
git push origin v0.1.0
```

The tag must be `vMAJOR.MINOR.PATCH`, optionally with a `-prerelease` suffix.
The workflow refuses anything else before it builds, because §20's version
comparison refuses it too — and a release no agent can parse is one that fails
days later, in a log nobody is reading, rather than here where somebody is
watching.

What then runs, in order, with each stage gating the next:

| Job | What it is for |
|---|---|
| `gate` | `build`, `vet`, `test -race` against the exact tree being shipped. A tag can be pushed at any commit, including one that never passed `ci.yml` |
| `build` | Four targets from one Linux runner, `-trimpath`, `-s -w`, `-X main.version=<tag>`; archives; the sha256 manifest, verified with `sha256sum -c` on the spot |
| `smoke` | Unpacks the artefact on `macos-latest` and `ubuntu-latest` and runs `version --short`, `init` and `doctor`. `version --short` is not a formality — self-update runs exactly that on a downloaded binary and refuses the release if the answer disagrees |
| `sign` | minisign over the manifest, then **verification against the committed `trusted.pub`**. That check is what catches an Actions secret holding a key no binary in the release trusts, which is otherwise discovered by an agent, silently, a day later |
| `publish` | `gh release create` with all six assets and notes |

---

## 5. When it goes wrong

**`publish` is deliberately not idempotent.** It does not pass `--clobber` and
it fails if the release exists. A re-run over a published artefact, or a moved
tag, wants a person rather than a retry.

To withdraw a release and start again:

```bash
gh release delete v0.1.0 --yes
git push --delete origin v0.1.0
git tag -d v0.1.0
```

That is safe *only* before anyone has installed it. Afterwards, do not reuse the
number: agents refuse a version that is not strictly newer than the one they are
running, so a v0.1.0 that was replaced by a different v0.1.0 is a machine that
will never move again. Cut v0.1.1.

**A failed `sign` leaves the archives built and unpublished**, which is the
right place to fail. Fix the secret and re-run the workflow.

---

## 6. What this pipeline does not do

Stated rather than left to be discovered:

- **The macOS binaries are ad-hoc signed by the Go linker, not by a Developer
  ID, and not notarized.** That is enough to execute — macOS requires an arm64
  binary to carry at least an ad-hoc signature, and `install.sh` strips the
  quarantine attribute `curl` leaves behind — and it is not enough for
  Gatekeeper to approve a double-click. Notarizing a CLI needs a Developer ID
  certificate in CI and is worth doing the first time someone installs by
  double-clicking rather than by `curl | sh`.
- **`darwin/amd64` and `linux/arm64` are published untested.** The smoke job
  covers `darwin/arm64` and `linux/amd64`, which are what `macos-latest` and
  `ubuntu-latest` are. Adding the other two means runners this repository's
  plan does not have.
- **`install.sh` resolves `latest` through `api.github.com`**, which is
  rate-limited by IP at 60 requests an hour unauthenticated. A machine behind a
  shared NAT can exhaust that without having run the script, and
  `LATEM_VERSION` is the way past it.
- **`install.sh` cannot install from a private repository.** Release assets
  behind a private repo need a token, and the script deliberately sends none.
  Until this repository is public, install by downloading the asset with `gh`
  and running the tar and rename steps by hand.
