# Install and set up a machine

The [README](../README.md#quickstart) has the short version. This is the long
one: what each install does, `latem setup` step by step, checking a release by
hand, updating, and setting a machine up without `latem setup`.

## Install

```console
$ curl -fsSL https://raw.githubusercontent.com/Abyssinia-Labs/latem/main/install.sh | sh
```

Never as root. The script detects your OS and architecture, verifies the
download, installs to `~/.local/bin` with `ltm` beside it, latem's short name,
and runs `latem doctor`.

Or with Homebrew, on macOS or Linux:

```console
$ brew install abyssinia-labs/tap/latem
```

The same release archive, whose checksums the tap verifies against latem's
signing key before it writes them into the formula. Homebrew updates it with
`brew upgrade latem` (`latem update` refuses to write into Homebrew's directory),
and the running agent moves onto the new version by itself, the first moment no
build is in flight.

## `latem setup`

A transcript, abridged, of `latem setup --plain`, which asks one question at a
time where a terminal would get a screen at step 3:

```console
$ latem setup --plain
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
   [`latem-ubuntu`](../images/ubuntu/README.md) pinned by digest (`--image`
   names another).
2. **repositories** — adds the repositories `--repo owner/name` names, or on
   a terminal with none configured asks which, and checks each one's
   workflows without a checkout: how many jobs this machine can run, why not
   the rest, and the `[label_aliases]` and `[actions]` entries that would let
   more run, which it adds when you say yes.
3. **toolchains** — shows what this machine builds, kind by kind, and what is
   in the way of each (with no repository, it asks what the machine will
   build). On a terminal this is a screen: it finds what is installed, sets
   the paths a step needs, runs the installs a command can do once you say
   yes — `brew install xcodegen`, `xcodebuild -downloadPlatform iOS`, a
   `docker pull` of the runner's image — opens a shell for the ones that are
   yours, and turns a kind on with a label once a check of it passes. It never
   runs `sudo`, accepts a licence or installs Xcode — those it says, for you
   to do. A keg-only install such as `openjdk@17` is on no PATH the service
   has, so setup adds it to the runner's `[runner.native] path_prepend` and
   `env` in config.toml. Off a terminal it prints the plan and goes on;
   `--plain` asks one question at a time, as above.
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

**Driving step 3 from an agent.** An agent with a shell on the machine, such
as Claude Code on it or over `ssh`, has no screen to drive. It reads
`latem setup --json` and closes each item with
`latem setup apply <id>`, or a whole kind with `latem setup apply --kind
android`, which ends by turning the kind on once its check passes. Each
command is one the agent's harness shows you before it runs. Apply does what
the screen would, and refuses what is yours with exit `4`, printing what you
would type: anything with sudo, a licence, Xcode, a secret's value, and the
two settings that give something up (`allow_unisolated` and
`require_pinned = false`), which only you, at a terminal, can write. The agent
then stops and asks you. [Commands](commands.md#latem-setup-apply-closing-what-is-in-the-way-from-a-shell)
has a transcript.

## Verify what you installed

`install.sh` does both checks below before it writes anything. By hand, for a
release downloaded directly, where `x.y.z` is the
[latest release](https://github.com/Abyssinia-Labs/latem/releases/latest)'s
version without its `v`:

```console
$ VERSION=x.y.z
$ gh release download "v$VERSION" --repo Abyssinia-Labs/latem --pattern '*darwin_arm64*'
$ gh release download "v$VERSION" --repo Abyssinia-Labs/latem --pattern '*checksums*'
$ minisign -Vm "latem_${VERSION}_checksums.txt" \
      -P 'RWTTZK1Cxjg51w6BUpg5KpkKhOHzUT+EDhgidVqHUHsiTr6nPjKC3esu'
Signature and comment signature verified
Trusted comment: latem vx.y.z

$ shasum -a 256 -c "latem_${VERSION}_checksums.txt" 2>/dev/null | grep darwin_arm64
latem_x.y.z_darwin_arm64.tar.gz: OK

$ mkdir -p ~/.local/bin && tar -xzf "latem_${VERSION}_darwin_arm64.tar.gz" -C ~/.local/bin latem
```

The signature is a separate asset from the manifest, which is why that is two
downloads rather than one. `minisign` is not on a stock macOS: `brew install
minisign`.

**`~/.local/bin` has to be on your `PATH`** — `install.sh` warns when it is not,
and a manual install has nobody to warn it. `latem version` must print the
version you installed; if it prints another, an older latem is earlier on your
`PATH` and `command -v latem` says which.

## What "verifies the download" means

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
terminal (`LATEM_NO_UPDATE_NOTIFIER=1` or `CI=1` turns that off). An agent with
`[agent] auto_update = true` does the same for itself, daily, and replaces
itself only while no build is in flight (RUNNER_SPEC §20, A56). A binary built from a tree with
no signing key in it refuses that setting at load, because an agent that
declined every release into a log nobody reads would be worse than one that
never offered to update.

The release feed is `api.github.com` and the agent sends no token, so a fork
kept private answers 404 to its own agents. `latem doctor` says so rather than
leaving it to a daily line in a log nobody is reading.

## Build from source

With Go 1.25 or newer:

```console
$ git clone https://github.com/Abyssinia-Labs/latem && cd latem
$ make latem && ./bin/latem doctor
```

## Setting up by hand

Without `latem setup`, the same machine is four commands:

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

The other is a job to run: one in `.latem/jobs.yaml`, as
[Give the repository a job](jobs.md#give-the-repository-a-job) says, or one of
the repository's GitHub workflows, which
[`latem workflows check`](jobs.md#github-workflows) reads.
