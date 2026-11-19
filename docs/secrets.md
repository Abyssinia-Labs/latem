# Where secret values live

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
| 2 | the vault | `pinkvault set <repo>/<env>/<NAME>`, on any machine that may write the collection; read only with `[secrets] vault = true`; see [From the vault](#from-the-vault) |
| 3 | File | `latem secrets set --store file`, or a `NAME=value` line in `~/.latem/secrets.env`, mode `0600` |
| 4 | Keychain | `latem secrets set` on a Mac |
| 5 | Command | a `[secrets.commands]` entry in `config.toml` that prints the value |

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

## Pull requests

**A pull request's build gets none of these values until someone on the
machine approves its commit (A85).** Its job definition comes from its own
commit, so a pull request, and a fork's above all, can name any secret and
any `environment:` it likes. Redaction is no defence: a value encoded or sent
to another host leaves no trace in the log.

So a pull request's job that asks for a secret waits in `awaiting_approval`
before any store is asked for anything. So does one that would run on a native
runner, whether it asks or not, because a native step runs as this machine's
user, with `secrets.env`, the Keychain and the vault's socket in reach:

```console
$ latem list --state awaiting_approval     # the runs waiting, and their refs
$ latem logs <run-id>                      # its commit, and what it asks for: secrets, an environment, a native runner
$ latem approve <run-id>                   # after reading the commit; it prints the commit it approves
```

The approval is local and covers that one commit. Nothing in the pull request
and nothing the control plane sends can give it. The next push to the pull
request is a new commit, and is asked about again. A pull request's job that
asks for nothing and runs on docker builds without approval, so its checks
still run.

The rule follows the ref the machine builds, `refs/pull/…`, and not what the
event says about the pull request. A fork and a branch of the repository are
treated alike: the payload that tells them apart is relayed by the control
plane, and nothing on the machine can check it.

## From the vault

PinkVault, which was [OpenVault](https://github.com/Abyssinia-Labs/openvault)
and then latem vault, keeps one encrypted copy of the values and syncs it
between your machines, so a value is set once rather than on each machine
that builds the repository. A machine reads it when its `config.toml` says
so (A86):

```toml
[secrets]
vault = true
```

Install it from Abyssinia Labs' tap. Homebrew refuses a formula from a tap
that is not its own until you trust it:

```console
$ brew trust --formula abyssinia-labs/tap/pinkvault
$ brew install abyssinia-labs/tap/pinkvault
```

The `pinkvault` formula arrives with PinkVault 0.7.0. Until then, install
`abyssinia-labs/tap/latem-vault`, which stays as its alias for one release.
`latem setup` offers to run both on a machine that reads the vault and has no
vault command, as the item `vault-install`, with whichever of the two formulas
the tap here has. `latem vault …` runs the vault's command with the same
arguments: `latem vault collections` is `pinkvault collections`, or
`pink collections` for short (A110, A113).

**Every name it has had works** (A104, A113). `openvault = true`, the key
before the first rename, still turns it on. latem finds the vault's directory
as `$PINKVAULT_HOME`, then `~/.pinkvault` once it exists, then
`$LATEM_VAULT_HOME`, then `~/.latem-vault` once it exists, then
`$OPENVAULT_HOME`, then `~/.openvault`. It finds its command as `pinkvault`,
then `latem-vault`, then `openvault`, each on `PATH`, in `~/.cargo/bin` (or
`$CARGO_HOME/bin`), and in Homebrew's `bin`. What latem tells you to type
names the command it found; the examples here use `pinkvault`, which is
`latem-vault` or `openvault` on a vault not moved yet.

Each repository is a collection named exactly as its `[repos.<key>]` key,
hyphens included, and an entry is named by environment:

| A job of repo `R` looks for | In the vault |
|---|---|
| `R__E__NAME` (environment `E`) | collection `R`, entry `E/NAME` |
| `R__NAME` | collection `R`, entry `NAME`, at the top of the collection |
| `NAME`, machine-wide | nothing: a machine-wide name is in no collection, and comes from the stores below |

```console
$ pinkvault collections create latem-cloud                      # once, on an admin device
$ pinkvault set latem-cloud/production/CONVEX_DEPLOY_KEY --secret < key.txt
$ pinkvault scope <device> latem-cloud read                     # each machine that builds it
```

The vault comes straight after the environment, before secrets.env and the
Keychain: it is the copy every machine shares, and a machine's own copy
is the one that goes stale. So a value both hold is the vault's, `latem
secrets list` shows store `vault` first, and `latem secrets set` warns when the
value it stores will not be read.

The value is read when the job starts, over the vault service's socket,
`service.sock` in the vault's directory, and never sits in the agent's
environment. latem never runs the vault's command to read a value, as it
which can stop to ask for a passphrase. **If the service is not running, a
job that needs a value from it fails with `E_SECRET_BACKEND` and can be
retried**, as for a locked Keychain. It does not read as unset: a deploy
should not run without its token. `pinkvault service install` (OpenVault 0.2.2
and later, under any of its names) has the system start the service at login, and again if it stops. It
needs a key that opens with nobody there, as a key in the Secure Enclave
does: on a machine whose key file has a passphrase, remove it with
`pinkvault keys passphrase --remove`, or start `pinkvault service` by hand
after each reboot. latem says so only for a key that has one, or whose key
file it could not read. A job that reads only machine-wide names never asks
the vault.

`latem doctor` says whether the service answers and which repositories this
machine has no collection for — "no collection wake" — by name. For those
repositories, the vault simply holds nothing, and the stores after it answer.

**`latem setup` looks at the vault first** (A97). For a secret or a variable
that no store here holds, it names the step that closes the gap. With a vault
on this device that holds one of the machine's repositories and a config that
does not read it yet, that step is turning it on, which `latem setup apply
vault-on` writes (`openvault-on` until A104, which `setup apply` still takes
for one release). Otherwise it is the service (`pinkvault service
install`), joining the vault (`pinkvault join`, then `pinkvault approve`
elsewhere), the collection (`pinkvault scope <this device> R read` on an admin
device), or the name itself (`pinkvault set R/E/NAME --secret`). Without
the vault, it is still `latem secrets set`. [Commands](commands.md#latem-setup-apply-closing-what-is-in-the-way-from-a-shell)
has the table.

A pull request's job asks the vault nothing until its commit is approved, as
for every other store; see [Pull requests](#pull-requests).

### Variables

A workflow's `${{ vars.NAME }}` is read from the vault too, on the same
machines, with the same `vault = true` (A96). Only an entry of kind
variable answers: a secret is masked in a log and a variable is not, so a
secret never reaches `vars`, whatever it is called.

| A job of repo `R` in environment `E` reads `vars.NAME` from, first that holds it |
|---|
| the vault, collection `R`, entry `E/NAME` |
| `config.toml`, `[repos.R.environments.E.vars]` |
| the vault, collection `R`, entry `NAME` |
| `config.toml`, `[repos.R.vars]` |

To move them, put each in the vault on a device that may write the
collection. Nothing needs taking out of `config.toml` first: the vault's
value is read before it, and `config.toml`'s only where the vault holds none.

```console
$ printf %s true | pinkvault set tsedal/LATEM --var        # the repository's own
$ printf %s https://staging.example.com | pinkvault set tsedal/staging/BASE_URL --var
$ pinkvault import-env --collection tsedal --env staging --var BASE_URL --var REGION staging.env
$ latem vars list --repo tsedal
REPO    ENVIRONMENT  NAME      VALUE                        SOURCE
tsedal  -            LATEM     true                         vault, config.toml (vault answers)
tsedal  staging      BASE_URL  https://staging.example.com  vault
```

`import-env` reads a file of `NAME=value` lines into one environment's
folder, and stores a name as a variable only when `--var` names it; every
other name in the file goes in as a secret.

`latem vars set` still writes `config.toml`, and warns when the vault holds
the name it sets, since a job reads the vault's first. latem never writes to
the vault.

The vault is asked when a job's expression first reads `vars`: once per job,
over the same socket. A job that reads none never asks. **If the service is
not running, a job that reads a variable fails with `E_SECRET_BACKEND` and
can be retried**, as for a secret. It never runs with the variable empty, or
with `config.toml`'s older copy. A plan that reads one is refused the same
way.

The heartbeat tells the control plane a digest of each of a repository's own
variables, so that a job goes only to machines that would answer it alike
(A68). The agent reads them from the vault once a minute rather than on every
beat. While the service does not answer, it goes on saying the digests it
read last, so a service that restarts does not set the machine apart from
the others. It keeps them, digests only, in `~/.latem/vault-vars.json`
for the next start, reading `openvault-vars.json` once when that is all
there is.

### Files

A vault file (OpenVault 0.4 and later), such as a keystore, a `.p8` or a
service account's JSON, reaches a step as the path of a private copy in the
job's temporary directory, never as its bytes (A88). A workflow reads it by
the name `pinkvault run` gives it: the last part of its name, upper-cased,
with everything but A–Z and 0–9 turned into `_`.

| A job of repo `R` reads | It is |
|---|---|
| `secrets.UPLOAD_JKS`, in environment `E` | a file such as `E/upload.jks`, or `E/android/upload.jks`, anywhere in folder `E` |
| `secrets.GOOGLE_SERVICES_JSON` | a file `google-services.json` at the top of collection `R` |

```console
$ pinkvault import upload.jks --collection app --into production
```

```yaml
    environment: production
    steps:
      - run: ./gradlew bundleRelease -Pandroid.injected.signing.store.file="$UPLOAD_JKS"
        env:
          UPLOAD_JKS: ${{ secrets.UPLOAD_JKS }}
```

The copy is written before the first step, readable only by the job's user,
and goes with the job. A secret or variable of the same name comes first. Only
a native runner gets files: a docker job that reads one fails with
`E_MISSING_SECRET`, because a docker job's temporary files can be read by
every user in its container. A file larger than 16 MiB is refused; read those
with `pinkvault run`.
