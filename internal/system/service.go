package system

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/template"
	"time"
)

// Service naming. These are interface: an operator types the unit name into
// `systemctl` and the label into `launchctl`, and changing either breaks every
// runbook written against it.
const (
	// ServiceLabel is the launchd label and the reverse-DNS identity of the
	// LaunchAgent.
	ServiceLabel = "com.latem.agent"
	// SystemdUnitName is the systemd *user* unit's filename.
	SystemdUnitName = "latem.service"
)

// StopMargin is how much longer the service manager waits than the agent's own
// `shutdown_grace`, covering §6's SIGTERM-then-30s-then-SIGKILL escalation on
// each run's process group.
const StopMargin = 30 * time.Second

// DefaultStopTimeout is RUNNER_SPEC §23's 1830 seconds: `shutdown_grace`
// (30 minutes, because an iOS build is long) plus [StopMargin].
//
// Getting this wrong in the direction of "too short" is the expensive one. The
// service manager kills the agent partway through its own graceful shutdown,
// which is precisely when it is tearing down containers and Tart VMs, and a
// leaked Tart VM carrying an Xcode image costs 90 GB.
const DefaultStopTimeout = 30*time.Minute + StopMargin

// StopTimeoutFor is the service-manager timeout matching an agent's configured
// shutdown grace.
func StopTimeoutFor(shutdownGrace time.Duration) time.Duration {
	if shutdownGrace <= 0 {
		return DefaultStopTimeout
	}
	return shutdownGrace + StopMargin
}

// DefaultRestartDelay is §23's RestartSec=5, mirrored as launchd's
// ThrottleInterval.
const DefaultRestartDelay = 5 * time.Second

// DefaultLaunchdPath is the PATH a LaunchAgent gets.
//
// launchd hands a process a minimal PATH that contains neither Homebrew
// prefix, so an agent started at login cannot find `tart`, `fastlane`, or a
// Homebrew `git` even though the operator's own shell finds all three. The
// symptom is a machine that works when started by hand and fails after a
// reboot, which is the hardest kind of fault to attribute.
const DefaultLaunchdPath = "/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin"

// ServiceSpec describes the agent service to be generated.
type ServiceSpec struct {
	// ExecPath is the absolute path of the latem binary.
	ExecPath string
	// Args are the arguments after the binary. Empty means ["agent"].
	Args []string
	// Home is the user's home directory; the unit and plist are written
	// beneath it. Both service managers run this as an unprivileged user,
	// which is §23's requirement, not a convenience.
	Home string
	// User is the account the service belongs to, for `loginctl
	// enable-linger`.
	User string
	// ConfigDir is passed as LATEM_CONFIG_DIR when non-empty, so a second
	// agent installed as a service keeps its lock, socket, database, work
	// directory, and caches relocated together (§8).
	ConfigDir string
	// LogDir receives the LaunchAgent's stdout and stderr. launchd discards
	// both by default, which turns "the agent will not start" into a fault
	// with no evidence. Empty means <Home>/.latem/logs.
	LogDir string
	// StopTimeout is how long the service manager waits after SIGTERM. Zero
	// uses [DefaultStopTimeout].
	StopTimeout time.Duration
	// RestartDelay is the pause before a restart. Zero uses
	// [DefaultRestartDelay].
	RestartDelay time.Duration
	// Path is the PATH the service runs with. Empty uses
	// [DefaultLaunchdPath] on macOS and leaves systemd's default on Linux,
	// where a user unit already inherits a usable one.
	Path string
}

func (s ServiceSpec) normalize() ServiceSpec {
	if len(s.Args) == 0 {
		s.Args = []string{"agent"}
	}
	if s.StopTimeout <= 0 {
		s.StopTimeout = DefaultStopTimeout
	}
	if s.RestartDelay <= 0 {
		s.RestartDelay = DefaultRestartDelay
	}
	if s.LogDir == "" && s.Home != "" {
		s.LogDir = filepath.Join(s.Home, ".latem", "logs")
	}
	return s
}

// Validate reports why the spec cannot produce a working service.
func (s ServiceSpec) Validate() error {
	if s.ExecPath == "" {
		return fmt.Errorf("system: service spec needs the path to the latem binary")
	}
	if !filepath.IsAbs(s.ExecPath) {
		return fmt.Errorf("system: service exec path %q must be absolute — a service manager has no working directory to resolve it against", s.ExecPath)
	}
	if s.Home == "" {
		return fmt.Errorf("system: service spec needs a home directory")
	}
	return nil
}

// SystemdUnitPath is where the user unit belongs (§23).
func SystemdUnitPath(home string) string {
	return filepath.Join(home, ".config", "systemd", "user", SystemdUnitName)
}

// LaunchAgentPath is where the plist belongs (§23).
//
// ~/Library/LaunchAgents, never /Library/LaunchDaemons. §23 states the reason
// first among its three: `codesign` needs a real user session with keychain
// access, and a daemon context fails with errors that point nowhere useful.
func LaunchAgentPath(home string) string {
	return filepath.Join(home, "Library", "LaunchAgents", ServiceLabel+".plist")
}

var systemdTemplate = template.Must(template.New("unit").Parse(
	`[Unit]
Description=Latem runner agent
Documentation=https://github.com/Abyssinia-Labs/latem
After=network-online.target docker.service
Wants=network-online.target

[Service]
Type=simple
ExecStart={{.Exec}}
Restart=always
RestartSec={{.RestartSec}}
KillSignal=SIGTERM
# shutdown_grace (30m) plus a margin for the SIGTERM/SIGKILL escalation on
# each run's process group. Too short kills the agent while it is tearing down
# containers and VMs, and a leaked Tart VM costs 90 GB.
TimeoutStopSec={{.StopSec}}
{{- range .Environment}}
Environment={{.}}
{{- end}}

[Install]
WantedBy=default.target
`))

// SystemdUnit renders the systemd user unit of RUNNER_SPEC §23.
//
// A path under the home directory is rendered with systemd's %h specifier, the
// way §23 writes it, so a unit generated here and a unit copied from the spec
// are the same file.
func (s ServiceSpec) SystemdUnit() (string, error) {
	s = s.normalize()
	if err := s.Validate(); err != nil {
		return "", err
	}
	exec := shellQuote(homeSpecifier(s.ExecPath, s.Home, "%h"))
	for _, a := range s.Args {
		exec += " " + shellQuote(a)
	}
	env := []string{}
	if s.ConfigDir != "" {
		env = append(env, fmt.Sprintf("LATEM_CONFIG_DIR=%s", homeSpecifier(s.ConfigDir, s.Home, "%h")))
	}
	if s.Path != "" {
		env = append(env, "PATH="+s.Path)
	}
	var buf bytes.Buffer
	err := systemdTemplate.Execute(&buf, struct {
		Exec        string
		RestartSec  int
		StopSec     int
		Environment []string
	}{
		Exec:        exec,
		RestartSec:  int(s.RestartDelay.Seconds()),
		StopSec:     int(s.StopTimeout.Seconds()),
		Environment: env,
	})
	return buf.String(), err
}

var plistTemplate = template.Must(template.New("plist").Parse(
	`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>{{.Label}}</string>
	<key>ProgramArguments</key>
	<array>
{{- range .Argv}}
		<string>{{.}}</string>
{{- end}}
	</array>
	<key>RunAtLoad</key>
	<true/>
	<key>KeepAlive</key>
	<true/>
	<key>ThrottleInterval</key>
	<integer>{{.ThrottleInterval}}</integer>
	<key>ExitTimeOut</key>
	<integer>{{.ExitTimeOut}}</integer>
	<!-- Standard, not Background: launchd throttles a Background job's CPU and
	     disk I/O, and an Xcode build under that policy is slower for reasons
	     that never appear in any log. -->
	<key>ProcessType</key>
	<string>Standard</string>
	<key>WorkingDirectory</key>
	<string>{{.WorkingDirectory}}</string>
	<key>StandardOutPath</key>
	<string>{{.StdoutPath}}</string>
	<key>StandardErrorPath</key>
	<string>{{.StderrPath}}</string>
	<key>EnvironmentVariables</key>
	<dict>
{{- range .Environment}}
		<key>{{.Key}}</key>
		<string>{{.Value}}</string>
{{- end}}
	</dict>
</dict>
</plist>
`))

type plistEnv struct{ Key, Value string }

// LaunchAgentPlist renders the macOS LaunchAgent of RUNNER_SPEC §23.
func (s ServiceSpec) LaunchAgentPlist() (string, error) {
	s = s.normalize()
	if err := s.Validate(); err != nil {
		return "", err
	}
	argv := make([]string, 0, len(s.Args)+1)
	argv = append(argv, xmlEscape(s.ExecPath))
	for _, a := range s.Args {
		argv = append(argv, xmlEscape(a))
	}
	path := s.Path
	if path == "" {
		path = DefaultLaunchdPath
	}
	env := []plistEnv{{Key: "PATH", Value: xmlEscape(path)}}
	if s.ConfigDir != "" {
		env = append(env, plistEnv{Key: "LATEM_CONFIG_DIR", Value: xmlEscape(s.ConfigDir)})
	}
	var buf bytes.Buffer
	err := plistTemplate.Execute(&buf, struct {
		Label            string
		Argv             []string
		ThrottleInterval int
		ExitTimeOut      int
		WorkingDirectory string
		StdoutPath       string
		StderrPath       string
		Environment      []plistEnv
	}{
		Label:            ServiceLabel,
		Argv:             argv,
		ThrottleInterval: int(s.RestartDelay.Seconds()),
		ExitTimeOut:      int(s.StopTimeout.Seconds()),
		WorkingDirectory: xmlEscape(s.Home),
		StdoutPath:       xmlEscape(filepath.Join(s.LogDir, "agent.out.log")),
		StderrPath:       xmlEscape(filepath.Join(s.LogDir, "agent.err.log")),
		Environment:      env,
	})
	return buf.String(), err
}

// Render produces the service definition for this platform, and where it goes.
func (s *System) Render(spec ServiceSpec) (path, content string, err error) {
	if spec.Home == "" {
		spec.Home = s.home
	}
	if spec.User == "" {
		spec.User = s.username
	}
	switch {
	case s.IsMacOS():
		content, err = spec.LaunchAgentPlist()
		return LaunchAgentPath(spec.Home), content, err
	case s.IsLinux():
		content, err = spec.SystemdUnit()
		return SystemdUnitPath(spec.Home), content, err
	default:
		return "", "", fmt.Errorf("system: no service integration for %s — run `latem agent` under your own supervisor", s.goos)
	}
}

// homeSpecifier rewrites a path under home to use a service manager's home
// specifier, leaving anything else alone.
func homeSpecifier(path, home, specifier string) string {
	if home == "" || path == home {
		return path
	}
	prefix := home
	if !strings.HasSuffix(prefix, string(filepath.Separator)) {
		prefix += string(filepath.Separator)
	}
	if rest, ok := strings.CutPrefix(path, prefix); ok {
		return specifier + "/" + filepath.ToSlash(rest)
	}
	return path
}

// shellQuote quotes an argument for a systemd ExecStart line.
//
// systemd splits ExecStart on whitespace and understands double quotes, so a
// binary under /home/some user/ needs them. It is a small thing that produces
// a service which fails at boot with "No such file or directory" naming half a
// path.
//
// Only whitespace and quoting characters trigger it. systemd's own specifiers
// — the %h that §23 writes into ExecStart — expand inside quotes too, but
// leaving them bare keeps a generated unit textually identical to the one in
// the spec, which is worth more than uniformity here.
func shellQuote(s string) string {
	if s != "" && !strings.ContainsAny(s, " \t\n\"'\\") {
		return s
	}
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`)
	return `"` + r.Replace(s) + `"`
}

func xmlEscape(s string) string {
	var buf bytes.Buffer
	// EscapeText only fails when the writer does, and bytes.Buffer does not.
	_ = xml.EscapeText(&buf, []byte(s))
	return buf.String()
}

// WriteService writes the rendered unit or plist, creating its directory.
//
// Mode 0644, deliberately: a systemd unit and a LaunchAgent are read by a
// service manager that may not be this user, they contain no secrets, and a
// 0600 unit is a service that silently never starts.
func (s *System) WriteService(spec ServiceSpec) (string, error) {
	path, content, err := s.Render(spec)
	if err != nil {
		return "", err
	}
	// 0750, not 0755: the only reader is a service manager running as this
	// same user, so nothing outside the account needs to traverse it. An
	// already-existing ~/Library/LaunchAgents keeps whatever mode it has.
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return "", fmt.Errorf("system: creating %s: %w", filepath.Dir(path), err)
	}
	// The log directory too, and this is not housekeeping. launchd does not
	// create the parent of StandardOutPath: if it is missing the job fails to
	// spawn, and the diagnostic for that failure is written to the file it
	// could not open. On a machine that has never run the agent — which is
	// every machine at first install, and the Mac this targets — the
	// directory does not exist yet, so `service install` before a first `agent`
	// run produced a service that silently never started.
	if dir := s.logDirFor(spec); dir != "" {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return "", fmt.Errorf("system: creating %s: %w", dir, err)
		}
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil { //nolint:gosec // a service definition must be readable by the service manager
		return "", fmt.Errorf("system: writing %s: %w", path, err)
	}
	return path, nil
}

// logDirFor reports the directory the service will write stdout and stderr to,
// after the same defaulting Render applies. Empty when the platform's service
// definition does not redirect output (systemd journals it instead).
func (s *System) logDirFor(spec ServiceSpec) string {
	spec = spec.normalize()
	if !s.IsMacOS() {
		return ""
	}
	return spec.LogDir
}
