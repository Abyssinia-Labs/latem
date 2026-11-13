# Configuration

`~/.latem/config.toml`, mode `0600`. One agent owns a config directory, a
machine identity, and a set of labelled runners — "two roles on one box" means
two `[[runner]]` blocks, not two agents.

```toml
schema_version = 1

[machine]
name   = "mac-mini"
labels = ["macos", "arm64", "xcode-26", "ios"]

[agent]
shutdown_grace = "30m"           # an iOS build is long
metrics_addr   = "127.0.0.1:9838"  # loopback only; "" disables (INV-1)

[repos.myapp]
url          = "git@github.com:me/myapp.git"   # ssh, https, or a local path
allowed_refs = ["main", "release/*"]     # expands to refs/heads/* (INV-5)
require_approval_otherwise = true

[[runner]]
name        = "ios"
executor    = "native"           # the default; Tart is the isolation upgrade
labels      = ["macos", "arm64", "xcode", "ios"]
concurrency = 1                  # enforced — parallel Xcode builds contend

  [runner.native]
  min_free_gb = 60
```

**`url` may also be an absolute path to a repository on this machine** —
`/Users/me/src/myapp` or a bare `…/myapp.git`. latem mirrors a path exactly as
it mirrors a remote, and it is the one form that needs no credential at all,
which makes it the fastest way to try latem against real code before deciding
how the machine should authenticate.

`require_approval_otherwise = true` is shown here because it is the setting most
worth knowing about, and **`latem init` writes it commented out**. Without it, a
ref outside `allowed_refs` is rejected rather than parked:

```console
latem: r_… rejected (E_INVALID_REF): ref "refs/heads/feature/x" is outside allowed_refs for "myapp" and approval is disabled
```

**To add a repository, use `latem repo add`:**

```console
$ latem repo add me/myapp
checking https://github.com/me/myapp.git the way the agent will fetch it …
added repos.myapp to /Users/me/.latem/config.toml
  url           https://github.com/me/myapp.git
  allowed refs  main, refs/pull/*/merge

the agent (pid 4182) reloaded config.toml
```

It writes `[repos.<name>]` with the https URL and the refs a build may use
without approval — the repository's default branch, and `refs/pull/*/merge`,
which is what a pull request's checks are built from (`--allowed-refs`
replaces both, `--require-approval` parks anything else). **A pull request's
build gets none of this machine's secrets unless you approve its commit**
(A85). A pull request's job builds without approval only when it asks for no
secret and its runner isolates it (docker). One that asks for a secret, or
would run on a native runner, waits in `awaiting_approval` until someone on
this machine reads the commit and runs `latem approve <run-id>`. That is
because its job definition is the pull request's own: a fork can name any
secret, and any environment, in it. The approval covers that one commit,
and the next push is asked about again. It checks the
repository first with `git ls-remote` the way the agent runs git: no terminal
to prompt on, and none of your shell's ssh agent, which a service does not
inherit — so a repository only your terminal can read is refused here rather
than failing every run at fetch. `latem repo list` shows what is allowed and
`latem repo remove <key|owner/name>` takes one out. All three edit `config.toml` as
text and leave the rest of the file, comments included, as you wrote it.

What this machine can run of a repository it allows, and how to run one of its
jobs before you push, are in [Jobs](jobs.md#github-workflows).

**Edit `config.toml` by hand for anything else**, then tell the running
agent, or the edit does nothing:

```console
$ latem service reload
the agent (pid 4182) reloaded config.toml
```

The agent validates `config.toml` when it starts, because a bad config has to
be fatal at load rather than forty minutes into somebody's build, and until it
re-reads the file it is running what it loaded — so a repo you have just added
is still `E_UNKNOWN_REPO` to a run. `service reload` checks the file first,
sends nothing if it does not load, and prints what the agent applied: repos
(with their refs, vars and environments), `[label_aliases]`, `[actions]` and
`log_retention`. Anything else it read — a `[[runner]]` above all — is listed
as needing `latem service restart`. A file that fails to load is refused and
the agent keeps the config it had.

Every rule is validated at load and fatal there: a runner that starts on a
broken config does not fail at startup, it fails forty minutes into somebody's
build. Disk floors are per runner — 40 GB for docker, 60 for native, 200 for
tart — because a global 20 GB admits an iOS build the machine cannot finish.

`warm_retention` (7 days unless set) is how long a warm workspace, one a job
keeps with `workspace: warm`, stays on disk with no run using it. It needs a
restart to change.

## Metrics

`metrics_addr` serves Prometheus text at `/metrics` and a JSON snapshot at
`/debug/vars`, on loopback and nowhere else. INV-1 forbids a routable listener;
it does not forbid this one. Anyone wanting the numbers off-machine points a
local Prometheus at it or tunnels — the agent does not grow a remote surface
for observability.

Exposed: runs by terminal state, run duration, queue depth, cache sizes, disk
free per runner, runner availability, broker connection state, and dropped log
frames.
