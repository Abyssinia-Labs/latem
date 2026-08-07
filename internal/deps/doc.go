// Package deps pins the module's third-party dependencies.
//
// It contains no code. Its only purpose is to keep every dependency listed in
// RUNNER_SPEC §3 present in go.mod before anything imports it, so that `go mod
// tidy` does not prune a library a workstream has not reached yet.
//
// This exists to enforce one of the parallel-work rules in docs/PLAN.md:
// go.mod has exactly one owner, WS-00, and nobody else runs `go get`.
// Simultaneous dependency additions conflict textually and semantically at the
// same time, and are the most common way parallel work on one Go repository
// turns into merge archaeology. Adding a dependency is a one-line pull request
// against this package — which is also what the spec's "justify additions in
// review" line asks for.
//
// The blank imports live in deps.go behind a build tag, so they cost nothing
// at build time while remaining visible to `go mod tidy`.
package deps
