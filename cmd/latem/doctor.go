package main

import (
	"fmt"
	"io"
	"sort"

	"github.com/spf13/cobra"
)

// level is a check's outcome.
//
// The distinction between warn and fail is what makes `doctor` usable as a CI
// gate. Fail means "this machine cannot do the work it is configured for".
// Warn means "this will bite you later" — a missing tart on a machine with no
// tart runner, a mirror nobody has touched in three months. Only fail changes
// the exit code, because a doctor that exits non-zero for things the operator
// deliberately does not have is a doctor people stop running.
type level int

const (
	levelPass level = iota
	levelWarn
	levelFail
	// levelSkip is a check that does not apply here: a macOS check on Linux,
	// a Tart image check with no Tart runner configured. It is printed rather
	// than hidden, because a check silently not running looks identical to a
	// check that passed.
	levelSkip
)

func (l level) marker() string {
	switch l {
	case levelPass:
		return "[ ok ]"
	case levelWarn:
		return "[warn]"
	case levelFail:
		return "[FAIL]"
	default:
		return "[skip]"
	}
}

func (l level) color() string {
	switch l {
	case levelPass:
		return "\x1b[32m"
	case levelWarn:
		return "\x1b[33m"
	case levelFail:
		return "\x1b[31m"
	default:
		return "\x1b[90m"
	}
}

// check is one diagnosis.
type check struct {
	// ID is stable and greppable. An operator pastes it into a search and a
	// script matches on it, so renaming one is a breaking change.
	ID    string
	Title string
	Level level
	// Detail is what was found, in one line.
	Detail string
	// Fix is the exact command, or commands, that repair this. RUNNER_SPEC
	// §19 is explicit that naming the fix is the point of the whole
	// subcommand: it is what turns the failure table in §18.1 into something
	// an operator can self-service.
	Fix []string
	// Manual is a repair a person performs rather than a command anything can
	// run — a GUI setting, an installer. It is separate from Fix because
	// `--fix` prints Fix under a heading that says "review before running",
	// and a line of prose in that list is one an operator pastes and watches
	// fail. Naming the step is still worth doing; implying it is a command is
	// not.
	Manual []string
	// Notes carry the explanation a bare command cannot — most importantly
	// FileVault's, where there are two valid answers and choosing one on the
	// operator's behalf would be wrong.
	Notes []string
}

func pass(id, title, detail string) check {
	return check{ID: id, Title: title, Level: levelPass, Detail: detail}
}

func warn(id, title, detail string, fix ...string) check {
	return check{ID: id, Title: title, Level: levelWarn, Detail: detail, Fix: fix}
}

func fail(id, title, detail string, fix ...string) check {
	return check{ID: id, Title: title, Level: levelFail, Detail: detail, Fix: fix}
}

func skip(id, title, detail string) check {
	return check{ID: id, Title: title, Level: levelSkip, Detail: detail}
}

func (c check) withManual(steps ...string) check {
	c.Manual = append(c.Manual, steps...)
	return c
}

func (c check) withNotes(notes ...string) check {
	c.Notes = append(c.Notes, notes...)
	return c
}

// section groups related checks under a heading.
type section struct {
	Name   string
	Checks []check
}

// report is a whole doctor run.
type report struct {
	Sections []section
	// fixHint is the last line of a report with failures. Empty says nothing:
	// `store check`'s fixes are all under their checks already.
	fixHint string
}

func (r *report) section(name string, checks ...check) {
	if len(checks) == 0 {
		return
	}
	r.Sections = append(r.Sections, section{Name: name, Checks: checks})
}

// counts summarises the run.
func (r report) counts() (passed, warned, failed, skipped int) {
	for _, s := range r.Sections {
		for _, c := range s.Checks {
			switch c.Level {
			case levelPass:
				passed++
			case levelWarn:
				warned++
			case levelFail:
				failed++
			case levelSkip:
				skipped++
			}
		}
	}
	return
}

// fixes collects every remediation, deduplicated, in the order encountered.
// manualSteps is the Manual half of the remediation, collected exactly as
// fixes() collects the runnable half.
func (r report) manualSteps() []string {
	var out []string
	seen := map[string]bool{}
	for _, s := range r.Sections {
		for _, c := range s.Checks {
			if c.Level != levelFail && c.Level != levelWarn {
				continue
			}
			for _, m := range c.Manual {
				if !seen[m] {
					seen[m] = true
					out = append(out, m)
				}
			}
		}
	}
	return out
}

func (r report) fixes() []string {
	var out []string
	seen := map[string]bool{}
	for _, s := range r.Sections {
		for _, c := range s.Checks {
			if c.Level != levelFail && c.Level != levelWarn {
				continue
			}
			for _, f := range c.Fix {
				if !seen[f] {
					seen[f] = true
					out = append(out, f)
				}
			}
		}
	}
	return out
}

func newDoctorCommand() *cobra.Command {
	var (
		withSecrets bool
		showFix     bool
	)
	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Check this machine and name the fix for anything wrong",
		Long: `Check this machine and name the fix for anything wrong.

Every check that fails prints the exact command that repairs it. That is the
point of the subcommand: RUNNER_SPEC §19 calls it a first-class deliverable
because a self-hosted runner's failure modes are almost all environmental, and
almost none of them produce an error message that names the environment.

What it checks (§19, §23):
  - git, docker, and tart: present, and new enough
  - the Docker daemon, and whether this user can reach it
  - the base image contract (§13.1) by running the image once
  - the NVIDIA toolkit when a gpu runner is configured, and /dev/kvm for
    Android emulator jobs
  - macOS: LaunchAgent versus LaunchDaemon, auto-login, FileVault, the
    keychain, the selected Xcode, and the licence
  - the Tart base image and its real on-disk size against min_free_gb
  - that mirrors and work_dir are on one filesystem, without which git stops
    hardlinking and every run gets slower with nothing to point at
  - every configured repo, reachable with the credentials this machine has
  - directory and file modes, loopback metrics_addr, and clock skew

--secrets additionally resolves every locally-known secret name. Only names
are ever printed; a value never leaves this process (INV-4).
--fix prints a consolidated list of remediation commands. It does not run them.

Exits non-zero when any check fails. Warnings do not change the exit code.`,
		Args: noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			d := newDoctor()
			d.includeSecrets = withSecrets
			rep := d.run(cmd.Context())

			out := cmd.OutOrStdout()
			printReport(out, rep, showFix, useColor(out))

			_, _, failed, _ := rep.counts()
			if failed > 0 {
				return &ExitError{
					Code: ExitFailure,
					Err:  fmt.Errorf("%d check(s) failed", failed),
				}
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&withSecrets, "secrets", false, "also resolve every locally-known secret name (names only)")
	cmd.Flags().BoolVar(&showFix, "fix", false, "print a consolidated list of remediation commands (does not run them)")
	return cmd
}

func printReport(w io.Writer, rep report, showFix, color bool) {
	const reset = "\x1b[0m"
	paint := func(l level, s string) string {
		if !color {
			return s
		}
		return l.color() + s + reset
	}

	width := 0
	for _, s := range rep.Sections {
		for _, c := range s.Checks {
			if n := len(c.Title); n > width {
				width = n
			}
		}
	}

	st := styler{color: color}
	for _, s := range rep.Sections {
		printf(w, "\n%s\n", st.heading(s.Name))
		for _, c := range s.Checks {
			printf(w, "  %s %-*s  %s\n", paint(c.Level, c.Level.marker()), width, c.Title, c.Detail)
			for _, n := range c.Notes {
				for _, line := range wrapLines(n, 72) {
					printf(w, "         %s\n", line)
				}
			}
			for _, f := range c.Fix {
				printf(w, "         fix: %s\n", f)
			}
			// Manual is rendered here too, and labelled differently. Splitting
			// the two was right for --fix's pasteable list; leaving Manual out
			// of the per-check view was not, because a check whose repair is a
			// GUI step then named no repair at all — and auto-login, which has
			// no command, is the one that fails most often on a fresh Mac.
			for _, m := range c.Manual {
				printf(w, "         by hand: %s\n", m)
			}
		}
	}

	passed, warned, failed, skipped := rep.counts()
	printf(w, "\n%d passed, %d warning(s), %d failed, %d skipped\n", passed, warned, failed, skipped)

	if showFix {
		fixes, manual := rep.fixes(), rep.manualSteps()
		switch {
		case len(fixes) == 0 && len(manual) == 0:
			printf(w, "\nnothing to fix.\n")
		default:
			if len(fixes) > 0 {
				printf(w, "\nRemediation — review before running; latem does not run these for you:\n\n")
				for _, f := range fixes {
					printf(w, "  %s\n", f)
				}
			}
			// Under its own heading, because everything above it can be pasted
			// into a shell and nothing below it can.
			if len(manual) > 0 {
				printf(w, "\nBy hand — these are not commands:\n\n")
				for _, m := range manual {
					printf(w, "  %s\n", m)
				}
			}
		}
	}

	if failed > 0 && !showFix && rep.fixHint != "" {
		printf(w, "\n%s\n", rep.fixHint)
	}
}

// sortedStrings is used where a check iterates a map, so two runs of doctor on
// an unchanged machine produce the same output and can be diffed.
func sortedStrings[T any](m map[string]T) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
