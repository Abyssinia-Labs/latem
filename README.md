# Latem

**Self-hosted CI for machines you own.** *(LAY-tem — "metal", reversed.)*

## Why this exists

You already own machines that can build your software. The laptop on your desk,
the box under it, the server you are paying for anyway. They are faster than
what you are renting by the minute, they are already configured the way your
project needs, and they are sitting idle while a queue somewhere else bills you
for a slower one.

So the compute is not the missing piece. The plumbing is.

**Hosted CI rents you a machine and then throws it away.** That single fact is
where the cost and most of the ritual come from. Caching exists as a save step,
a restore step, a key to get wrong and a ceiling to hit, because the machine
that warmed the cache will not be there next time. Anything that takes real
setup — a toolchain, a device, a signing identity, a licence — either gets
rebuilt every run or gets handed to somebody else's computer to keep.

**The self-hosted runners already run on hardware you own.** Buildkite,
Woodpecker, Drone, Forgejo Actions, Actions Runner Controller — that is the
right idea, and it is why latem does not try to replace them at that layer.
What they leave you is everything between "I have a machine" and "it builds my
software unattended":

- **The machine itself.** A label pointing at a box somebody set up by hand.
  Nothing in the tool knows whether the toolchain is the one the project needs,
  whether the credential works from a service rather than from your shell, or
  whether the thing will come back after a reboot — and none of those fail with
  an error that names itself.
- **The cache**, still modelled as though the machine were disposable, when
  yours is the same machine every time.
- **The threat model.** A box on your network, holding your credentials,
  running what a webhook asked it to run, usually gets a paragraph.

Those three are what the rest of this page is about.

**The hardest version of this is a Mac**, and latem is built to pass it: an iOS
release needs a real user session, an unlocked keychain, signing identities and
a simulator runtime that matches the SDK, and it is the job hosted CI charges
the most for. If a runner can do that unattended it can do the rest. That is why
iOS shows up throughout this page as evidence — not because latem is an iOS
tool.

## Two ways to take it

**Beside what you already have.** Leave your pipeline where it is and move one
job — the long, expensive, awkward one — onto a machine you own. Nothing is
ripped out and nothing is migrated. A hook binds one repository and one ref
pattern to one job on one machine; the git host supplies the push, never the
work; your existing CI never learns about it.
[`app.latem.dev`](https://app.latem.dev) is the control plane that holds those
hooks, and it only ever sends names — §5's INV-2 below is what makes pointing a
machine that holds your credentials at somebody else's server a reasonable
thing to do.

**Instead of what you already have.** Or take all of it. `latem run` needs no
account, no hook and no network: the local broker is the default and the hosted
one is opt-in (`RUNNER_SPEC` G2). A machine that never registers is a supported
machine, not a trial.

It is the same binary either way, and going from the first to the second
installs nothing new.

---

Latem exists for the three things the other runners leave to you.

### 1. Your machine is the build machine

Two executors, behind one interface.

**Docker** runs a job in a container, on any host with a daemon. It is the
isolating one: a build reaches the workspace, the caches it declared, and
nothing else — including the daemon itself, so a step that runs `docker build`
or `wrangler containers build` has nothing to talk to and needs a native runner
on a Linux host. Its image is
[`latem-ubuntu`](images/ubuntu/README.md), which stands in for GitHub's
`ubuntu-latest`.

**Native** runs a job on the host's own OS, as the agent's user. It needs
nothing installed and no disk, and it is how you use a machine that is already
set up — the Xcode you have, the keychain you have, the devices plugged into
it, the licence tied to it. A container cannot borrow any of that.

The question that decides between them is not how big your team is. It is
**whose code runs on this machine.** Your own repositories, native is the
direct answer. Anything a contributor outside the team can influence, or a
third-party action you did not write, wants the container — because native runs
steps as you, with everything your user can reach.
[`docs/RUNNER_SPEC.md`](docs/RUNNER_SPEC.md) §13.0 is the long version, and
[`docs/deploy.md`](docs/deploy.md) is the ordered runbook for bringing a machine
up from scratch.

Native runs on Linux and macOS today. **Windows is a goal and is not
implemented** — the executor is Unix-only by construction, and a Windows host
needs its own answer for process groups, shells and the between-step channel
(`RUNNER_SPEC` A47). A Windows machine can still run Docker jobs. Tart, which
would give a macOS build an ephemeral VM, is specified in §13.2 and backlogged;
nothing depends on it.

A job picks a runner by label, not by executor, so moving a machine between the
two is a config edit and touches no repository. The step contract is identical
across both — each step is a new process, the filesystem persists, `$LATEM_ENV`
carries variables between them — so a job that works on one works on the other.

### 2. Caches are mounted, not archived

Hosted CI needs save-and-restore because its machines are ephemeral. Yours are
not.

```yaml
# .latem/jobs.yaml — declared once at the top level
caches:
  - name: pnpm-store
    path: /home/runner/.local/share/pnpm/store

jobs:
  test:
    caches: [pnpm-store]      # jobs reference it by name
```

That is a Docker named volume, or a host directory shared into the VM. There is
no tarball, no upload, no size ceiling, no cache key to get wrong, and no cache
step in the job. `~/.gradle` is simply still there. A cold `pod install` and a
warm one differ by the thing that actually differs.

Caches are scoped per runner, never shared across runners, and a runner at
concurrency > 1 takes a per-cache lock. Two runs writing one `~/.gradle`
corrupt each other in ways that surface as intermittent failures nobody
attributes to caching, so that is prevented structurally rather than
documented.

**A workflow's `uses: actions/cache` becomes one of these.** The `path:` is
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
`download-artifact` work too, collecting into §17's store when their step runs.

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
`persist-credentials: false` leaves no credential. The step's git runs inside
the job's environment, so a docker image needs git, and `main`'s branch
protection has to let the App push.

### 3. Five security invariants, written down and tested

A self-hosted runner is a machine on your network holding your signing
certificates and running code a webhook asked it to run. Latem's threat model
is a document with tests attached, not a paragraph in a README.

| | |
|---|---|
| **INV-1** | The agent makes only outbound connections. No listener on any non-routable interface, no port to forward, no inbound firewall rule. The local CLI talks over a `0600` Unix socket. |
| **INV-2** | The control plane supplies names, never definitions. The inbound command set is closed at two kinds — `run` and `cancel`. A `run` carries `{id, repo, job, ref, inputs, origin}` and has no field able to express a command, an image, a path, an executor, or a timeout. |
| **INV-3** | Nothing from outside the trust boundary reaches a shell. Every git invocation passes argv as a slice with an options terminator, so a ref named `--upload-pack=/bin/sh` is a ref. |
| **INV-4** | Secrets never leave the machine. Redaction happens at the point of emission, so no sink can be added that bypasses it — including multi-line values, line by line, because `APP_STORE_CONNECT_KEY_P8` is a PEM block and redacting only the whole value redacts nothing. |
| **INV-5** | The job registry comes from your disk at the resolved commit. `jobs.yaml` is read from the checked-out workspace, never fetched from a broker, so a run is reproducible from the commit alone. Refs outside your allowlist are rejected outright; set `require_approval_otherwise = true` to park them for a local `latem approve` instead, and what that approves is one commit — if the branch moves before the run starts, it is parked again rather than built. **Parking is opt-in: `latem init` leaves the setting commented out, so the default is rejection.** |

Each has a test named after it. A pull request that weakens one is rejected
regardless of its other merits. The full statement is
[`docs/RUNNER_SPEC.md` §5](docs/RUNNER_SPEC.md).

---

## Install

```console
$ curl -fsSL https://raw.githubusercontent.com/Abyssinia-Labs/latem/main/install.sh | sh
```

Never as root. The script detects your OS and architecture, verifies the
download, installs to `~/.local/bin`, and runs `latem doctor`. Then:

```console
$ latem setup
1/6 config
  wrote /Users/me/.latem/config.toml
  runners: macos (native), linux (docker)

2/6 repositories
  a repository this machine should build (owner/name), or Enter to skip: me/myapp
checking https://github.com/me/myapp.git the way the agent will fetch it …
added repos.myapp to /Users/me/.latem/config.toml

  6 of 9 jobs in myapp can run on this machine
    checks.yml: e2e — uses oven-sh/setup-bun@v2: oven-sh/setup-bun is not in [actions] allowed
    …
  its workflows need, in config.toml:
    [label_aliases] "blacksmith-4vcpu-ubuntu-2404" = ["linux"]
    [actions] allowed += "oven-sh/setup-bun"
  add them? [Y/n] y
  added 2 entries
  another (owner/name), or Enter to go on:

3/6 toolchains

  xcodegen — for myapp apple.yml apple
    run `brew install xcodegen`? [y/N] y
    …
  JDK 17 (this machine has JDK [21]) — for myapp android.yml build
    run `brew install openjdk@17`? [y/N] y
    …
    the macos runner's steps find it now: in config.toml, [runner.native]
      path_prepend = ["/opt/homebrew/opt/openjdk@17/bin"]
      JAVA_HOME = "/opt/homebrew/opt/openjdk@17/libexec/openjdk.jdk/Contents/Home"

  Xcode 27 (this machine has Xcode 26.0.1) — for myapp apple.yml apple
    by hand: install Xcode 27 beside the one there; a step selects it with DEVELOPER_DIR

  1 still missing; `latem doctor` lists it

4/6 register
to register this machine, open

  https://app.latem.dev/connect?code=WDJB-MJHT

and approve the code  WDJB-MJHT
(opened in your browser)

waiting for approval — the code expires in 10m0s …
  approved
verifying wss://gw.latem.dev/agent …
  connection verified

5/6 service
  …
6/6 doctor
  …
```

`latem setup` does the six things a fresh machine needs, and skips each one
that is already done, so it is safe to run again:

1. **config** — writes `~/.latem/config.toml` when there is none: a native
   `macos` runner on a Mac, and a `linux` docker runner wherever a Docker
   daemon answers (always, on Linux), whose image is
   [`latem-ubuntu`](images/ubuntu/README.md) pinned by digest (`--image`
   names another).
2. **repositories** — adds the repositories `--repo owner/name` names, or on
   a terminal with none configured asks which, and checks each one's
   workflows without a checkout: how many jobs this machine can run, why not
   the rest, and the `[label_aliases]` and `[actions]` entries that would let
   more run, which it adds when you say yes.
3. **toolchains** — says what the jobs this machine takes need that it
   lacks (with no repository, it asks what the machine will build) and, on a
   terminal, offers each install a command can do, one at a time, `[y/N]`:
   `brew install xcodegen`, `xcodebuild -downloadPlatform iOS`, a `docker
   pull` of the runner's image. It never runs `sudo`, accepts a licence or
   installs Xcode — those it says, for you to do. A keg-only install such as
   `openjdk@17` is on no PATH the service has, so setup adds it to the
   runner's `[runner.native] path_prepend` and `env` in config.toml.
4. **register** — approves the machine in the browser. With no token,
   `latem register` asks the control plane for a code and opens the page to
   approve it on; you approve it signed in, seeing the machine's name, OS and
   labels, and the machine collects its own token. Over ssh the link is
   printed instead — open it anywhere you are signed in. `--url` points at a
   control plane other than the hosted one; `--no-register` keeps the
   machine local, which is a fully supported way to run it.
5. **service** — installs the agent as a user service and starts it, or
   restarts it when the config or the registration just changed.
6. **doctor** — checks the result and names the fix for anything wrong.

Then allow more repositories, see what of each this machine can run — read
from GitHub, no checkout needed — and give them what their workflows read:

```console
$ latem repo add me/myapp
$ latem workflows check --repo myapp
$ latem secrets set --repo myapp DEPLOY_TOKEN
```

`latem register --token-file F` still takes a token you already have.

**Verify what you installed**, or have the script do it for you — it does both
checks below before it writes anything. By hand, for a release downloaded
directly:

```console
$ gh release download v0.1.2 --repo Abyssinia-Labs/latem --pattern '*darwin_arm64*'
$ gh release download v0.1.2 --repo Abyssinia-Labs/latem --pattern '*checksums*'
$ minisign -Vm latem_0.1.2_checksums.txt \
      -P 'RWTTZK1Cxjg51w6BUpg5KpkKhOHzUT+EDhgidVqHUHsiTr6nPjKC3esu'
Signature and comment signature verified
Trusted comment: latem v0.1.2

$ shasum -a 256 -c latem_0.1.2_checksums.txt 2>/dev/null | grep darwin_arm64
latem_0.1.2_darwin_arm64.tar.gz: OK

$ mkdir -p ~/.local/bin && tar -xzf latem_0.1.2_darwin_arm64.tar.gz -C ~/.local/bin latem
```

The signature is a separate asset from the manifest, which is why that is two
downloads rather than one. `minisign` is not on a stock macOS: `brew install
minisign`.

**`~/.local/bin` has to be on your `PATH`** — `install.sh` warns when it is not,
and a manual install has nobody to warn it. `latem version` must print the
version you installed; if it prints another, an older latem is earlier on your
`PATH` and `command -v latem` says which.

Otherwise, build from source with Go 1.25 or newer:

```console
$ git clone https://github.com/Abyssinia-Labs/latem && cd latem
$ make latem && ./bin/latem doctor
```

### What "verifies the download" means

Two checks, and they prove different things:

- The **sha256** against the release's checksums manifest. Not optional, and
  there is no flag to skip it. It proves the file arrived intact.
- The **minisign signature** over that manifest, when `minisign` is installed.
  This is the one that proves who produced it — the manifest comes from the
  same server as the binary, so a checksum match alone says nothing about who
  served either. When the tool is missing the script says so and what it did
  not check, rather than implying more than it did.

`latem update` performs the same two checks against a key compiled into the
binary, replaces the binary it runs from, and asks the running agent to move
onto it; the agent waits for the builds in flight and re-executes in place,
keeping its pid. Every other command mentions a newer release once a day, on a
terminal (`LATEM_NO_UPDATE_NOTIFIER=1` turns that off). An agent with
`[agent] auto_update = true` does the same for itself, daily, and replaces
itself only while no build is in flight (RUNNER_SPEC §20, A56). A binary built from a tree with
no signing key in it refuses that setting at load, because an agent that
declined every release into a log nobody reads would be worse than one that
never offered to update.

The release feed is `api.github.com` and the agent sends no token, so a fork
kept private answers 404 to its own agents. `latem doctor` says so rather than
leaving it to a daily line in a log nobody is reading.

Then:

```console
$ latem init --repo myapp=git@github.com:me/myapp.git   # write a config.toml
$ latem doctor            # check the machine; every failure names its fix
$ latem service install   # run the agent at boot
$ latem run myapp test    # trigger a build
```

Two things that sequence assumes, neither of which it creates. Both are one-time
and both are quick, but a run fails without them.

### Give the machine a git credential

Latem mirrors your repository over the URL in `config.toml`, so **the machine
needs its own credential** — a fresh account has none, and `latem init`'s example
is an ssh URL. Without one, `doctor` fails the repo check and every run fails
`E_SOURCE` with `Permission denied (publickey)`.

The test is not "can *you* clone it" but "can a process started by launchd or
systemd clone it, with no terminal to prompt at". That rules out more than it
looks like it does, so the two forms that pass are given first.

**https with a credential helper** is the shortest route on a machine that has
`gh`, and the helper answers without a terminal:

```console
$ gh auth login --git-protocol https
$ latem doctor                          # the repo check should now be [ ok ]
```

Use the https URL in `config.toml` to match — `https://github.com/me/myapp.git`.

**A deploy key in `~/.ssh`, with no passphrase and an `ssh config` entry**, is
the other, and the one to use when the credential should be scoped to a single
repository:

```console
$ ssh-keygen -t ed25519 -N '' -f ~/.ssh/myapp_deploy      # -N '' — no passphrase
$ cat >> ~/.ssh/config <<'CONF'
Host github.com
  IdentityFile ~/.ssh/myapp_deploy
  IdentitiesOnly yes
CONF
$ ssh-keyscan github.com >> ~/.ssh/known_hosts            # before the first run
```

Then add the `.pub` half as a deploy key on the repository.

**What does not work, and is the easy mistake:** `gh auth login
--git-protocol ssh` generates a *passphrase-protected* key, adds no `ssh config`
entry, and leaves the passphrase for your `ssh-agent` to hold. An agent started
by launchd does not inherit your interactive `ssh-agent`, so it has no way to
unlock that key and no terminal to ask you for it. It will work in your shell
and fail in the service — the worst shape a credential problem can take. If you
have already done it, either add the key to the login keychain
(`ssh-add --apple-use-keychain ~/.ssh/id_ed25519`, plus `UseKeychain yes` and
`AddKeysToAgent yes` in `~/.ssh/config`), or generate a passphraseless deploy
key as above.

**With a control plane, a job does not need either of these for its own
repository.** A plan and a job bring a read-only token for it, so a machine with no
GitHub login at all can build. A login is still what fetches private submodules and
private actions from another repository, and what `latem run` uses. On Linux, `gh`
usually keeps it in a desktop keyring, which stays locked after a reboot until
someone logs in, so those wait for a login too. `latem doctor` says so.

**`known_hosts` matters for the same reason.** On a fresh account it does not
exist, so the first ssh connection wants to ask you to accept a host key — and a
service has nobody to ask. `ssh-keyscan` above does it in advance.

**`latem doctor` now runs this check for you**, and runs it the way the service
will: the repo probe drops `SSH_AUTH_SOCK` before asking. A credential that
works only because your shell is holding the passphrase fails the check rather
than passing it, and `doctor` says so in those words — *readable from your
shell, not from a service* — instead of reporting the machine healthy and
leaving the first real run to fail `E_SOURCE`.

To see it by hand, that probe is:

```console
$ env -u SSH_AUTH_SOCK git ls-remote git@github.com:me/myapp.git
```

Dropping `SSH_AUTH_SOCK` is the point. It is what a launchd-started service
does not inherit, so a key that works only because your `ssh-agent` is holding
the passphrase fails here exactly as it will fail in the service — which a plain
`git ls-remote` in your own shell will not tell you. Everything else about the
environment is left alone, because an https helper needs `PATH` to find the
program it shells out to.

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

## Writing a pipeline

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
    # Resolved on the machine, never in this file. See "Where secret values
    # live" below.
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
[`examples/`](examples/): a plain Xcode app and a plain Gradle app, both taken
to a real store listing, with the `ci/` scripts they call. Everything else is
indexed in [`docs/`](docs/) — the runbook for bringing up a machine, the
specification, and the acceptance tests with their findings.

## `latem doctor`

The reason a clean machine reaches a green build in under five minutes is that
almost every failure mode of a self-hosted runner is environmental, and almost
none of them produce an error that names the environment. `doctor` checks
thirty-odd things and prints the exact command that repairs each one.

```console
$ latem doctor

Tooling
  [ ok ] git                     git version 2.50.1 (Apple Git-155)
  [ ok ] docker daemon           reachable
  [skip] tart                    not installed, and no runner uses it

Disk
  [ ok ] free disk / ios         394.7 GB free, floor 60 GB (native)
  [ ok ] mirrors and work_dir    one filesystem, so clones hardlink

Executors
  [FAIL] base image / linux-x64  ghcr.io/me/ci-base:latest does not satisfy the §13.1 contract
         user-1000: no /etc/passwd entry for uid 1000; add one named runner with gid 1000
         tool-git: git is missing
         fix: add to your image: RUN useradd -u 1000 -m runner && apt-get install -y bash git

macOS session
  [warn] FileVault               on — an unattended reboot stops before login
         fix: sudo fdesetup authrestart
         fix: sudo fdesetup disable

18 passed, 2 warning(s), 4 failed, 3 skipped
```

Among the checks worth knowing about:

- **Mirrors and `work_dir` on one filesystem.** Latem clones each workspace
  from a bare mirror with `git clone --local`, which hardlinks the object
  store. Across filesystems, git silently copies it instead — no error, no
  warning, just a multi-gigabyte copy per run and a machine that got slower for
  reasons nobody attributes correctly.
- **The Docker base image contract.** `doctor` runs your image once and checks
  that uid 1000 exists, `$HOME` is writable, and `bash` and `git` are present.
  An image missing them fails forty minutes into a build as npm `EACCES` or a
  Gradle daemon that will not start — errors that look like tooling bugs.
- **FileVault**, which quietly falsifies "survives reboots". See below.
- **Every configured repo, reachable with the credentials this machine has.**
  An agent started by launchd does not inherit your interactive ssh agent.
- **Clock skew against the broker**, because token validation fails
  confusingly otherwise.
- **Whether an App Store Connect credential set agrees with itself**, under
  `--secrets`. Not whether each value is present — whether the key id names the
  key that signs the token, and whether Apple accepts it. Every way of getting
  that wrong produces the same 401 from `fastlane pilot upload`, two minutes
  in, after a full archive and export. Verified on a laptop holding four
  `AuthKey_*.p8` files, all valid, one of them the right one.
- **The locale a step's shell actually resolves to.** The agent sets
  `LANG=en_US.UTF-8`, and a login shell's profile can still overrule it, so
  `doctor` asks the shell rather than trusting the executor. Without it
  `pod install` aborts naming Unicode instead of the locale.
- **A committed Xcode scheme**, read from the repo's bare mirror. Xcode writes
  schemes to `xcuserdata/` by default and nobody commits that, so this works on
  every developer's machine and fails on every runner — where every run is a
  fresh clone.

`latem doctor --secrets` also resolves every locally-known secret name — names
only, never values — and warns about any short enough that the redactor will
skip it. `latem doctor --fix` prints a consolidated list of the remediation
commands; it never runs them.

### Where secret values live

A job's `secrets: [NAME, …]` — or a workflow's `${{ secrets.NAME }}` — names
what to resolve. The values are on the machine, never in the repository.
**`latem secrets set` is the way in:**

```console
$ latem secrets set --repo latem-cloud --env production CONVEX_DEPLOY_KEY
value for CONVEX_DEPLOY_KEY (repo latem-cloud, environment production, not echoed):
stored CONVEX_DEPLOY_KEY for repo latem-cloud, environment production, in the Keychain as latem_cloud__production__CONVEX_DEPLOY_KEY
$ pbpaste | latem secrets set CLOUDFLARE_API_TOKEN
$ latem secrets set --repo myapp APP_STORE_CONNECT_KEY_P8 < AuthKey_ABC123.p8
$ latem secrets list
SCOPE                     NAME                      STORE
*                         CLOUDFLARE_API_TOKEN      keychain
latem-cloud / production  CONVEX_DEPLOY_KEY         keychain
myapp                     APP_STORE_CONNECT_KEY_P8  keychain
```

The value comes from stdin or a prompt that does not echo, never from an
argument — an argument is in your shell history and in every user's process
listing. Piped in, a one-line value loses the newline `echo` and `pbpaste`
add, and a multi-line one (a PEM) is kept byte for byte. On a Mac it goes in
the Keychain under the service `latem`; elsewhere in `~/.latem/secrets.env`
at mode `0600`; `--store` picks. `latem secrets list` prints names and scopes,
never values, and `latem secrets rm` removes one. Every run reads the stores
afresh, so nothing needs a reload.

**`--repo` and `--env` say which jobs a value is for.** A workflow job of repo
`R` in environment `E` looks a name up as `R__E__NAME`, then `R__NAME`, then
`NAME` — most specific first (A50) — and `set` files the value under exactly
the name that lookup builds. A value with neither flag is for every job on
the machine.

**`latem secrets copy` moves a value to another machine** that builds the
same repository, over ssh, on its stdin — never in an argument there either:

```console
$ latem secrets copy --to mini --repo tsedal --env staging CLOUDFLARE_API_TOKEN CONVEX_DEPLOY_KEY
```

It reads each value from secrets.env or the Keychain here, under exactly that
scope, and runs `latem secrets set` on the other machine with the same scope.
A value with a control character in it — the escape an arrow key types at a
prompt that does not echo — is refused by `set`, and so by `copy`.

Behind the command, the lookup order is fixed, and the first that answers
wins:

| | Source | Set by |
|---|---|---|
| 1 | Environment | `LATEM_SECRET_<NAME>` in the agent's environment — the override, useful for `LATEM_SECRET_X=… latem run` to answer "is it the secret or is it the build?" |
| 2 | File | `latem secrets set --store file`, or a `NAME=value` line in `~/.latem/secrets.env`, mode `0600` |
| 3 | Keychain | `latem secrets set` on a Mac |
| 4 | Command | a `[secrets.commands]` entry in `config.toml` that prints the value |

**By hand, `secrets.env` holds a multi-line value only in double quotes with
`\n` escapes** — `KEY="-----BEGIN…\n…\n-----END…\n"` — and a quoted block
spanning real lines is a parse error. `latem secrets set --store file` writes
that quoting for you. Never put a value in a `security add-generic-password -w`
argument: `latem secrets set` hands it to `security` over stdin instead.

**Or name a command**, which works equally well and is where a secret lives when
it comes from a password manager rather than this machine:

```toml
[secrets.commands]
APP_STORE_CONNECT_KEY_P8 = "cat /Users/admin/.config/latem/AuthKey_ABC123.p8"
MATCH_PASSWORD           = "op read op://Private/match/password"
```

The string is tokenised with shell *quoting* rules and no shell *semantics*
(INV-3): whitespace separates words, quotes group, and a backslash escapes one
character. A pipe, `$`, a backtick or a redirect is rejected rather than
interpreted — so put anything that needs a shell in a script and name the script.

Redaction does not depend on which source a value came from, and multi-line
values are redacted line by line (INV-4).

## Running a Mac headless

Three things have to be true for a macOS machine to come back on its own after
a reboot, and `doctor` checks all three.

**It must be a LaunchAgent, not a LaunchDaemon.** `codesign` needs a real user
session with keychain access. In a daemon context it fails with errors that
point nowhere useful, and this is the single most common install failure.
`latem service install` writes `~/Library/LaunchAgents/com.latem.agent.plist`
and never touches `/Library/LaunchDaemons`.

**Auto-login must be enabled** for the account the agent runs as, so a session
exists after a reboot.

**FileVault is a decision you have to make.** With it on, an unattended reboot
stops at the pre-boot unlock screen: no login, no session, no LaunchAgent, no
builds — until somebody walks over to the machine and types a password. There
is nothing in any log, because the agent never started to write one. The
symptom is "my machine went offline overnight and nothing explains it".

Two valid answers, and Latem picks neither:

- **Disable FileVault**, accepting that a stolen machine yields its signing
  certificates and keystores.
- **Keep it and reboot only via `sudo fdesetup authrestart`**, which unlocks
  the disk for exactly the next boot and works over ssh. A kernel panic or a
  power cut still parks the machine at the unlock screen.

A machine holding signing certificates is a reasonable thing to want
encrypted. The point is that it should be a decision rather than a surprise.

## Configuration

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
replaces both, `--require-approval` parks anything else). It checks the
repository first with `git ls-remote` the way the agent runs git: no terminal
to prompt on, and none of your shell's ssh agent, which a service does not
inherit — so a repository only your terminal can read is refused here rather
than failing every run at fetch. `latem repo list` shows what is allowed and
`latem repo remove <name>` takes one out. All three edit `config.toml` as
text and leave the rest of the file, comments included, as you wrote it.

**Then ask what this machine can run.** `latem workflows check` reads a
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
reusable workflow, `services:`, `container:`. A matrix job is checked leg by
leg, each with its own `runs-on`. `--fix` writes the
`[label_aliases]` and `[actions]` entries it suggests and reloads the agent;
pinning an action and storing a secret stay yours. It exits 1 while any job
cannot run, so it also works as a gate.

**Then try one, before you push it.** `latem try <job>` runs one workflow job
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
has the design.

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
the `config.toml` above does. A job whose labels are not a subset of some
runner's never starts.

The schema is strict: an unknown key is an error rather than a warning, so a
typo fails the run that introduced it instead of silently doing something else.
**Required are `version: 1`, at least one job, and per job `runs_on` and
`steps`.** Everything else is optional — `timeout` defaults to 30m, the shell
defaults to `bash`.

`artifacts` is a list of globs relative to the workspace root, each with an
optional `retain`. Collected files land in `~/.latem/artifacts/<run-id>/`
and stay on that machine: a control plane is told each one's path, size and
digest, never its content. `latem artifacts <run-id>` prints where each file is,
and you copy it off with `cp` or `scp`.

## Commands

```
latem setup [--url U] [--repo owner/name] [--no-register]  config, repositories, toolchains, registration, service and doctor, idempotently
latem setup --json                                        what this machine builds and what is in the way; changes nothing
latem setup android-sdk                                   Google's Android command-line tools in ~/Android/Sdk, on Linux
latem init --repo <name>=<url>       Write a starting config.toml for this machine
latem agent                          Run the daemon (foreground; see `latem service install`)
latem run <repo> <job> [--ref R] [--input k=v] [--follow] [--wait]
latem try [-C dir] [--workflow F] [--event E] [--input k=v] [--matrix k=v] [--detach] <job>   a workflow job from this working tree, uncommitted changes included; no push
latem list [--all] [--state S] [--repo R] [--limit N]
latem logs <run-id> [-f]
latem artifacts <run-id>             what a run collected, with each file's path on this machine; reads no content
latem cancel <run-id> [--reason R]
latem approve <run-id>               Local only; pins that run's commit (INV-5)
latem register [--url U] [--token-file F]   no token: approve this machine in the browser
latem repo add <owner/name> | list | remove <name>
latem secrets set | list | rm [--repo R] [--env E] NAME   value from stdin or a hidden prompt
latem secrets copy --to HOST [--repo R] [--env E] NAME... to another machine, on ssh's stdin
latem vars set | list | rm --repo R [--env E] NAME [VALUE]
latem workflows check [path | --repo R] [--offline] [--json] [--fix]  which workflow jobs this machine can run, and the fix for the rest
latem store check [path | --repo R] [--platform ios|android] [--json]  whether a preview would reach testers from this machine; read-only
latem doctor [--secrets] [--fix]
latem cache ls | prune | rm <name>
latem service install [--print] | uninstall | restart | reload | status
latem update [--check]               install the latest release; the agent moves onto it when idle
latem version
```

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

### Run states

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

`--print` on `service install` renders the unit or plist to stdout without
installing it. Read it before you install it: that flag is what caught a
LaunchAgent pointing its log destination at a directory nothing created, which
launchd reports by failing to start and writing the reason to the file it could
not open.

Exit codes are contract:

| | |
|---|---|
| `0` | success |
| `1` | failure — a check failed, a run failed, an operation did not complete |
| `2` | usage — an unknown flag, a missing argument, an unknown subcommand |
| `3` | no agent is listening (`E_AGENT_DOWN`) |

Three is the interesting one. `latem run --wait` finding nothing to wait for is
not the same event as a build that failed, and a script has to be able to tell
them apart without parsing English.

**`latem run` on its own exits `0` as soon as the run is accepted**, before it
has done anything — it prints a run id and returns. That `0` means "queued", not
"succeeded", and a run that fails a second later has already exited `0`. **Use
`--wait` or `--follow` in anything that checks the result**; only those make the
exit code describe the build:

```console
$ latem run myapp test --wait
latem: r_90782d84ba6689ec1739424c failed (E_NO_REGISTRY): …
$ echo $?
1
```

`latem logs` also exits `0` for a run that failed — it succeeded in printing the
logs. The state is in the output, not the status.

## Metrics

`metrics_addr` serves Prometheus text at `/metrics` and a JSON snapshot at
`/debug/vars`, on loopback and nowhere else. INV-1 forbids a routable listener;
it does not forbid this one. Anyone wanting the numbers off-machine points a
local Prometheus at it or tunnels — the agent does not grow a remote surface
for observability.

Exposed: runs by terminal state, run duration, queue depth, cache sizes, disk
free per runner, runner availability, broker connection state, and dropped log
frames.

## Status

**v0.1.2 is the current release.** Every command above is wired and does what it
says; the version number is low because the surface is still moving, not because
the parts described here are sketches.

The claim this project exists to make has been tested rather than argued: on
2026-08-11 the agent, started by launchd rather than by a shell, took
`ios-release` from a bare clone to a signed build in TestFlight, with `codesign`
reaching the login keychain from a launchd-spawned process. INV-4 was then
checked against that run and not against a fixture — neither signing password,
nor the issuer id, nor any interior line of the App Store Connect PEM appears
in the run log, in any other log, or in the sqlite store. The same sequence has
since been run on a second machine that had never seen the software
([`docs/deploy.md`](docs/deploy.md)), which found three defects the first could
not.

Built: the Docker and native executors, mirror-and-hardlink source acquisition,
the registry and its admission gates, commit-pinned approval, the secrets chain
and the redactor, mounted caches and artifact collection, the run state machine,
the agent and its local broker, the remote broker, and the release pipeline —
four cross-compiled targets, a signed checksums manifest, a smoke job that runs
the result on a real machine, and self-update that replaces the binary only
between builds. All five invariants have tests named after them and all five
are green.

Not built, and honestly named:

- **A 1.0.** The surface described here is stable enough to build on and not
  yet frozen: a flag may be renamed, and a config key may grow a better
  spelling, between now and a 1.0. Releases are signed and self-update refuses
  anything the key in your binary does not vouch for, so upgrading is not the
  risk; a rename you have to notice is.
- **The Tart executor**, deferred to v2 by amendment A31. Native is the default
  because the first deployment builds repositories its operator controls; Tart
  is the isolation upgrade for when that stops being true, and `RUNNER_SPEC`
  §13.0 is the criterion rather than a preference.
- **A reboot with nobody logged in.** [`docs/deploy.md`](docs/deploy.md) §1–§8
  have been followed end to end on two machines; §9 is the one section still
  written from the spec rather than from a terminal, because a LaunchAgent
  needs a user session and auto-login and FileVault decide whether one exists.
- **The far end of the remote broker.** The control plane lives in its own
  repository now, so nothing here exercises a real server. What pins the wire
  is [`docs/PROTOCOL.md`](docs/PROTOCOL.md) and `test/conformance`, which
  replays recorded frames in both directions against committed fixtures.

Where to read further, in the order a new operator needs them:

- [`docs/RUNNER_SPEC.md`](docs/RUNNER_SPEC.md) — the specification, including
  §5's invariants and the forty-one amendments the implementation waves
  produced.
  The amendments are the interesting part: each one names the defect that
  forced it.
- [`docs/deploy.md`](docs/deploy.md) — the ordered runbook for bringing up a
  Mac runner, written from two machines rather than from the spec.
- [`docs/ios.md`](docs/ios.md), [`docs/android.md`](docs/android.md) — the
  mobile pipelines, including the failure table `doctor` turns into a
  self-service fix.

Apache-2.0.
