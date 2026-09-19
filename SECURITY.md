# Security

Latem is a machine on your network holding your signing certificates and
running code a webhook asked it to run. Its threat model is a document with
tests attached — RUNNER_SPEC §5, and `test/invariants` — rather than a
paragraph here.

## Reporting a vulnerability

**Use GitHub's private vulnerability reporting**, on the Security tab of this
repository ("Report a vulnerability"). It opens a channel visible only to the
maintainers, which is what you want for anything below.

Please do not open a public issue for a suspected vulnerability. A public issue
is a disclosure, and it is one you cannot take back.

Useful in a report, roughly in order: which invariant you believe is broken, the
version (`latem version` prints it, with the revision), and the smallest thing
you ran that shows it. A failing test is worth more than a description, and this
repository is set up so that one can be written.

## What counts

The five invariants are the promises, so a way to break one of them is a
vulnerability:

| | |
|---|---|
| **INV-1** | The agent makes only outbound connections. A listener on any routable interface, or any inbound path into the agent, breaks this. |
| **INV-2** | The control plane supplies names, never definitions. Anything that lets a broker specify a command, an image, a path, an executor or a timeout breaks this. |
| **INV-3** | Nothing from outside the trust boundary reaches a shell. A ref, an input or a repo name that gets interpreted rather than passed breaks this. |
| **INV-4** | Secrets never leave the machine. A sink that emits a secret unredacted — a log, a run row, an error reaching a broker — breaks this. |
| **INV-5** | The job registry comes from your disk at the resolved commit. Anything that makes a run execute a definition the commit does not contain breaks this. |

Also in scope: the release chain — a way to make an agent install or self-update
to something this project's key did not sign — and privilege or ownership
mistakes in what the agent writes to disk (`~/.latem` is `0700`, `config.toml`
and `secrets.env` are `0600`, and the agent refuses to start otherwise).

## What does not

Stated plainly, because otherwise it wastes your time and ours.

**The native executor has no isolation, by design.** A job on the native
executor runs as the user the agent runs as, with that user's keychain and
signing certificates, and can read and write anything that user can. This is
documented in the README's first section and in RUNNER_SPEC §13.0, and it is why
the choice of executor is described there as a question about *whose code runs
on this machine* rather than about team size. A demonstration that a native job
can reach the operator's files is the executor working as specified. Use the
Docker executor, or Tart when it lands, for code you do not control.

**An operator's own configuration.** `config.toml` is trusted input: it is the
file that says which repositories may be built and which commands may run. A
finding that depends on an attacker already being able to edit it, or to run as
the agent's user, is a finding about that machine's access control.

**Anything a job's own `jobs.yaml` does within its run.** A job can run arbitrary
commands — that is what a CI runner is. INV-5 is about *which* definition runs,
not about what a definition you committed is allowed to do.

## Releases

Every release is signed with minisign key `D73938C642AD64D3`, whose public half
is committed at `internal/selfupdate/trusted.pub`, embedded in every binary, and
repeated in `install.sh`. A binary reports the key it trusts:

```console
$ latem version
```

The installer verifies a sha256 against the release's manifest and, when
`minisign` is present, the signature over that manifest — the second is the one
that proves who produced it. An agent with `auto_update = true` performs both
for itself against the key compiled into the binary already running (§20), and a
build from a tree with no signing key refuses that setting at load rather than
declining every update into a log nobody reads.

If you believe a published artefact does not match its signature, treat that as a
vulnerability report rather than a bug report.

## Supported versions

Latem is pre-1.0 and support runs to the latest release only. There are no
backports; a fix ships in the next version.
