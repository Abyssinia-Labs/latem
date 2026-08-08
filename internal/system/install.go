package system

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
)

// Step is one action taken, or that would be taken, while installing or
// removing the service.
//
// Every step carries its argv so `--dry-run` and the real thing print the same
// list. A remediation an operator is shown and a remediation the tool performs
// drifting apart is how a runbook stops being true.
type Step struct {
	// Desc says what the step is for, in the operator's terms.
	Desc string
	// Argv is the command, or nil for a step that only touches files.
	Argv []string
	// Path is the file written or removed, when the step is a file operation.
	Path string
	// Output is the command's combined output, captured for display.
	Output string
	// Err is why the step failed. A failed step does not stop the sequence;
	// see [ServiceReport].
	Err error
	// Optional marks a step whose failure is expected and harmless — tearing
	// down a service that was not loaded, most often.
	Optional bool
}

// String renders the step as the command an operator could type.
func (s Step) String() string {
	if len(s.Argv) == 0 {
		return s.Desc
	}
	return strings.Join(s.Argv, " ")
}

// ServiceReport is everything an install or uninstall did.
//
// The steps are the deliverable as much as the outcome is. Service management
// is the part of installation most likely to half-succeed — the unit written
// but linger not enabled, the plist bootstrapped but the old job still loaded
// — and an operator who can see which of six commands failed can finish the
// job by hand.
type ServiceReport struct {
	// Path is the unit or plist that was written or removed.
	Path string
	// Steps are in execution order.
	Steps []Step
	// Notes are things the operator should know that are not failures.
	Notes []string
}

// Err joins the failures of every non-optional step.
func (r ServiceReport) Err() error {
	var errs []error
	for _, s := range r.Steps {
		if s.Err != nil && !s.Optional {
			errs = append(errs, fmt.Errorf("%s: %w", s.String(), s.Err))
		}
	}
	return errors.Join(errs...)
}

// InstallOptions bounds what installation is allowed to do.
type InstallOptions struct {
	// DryRun renders and reports without writing or running anything.
	DryRun bool
	// SkipLinger omits `loginctl enable-linger`. It exists for a container or
	// a CI runner where linger is meaningless and the command fails noisily.
	SkipLinger bool
	// SkipStart installs the definition without starting the agent.
	SkipStart bool
}

// InstallService writes the service definition and registers it.
//
// It never requires root. On Linux this is a systemd *user* unit plus
// `loginctl enable-linger`, and on macOS a LaunchAgent under ~/Library — both
// because §23's argument is that the agent needs a user session: `codesign`
// reaches the login keychain from a LaunchAgent and from nothing else.
func (s *System) InstallService(ctx context.Context, spec ServiceSpec, opts InstallOptions) (ServiceReport, error) {
	if spec.Home == "" {
		spec.Home = s.home
	}
	if spec.User == "" {
		spec.User = s.username
	}
	path, content, err := s.Render(spec)
	if err != nil {
		return ServiceReport{}, err
	}
	rep := ServiceReport{Path: path}

	if opts.DryRun {
		// The rendered definition is deliberately not repeated here. It is
		// forty lines of XML, it would bury the six commands that are the
		// point of a dry run, and `latem service install --print` exists to
		// show exactly that.
		_ = content
		rep.Steps = append(rep.Steps, Step{Desc: "write service definition", Path: path})
	} else {
		written, werr := s.WriteService(spec)
		rep.Steps = append(rep.Steps, Step{Desc: "write service definition", Path: written, Err: werr})
		if werr != nil {
			return rep, rep.Err()
		}
	}

	for _, st := range s.registerSteps(path, spec, opts) {
		if opts.DryRun {
			rep.Steps = append(rep.Steps, st)
			continue
		}
		rep.Steps = append(rep.Steps, s.runStep(ctx, st))
	}
	if s.IsMacOS() {
		rep.Notes = append(rep.Notes,
			"a LaunchAgent only runs while this user has a session: check `latem doctor` for auto-login and FileVault")
	}
	return rep, rep.Err()
}

// UninstallService stops the service and removes its definition.
func (s *System) UninstallService(ctx context.Context, opts InstallOptions) (ServiceReport, error) {
	var path string
	switch {
	case s.IsMacOS():
		path = LaunchAgentPath(s.home)
	case s.IsLinux():
		path = SystemdUnitPath(s.home)
	default:
		return ServiceReport{}, fmt.Errorf("system: no service integration for %s", s.goos)
	}
	rep := ServiceReport{Path: path}

	for _, st := range s.deregisterSteps(path) {
		if opts.DryRun {
			rep.Steps = append(rep.Steps, st)
			continue
		}
		rep.Steps = append(rep.Steps, s.runStep(ctx, st))
	}

	rm := Step{Desc: "remove service definition", Path: path}
	if !opts.DryRun {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			rm.Err = err
		}
	}
	rep.Steps = append(rep.Steps, rm)
	return rep, rep.Err()
}

func (s *System) registerSteps(path string, spec ServiceSpec, opts InstallOptions) []Step {
	if s.IsMacOS() {
		domain := s.guiDomain()
		steps := []Step{
			// Bootout first, and tolerate its failure: launchctl refuses to
			// bootstrap a label that is already loaded, so a reinstall on a
			// running machine fails at the second command otherwise.
			{Desc: "unload any previous agent", Argv: []string{"launchctl", "bootout", domain + "/" + ServiceLabel}, Optional: true},
			{Desc: "load the LaunchAgent", Argv: []string{"launchctl", "bootstrap", domain, path}},
			{Desc: "enable the LaunchAgent", Argv: []string{"launchctl", "enable", domain + "/" + ServiceLabel}, Optional: true},
		}
		if !opts.SkipStart {
			steps = append(steps, Step{Desc: "start the agent", Argv: []string{"launchctl", "kickstart", "-k", domain + "/" + ServiceLabel}})
		}
		return steps
	}

	steps := []Step{
		{Desc: "reload systemd user units", Argv: []string{"systemctl", "--user", "daemon-reload"}},
	}
	if !opts.SkipLinger {
		// Without linger the unit stops the moment the operator's session
		// ends, which on a headless build machine is seconds after the
		// install. It looks healthy right up until they disconnect (§23).
		steps = append(steps, Step{Desc: "keep the service running after logout", Argv: s.EnableLingerArgv(spec.User)})
	}
	enable := []string{"systemctl", "--user", "enable"}
	if !opts.SkipStart {
		enable = append(enable, "--now")
	}
	steps = append(steps, Step{Desc: "enable the service at boot", Argv: append(enable, SystemdUnitName)})
	return steps
}

func (s *System) deregisterSteps(path string) []Step {
	if s.IsMacOS() {
		domain := s.guiDomain()
		return []Step{
			{Desc: "stop and unload the LaunchAgent", Argv: []string{"launchctl", "bootout", domain + "/" + ServiceLabel}, Optional: true},
		}
	}
	return []Step{
		{Desc: "stop the service", Argv: []string{"systemctl", "--user", "disable", "--now", SystemdUnitName}, Optional: true},
		{Desc: "reload systemd user units", Argv: []string{"systemctl", "--user", "daemon-reload"}, Optional: true},
		{Desc: "note: linger is left enabled", Argv: nil, Path: path, Optional: true},
	}
}

func (s *System) guiDomain() string {
	return fmt.Sprintf("gui/%d", s.getuid())
}

func (s *System) runStep(ctx context.Context, st Step) Step {
	if len(st.Argv) == 0 {
		return st
	}
	out, err := s.Exec(ctx, st.Argv[0], st.Argv[1:]...)
	st.Output = strings.TrimSpace(string(out))
	st.Err = err
	return st
}

// ServiceState is what a service manager says about the agent service.
type ServiceState struct {
	// Installed reports whether the unit or plist is on disk.
	Installed bool
	// Path is where it is, or would be.
	Path string
	// Loaded reports whether the service manager knows about it.
	Loaded bool
	// Running reports whether the agent process is up.
	Running bool
	// Detail is the service manager's own words, for display.
	Detail string
}

// ServiceStatus reports on the installed service.
func (s *System) ServiceStatus(ctx context.Context) (ServiceState, error) {
	st := ServiceState{}
	switch {
	case s.IsMacOS():
		st.Path = LaunchAgentPath(s.home)
	case s.IsLinux():
		st.Path = SystemdUnitPath(s.home)
	default:
		return st, fmt.Errorf("system: no service integration for %s", s.goos)
	}
	if _, err := os.Stat(st.Path); err == nil {
		st.Installed = true
	}

	if s.IsMacOS() {
		out, err := s.Exec(ctx, "launchctl", "print", s.guiDomain()+"/"+ServiceLabel)
		if err != nil {
			st.Detail = firstLine(out)
			return st, nil
		}
		st.Loaded = true
		st.Detail = firstLine(out)
		text := string(out)
		// launchctl prints "state = running" for a live job and "state = not
		// running" for one that is loaded and stopped. The negative form
		// contains the positive, so order matters.
		st.Running = strings.Contains(text, "state = running")
		return st, nil
	}

	out, _ := s.Exec(ctx, "systemctl", "--user", "is-active", SystemdUnitName)
	state := firstLine(out)
	st.Detail = state
	st.Loaded = state != "" && state != "inactive" || st.Installed
	st.Running = state == "active"
	return st, nil
}
