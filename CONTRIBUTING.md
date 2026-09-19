# Contributing

## The gate

```console
$ make check
```

Build, `go vet`, `go test -race`, and lint. CI runs the same four and nothing
else, so a green `make check` is the contract — with one setup step, because
lint is the part that can silently differ:

```console
$ make lint-install     # fetches the exact golangci-lint CI pins
```

That matters more than it sounds. A golangci-lint carries the gofmt of the Go
release it was built with, so a different build genuinely disagrees about
whether this tree is formatted. `make lint` refuses to answer when the installed
version is not the pinned one, rather than giving you a green that does not
transfer. The version lives in `.golangci-lint-version`, which the Makefile and
both workflows read.

Integration tests need a Docker daemon and are behind a build tag:

```console
$ make test-integration
```

They run serially on purpose (`-p 1`): the packages share one daemon and one
`latem.managed` label namespace, and in parallel a reconciliation sweep in one
removes containers belonging to another. Every one of them skips itself when no
daemon answers, so this is safe without Docker — and CI checks for a daemon
before running them, because a skipped suite and a passing one look identical.

## The specification is the argument

`docs/RUNNER_SPEC.md` is not documentation written after the code. It is where a
behaviour is decided, and the code follows it. If you want to change what latem
does, the change starts there — as an **amendment**: a numbered entry saying
what changed and what forced the change. The amendment log is the most useful
part of that document, because it records the arguments that turned out to be
wrong.

The five invariants in §5 are the promises. Each has a test named after it under
`test/invariants`. A change that makes one of those tests inconvenient is a
change to a promise, and wants an amendment rather than a smaller test.

## Tests

The conventions here are stricter than the average Go project, and for reasons
worth knowing:

- **A test's name is a sentence about behaviour**, not about a function.
  `TestSink_AckedIsWhatTheFarSideTookAndNotWhatWasSent` says what would be wrong
  if it failed.
- **Await the thing you assert on.** Two flakes in this repository had the same
  shape: waiting for a signal *adjacent* to the one being checked — a handshake
  count rather than the frame it carries, an in-memory counter rather than the
  row it will be written to. Both passed on a workstation and failed on a loaded
  runner.
- **A comment explains why, not what.** The code says what.

## Commits

Messages here are long, and deliberately. A commit explains what was wrong, what
it does about it, and what it considered and rejected — because the next person
to read it is trying to decide whether to change the same lines, and the
reasoning is the part they cannot reconstruct from the diff.

Prefix with the area: `doctor:`, `run:`, `secrets:`, `ci:`, `README:`.

## Reporting a security issue

Not here. See [`SECURITY.md`](SECURITY.md) — a public issue is a disclosure you
cannot take back.
