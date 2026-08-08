// Package docker implements [executor.Executor] on top of the Docker Engine
// API (RUNNER_SPEC §13.1).
//
// One container serves one run. Every step is a separate `docker exec` into
// that container, which is the whole reason §9's step contract reads the way
// it does: an exec is a NEW PROCESS, so a step's working directory, exported
// variables, shell functions and traps die with it. Only the filesystem
// survives, and it survives for the whole run. Draft 01 of the spec claimed
// otherwise (finding F1); this package is written so that the claim cannot
// quietly become true again — [runEnv.Exec] sets WorkingDir and the complete
// environment on every single exec and never carries process state forward.
//
// The one sanctioned channel between steps is $LATEM_ENV: a file inside the
// container that a step may append `KEY=value` lines to, which this package
// reads, truncates and merges after each step. See [parseEnvFile] for the
// constraints §9 puts on it, all of which exist because that file is
// job-authored data the agent then trusts.
//
// # What is not here
//
//   - Cache volume naming and pruning (§14) belong to WS-08; this package
//     mounts whatever [executor.CacheMount] it is handed.
//   - Artifact retention, hashing and the ArtifactRef report (§17) belong to
//     WS-08 too. [runEnv.Fetch] only resolves a glob inside the workspace and
//     opens the files, refusing anything that escapes it.
//   - Disk floors (`min_free_gb`, §14.4) are a config and pruning concern, so
//     [Executor.Preflight] does not check them.
//
// # Testing
//
// Everything that does not need a daemon is behind an interface ([APIClient])
// or is a pure function, and is covered by the default `go test` gate. Tests
// that need a real Docker daemon are behind the `integration` build tag.
package docker
