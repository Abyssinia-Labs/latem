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
	if len(p.Repos) == 0 {
		// `latem setup` writes this file before there is a repo to name. An
		// empty allowlist loads, and rejects every run until one is added.
		b.WriteString(`#
# None yet: "latem repo add <owner/name>" adds one, checks this machine can
# fetch it, and tells the running agent.
`)
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
		if p.Docker {
			b.WriteString("\n")
			writeDockerRunner(&b, p, "Docker Desktop's Linux VM")
		}
	default:
		writeDockerRunner(&b, p, "")
	}

	b.WriteString(`
# --- actions -------------------------------------------------------------
#
# A workflow may say "uses: owner/repo@ref", which is code you did not write
# running where your credentials are. Nothing is allowed until you say so.
#
# [actions]
#   allowed = ["actions/*"]        # * covers one segment; "*/*" is everyone
#   require_pinned = true          # default: a tag can be moved by its owner
#   allow_unisolated = false       # default: such a job needs a docker runner,
#                                  # because native runs steps as this user

# --- runs-on aliases ------------------------------------------------------
#
# A workflow's runs-on is matched against the labels above. GitHub's hosted
# labels (ubuntu-latest, macos-latest and their versions) already map onto
# linux and macos; name anything else a workflow uses here.
#
# [label_aliases]
#   "blacksmith-4vcpu-ubuntu-2404" = ["linux"]

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

// defaultDockerImage is the image a docker runner gets when nobody names one:
// latem-ubuntu (images/ubuntu), which stands in for GitHub's ubuntu-latest,
// named by digest so a runner's base image moves only when this line does.
// Published by .github/workflows/images.yml; its run summary names the next
// digest to put here.
const defaultDockerImage = "ghcr.io/abyssinia-labs/latem-ubuntu:24.04@sha256:ea43a6e728814f5df5a65eb7fbd94e79a45cc48963d0c6040cccb2221cfc5bf1"

// writeDockerRunner is the linux docker runner: the only runner on Linux, and
// on a Mac the second one, beside native, when a Docker daemon answers.
func writeDockerRunner(b *strings.Builder, p initParams, where string) {
	image := p.Image
	if image == "" {
		image = defaultDockerImage
	}
	slots := p.DockerConcurrency
	if slots == 0 {
		slots = 2
	}
	intro := "docker is the Linux executor (§13.1)."
	if where != "" {
		intro = "docker runs Linux jobs here, in " + where + " (§13.1)."
	}
	fmt.Fprintf(b, `# --- runner: this machine's execution slot --------------------------------
#
# %s The image is latem-ubuntu, which
# stands in for GitHub's ubuntu-latest (images/ubuntu/README.md); latem pulls
# it on first use. Any image will do that meets three requirements, which
# "latem doctor" checks by running it once: a non-root user "runner" with uid
# 1000 and a writable $HOME, plus bash, git and ca-certificates.

[[runner]]
name        = "linux"
executor    = "docker"
labels      = ["linux", %q, "docker"]
concurrency = %d

  [runner.docker]
  default_image = %q
  min_free_gb   = 40
  # A workflow step may be a container: uses: docker://image. A tag is a
  # mutable reference, so the digest it resolved to is always written to the
  # run log. Set this to refuse anything not named by digest instead.
  # require_pinned_images = true
`, intro, p.GOARCH, slots, image)
}

func configSecretsName() string { return "secrets.env" }

func quoteList(in []string) string {
	out := make([]string, len(in))
	for i, s := range in {
		out[i] = strconv.Quote(s)
	}
	return strings.Join(out, ", ")
}
