# Latem

**Self-hosted CI for machines you own.** *(LAY-tem — "metal", reversed.)*

Latem runs your CI on the laptop on your desk, the box under it, or the server
you already pay for, Xcode and mobile builds included. An agent on each machine
runs your repositories' GitHub workflow jobs, or jobs from `.latem/jobs.yaml`,
with caches that stay mounted between runs and a threat model that has a test
for each invariant. A machine can run on its own, with no account, or be
registered with [app.latem.dev](https://app.latem.dev), which sends it work and
only ever sends names. [Why it exists](docs/why.md).

## Quickstart

### 1. Install

With Homebrew, on macOS or Linux:

```console
$ brew install abyssinia-labs/tap/latem
```

Or with the install script, which puts it in `~/.local/bin`. Never run it as root:

```console
$ curl -fsSL https://raw.githubusercontent.com/Abyssinia-Labs/latem/main/install.sh | sh
```

Both check the download against the release's checksums, which latem's key
signs ([what each check proves](docs/install.md#what-verifies-the-download-means)).
Both also install `ltm` beside `latem`: the same binary under a shorter name, so
every command on this page also works as `ltm …`.

### 2. Set up the machine

```console
$ latem setup
```

`latem setup` writes `~/.latem/config.toml` with a runner for this machine: a
native `macos` runner on a Mac, and a `linux` Docker runner wherever a Docker
daemon answers. It asks which GitHub repositories the machine should build
(`--repo owner/name` names them up front) and checks their workflows. It
offers to install the toolchains their jobs need, then registers the machine
with app.latem.dev in the browser. `--no-register` keeps the machine local,
which is fully supported. Last, it installs the agent as a user service and
runs `latem doctor`. Each step skips what is already done, so it is safe to run
again. [Step by step](docs/install.md#latem-setup).

### 3. A first green run

From a checkout of one of those repositories, ask which of its workflow jobs
this machine can run, then run one from your working tree:

```console
$ cd ~/src/myapp
$ latem workflows check      # every job, and the fix for each one this machine cannot run
$ latem try beacon           # one job, by its id under jobs:, uncommitted changes included
try checks.yml › beacon at 366325402a69, as a push on refs/heads/try-demo
  2 path(s) differ from HEAD 6a4f5987bb52: M .github/workflows/checks.yml, A TRY-UNTRACKED.txt
  runs on linux
…
[latem] run succeeded
```

`latem try` needs no push, and nothing about the run reaches a control plane.
It follows the run's log and exits with the run's result. `workflows check
--fix` writes the label aliases and allowed actions it suggests.
[More on workflows](docs/jobs.md#github-workflows).

Or give the repository a `.latem/jobs.yaml`, commit it and push it:

```yaml
version: 1

jobs:
  test:
    runs_on: [macos]        # a subset of one runner's labels: macos, or linux
    steps:
      - name: test
        run: echo hello
```

```console
$ latem run myapp test
```

On a terminal `latem run` follows the run the same way. `--detach` queues it and
prints the run id instead, which is what a script gets. `latem list --all` shows
how each run ended. [Writing jobs.yaml](docs/jobs.md#latemjobsyaml).

When something fails, `latem doctor` checks thirty-odd things a self-hosted
runner gets wrong and prints the command that fixes each one.
[What it checks](docs/doctor.md).

Then:

```console
$ latem repo add me/another                        # allow another repository
$ latem secrets set --repo myapp DEPLOY_TOKEN      # a value its jobs read, from a hidden prompt
$ latem update                                     # the latest release (Homebrew: brew upgrade latem)
```

## What it does

- **Your machine is the build machine.** A job runs in Docker, which isolates
  it, or natively on the host, with the Xcode, keychain and devices already
  there. Choose by *whose code runs on this machine*: native runs steps as
  you, with everything your user can reach.
  [The executors](docs/why.md#1-your-machine-is-the-build-machine).
- **Caches are mounted, not archived.** A cache is a directory that is still
  there next run, with no save step, restore step or key.
  A workflow's `actions/cache` becomes one.
  [Caches](docs/why.md#2-caches-are-mounted-not-archived).
- **Five security invariants, written down and tested.** Outbound connections
  only. A control plane sends names, never definitions. Nothing from outside
  reaches a shell. Secrets never leave the machine. Jobs come from your
  repository at the commit being built.
  [The invariants](docs/why.md#3-five-security-invariants-written-down-and-tested).

## Commands

```
latem setup [--repo owner/name] [--url U] [--no-register] [--no-service] [--plain]   config, repositories, toolchains, registration, service and doctor; safe to run again
latem setup --json                   what this machine builds and what is in the way; changes nothing
latem setup android-sdk              Google's Android command-line tools in $ANDROID_HOME or ~/Android/Sdk, on Linux
latem init --repo <name>=<url>       write a starting config.toml for this machine
latem agent                          run the agent in the foreground (`latem service install` runs it at boot)
latem run <repo> <job> [--ref R] [--input k=v] [--follow | --wait | --detach]   a .latem/jobs.yaml job; follows it on a terminal
latem run <repo> <job> --changes FILE   the same job on a commit this machine has, with the files in FILE changed on top; no push
latem try [-C dir] [--workflow F] [--event E] [--input k=v] [--ref R] [--matrix k=v] [--repo R] [--detach] <job>   a workflow job from this working tree, uncommitted changes included; no push
latem list [--all] [--state S] [--repo R] [--limit N]
latem logs <run-id> [-f] [-n N]
latem result <run-id> [--json]       how each step ended, and the errors their output named, by file and line
latem artifacts <run-id>             what a run collected, with each file's path on this machine; reads no content
latem cancel <run-id> [--reason R]
latem approve <run-id>               local only, at a terminal: you type the commit it pins (INV-5)
latem register [--url U] [--token-file F]   no token: approve this machine in the browser
latem repo add <owner/name> | list | remove <key|owner/name>
latem secrets set | list | rm [--repo R] [--env E] NAME   value from stdin or a hidden prompt
latem secrets copy --to HOST [-i KEY] [--repo R] [--env E] NAME...   to another machine, on ssh's stdin
latem vars set | rm --repo R [--env E] NAME [VALUE]
latem vars list [--repo R] [--env E]
latem workflows check [path | --repo R] [--offline] [--json] [--fix] [-v]   which workflow jobs this machine can run, and the fix for the rest
latem store check [path | --repo R] [--platform ios|android] [--json]   whether a preview would reach testers from this machine; read-only
latem doctor [--secrets] [--fix]     check this machine and name the fix for anything wrong
latem mcp                            this machine's builds, and what it can build, to an agent over MCP, started by ssh; never approves
latem cache ls | prune | rm <name>
latem service install [--print] | uninstall | restart | reload | status
latem update [--check]               install the latest release; the agent moves onto it when idle
latem version
```

| Exit | |
|---|---|
| `0` | success |
| `1` | failure: a check failed, a run failed, an operation did not complete |
| `2` | usage: an unknown flag, a missing argument, an unknown subcommand |
| `3` | no agent is listening (`E_AGENT_DOWN`) |

`latem <command> --help` has the rest. [Commands](docs/commands.md) covers
`latem list`, the run states, and what `latem run`'s exit code means.

## Documentation

| | |
|---|---|
| [Install and set up](docs/install.md) | install.sh and Homebrew, `latem setup` step by step, checking a release by hand, updates, and a machine's git credential |
| [Jobs](docs/jobs.md) | GitHub workflows (`workflows check`, `latem try`, the steps latem performs itself) and `.latem/jobs.yaml` |
| [Configuration](docs/configuration.md) | `config.toml`, adding repositories, reloading the agent, metrics |
| [Secrets](docs/secrets.md) | where values live, scopes, copying them between machines, reading them from OpenVault, the lookup order |
| [`latem doctor`](docs/doctor.md) | what it checks, and running a Mac headless |
| [Commands](docs/commands.md) | `latem list`, run states, exit codes, `latem run`'s output |
| [Why latem](docs/why.md) | why it exists, the executors, caches, the five invariants, and what is built |
| [`docs/`](docs/README.md) | the rest: the runbook for bringing up a Mac, the iOS and Android pipelines, the specification and the wire protocol |
| [`examples/`](examples/) | a plain Xcode app and a plain Gradle app, both taken to a real store listing |

## Status

See the [latest release](https://github.com/Abyssinia-Labs/latem/releases/latest)
and its notes. Every command above is wired and does what it says. The version
is below 1.0 because the surface is still moving, so a flag may be renamed
before 1.0. Releases are signed, and an update refuses anything the key in your
binary does not vouch for. [What is built and what is not](docs/why.md#status).

[Contributing](CONTRIBUTING.md) · [Security](SECURITY.md) · Apache-2.0.
