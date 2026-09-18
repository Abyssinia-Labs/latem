package config

import (
	"strings"
	"testing"
)

// Every case here is a config an operator could plausibly write, paired with
// the phrase the error must contain. The phrase is checked rather than the
// error type because the deliverable is not "it fails" — §8's argument is that
// a broken config must fail *here*, with a message that says what to change,
// instead of failing inside a build where diagnosis is expensive. A test that
// only asserted err != nil would pass for "invalid configuration".
func TestLoad_InvalidConfigs(t *testing.T) {
	tests := []struct {
		name string
		toml string
		want string
	}{
		{
			name: "schema_version missing",
			toml: `
[repos.app]
url = "git@github.com:me/app.git"
allowed_refs = ["main"]

[[runner]]
name = "linux"
executor = "docker"
labels = ["linux"]
`,
			want: "schema_version = 1",
		},
		{
			name: "schema_version from a newer agent",
			toml: minimalValid + "\n",
			// replaced below
			want: "",
		},
		{
			// A knob that cannot work is refused at load rather than accepted
			// and ignored. Before this, `auto_update = true` parsed, defaulted,
			// and was read by nothing: the agent never updated itself and said
			// nothing about why.
			name: "auto_update is refused while self-update does not exist",
			toml: minimalValid + `
[agent]
auto_update = true
`,
			want: "self-update is not implemented",
		},
		{
			name: "unknown key is an error, not a warning",
			toml: minimalValid + `
[agent]
shutdwn_grace = "10m"
`,
			want: `unknown key "agent.shutdwn_grace"`,
		},
		{
			name: "unknown key inside a runner block",
			toml: `
schema_version = 1

[repos.app]
url = "git@github.com:me/app.git"
allowed_refs = ["main"]

[[runner]]
name = "linux"
executor = "docker"
labels = ["linux"]
concurency = 2
`,
			want: "unknown key",
		},
		{
			name: "duplicate runner names",
			toml: `
schema_version = 1

[repos.app]
url = "git@github.com:me/app.git"
allowed_refs = ["main"]

[[runner]]
name = "linux"
executor = "docker"
labels = ["linux"]

[[runner]]
name = "linux"
executor = "docker"
labels = ["linux", "amd64"]
`,
			want: "duplicate name",
		},
		{
			name: "runner name that would escape the cache directory",
			toml: `
schema_version = 1

[repos.app]
url = "git@github.com:me/app.git"
allowed_refs = ["main"]

[[runner]]
name = "../../etc"
executor = "docker"
labels = ["linux"]
`,
			want: "Docker volume name and a cache directory",
		},
		{
			name: "runner with no labels can never match a job",
			toml: `
schema_version = 1

[repos.app]
url = "git@github.com:me/app.git"
allowed_refs = ["main"]

[[runner]]
name = "linux"
executor = "docker"
labels = []
`,
			want: "runs_on is matched against these",
		},
		{
			name: "uppercase label can never match runs_on",
			toml: `
schema_version = 1

[repos.app]
url = "git@github.com:me/app.git"
allowed_refs = ["main"]

[[runner]]
name = "ios"
executor = "tart"
labels = ["macos", "Xcode-16"]

  [runner.tart]
  base_image = "latem-xcode-26"
`,
			want: `label "Xcode-16"`,
		},
		{
			name: "duplicate label",
			toml: `
schema_version = 1

[repos.app]
url = "git@github.com:me/app.git"
allowed_refs = ["main"]

[[runner]]
name = "linux"
executor = "docker"
labels = ["linux", "linux"]
`,
			want: "appears twice",
		},
		{
			name: "no runners at all",
			toml: `
schema_version = 1

[repos.app]
url = "git@github.com:me/app.git"
allowed_refs = ["main"]
`,
			want: "no execution slot",
		},
		{
			name: "no repos at all",
			toml: `
schema_version = 1

[[runner]]
name = "linux"
executor = "docker"
labels = ["linux"]
`,
			want: "reject every run",
		},
		{
			name: "unknown executor",
			toml: `
schema_version = 1

[repos.app]
url = "git@github.com:me/app.git"
allowed_refs = ["main"]

[[runner]]
name = "linux"
executor = "podman"
labels = ["linux"]
`,
			want: `executor is "podman"`,
		},
		{
			name: "missing executor",
			toml: `
schema_version = 1

[repos.app]
url = "git@github.com:me/app.git"
allowed_refs = ["main"]

[[runner]]
name = "linux"
labels = ["linux"]
`,
			want: "executor is required",
		},
		{
			name: "tart concurrency above Apple's licence ceiling",
			toml: `
schema_version = 1

[repos.app]
url = "git@github.com:me/app.git"
allowed_refs = ["main"]

[[runner]]
name = "ios"
executor = "tart"
labels = ["macos", "xcode"]
concurrency = 3

  [runner.tart]
  base_image = "latem-xcode-26"
`,
			want: "Apple's licence",
		},
		{
			name: "native concurrency above one",
			toml: `
schema_version = 1

[repos.app]
url = "git@github.com:me/app.git"
allowed_refs = ["main"]

[[runner]]
name = "host"
executor = "native"
labels = ["macos"]
concurrency = 2
`,
			want: "a native runner must be 1",
		},
		{
			name: "negative concurrency",
			toml: `
schema_version = 1

[repos.app]
url = "git@github.com:me/app.git"
allowed_refs = ["main"]

[[runner]]
name = "linux"
executor = "docker"
labels = ["linux"]
concurrency = -1
`,
			want: "must be at least 1",
		},
		{
			name: "tart block on a docker runner is silently ignored at runtime",
			toml: `
schema_version = 1

[repos.app]
url = "git@github.com:me/app.git"
allowed_refs = ["main"]

[[runner]]
name = "linux"
executor = "docker"
labels = ["linux"]

  [runner.tart]
  base_image = "latem-xcode-26"
`,
			want: "would be ignored",
		},
		{
			name: "tart runner without a base image",
			toml: `
schema_version = 1

[repos.app]
url = "git@github.com:me/app.git"
allowed_refs = ["main"]

[[runner]]
name = "ios"
executor = "tart"
labels = ["macos"]
`,
			want: "tart.base_image is required",
		},
		{
			name: "derived_data outside the declared set",
			toml: `
schema_version = 1

[repos.app]
url = "git@github.com:me/app.git"
allowed_refs = ["main"]

[[runner]]
name = "ios"
executor = "tart"
labels = ["macos"]

  [runner.tart]
  base_image = "latem-xcode-26"
  derived_data = "host"
`,
			want: `tart.derived_data is "host"`,
		},
		{
			name: "negative min_free_gb",
			toml: `
schema_version = 1

[repos.app]
url = "git@github.com:me/app.git"
allowed_refs = ["main"]

[[runner]]
name = "linux"
executor = "docker"
labels = ["linux"]

  [runner.docker]
  min_free_gb = -1
`,
			want: "docker.min_free_gb is -1",
		},
		{
			name: "unparseable docker memory",
			toml: `
schema_version = 1

[repos.app]
url = "git@github.com:me/app.git"
allowed_refs = ["main"]

[[runner]]
name = "linux"
executor = "docker"
labels = ["linux"]

  [runner.docker]
  memory = "eight gigs"
`,
			want: "docker.memory",
		},
		{
			name: "negative docker cpus",
			toml: `
schema_version = 1

[repos.app]
url = "git@github.com:me/app.git"
allowed_refs = ["main"]

[[runner]]
name = "linux"
executor = "docker"
labels = ["linux"]

  [runner.docker]
  cpus = -2.0
`,
			want: "docker.cpus",
		},
		{
			name: "negative tart cpus and memory",
			toml: `
schema_version = 1

[repos.app]
url = "git@github.com:me/app.git"
allowed_refs = ["main"]

[[runner]]
name = "ios"
executor = "tart"
labels = ["macos"]

  [runner.tart]
  base_image = "x"
  cpus = -1
  memory_gb = -4
  boot_timeout = "-3m"
`,
			want: "tart.memory_gb is -4",
		},
		{
			name: "allowed_refs glob that does not compile",
			toml: `
schema_version = 1

[repos.app]
url = "git@github.com:me/app.git"
allowed_refs = ["release/[1-2"]

[[runner]]
name = "linux"
executor = "docker"
labels = ["linux"]
`,
			want: "not a valid glob",
		},
		{
			name: "empty allowed_refs pattern",
			toml: `
schema_version = 1

[repos.app]
url = "git@github.com:me/app.git"
allowed_refs = ["main", ""]

[[runner]]
name = "linux"
executor = "docker"
labels = ["linux"]
`,
			want: "empty pattern",
		},
		{
			name: "repo that can never run anything",
			toml: `
schema_version = 1

[repos.app]
url = "git@github.com:me/app.git"
allowed_refs = []

[[runner]]
name = "linux"
executor = "docker"
labels = ["linux"]
`,
			want: "no ref can ever run",
		},
		{
			name: "repo with no url",
			toml: `
schema_version = 1

[repos.app]
allowed_refs = ["main"]

[[runner]]
name = "linux"
executor = "docker"
labels = ["linux"]
`,
			want: "repos.app: url is required",
		},
		{
			name: "repo key that would escape the mirrors directory",
			toml: `
schema_version = 1

[repos."../evil"]
url = "git@github.com:me/app.git"
allowed_refs = ["main"]

[[runner]]
name = "linux"
executor = "docker"
labels = ["linux"]
`,
			want: "mirrors/<name>.git",
		},
		{
			name: "negative approval_ttl",
			toml: `
schema_version = 1

[repos.app]
url = "git@github.com:me/app.git"
allowed_refs = ["main"]
approval_ttl = "-1h"

[[runner]]
name = "linux"
executor = "docker"
labels = ["linux"]
`,
			want: "approval_ttl",
		},
		{
			name: "remote_logs outside the declared set",
			toml: minimalValid + `
[agent]
remote_logs = "never"
`,
			want: `remote_logs is "never"`,
		},
		{
			name: "negative shutdown_grace",
			toml: minimalValid + `
[agent]
shutdown_grace = "-30m"
`,
			want: "agent.shutdown_grace",
		},
		{
			name: "negative queue_ttl",
			toml: minimalValid + `
[agent]
queue_ttl = "-72h"
`,
			want: "agent.queue_ttl",
		},
		{
			name: "negative log_retention",
			toml: minimalValid + `
[agent]
log_retention = "-14d"
`,
			want: "agent.log_retention",
		},
		{
			name: "unparseable duration",
			toml: minimalValid + `
[agent]
queue_ttl = "soon"
`,
			want: "invalid duration",
		},
		{
			name: "machine name with control characters",
			toml: minimalValid + `
[machine]
name = "mac\tmini"
`,
			want: "machine.name",
		},
		{
			name: "hand-edited machine id",
			toml: minimalValid + `
[machine]
id = "mch 01H/../x"
`,
			want: "machine.id",
		},
		{
			name: "machine label outside the class",
			toml: minimalValid + `
[machine]
labels = ["MacOS"]
`,
			want: "machine.labels",
		},
		{
			name: "remote broker enabled with no url",
			toml: minimalValid + `
[broker.remote]
enabled = true
`,
			want: "broker.remote.url is empty",
		},
		{
			name: "plaintext websocket to a remote host leaks the machine token",
			toml: minimalValid + `
[broker.remote]
enabled = true
url = "ws://api.example.com/agent"
`,
			want: "clear text",
		},
		{
			name: "https instead of wss",
			toml: minimalValid + `
[broker.remote]
url = "https://api.example.com/agent"
`,
			want: "needs wss://",
		},
		{
			name: "url with no scheme",
			toml: minimalValid + `
[broker.remote]
url = "api.example.com/agent"
`,
			want: "has no scheme",
		},
		{
			name: "both brokers disabled",
			toml: minimalValid + `
[broker.local]
enabled = false
`,
			want: "nothing can ever start a run",
		},
		{
			name: "unknown secrets provider",
			toml: minimalValid + `
[secrets]
provider = "vault"
`,
			want: `the only provider is "chain"`,
		},
		{
			name: "secret name that is not an environment variable name",
			toml: minimalValid + `
[secrets.commands]
"match-password" = "op read op://Private/match/password"
`,
			want: "secrets.commands.match-password",
		},
		{
			name: "secret command left empty",
			toml: minimalValid + `
[secrets.commands]
MATCH_PASSWORD = "   "
`,
			want: "command is empty",
		},
		{
			name: "duplicate key",
			toml: `
schema_version = 1
schema_version = 2
`,
			want: "schema_version",
		},
		{
			name: "wrong type for a key",
			toml: `
schema_version = "one"
`,
			want: "schema_version",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			toml, want := tt.toml, tt.want
			if tt.name == "schema_version from a newer agent" {
				toml = strings.Replace(minimalValid, "schema_version = 1", "schema_version = 2", 1)
				want = "written for a newer latem"
			}

			f := newFixture(t, toml)
			_, err := Load(f.Opts)
			if err == nil {
				t.Fatalf("this config loaded successfully; it should not have")
			}
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error does not name the problem.\n  want substring: %s\n  got: %v", want, err)
			}
		})
	}
}

// A schema below the current one gets a different hint from one above it,
// because the action is different: update the file versus upgrade the binary.
// While SchemaVersion is 1 the only way to reach this branch is a negative
// number, which is exactly the typo it needs to survive.
func TestLoad_LowerSchemaVersionHint(t *testing.T) {
	f := newFixture(t, strings.Replace(minimalValid, "schema_version = 1", "schema_version = -1", 1))
	_, err := Load(f.Opts)
	if err == nil {
		t.Fatal("a negative schema_version loaded successfully")
	}
	if !strings.Contains(err.Error(), "predates this release") {
		t.Errorf("hint does not tell the operator to update the file: %v", err)
	}
}

// INV-1 lives or dies here. RUNNER_SPEC §20 puts the check in the parser
// precisely because it is the only place it can be enforced before something
// binds, and §5 says a change that weakens an invariant is rejected regardless
// of its other merits.
func TestValidateMetricsAddr_INV1(t *testing.T) {
	tests := []struct {
		name string
		addr string
		ok   bool
		want string
	}{
		{name: "documented default", addr: "127.0.0.1:9838", ok: true},
		{name: "other loopback v4", addr: "127.0.0.2:9838", ok: true},
		{name: "loopback v6", addr: "[::1]:9838", ok: true},
		{name: "localhost resolves to loopback only", addr: "localhost:9838", ok: true},
		{name: "name resolving to loopback", addr: "loop.example:9838", ok: true},

		{
			name: "bare port binds every interface",
			addr: ":9838",
			want: "binds every interface",
		},
		{
			name: "wildcard v4",
			addr: "0.0.0.0:9838",
			want: "not a loopback address",
		},
		{
			name: "wildcard v6",
			addr: "[::]:9838",
			want: "not a loopback address",
		},
		{
			name: "a real interface address",
			addr: "192.168.1.20:9838",
			want: "not a loopback address",
		},
		{
			name: "public address",
			addr: "203.0.113.5:9838",
			want: "not a loopback address",
		},
		{
			name: "a name that resolves to loopback and to something else",
			addr: "split.example:9838",
			want: "not a loopback address",
		},
		{
			name: "a name that resolves off-machine",
			addr: "far.example:9838",
			want: "not a loopback address",
		},
		{
			name: "a name that does not resolve",
			addr: "nowhere.example:9838",
			want: "cannot resolve",
		},
		{
			name: "a name that resolves to nothing",
			addr: "void.example:9838",
			want: "resolves to nothing",
		},
		{
			name: "no port",
			addr: "127.0.0.1",
			want: `is not "host:port"`,
		},
		{
			name: "named port",
			addr: "127.0.0.1:metrics",
			want: "port must be a number",
		},
		{
			name: "port zero picks a random one nothing can find",
			addr: "127.0.0.1:0",
			want: "port must be a number",
		},
		{
			name: "port out of range",
			addr: "127.0.0.1:70000",
			want: "port must be a number",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateMetricsAddr(tt.addr, testLookupIP)
			if tt.ok {
				if err != nil {
					t.Fatalf("rejected a loopback address: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("accepted %q, which INV-1 forbids", tt.addr)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("want substring %q, got: %v", tt.want, err)
			}
		})
	}
}

// A non-loopback metrics_addr must be fatal at load, not merely reported by a
// helper nobody calls.
func TestLoad_RejectsNonLoopbackMetricsAddr(t *testing.T) {
	f := newFixture(t, minimalValid+`
[agent]
metrics_addr = "0.0.0.0:9838"
`)
	_, err := Load(f.Opts)
	if err == nil {
		t.Fatal("a config binding metrics to every interface loaded successfully (INV-1)")
	}
	if !strings.Contains(err.Error(), "loopback") {
		t.Errorf("error does not mention loopback: %v", err)
	}
}

// §8 asks for every problem at once. Someone repairing a config should not
// have to restart the agent once per mistake.
func TestLoad_ReportsEveryProblemAtOnce(t *testing.T) {
	f := newFixture(t, `
schema_version = 1

[agent]
remote_logs  = "sometimes"
metrics_addr = "0.0.0.0:9838"
queue_ttl    = "-1h"

[repos.app]
url = ""
allowed_refs = ["release/[1-2"]

[[runner]]
name = "ios"
executor = "tart"
labels = ["macos"]
concurrency = 4
`)

	_, err := Load(f.Opts)
	if err == nil {
		t.Fatal("expected a failure")
	}
	problems := Problems(err)
	if len(problems) < 6 {
		t.Fatalf("got %d problems, want at least 6 — the point is to see them all in one pass:\n%v", len(problems), err)
	}

	msg := err.Error()
	for _, want := range []string{
		"remote_logs", "loopback", "queue_ttl",
		"url is required", "not a valid glob", "Apple's licence", "base_image",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("the combined report omits %q:\n%v", want, msg)
		}
	}
}

// Parse validates content with no file behind it, which is what `latem doctor`
// needs to check a candidate config.
func TestParse_SkipsFilesystemChecks(t *testing.T) {
	opts := Options{
		Home:     "/nonexistent-home",
		Getenv:   func(string) string { return "" },
		LookupIP: testLookupIP,
	}
	cfg, err := Parse([]byte(minimalValid), opts)
	if err != nil {
		t.Fatalf("Parse should not stat anything: %v", err)
	}
	if cfg.Paths.Dir != "/nonexistent-home/"+DirName {
		t.Errorf("Paths.Dir = %q", cfg.Paths.Dir)
	}

	if _, err := Parse([]byte("schema_version = 1\n"), opts); err == nil {
		t.Error("Parse accepted a config with no repos and no runners")
	}
}
