# Artifacts across jobs

The design behind amendment A84 in [`RUNNER_SPEC.md`](RUNNER_SPEC.md), written
on 2026-11-08 before it was built. On GitHub, one job of a workflow run can
`download-artifact` what an earlier job uploaded. latem runs each job as its own
run, on whichever machine takes it. A run's artifacts stay on that machine
(A49, §17), so until A84 the compiler refused such a download (A77).

## The jobs waiting on it

Every `download-artifact` in the Abyssinia-Labs workflows reads another job's
upload. Sizes are measured from the last real artifact. Runs a day are counted
over the 30 days to 2026-11-08.

| Repository, workflow | Uploads (runs on) | Downloads (runs on) | Artifact | Size | Runs a day |
|---|---|---|---|---|---|
| sokie `deploy-production`, `deploy-staging` | `cli/package/pack` (Linux) | `cli/publish` (Linux) | the npm tarball | 0.70 MB | 2.1 (64 versions on npm) |
| take-one `mac-release` | `build` (macOS) | `publish` (Linux) | the DMG | 3.2 MB | 0 (6 runs, all on 2026-10-02) |
| wake-cloud `mac-release` | `build` (macOS) | `publish` (Linux) | the DMG | 47.9 MB | 0.2 (6 runs) |
| antescript `deploy-production` | `sitemap-before` (Linux) | `indexnow` (Linux, four needs down) | the sitemap | 2.5 KB | 0.37 (11 runs) |

Together that is about **11.7 MB a day** going from one job to another.

Every one of them can run both its jobs on one machine. Both Macs have a native
macOS runner and a Docker Linux runner, and the Linux pairs run anywhere Linux
does.

## The options

- **(a) A relay through latem-cloud.** The uploading machine puts the artifact
  in object storage (Cloudflare R2) through a short-lived signed URL. The
  downloading machine fetches it through another one.
- **(b) Placement.** The control plane sends the downloading job to the machine
  that ran the uploading job, and that machine copies from its own store.
- **(c) Machine to machine.** The downloading machine fetches from the uploading
  one.

## Against the invariants

| | (a) relay | (b) placement | (c) direct |
|---|---|---|---|
| §17: "content never crosses the boundary" | Broken by design | Kept | Kept |
| INV-4: secrets never leave the machine | An artifact can carry one (a bundle with a baked-in key, a signing by-product); it would sit in a bucket the control plane holds | Kept | Kept |
| INV-2 and the threat model: the control plane cannot make a machine run code it chose | Broken. The downloader gets bytes the control plane served, and their digest comes through the same control plane (`needs.<job>.outputs`). The machines share no key to check them with. A step that runs what it downloaded runs the control plane's code | Kept. Every byte a download can restore was written by this machine's own run of the same repository, commit and workflow file | Kept, if the peer is authenticated |
| INV-5: the definition comes from the pinned commit | Kept | Kept. The agent works out from the commit which jobs a download may read; the control plane's list only places the job | Kept |
| INV-1: outbound connections only | Kept | Kept | **Broken**: one machine has to accept a connection. Without that it is a relay, which is (a) |

(c) is out on INV-1. (a) is out on §17 and the threat model, whatever it costs.

## Cost

R2's prices: storage $0.015 per GB-month after 10 GB free, writes $4.50 per
million after 1 million free, reads $0.36 per million after 10 million free,
and no egress charge. GitHub keeps an artifact 90 days unless told otherwise,
and so would (a). Dollar amounts are per month.

| | 1× (11.7 MB/day) | 10× | 100× |
|---|---|---|---|
| **(a)** bytes stored, 90-day retention | 1.05 GB, free | 10.5 GB, $0.01 | 105 GB, $1.43 |
| **(a)** bytes stored, 1-day retention | 12 MB, free | 117 MB, free | 1.2 GB, free |
| **(a)** R2 operations (a write and a read per artifact) | ~8 a day, free | ~80 a day, free | ~800 a day, free |
| **(a)** Worker requests to sign URLs | ~8 a day | ~80 | ~800 |
| **(a)** each machine's own uplink, up and down | 11.7 MB a day each way | 117 MB | 1.17 GB |
| **(b)** bytes leaving any machine | 0 | 0 | 0 |
| **(b)** latem-cloud storage | one list of 1 to 3 job keys on each downloading job's row, about 30 bytes, on 5 jobs in the organisation | same per job | same per job |
| **(b)** latem-cloud reads and writes | none new: the scheduler already loads every job of the run, `machineId` included, which a finished job keeps | none new | none new |
| **(b)** on the machine | already stored there (A49). A Docker download makes one more copy into the run's volume, gone when the job ends: 48 MB for Wake's DMG, for a few minutes | same | same |

(a) is cheap in dollars. What it adds is an egress path with retries, which is
how 663 MB went unnoticed once, a bucket that needs a payment method on the
Cloudflare account, and a decision for the user. (b) costs nothing at any scale.

## What (b) gives up

- **A job that reads two jobs that ran on two machines cannot run.** None of the
  jobs above does that. The control plane fails it and names the machines.
- **The downloading job waits for the uploading machine.** While that machine is
  offline the job waits. Once it is *lost*, by the control plane's existing
  partition limit, the job fails and says why. That limit is the deadline.
- **That machine must have a runner for the downloading job's labels.** If it has
  none, the job fails at once and says so. If its runner is only busy, the job
  waits for it instead of going elsewhere.
- **An upload inside a composite action is not seen.** The plan reads a job's own
  `upload-artifact` steps.

## Retention and cleanup

Nothing changes. The uploading run's files stay under
`~/.latem/artifacts/<run-id>/` until `retention-days` or the 50 GB budget
removes them in the daily sweep. The budget evicts the oldest runs first, and a
downloading job follows its upload by minutes. A download after the files are
gone fails with `E_ARTIFACT` and names the run it looked in.

## What changes

In latem (A84, protocol 2.9):

1. **The plan.** A job whose `download-artifact` reads what an earlier job
   uploaded carries `artifacts_from`, the keys of those jobs. "Earlier" means a
   job it needs, directly or further up. A name matches when both are known and
   equal, or when either is only known as the job runs. It is sent only to a
   control plane that says 2.9. An older control plane gets the job with an
   `error` naming 2.9, as a call was planned before 2.8.
2. **The compiler** stops refusing such a download. It still refuses one that no
   step before it and no earlier job could satisfy.
3. **The run.** A download looks in its own run first. Next it looks in the runs
   on this machine of the same workflow run, repository, commit and workflow
   file whose job is one the agent works out from the commit. The newest of
   them that has the artifact wins.
4. **Docker's `runner.temp`.** It is inside the run's volume. Docker's archive
   API, which A53's `PutTemp` already writes through, copies a download into it
   and an upload out of it, capped in size. sokie's
   `pack` uploads from there, and every `publish` downloads there.

In latem-cloud, which this change does not touch: decode `artifacts_from` from a
2.9 plan, keep it on the job's row, and place such a job only on the machine
that ran those jobs. The PR lists the files and functions.
