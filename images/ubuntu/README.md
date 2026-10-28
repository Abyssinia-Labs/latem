# latem-ubuntu

A base image for latem's docker executor (RUNNER_SPEC §13.1) that stands in for
GitHub's `ubuntu-latest` closely enough to run workflows written against it. It
is multi-arch, linux/amd64 and linux/arm64, because the same jobs run on an
x86_64 Linux desktop and, through Docker Desktop, on an Apple silicon Mac.

> **Status: never built.** It was written on a machine whose Docker daemon was
> not running, so no layer here has executed. Upstream URLs and checksums were
> fetched and verified (`SHA256SUMS`), and the scripts pass `bash -n`; nothing
> more. The first person to build it should run `./build.sh && ./check.sh`,
> put the size below, and delete this note.

## What is in it

| | Version | Where |
|---|---|---|
| Ubuntu | 24.04 | `ubuntu:24.04` |
| User | `runner`, uid/gid 1000, passwordless sudo | §13.1 |
| Node | 24 (default on `PATH`) and 22 | `/opt/hostedtoolcache/node`, owned by uid 1000 |
| Python | 3.12 as `python3` (system); 3.11 and 3.13 as `python3.11`, `python3.13` | 3.11/3.13 in `/opt/hostedtoolcache/Python` |
| Docker | CLI, buildx, compose; no daemon | download.docker.com |
| Others | git, git-lfs, curl, wget, jq, gh, AWS CLI v2, zip, unzip, tar, xz, zstd, rsync, openssh-client, gnupg, build-essential | apt, except AWS CLI |
| Playwright | Chromium's system packages, xvfb, fonts; no browser | `playwright install-deps chromium` |

Exact versions are the `ARG`s in the `Dockerfile`.

**Not in it, on purpose:** bun (`setup-bun` installs it per job), any Playwright
browser (each project pins its own Playwright, and browser builds follow the
pin), and a Docker daemon.

The toolcache is laid out the way `actions/setup-node` and
`actions/setup-python` expect, and `RUNNER_TOOL_CACHE` points at it. latem does
not run JavaScript actions yet; once it does, those actions will find these
versions instead of downloading them. The Python builds are the ones
setup-python installs on `ubuntu-latest`.

`npm install -g` works without sudo, because the toolcache is owned by uid 1000
and npm's global prefix is the Node directory. `playwright install --with-deps`
only downloads the browser. It still runs `sudo apt-get update`, and any package
a different Playwright version wants that this one did not is installed then.

**Size:** not yet measured. `./check.sh` prints it.

## Build, check, publish

```bash
./build.sh            # this machine's arch → latem-ubuntu:local
./check.sh            # §13.1, the tools above, sudo/apt/npm/Playwright
./build.sh --all      # both arches; proves they build, publishes nothing
```

`--all` and `--push` create a `docker-container` buildx builder named `latem` on
first use. Building arm64 on an x86_64 host needs QEMU registered, once per
boot. The script detects when it is missing and prints the command. Docker
Desktop already has it.

To publish (the default `IMAGE` is shown):

```bash
docker login ghcr.io
IMAGE=ghcr.io/abyssinia-labs/latem-ubuntu ./build.sh --push
```

This pushes `:24.04` and `:24.04-YYYYMMDD`. Name the image by digest in each
runner's config:

```toml
[runner.docker]
default_image = "ghcr.io/abyssinia-labs/latem-ubuntu@sha256:…"
```

Two things about pulling:

- **latem pulls without credentials**, so it cannot fetch a private package on
  its own. The package inherits the repository's visibility. Either make it
  public, or `docker login ghcr.io && docker pull …` on each runner once.
  latem uses an image that is already present.
- **latem never re-pulls a tag it already has.** That is deliberate (a moving
  base image is a reproducibility bug), and it is another reason to use a
  digest.

Then run `latem doctor`. It runs the image once and checks the §13.1 contract.

### Updating

Edit an `ARG` in the `Dockerfile`, run `./pin.sh` (it rewrites `SHA256SUMS`;
requires curl and jq), read the diff, then rebuild. Rebuild monthly even with no
bump, because apt packages come from whatever `ubuntu:24.04` and the archive
hold on the day of the build.

## Who a step runs as

This differs by platform, and the difference comes from latem, not the image.

- **Linux.** latem runs each step as the agent's own `uid:gid`. On a host where
  that is 1000:1000, the step is `runner` with `HOME=/home/runner`, sudo works,
  and the toolcache is writable. That matches `ubuntu-latest`. With any other
  uid, sudo fails with "you do not exist in the passwd database" and
  `npm install -g` fails with EACCES.
- **Docker Desktop (macOS).** latem passes no user, so steps run as the image's
  default user, which is root. Everything above still works, because root needs
  no sudo. But `HOME` is `/root`, and files a job creates are root's.

The image cannot fix the second case by declaring `USER runner`. On Linux, latem
chowns each run's fresh volumes in a probe container that runs as the image's
default user and must be root to do it. With `USER runner`, every Linux run
would fail in Prepare. The `Dockerfile` explains this where `USER` would be.

## Docker inside a job

The image has the Docker CLI and buildx (for `wrangler containers build` and the
like), but latem does not currently mount a daemon socket into the job
container, so they have nothing to talk to yet. When it does, the step's uid
needs the socket's group. The host's `docker` gid is whatever the host chose and
will not match anything in this image, so that belongs in latem (a `GroupAdd`),
not here.
