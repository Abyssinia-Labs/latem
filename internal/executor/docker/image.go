package docker

import (
	"context"
	"strings"

	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

// imageProbeScript reports the base image contract's facts, one per line.
//
// It runs under /bin/sh, not bash, because whether bash exists is one of the
// things being tested. It reads /etc/passwd with awk rather than calling
// `getent`, which busybox does not provide, or `id -nu 1000`, whose operand
// handling differs between coreutils and busybox — the check has to work on
// the minimal images operators actually build from.
//
// It is a compile-time constant handed to the shell as a single argv element,
// so nothing here is interpolated (INV-3).
const imageProbeScript = `
echo "passwd_name=$(awk -F: '$3 == 1000 { print $1; exit }' /etc/passwd 2>/dev/null)"
echo "group_name=$(awk -F: '$3 == 1000 { print $1; exit }' /etc/group 2>/dev/null)"
echo "home=${HOME:-}"
if [ -n "${HOME:-}" ] && [ -d "${HOME}" ]; then echo "home_exists=1"; else echo "home_exists=0"; fi
if [ -n "${HOME:-}" ] && touch "${HOME}/.latem-doctor-probe" 2>/dev/null; then
  echo "home_writable=1"
  rm -f "${HOME}/.latem-doctor-probe"
else
  echo "home_writable=0"
fi
for t in bash git; do
  if command -v "$t" >/dev/null 2>&1; then echo "tool_$t=1"; else echo "tool_$t=0"; fi
done
if [ -f /etc/ssl/certs/ca-certificates.crt ] || [ -f /etc/pki/tls/certs/ca-bundle.crt ]; then
  echo "ca=1"
else
  echo "ca=0"
fi
`

// ImageCheck is one requirement from the base image contract (§13.1).
type ImageCheck struct {
	// Name is stable; `latem doctor` prints it and an operator greps for it.
	Name string
	OK   bool
	// Detail says what was found and, when it failed, what to do about it.
	Detail string
}

// ImageReport is the result of [Executor.CheckImage].
type ImageReport struct {
	Image  string
	Checks []ImageCheck
}

// OK reports whether every check passed.
func (r ImageReport) OK() bool {
	for _, c := range r.Checks {
		if !c.OK {
			return false
		}
	}
	return true
}

// Failures lists the checks that did not pass, in declaration order.
func (r ImageReport) Failures() []ImageCheck {
	var out []ImageCheck
	for _, c := range r.Checks {
		if !c.OK {
			out = append(out, c)
		}
	}
	return out
}

// Err renders the failures as a single E_EXECUTOR error, or nil.
func (r ImageReport) Err() error {
	failures := r.Failures()
	if len(failures) == 0 {
		return nil
	}
	parts := make([]string, 0, len(failures))
	for _, c := range failures {
		parts = append(parts, c.Name+": "+c.Detail)
	}
	return codeErr(protocol.ErrExecutor, "image %s does not satisfy the base image contract: %s",
		r.Image, strings.Join(parts, "; "))
}

// CheckImage runs the image once and reports whether it satisfies the base
// image contract (§13.1).
//
// This is what `latem doctor` (WS-12) calls, and the reason it exists as a
// separate check rather than as validation inside Prepare is the failure mode
// it prevents. An image with no uid 1000 in /etc/passwd leaves $HOME unset or
// pointing somewhere unwritable, and the resulting failures surface forty
// minutes into a build as npm EACCES, a Gradle daemon that will not start, or
// CocoaPods failing to write its cache — errors that look like tooling bugs
// and send an operator to the wrong repository entirely.
//
// The probe runs as 1000:1000 deliberately: that is the uid a run will use, so
// $HOME is resolved the same way the daemon will resolve it for a real step.
func (e *Executor) CheckImage(ctx context.Context, ref string) (ImageReport, error) {
	res, err := e.runProbe(ctx, probeSpec{
		image: ref,
		user:  "1000:1000",
		cmd:   []string{"/bin/sh", "-c", imageProbeScript},
	})
	if err != nil {
		return ImageReport{Image: ref}, wrapErr(protocol.ErrExecutor, err,
			"probe image %q — does it have /bin/sh?", ref)
	}
	return buildImageReport(ref, parseProbeOutput(res.output)), nil
}

// parseProbeOutput turns the probe's key=value lines into a map, ignoring
// anything else the image's shell decided to print.
func parseProbeOutput(out string) map[string]string {
	fields := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(strings.TrimSuffix(line, "\r")), "=")
		if !ok || k == "" {
			continue
		}
		fields[k] = v
	}
	return fields
}

// buildImageReport turns the probe's facts into the contract's checks.
//
// Split from [Executor.CheckImage] so the mapping from facts to verdicts —
// which is where the judgement is — is testable without a daemon.
func buildImageReport(ref string, f map[string]string) ImageReport {
	r := ImageReport{Image: ref}

	user := f["passwd_name"]
	r.Checks = append(r.Checks, ImageCheck{
		Name:   "user-1000",
		OK:     user == "runner",
		Detail: userDetail(user),
	})

	group := f["group_name"]
	r.Checks = append(r.Checks, ImageCheck{
		Name:   "group-1000",
		OK:     group != "",
		Detail: groupDetail(group),
	})

	home := f["home"]
	homeExists := home != "" && f["home_exists"] == "1"
	r.Checks = append(r.Checks, ImageCheck{
		Name: "home-exists",
		OK:   homeExists,
		Detail: pick(homeExists,
			"$HOME is "+home,
			"$HOME is "+quoteOrUnset(home)+" and does not exist; "+
				"a uid with no /etc/passwd entry gets no usable home, and npm, Gradle and CocoaPods all fail on it"),
	})

	homeWritable := f["home_writable"] == "1"
	r.Checks = append(r.Checks, ImageCheck{
		Name: "home-writable",
		OK:   homeWritable,
		Detail: pick(homeWritable,
			"$HOME is writable by uid 1000",
			"$HOME is not writable by uid 1000; chown it to 1000:1000 in the image"),
	})

	for _, tool := range []string{"bash", "git"} {
		present := f["tool_"+tool] == "1"
		r.Checks = append(r.Checks, ImageCheck{
			Name: "tool-" + tool,
			OK:   present,
			Detail: pick(present,
				tool+" is on PATH",
				tool+" is missing; the agent does not shell out to it, but effectively every job does"),
		})
	}

	ca := f["ca"] == "1"
	r.Checks = append(r.Checks, ImageCheck{
		Name: "ca-certificates",
		OK:   ca,
		Detail: pick(ca,
			"a CA bundle is installed",
			"no CA bundle found; every https fetch in a job will fail with a certificate error"),
	})

	return r
}

func userDetail(user string) string {
	switch user {
	case "runner":
		return "uid 1000 is runner"
	case "":
		return "no /etc/passwd entry for uid 1000; add one named runner with gid 1000"
	default:
		return "uid 1000 is " + user + ", not runner"
	}
}

func groupDetail(group string) string {
	if group == "" {
		return "no /etc/group entry for gid 1000; add one"
	}
	return "gid 1000 is " + group
}

func pick(ok bool, yes, no string) string {
	if ok {
		return yes
	}
	return no
}

func quoteOrUnset(s string) string {
	if s == "" {
		return "unset"
	}
	return `"` + s + `"`
}
