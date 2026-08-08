// Package native implements [executor.Executor] by running steps directly on
// the host, as the logged-in user (RUNNER_SPEC §13.3).
//
// # There is no isolation
//
// This is worth stating without hedging, because every other sentence in this
// package assumes the reader has understood it. A step runs as the same user as
// the agent, with that user's home directory, that user's keychain, that user's
// PATH, and no filesystem, network, process or resource boundary of any kind. A
// job's `run` can read ~/.ssh, delete ~/Library, install a launch agent, or spend
// every core on the machine. [executor.Spec.Resources] is accepted and ignored:
// there is no cgroup and no hypervisor to enforce it, and pretending otherwise
// by silently dropping the limits without saying so is how an operator ends up
// believing a native runner is bounded.
//
// It exists because §13.3 asks for a fallback for when Tart is unavailable or
// the base image is not built. It is a convenience mode. On a machine that runs
// jobs from a repository the operator does not fully control, use §13.2's tart
// executor instead — that is what it is for.
//
// # concurrency MUST be 1
//
// §13.3 is normative about this and `internal/config` enforces it at load, but
// the reason belongs next to the code as well. Two Xcode builds on one host
// contend on DerivedData and on the keychain. The resulting failures — a module
// cache that disagrees with itself, a codesign that reports a missing identity
// that is plainly present — read as bugs in the job or in the repository, and
// nobody looks at the runner's concurrency setting for another two days. The
// executor does not police this itself: an [Executor] here has no idea how many
// runs the agent has handed it. It is a config invariant, and this paragraph is
// the reason it is one.
//
// # The step contract
//
// Every step is `bash -lc <run>` as a NEW PROCESS (§9, finding F1):
//
//   - A login shell, so the environment an operator actually installed their
//     tools into is present. Homebrew's PATH entries, rbenv's shims and nvm's
//     function all arrive through /etc/profile and ~/.bash_profile, and a
//     non-login `bash -c` finds none of them. That is a native-only concern:
//     the docker executor uses `bash -c` because a container image's PATH is
//     the image's own business.
//   - A new process, so nothing a step does to its own shell survives it. Not
//     `cd`, not `export`, not a function, not a `trap`. Exactly as in the
//     docker executor, [runEnv.Exec] sets the working directory and the whole
//     environment explicitly on every call rather than inheriting anything from
//     the last one. The filesystem is what carries, and it carries for the
//     whole run. TestExec_ShellStateDoesNotLeakButFilesDo is the regression
//     test for draft 01's claim that it did not.
//
// `run` reaches the shell as a single argv element — []string{"bash", "-lc",
// run} — and is never joined to anything (INV-3, [StepArgv]).
//
// The one sanctioned channel between steps is $LATEM_ENV: a host file outside
// the workspace that a step may append `KEY=value` lines to, read, truncated
// and merged after every step. [parseEnvFile] carries §9's constraints and the
// reason for each.
//
// # What survives a killed agent
//
// A step runs in a process group of its own ([setProcessGroup]), which is what
// makes a timeout or a cancel able to stop the tree rather than only the shell.
// Nothing in the agent runs when the agent itself is killed, though, and the
// group is then reparented to launchd and keeps going — an `xcodebuild` holding
// DerivedData and a keychain handle, competing with the next build for the
// machine. So each running step also leaves a record beside $LATEM_ENV, named
// [GroupRecordPrefix], holding the process group number and — through a flock on
// a descriptor the whole step tree inherits — the proof that the number still
// means what it meant. [Executor.Reap] collects them at startup, which is §6
// step 5 for the one resource this executor has. [stepGroup] is where the
// mechanism, and the residual risk it does not remove, are written down.
//
// # The workspace is the clone itself
//
// No copy, no mount, no bind. §12 produces a self-contained repository with a
// real `.git` directory, so `git rev-parse HEAD` and `git describe` behave here
// exactly as they do inside a container, and a step's relative paths mean what
// the job author wrote.
//
// # macOS session and keychain
//
// Two host conditions turn every `codesign` in a run into an error message that
// points nowhere near its cause, and both are invisible from inside the job:
//
//  1. The agent is running under a LaunchDaemon rather than a LaunchAgent in an
//     Aqua session (§23). §13.3 calls this the single most common install
//     failure. [DetectSession] names it; [Executor.Preflight] refuses to accept
//     work in a daemon context, because no amount of retrying fixes it.
//  2. The login keychain is locked — the normal state of a Mac that rebooted
//     unattended. [CheckKeychain] probes it; see that function for why the
//     probe is opt-in rather than run on every five-minute preflight.
//
// Both are exported as plain functions taking an injectable [Runner] so that
// `latem doctor` reports the same diagnosis the agent acts on, rather than a
// second implementation of it that drifts.
//
// # What is not here
//
//   - Auto-login and FileVault status (§23). They are install-time host facts,
//     they need no run and no executor, and `latem doctor` is where an operator
//     looks for them. A five-minute preflight that shells out to `fdesetup`
//     buys nothing an install check has not already bought.
//   - Disk floors (§14.4, 60 GB for native) are a config and pruning concern,
//     matching the docker executor.
//   - Cache naming, locking and pruning (§14) belong to WS-08. This package
//     takes the [executor.CacheMount] set it is handed and makes the target
//     path resolve to it; see [linkCache] for what "used in place" (§14.2) can
//     and cannot mean without a mount namespace.
//   - Artifact retention and hashing (§17) belong to WS-08. [runEnv.Fetch]
//     resolves a glob inside the workspace and opens what it matches, refusing
//     anything that escapes.
//
// # Testing
//
// Everything that does not need a live process is a pure function or takes a
// fake [Runner]. The rest genuinely executes `bash` against a [testing.T]
// temporary directory, needs neither Xcode nor credentials, and therefore runs
// in the default gate on any unix.
package native
