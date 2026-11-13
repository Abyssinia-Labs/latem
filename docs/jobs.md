# Jobs: GitHub workflows and `.latem/jobs.yaml`

A machine runs two kinds of job: a job in one of the repository's GitHub
workflows, and a job in `.latem/jobs.yaml`. `latem try` runs the first from
your working tree, and `latem run` the second at a pushed commit.

## GitHub workflows

**Ask what this machine can run.** `latem workflows check` reads a
checkout's `.github/workflows` and plans it as the control plane would — a
push to the default branch, a pull request into it, and a dispatch of each
file that has one — through the agent's own planner and compiler, offline:

```console
$ latem workflows check ~/src/latem-cloud
checking /Users/me/src/latem-cloud (repos.latem-cloud, Abyssinia-Labs/latem-cloud)

.github/workflows/checks.yml  "Checks"
  planned for: push to main, pull_request
  [FAIL] checks
         - uses oven-sh/setup-bun@v2: oven-sh/setup-bun is not in [actions] allowed
             fix: [actions] allowed += "oven-sh/setup-bun"
         - runs-on "blacksmith-4vcpu-ubuntu-2404": no runner here carries it
             fix: [label_aliases] "blacksmith-4vcpu-ubuntu-2404" = ["linux"]

0 of 1 jobs can run on this machine
```

For each job it names what stands in the way and the fix: a `runs-on` label
no runner carries, a remote action outside `[actions] allowed` or not pinned to
a commit, secrets with no value in any scope the run would look in (names
only), vars that would read as empty, and what latem refuses outright — a
workflow of another repository, `services:`, `container:`. A matrix job is checked leg by
leg, each with its own `runs-on`, and a job that calls a workflow of the
repository as the called workflow's jobs, each by its job path, `release/pack`. `--fix` writes the
`[label_aliases]` and `[actions]` entries it suggests and reloads the agent;
pinning an action and storing a secret stay yours. It exits 1 while any job
cannot run, so it also works as a gate.

**Try one before you push it.** `latem try <job>` runs one workflow job
from your working tree, uncommitted and untracked changes included, on this
machine's own runners, with no push and no control plane:

```console
$ latem try beacon
try checks.yml › beacon at 366325402a69, as a push on refs/heads/try-demo
  2 path(s) differ from HEAD 6a4f5987bb52: M .github/workflows/checks.yml, A TRY-UNTRACKED.txt
  runs on linux

[latem] on: and the job's if: were not evaluated: latem try runs the job it names
[latem] needs changes taken as succeeded, with no outputs: a try runs one job
…
[latem] run succeeded
```

The tree becomes a commit on no ref, the way `git add -A && git commit` would
make it, without touching your index, branches or stash. The job is read from
that commit, so the run is the commit's and the log names it. It runs the job
you name whatever `on:` and the job's `if:` say, and gets no GitHub token
unless this machine stores one (`latem secrets set --repo R GITHUB_TOKEN`).
Ctrl-C cancels it. Nothing about it reaches a control plane. RUNNER_SPEC A76
has the design. A job of a called workflow is tried by its job path,
`latem try release/pack`; trying the caller lists its jobs (A82).

### Steps latem performs

**A workflow's `uses: actions/cache` becomes a mounted cache**
([why mounted](why.md#2-caches-are-mounted-not-archived)). The `path:` is
mounted for the whole job and the step compiles to nothing, so a workflow you
already have gets a warm cache without being edited. Two things behave
differently, and they follow from the model rather than from a shortcut:

- **A mount accumulates; a keyed cache replaces.** `key` and `restore-keys`
  change nothing about what is mounted. That is what you want for the stores
  actions/cache is usually pointed at — npm, pnpm, Go modules, Gradle, Cargo,
  pip — which are content-addressed and meant to be shared. For a cached
  build-output directory it is not, so cache the store rather than the output.
- **`cache-hit` is always `false`.** The install step it gates runs, and is fast
  against a warm mount; `true` would let a stale mount skip work that was
  needed.

A path inside the workspace (`node_modules`) is refused, because a mount there
would shadow the checkout — cache `~/.npm` instead. `upload-artifact` and
`download-artifact` work too, collecting into the artifact store (RUNNER_SPEC §17) when their step runs:
from the workspace or `runner.temp`, with a name or path read from an earlier
step's outputs, and v7's `archive: false`. A docker runner's `runner.temp` is
copied out of and into the container. A download finds what an earlier step of
the same job uploaded, and what a job it needs uploaded. An artifact never
leaves the machine that made it, so a job that downloads another job's is run
on the machine that ran that job. `workflows check` says so under the job
(`artifacts from build`), and a control plane has to speak protocol 2.9 to place
it there. A job whose downloads read jobs that ran on two machines cannot run.
[`artifacts-across-jobs.md`](artifacts-across-jobs.md) has the reasons.

**`uses: actions/checkout` is a step latem performs**, with no action fetched.
Left bare, it checks out the run's commit — on a push, as the branch, with
`origin/<branch>` as its upstream — and makes the job's GitHub token the
workspace's git credential, as GitHub does. So a version-stamp job's bare
`git push` works, and goes out as the latem-ci App, which latem-cloud ignores
rather than re-running the workflow. `ref:` reaches any commit your
`allowed_refs` already trusts: one in the history of an admitted branch or tag,
such as `ref: main` after an earlier job pushed, a rollback tag, or
`needs.bump.outputs.sha`. Anything else fails the step. `fetch-depth` and
`fetch-tags` are answered by the full clone, `token:` must be a secret, and
`persist-credentials: false` leaves no credential. `sparse-checkout` leaves
only the paths it lists in the workspace, in cone mode unless
`sparse-checkout-cone-mode: false`; it needs git 2.35 or later. The step's git
runs inside the job's environment, so a docker image needs git, and `main`'s
branch protection has to let the App push.

## `.latem/jobs.yaml`

### Give the repository a job

**`latem run myapp test` runs the job named `test` from `.latem/jobs.yaml` in
your repository**, read at the commit being built — not from this machine. If the
file is not there, the run fails immediately:

```console
latem: r_… failed (E_NO_REGISTRY): .latem/jobs.yaml is not present at this commit
```

Most repositories do not have one yet, so **write it, commit it, and push it**.
A working tree change is not enough: latem mirrors from the remote and checks out
the resolved commit (INV-5). The smallest file that runs is:

```yaml
version: 1

jobs:
  test:
    runs_on: [macos]        # must be a subset of a runner's labels
    steps:
      - name: test
        run: echo hello
```

`version`, `runs_on` and `steps` are the only required keys, and the job name has
to match the one you pass to `latem run`. On macOS `latem init` generates the
labels `["macos", "arm64", "xcode"]`, so `runs_on` must be a subset of those.

### Writing a pipeline

A realistic registry, with every feature that earns its place in one. Nothing
here is macOS-specific except the labels.

```yaml
version: 1

# Declared once, mounted into every job that names them. No save step, no
# restore step, no cache key: the directory is simply still there next run.
caches:
  - name: node-modules-store
    path: /Users/ci/.local/share/pnpm/store
  - name: build-cache
    path: /Users/ci/.cache/turbo

defaults:
  timeout: 20m          # per job, unless a job overrides it

jobs:
  # `latem run myapp test`
  test:
    runs_on: [macos, arm64]
    caches: [node-modules-store]
    steps:
      - name: install
        run: pnpm install --frozen-lockfile
      - name: lint
        run: pnpm lint
      - name: unit tests
        run: pnpm test -- --reporter=dot

  # `latem run myapp build` — keeps what it produces.
  build:
    runs_on: [macos, arm64]
    timeout: 45m
    caches: [node-modules-store, build-cache]
    # Globs relative to the workspace root. Collected after the job, kept for
    # `retain`, and readable under ~/.latem/artifacts/<run-id>/.
    artifacts:
      - path: dist/**
        retain: 14d
      - path: reports/junit.xml
        retain: 90d
    steps:
      - name: install
        run: pnpm install --frozen-lockfile
      - name: build
        run: pnpm build
      # A step is a new process: `cd`, exports and shell functions do not
      # survive it. The filesystem does. $LATEM_ENV is the one channel between
      # steps — append KEY=value to it and the next step has it.
      - name: record the version
        run: echo "APP_VERSION=$(node -p 'require(\"./package.json\").version')" >> "$LATEM_ENV"
      - name: name the archive
        run: echo "building $APP_VERSION"

  # `latem run myapp release --ref v1.4.0 --input channel=beta`
  release:
    runs_on: [macos, arm64]
    timeout: 90m
    # Serialises across jobs on this machine, not merely across runners: two
    # releases signing at once contend for the same keychain.
    concurrency_group: release
    # Resolved on the machine, never in this file. See docs/secrets.md.
    secrets: [NPM_TOKEN, APP_STORE_CONNECT_KEY_P8]
    inputs:
      channel:
        type: enum
        values: [beta, production]
        default: beta
    steps:
      - name: publish
        # ${input.NAME} is the only interpolation, and only in env. A secret
        # cannot be interpolated at all — the step reads it from its
        # environment, which is what keeps it out of this file and out of logs.
        env:
          RELEASE_CHANNEL: ${input.channel}
        run: ci/release.sh
```

Two rules worth knowing before you write your own, because neither is obvious
and both are enforced at parse time:

- **`run` is literal.** It is the only place a command may be written, and
  nothing is interpolated into it. Anything conditional, looping, or
  multi-command belongs in a script in your repository that the step calls —
  which is also what makes it runnable on a laptop.
- **A value containing `": "` needs quoting or a block scalar.** `run: echo
  "Building: v2"` is a YAML error, not a latem one, and the message names the
  line. `run: |` is the usual answer.

**Worked examples that have actually run** live in
[`examples/`](../examples/): a plain Xcode app and a plain Gradle app, both taken
to a real store listing, with the `ci/` scripts they call. Everything else is
indexed in [`docs/`](README.md) — the runbook for bringing up a machine, the
specification, and the acceptance tests with their findings.

### The rules

Jobs live in `.latem/jobs.yaml` **in your own repository**, committed and
pushed — see [Give the repository a job](#give-the-repository-a-job) above.

**Caches are declared once at the top level, and jobs reference them by name.**
The declaration carries the name and the mount path; the job carries a list of
names and nothing else. Writing the name-and-path mapping inside a job is the
most common mistake, and it is rejected at parse time.

```yaml
version: 1

# Top-level: declare each cache once, with the path it mounts at.
caches:
  - name: pods
    path: /Users/admin/Library/Caches/CocoaPods
  - name: pnpm-store
    path: /Users/admin/.local/share/pnpm/store

jobs:
  ios-release:
    runs_on: [macos, xcode, ios]
    timeout: 90m
    caches: [pods, pnpm-store]      # names only — the paths are declared above
    secrets: [MATCH_PASSWORD, APP_STORE_CONNECT_KEY_P8]
    artifacts:
      - path: build/*.ipa
        retain: 30d
    steps:
      - name: install
        run: pnpm install --frozen-lockfile
      - name: build
        run: ci/ios/build.sh
```

`runs_on` is matched against a runner's `labels`. **`latem init` generates
`["macos", "arm64", "xcode"]` — not `ios`**, so this job would find no runner on
a freshly initialised machine until you add `ios` to the runner's `labels`, as
the `config.toml` in [Configuration](configuration.md) does. A job whose labels
are not a subset of some runner's never starts.

The schema is strict: an unknown key is an error rather than a warning, so a
typo fails the run that introduced it instead of silently doing something else.
**Required are `version: 1`, at least one job, and per job `runs_on` and
`steps`.** Everything else is optional — `timeout` defaults to 30m, the shell
defaults to `bash`.

**`workspace: warm` keeps the job's directory between its runs**, so a build
picks up where the last one stopped:

```yaml
jobs:
  test:
    runs_on: [linux]
    workspace: warm         # fresh, a new clone per run, unless you say so
    steps:
      - name: test
        run: cargo test
```

Every run starts with each tracked file at its commit, checked by content, and
with whatever the repository ignores still there: `target/`, `node_modules/`,
`.next/`. Only the files that differ are written, so a compiler that goes by
modification time rebuilds only what changed. A file that is neither tracked nor
ignored is removed. The directory is the job's own on that runner
(`~/.latem/warm/<repo>/<runner>/<job>/tree`), at the same path every run, and
runs of the job take turns on it. `latem cache ls` shows each warm workspace
and its size; one nobody has used for a week (`agent.warm_retention`) is
removed. A repository with submodules or git-lfs builds in a fresh clone, and
the log says so. RUNNER_SPEC A90 has the design.

`artifacts` is a list of globs relative to the workspace root, each with an
optional `retain`. Collected files land in `~/.latem/artifacts/<run-id>/`
and stay on that machine: a control plane is told each one's path, size and
digest, never its content. `latem artifacts <run-id>` prints where each file is,
and you copy it off with `cp` or `scp`.
