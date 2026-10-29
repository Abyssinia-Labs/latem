package main

import (
	"strings"
	"testing"
)

func TestVars_SetListRm(t *testing.T) {
	dir := configDirWith(t, baseConfig)
	reloads := noAgentRunning(t)

	steps := [][]string{
		{"vars", "set", "--repo", "app", "CLOUDFLARE_ACCOUNT_ID", "abc123"},
		{"vars", "set", "--repo", "me/app", "STAGING_READY", "true"},
		{"vars", "set", "--repo", "app", "--env", "production", "URL", `https://example.com/"quoted"`},
		{"vars", "set", "--repo", "app", "CLOUDFLARE_ACCOUNT_ID", "def456"},
	}
	for _, args := range steps {
		if out, err := latemIn(t, dir, "", args...); err != nil {
			t.Fatalf("%v: %v\n%s", args, err, out)
		}
	}
	if *reloads != len(steps) {
		t.Errorf("reloads = %d, want one per edit", *reloads)
	}
	r := loadConfigIn(t, dir).Repos["app"]
	if r.Vars["CLOUDFLARE_ACCOUNT_ID"] != "def456" || r.Vars["STAGING_READY"] != "true" {
		t.Errorf("vars = %v", r.Vars)
	}
	if got := r.VarsFor("production")["URL"]; got != `https://example.com/"quoted"` {
		t.Errorf("production URL = %q", got)
	}
	body := readConfigFile(t, dir)
	if strings.Count(body, "CLOUDFLARE_ACCOUNT_ID") != 1 || !strings.Contains(body, "# The operator's own note") {
		t.Errorf("the edit did not replace in place:\n%s", body)
	}

	out, err := latemIn(t, dir, "", "vars", "list", "--no-color")
	if err != nil {
		t.Fatal(err)
	}
	want := "REPO\tENVIRONMENT\tNAME\tVALUE\n" +
		"app\t-\tCLOUDFLARE_ACCOUNT_ID\tdef456\n" +
		"app\t-\tSTAGING_READY\ttrue\n" +
		"app\tproduction\tURL\thttps://example.com/\"quoted\"\n"
	if out != want {
		t.Errorf("vars list =\n%s\nwant\n%s", out, want)
	}

	if _, err := latemIn(t, dir, "", "vars", "rm", "--repo", "app", "--env", "production", "URL"); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(readConfigFile(t, dir), "environments") {
		t.Error("the emptied environment table was left behind")
	}
	if _, err := latemIn(t, dir, "", "vars", "rm", "--repo", "app", "NOPE"); err == nil {
		t.Error("removing an unset variable succeeded")
	}
}

func TestVars_Refusals(t *testing.T) {
	dir := configDirWith(t, baseConfig)
	noAgentRunning(t)
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"vars", "set", "NAME", "v"}, "--repo is required"},
		{[]string{"vars", "set", "--repo", "app", "GITHUB_SHA", "v"}, "GitHub's own"},
		{[]string{"vars", "set", "--repo", "app", "1BAD", "v"}, "not a variable name"},
	} {
		if _, err := latemIn(t, dir, "", tc.args...); exitCode(err) != ExitUsage || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%v: %v, want usage mentioning %q", tc.args, err, tc.want)
		}
	}
	if readConfigFile(t, dir) != baseConfig {
		t.Error("a refused edit changed the file")
	}
}
