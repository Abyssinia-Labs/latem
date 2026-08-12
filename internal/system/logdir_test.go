package system

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The service definition must not mix two config trees.
//
// Found by rendering a plist for a relocated --config-dir: it carried
// LATEM_CONFIG_DIR=<relocated> while pointing StandardOutPath at
// ~/.latem/logs. §8 says --config-dir relocates the lock, socket, sqlite, work
// dir and caches *together*, and this was the one place that did not follow.
func TestServiceLogDirFollowsSpec(t *testing.T) {
	sys := New(Options{GOOS: "darwin", Home: "/Users/tester", User: "tester"})

	relocated := ServiceSpec{
		ExecPath:  "/opt/latem/latem",
		Home:      "/Users/tester",
		User:      "tester",
		ConfigDir: "/srv/latem-alt",
		LogDir:    "/srv/latem-alt/logs",
	}
	_, content, err := sys.Render(relocated)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(content, "<string>/srv/latem-alt/logs/agent.out.log</string>") {
		t.Error("stdout does not follow the relocated config dir")
	}
	if strings.Contains(content, "/Users/tester/.latem/logs") {
		t.Error("plist still references the default log dir alongside a relocated config dir")
	}

	// With no LogDir the default is still $HOME/.latem/logs, so a plain
	// install on a stock machine is unchanged.
	_, content, err = sys.Render(ServiceSpec{
		ExecPath: "/opt/latem/latem", Home: "/Users/tester", User: "tester",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(content, "<string>/Users/tester/.latem/logs/agent.out.log</string>") {
		t.Error("default log dir regressed")
	}
}

// launchd does not create the parent of StandardOutPath. If it is missing the
// job fails to spawn, and the diagnostic goes to the file it could not open —
// so `service install` on a machine that has never run `agent` produced a
// service that silently never started. That is every machine at first install.
func TestWriteServiceCreatesLogDir(t *testing.T) {
	if _, err := os.Stat("/"); err != nil {
		t.Skip()
	}
	root := t.TempDir()
	home := filepath.Join(root, "home")
	logs := filepath.Join(root, "cfg", "logs")

	sys := New(Options{GOOS: "darwin", Home: home, User: "tester"})
	if _, err := sys.WriteService(ServiceSpec{
		ExecPath:  filepath.Join(root, "latem"),
		Home:      home,
		User:      "tester",
		ConfigDir: filepath.Join(root, "cfg"),
		LogDir:    logs,
	}); err != nil {
		t.Fatalf("WriteService: %v", err)
	}

	info, err := os.Stat(logs)
	if err != nil {
		t.Fatalf("log directory was not created: %v", err)
	}
	if !info.IsDir() {
		t.Fatal("log path exists but is not a directory")
	}
}
