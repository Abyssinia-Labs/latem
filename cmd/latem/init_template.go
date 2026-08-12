package main

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/Abyssinia-Labs/latem/internal/config"
)

// renderInitConfig produces the config.toml `latem init` writes.
//
// It is a string template rather than a marshalled struct on purpose. The
// comments are most of the value: this file is the one an operator edits at
// 2am, and a round-tripped struct would emit every default and explain none of
// them. The cost is that the output must be parsed back before it is trusted,
// which the caller does.
func renderInitConfig(p initParams) string {
	var b strings.Builder

	fmt.Fprintf(&b, `# latem — written by "latem init". Edit freely.
#
# Every key here is validated at load and every failure is fatal, because a
# runner that starts on a broken config does not fail at startup: it fails
# forty minutes into somebody's build. Run "latem doctor" after any edit.
#
# The full schema, with every optional key, is RUNNER_SPEC §8.

schema_version = %d

[machine]
name = %q

`, config.SchemaVersion, p.Machine)

	b.WriteString(`# --- repos: the allowlist -------------------------------------------------
#
# A repo absent from this table cannot be built, whatever a broker asks for
# (INV-2). The name on the left is what "latem run <name> <job>" takes.
#
# The repo must have .latem/jobs.yaml at its ROOT. That file is read from the
# checked-out commit and never from a broker (INV-5), so what runs is decided
# by the code you can see at that SHA.
`)
	for _, r := range p.Repos {
		fmt.Fprintf(&b, "\n[repos.%s]\nurl          = %q\nallowed_refs = [%s]\n",
			r.Name, r.URL, quoteList(p.Refs))
	}
	b.WriteString(`
# require_approval_otherwise = true
#   A ref outside allowed_refs is parked instead of rejected, and waits for a
#   local "latem approve". Approval pins the COMMIT, so a force-push after it
#   is granted parks the run again rather than building the new tip.

`)

	switch p.GOOS {
	case "darwin":
		fmt.Fprintf(&b, `# --- runner: this machine's execution slot --------------------------------
#
# native is the default macOS executor (§13.0): it runs steps on this Mac, with
# this Mac's Xcode, keychain and simulators. Nothing to install, no image to
# pull, no boot latency.
#
# It also has NO ISOLATION. A job's run can read ~/.ssh, delete ~/Library, or
# use your signing certificates. That is correct while this machine builds only
# refs your team controls, and wrong the moment it does not — at which point
# §13.2's tart executor is what you want.
#
# concurrency is 1 and enforced: two Xcode builds on one host contend on
# DerivedData and the keychain, and the failures read as bugs in the app.

[[runner]]
name        = "macos"
executor    = "native"
labels      = ["macos", %q, "xcode"]
concurrency = 1

  [runner.native]
  min_free_gb = 60
`, p.GOARCH)
	default:
		fmt.Fprintf(&b, `# --- runner: this machine's execution slot --------------------------------
#
# docker is the Linux executor (§13.1). The image is yours, but three things
# are required of it and "latem doctor" checks them by running it once: a
# non-root user "runner" with uid 1000 and a writable $HOME, plus bash, git and
# ca-certificates.

[[runner]]
name        = "linux"
executor    = "docker"
labels      = ["linux", %q, "docker"]
concurrency = 2

  [runner.docker]
  default_image = "ghcr.io/me/ci-base:latest"
  min_free_gb   = 40
`, p.GOARCH)
	}

	b.WriteString(`
# --- optional ------------------------------------------------------------
#
# [broker.remote]
#   A control plane. Entirely optional: the local broker works without one,
#   which is what makes "latem run" useful on a machine with no account
#   anywhere. "latem register" writes this section.
#
# [secrets]
#   Jobs name secrets; they never contain values (INV-2). Names resolve
#   through env -> secrets.env -> Keychain -> pass -> command (§15), and every
#   resolved value is registered with the log redactor before the first step
#   runs (§16). Put values in ` + configSecretsName() + ` at mode 0600.
`)
	return b.String()
}

func configSecretsName() string { return "secrets.env" }

func quoteList(in []string) string {
	out := make([]string, len(in))
	for i, s := range in {
		out[i] = strconv.Quote(s)
	}
	return strings.Join(out, ", ")
}
