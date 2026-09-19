## What this changes, and why

<!-- The why is the part a diff cannot carry. If this fixes something, say what
     was wrong and how it presented — the symptom is what the next person will
     search for. -->

## How it was verified

<!-- `make check` is the gate. Say what else you ran, especially if the change
     touches the Docker executor (`make test-integration`, which needs a daemon)
     or anything platform-specific: several defects in this repository passed on
     macOS and failed on Linux, or the reverse. -->

- [ ] `make check` passes
- [ ] `make test-integration` passes, or this change cannot affect it

## Does it change a documented behaviour?

<!-- If it changes what latem does rather than fixing what it does wrongly, it
     wants an amendment in docs/RUNNER_SPEC.md — a numbered entry saying what
     changed and what forced it. If it changes what an operator sees, the README
     probably needs the same edit. -->
