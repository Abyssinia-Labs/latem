package native

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

// CheckXcode is the other way a Mac stops building without saying why: the
// Xcode its steps use is being replaced, or has been and wants something
// before it works again (A64).
//
// The App Store updates Xcode in place, whenever it likes, on a machine that
// is otherwise working. For some minutes Xcode.app is half there, and every
// tool behind /usr/bin's shims — git, clang, make, xcodebuild — fails with it.
// After the update a new licence may want accepting, which fails the same
// tools until someone does, and new first-launch components may want
// installing, which fails xcodebuild. None of this is a job's fault and no
// retry fixes it, so the runner says it is unavailable, with the command that
// clears it, and takes work again on the next preflight that passes (§20).
// latem runs neither command: both need an administrator, and a licence is
// the operator's to accept (A60).
//
// A probe that does not answer in time is no verdict. By then the bundle's
// files are all there, and what makes xcodebuild slow is usually a build using
// the machine, so refusing would take a working runner dark until the next
// preflight, five minutes off, for being busy (A67).
//
// developerDir is the one a step would use: the runner's DEVELOPER_DIR when
// its [runner.native] env sets one, else xcode-select's. The Command Line
// Tools, or no developer directory at all, is not this check's business;
// doctor reports both.
func CheckXcode(ctx context.Context, r Runner, developerDir string) error {
	if r == nil {
		r = ExecRunner{}
	}
	selected := developerDir == ""
	if selected {
		out, err := r.Run(ctx, []string{"xcode-select", "-p"})
		if err != nil {
			return nil
		}
		developerDir = strings.TrimSpace(string(out))
	}
	app, ok := xcodeApp(developerDir)
	if !ok {
		return nil
	}
	if !exists(filepath.Join(app, "Contents", "Info.plist")) ||
		!exists(filepath.Join(developerDir, "usr", "bin", "xcodebuild")) {
		return codeErr(protocol.ErrExecutor,
			"native executor: %s is missing or half-installed — an App Store update replaces Xcode in place; "+
				"this runner takes work again once it is back", app)
	}

	name := "Xcode"
	if out, err := r.Run(ctx, []string{"plutil", "-extract", "CFBundleShortVersionString", "raw", "-o", "-",
		filepath.Join(app, "Contents", "Info.plist")}); err == nil {
		if v := strings.TrimSpace(string(out)); v != "" {
			name += " " + v
		}
	}
	// sudo resets the environment, so a runner's own DEVELOPER_DIR has to be
	// handed to xcodebuild through env for the fix to reach the right Xcode.
	sudo := "sudo xcodebuild"
	if !selected {
		sudo = "sudo env DEVELOPER_DIR=" + developerDir + " xcodebuild"
	}
	xcodebuild := []string{"env", "DEVELOPER_DIR=" + developerDir, "xcodebuild"}
	for _, c := range []struct{ probe, fix, what string }{
		{"-license check", "-license accept", "its licence accepted"},
		{"-checkFirstLaunchStatus", "-runFirstLaunch", "its first-launch setup"},
	} {
		_, err := r.Run(ctx, append(append([]string(nil), xcodebuild...), strings.Fields(c.probe)...))
		if err == nil || errors.Is(err, context.DeadlineExceeded) {
			continue
		}
		return codeErr(protocol.ErrExecutor,
			"native executor: %s at %s needs %s: `%s %s` on this Mac", name, app, c.what, sudo, c.fix)
	}
	return nil
}

// xcodeApp is the Xcode bundle a developer directory is inside, if it is.
func xcodeApp(developerDir string) (string, bool) {
	i := strings.Index(developerDir, ".app/Contents/Developer")
	if i < 0 {
		return "", false
	}
	return developerDir[:i+len(".app")], true
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
