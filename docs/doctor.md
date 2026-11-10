# `latem doctor`

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
- **Whether the OpenVault service answers**, when `[secrets] openvault = true`,
  and which configured repositories this machine holds no collection for, by
  name. A stopped service fails every job that needs a value only it holds,
  and a passphrase-protected key stops it at every reboot. The fix it names is
  `openvault service install`, which keeps the service running.
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
