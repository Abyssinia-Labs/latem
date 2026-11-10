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
| 2 | OpenVault | `openvault set <repo>/<env>/<NAME>`, on any machine that may write the collection; read only with `[secrets] openvault = true`; see [From OpenVault](#from-openvault) |
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
user, with `secrets.env`, the Keychain and OpenVault's socket in reach:

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

## From OpenVault

[OpenVault](https://github.com/Abyssinia-Labs/openvault) keeps one encrypted
copy of the values and syncs it between your machines, so a value is set once
rather than on each machine that builds the repository. A machine reads it
when its `config.toml` says so (A86):

```toml
[secrets]
openvault = true
```

Each repository is a collection named exactly as its `[repos.<key>]` key,
hyphens included, and an entry is named by environment:

| A job of repo `R` looks for | In OpenVault |
|---|---|
| `R__E__NAME` (environment `E`) | collection `R`, entry `E/NAME` |
| `R__NAME` | collection `R`, entry `NAME`, at the top of the collection |
| `NAME`, machine-wide | nothing: a machine-wide name is in no collection, and comes from the stores below |

```console
$ openvault collections create latem-cloud                      # once, on an admin device
$ openvault set latem-cloud/production/CONVEX_DEPLOY_KEY --secret < key.txt
$ openvault scope <device> latem-cloud read                     # each machine that builds it
```

OpenVault comes straight after the environment, before secrets.env and the
Keychain: the vault is the copy every machine shares, and a machine's own copy
is the one that goes stale. So a value both hold is the vault's, `latem
secrets list` shows `openvault` first, and `latem secrets set` warns when the
value it stores will not be read.

The value is read when the job starts, over the OpenVault service's socket
(`~/.openvault/service.sock`, or `$OPENVAULT_HOME/service.sock`), and never
sits in the agent's environment. latem never runs the `openvault` command,
which can stop to ask for a passphrase. **If the service is not running, a
job that needs a value from it fails with `E_SECRET_BACKEND` and can be
retried**, as for a locked Keychain. It does not read as unset: a deploy
should not run without its token. `openvault service install` (OpenVault 0.2.2
and later) has the system start the service at login, and again if it stops. It
needs a key that opens with nobody there: on a machine whose key file has a
passphrase, remove it with `openvault keys passphrase --remove`, or start
`openvault service` by hand after each reboot. A job that reads only
machine-wide names never asks OpenVault.

`latem doctor` says whether the service answers and which repositories this
machine has no collection for — "no collection wake" — by name. For those
repositories, OpenVault simply holds nothing, and the stores after it answer.

A pull request's job asks OpenVault nothing until its commit is approved, as
for every other store; see [Pull requests](#pull-requests).
