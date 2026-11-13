# Commands

The [README](../README.md#commands) lists every command and its flags, and
`latem <command> --help` is the full text of each. This page is what is worth
knowing beyond that. Every command also answers to `ltm`, latem's short name:
`ltm run myapp test` is `latem run myapp test`.

## `latem list`

**`latem list` shows only runs that have not finished.** A run that has already
succeeded or failed needs `--all`, which is how you confirm a build worked:

```console
$ latem list --all
RUN ID                      #   STATE      REPO   JOB   REF              STARTED   DURATION
r_a0d1c0b8aae59b84287b3729  42  succeeded  myapp  test  refs/heads/main  just now  1s
```

`--limit` caps the number printed and defaults to 50.

**`#` is the build number**, and a job gets it as `$LATEM_BUILD_NUMBER` so that
an artefact in TestFlight can be traced back to the run that produced it. It
comes from a sequence rather than a row count, so it is never reused even after
old runs are pruned — reissuing a build number App Store Connect has already
accepted is the failure that rule exists to prevent (amendment A36).

The columns above are aligned because this was a terminal. **Piped or
redirected, the same command emits raw tab-separated fields** — no padding, no
colour — so `latem list --all | cut -f3` is the state of every run and stays
that way whatever the values are.

## Run states

`--state S` takes one of these. Three of them are worth knowing about because
they are **not** `failed`, and a script that filters for `failed` alone misses
every one:

| State | Means |
|---|---|
| `queued` | admitted, waiting for a runner |
| `awaiting_approval` | outside `allowed_refs`; needs `latem approve` (INV-5) |
| `claimed` · `preparing` · `running` · `collecting` | in flight |
| `succeeded` | every step exited 0 |
| `failed` | a step exited non-zero |
| `timed_out` | exceeded the job or step budget — **not** `failed` |
| `cancelled` | `latem cancel` — **not** `failed` |
| `rejected` | refused at admission, e.g. `E_INVALID_REF` — **not** `failed` |
| `interrupted` | the agent stopped mid-run |

## `latem service install --print`

`--print` on `service install` renders the unit or plist to stdout without
installing it. Read it before you install it: that flag is what caught a
LaunchAgent pointing its log destination at a directory nothing created, which
launchd reports by failing to start and writing the reason to the file it could
not open.

## Exit codes

Exit codes are contract:

| | |
|---|---|
| `0` | success |
| `1` | failure — a check failed, a run failed, an operation did not complete |
| `2` | usage — an unknown flag, a missing argument, an unknown subcommand |
| `3` | no agent is listening (`E_AGENT_DOWN`) |
| `4` | `latem approve` needs a person at a terminal, and stdin or stdout is not one |

Three is the interesting one. A `latem run` finding no agent to wait for is
not the same event as a build that failed, and a script has to be able to tell
them apart without parsing English. Four is the same kind of answer: nothing
was tried, and the next step is a person's.

## `latem run`: following, waiting, and queueing

**On a terminal, `latem run` follows the run**: it prints the run id, then the
run's log as it is produced, and exits with the run's result, as `--follow`
does. Ctrl-C stops the following and leaves the run going; `latem logs -f
<run-id>` follows it again and `latem cancel <run-id>` stops it.

**When stdout is not a terminal, `latem run` exits `0` as soon as the run is
accepted**, before it has done anything: it prints the run id and returns, so
`id=$(latem run myapp test)` works. That `0` means "queued", not "succeeded",
and a run that fails a second later has already exited `0`. **Use `--wait` or
`--follow` in anything that checks the result**; only those make the exit code
describe the build:

```console
$ latem run myapp test --wait
latem: r_90782d84ba6689ec1739424c failed (E_NO_REGISTRY): …
$ echo $?
1
```

`--detach` is that queue-and-exit at a terminal. A run that is only queued is
queued even with no agent running, and starts when one does. A run that is
followed or waited for, with no agent listening, is exit `3` and nothing is
queued (RUNNER_SPEC §7.3, A81).

`latem logs` also exits `0` for a run that failed — it succeeded in printing the
logs. The state is in the output, not the status.

## `latem approve`: a person, at a terminal

A run waits in `awaiting_approval` when its ref is outside `allowed_refs`, or
when it is a pull request's job that asks for one of the machine's secrets or
would run on a native runner (A85). **`latem approve <run-id>` releases it, and
only a person at a terminal can run it.** It shows the commit the run would
build and why it is held, then asks for the commit's short sha:

```console
$ latem approve r_6f1c0e2a9b7d4c3e8a5f1b2d
A run is waiting for your approval

  run       r_6f1c0e2a9b7d4c3e8a5f1b2d  myapp / deploy
  ref       refs/pull/7/merge
  commit    0123456789abcdef0123456789abcdef01234567
  workflow  .github/workflows/deploy.yml, on pull_request
  from      remote by octocat
  held      a pull request's job asks for secrets DEPLOY_KEY of environment production; …
  expires   2026-11-13 09:00, in 23h

Its log so far: latem logs r_6f1c0e2a9b7d4c3e8a5f1b2d

To approve this commit, type its short sha (Enter leaves it held): 0123456
approved r_6f1c0e2a9b7d4c3e8a5f1b2d at 0123456789ab (refs/pull/7/merge) — it is queued
```

Seven characters or more of the commit, from the start, in either case: what
`git log --oneline` or the pull request's page shows, or latem's twelve.
Anything else, or Enter, leaves the run held and exits `1`. What was typed
before the question is thrown away. What you approve is that commit and no
other. If the run is held again at another commit while you answer, nothing is
approved, and running the command again shows the new one.

**Without a terminal on stdin and stdout it refuses, with exit `4`**, before it
reads anything, and no flag skips the question. That is what keeps an agent
from approving in the course of its work: Claude Code's shell, `ssh host
latem …` with no `-t` (which is how `latem mcp` is reached), a script and a
pipe have no terminal. It does not stop an agent that makes a terminal for
itself with `ssh -tt` or `script`. It is a guard against an agent approving,
not a wall against whoever holds the account, who could edit `config.toml`
instead (RUNNER_SPEC A89). Over ssh, log in and run it at the prompt.

## `latem result`: what broke, and where

**`latem result <run-id>` prints how the run went**, step by step, and the
errors its output named, each with the file and line the tool put on it:

```console
$ latem result r_a9ebf5ded6aac0b56fe40fbc
r_a9ebf5ded6aac0b56fe40fbc failed (E_STEP): step "test" exited 101
  job tier2 of anvers at 3b10592fa747, changes on cc276f84bc76, on macos, warm workspace, 1.8s

STEP  OUTCOME            TIME  ERRORS  WARNINGS
test  failure, exit 101  1.5s  1       0

1 error(s), 0 warning(s)
error crates/anvers-gate/tests/gate.rs:458:5  latem_e2e_fails  (cargo-test, step test)
    assertion `left == right` failed: the change broke arithmetic
      left: 2
     right: 3
```

`--json` is the same result as one document, for a program. It is read off the
run's log, after the redactor, by readers for rustc and clippy, `cargo test`,
go's compiler, vet and `go test`, tsc, `bun test`, vitest, biome, pytest, and
anything that prints `file:line:col: error:` the way clang and xcodebuild do.
A script of your own names a problem with
`echo "::error file=src/x.c,line=3::what is wrong"`. A suggested fix is shown
when the tool printed one, and is the tool's. A problem has a line when the
tool gave one: a file a formatter would change has only the file. A problem a
step names with `::error` and its tool also prints is shown once (A93). It
exits 0 whatever the run's state, as `latem logs` does (amendment A92).

## `latem mcp`: an agent on another machine

**An agent working on your laptop can watch and drive the builds on a build
machine** through `latem mcp`, which speaks the Model Context Protocol on stdin
and stdout. The agent's client starts it over ssh, so the machine opens no port:

```console
$ claude mcp add --scope user mini -- ssh mini .local/bin/latem mcp
```

latem is named by its path because ssh runs a command with a short `PATH` (on
a Mac, `/usr/bin:/bin:/usr/sbin:/sbin`), which has neither `~/.local/bin` nor
Homebrew's bin, so `ssh mini latem mcp` is `command not found`.
`.local/bin/latem` is where `install.sh` puts it, relative to the ssh login's
home. With Homebrew on a Mac with Apple silicon it is `/opt/homebrew/bin/latem`.
`latem mcp --help` on the build machine prints the line with its own path.

Then `/mcp` in Claude Code lists `mini` and its tools. `ssh mini` must log in
without a prompt — a key, and the host already in `known_hosts` — because
nobody is at the terminal to answer one. Add one server per machine.

The tools are the commands an operator would type over ssh: `listRuns` (by
repository, state, or the commit an agent just pushed), `getRun`,
`getRunLogs`, `waitForRun`, `startRun` (a `.latem/jobs.yaml` job, as
`latem run`), `cancelRun`, and `checkWorkflows` (as
`latem workflows check --repo`).

**`waitForRun` is one call, and it answers with the result.** It waits up to
ten minutes for the run to settle and returns it with what `latem result`
prints: the steps, and each error by file and line. There is nothing to poll.
A client that sends a progress token is told each step as it ends.

**It never approves.** A run held for approval waits for a person at the
machine to run `latem approve`, and the agent is told so. `latem try`, secrets,
vars, repositories and the service are not offered either (amendment A87). Nor
can the agent run `latem approve` over the same ssh: with no terminal it refuses
(A89).

**An agent can have its own unpushed change built**, by giving `startRun` the
commit it started from and the files it changed (`changes`), where the machine
allows it: `agent_changes = true` under the repository in config.toml, a base in
the history of an allowed ref, and a job the base's `.latem/jobs.yaml`
declares. [`jobs.md`](jobs.md) has the JSON and
[`configuration.md`](configuration.md) the line (amendment A91).
