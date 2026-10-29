package run

import (
	"context"
	"encoding/base64"
	"io"
	"strings"
	"testing"

	"github.com/Abyssinia-Labs/latem/internal/executor"
	"github.com/Abyssinia-Labs/latem/internal/logstream"
	"github.com/Abyssinia-Labs/latem/internal/source"
	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

// The deploy shape (A53): check out the run's commit with the job's token,
// stamp a version and push it, then check out `ref: main` to build what was
// pushed, and once more by commit without a credential.
const checkoutWorkflow = `
on: [push]
jobs:
  deploy:
    runs-on: [linux]
    steps:
      - uses: actions/checkout@v5
      - id: bump
        run: ./bump.sh
      - uses: actions/checkout@v5
        with:
          ref: main
      - uses: actions/checkout@v5
        with:
          ref: ${{ steps.bump.outputs.sha }}
          persist-credentials: false
      - run: ./deploy.sh
`

const pushedSHA = "fedcba9876543210fedcba9876543210fedcba98"

func TestCheckout_TheDeployShape(t *testing.T) {
	h := newHarness(t)
	h.source.registry = ""
	h.cfg.Repos["app"].GitHub = "acme/app"
	pinned := h.source.sha
	header := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + testToken))
	h.source.containing = map[string][]string{pushedSHA: {"refs/heads/main"}}
	h.env.outputs = func(step executor.Step) map[string]string {
		if step.Run == "./bump.sh" {
			return map[string]string{"sha": pushedSHA}
		}
		return nil
	}
	h.env.exec = func(_ context.Context, step executor.Step, stdout, _ io.Writer) (int, error) {
		switch step.Run {
		case "./bump.sh":
			// The version commit lands on main upstream, and the mirror
			// sees it at the next fetch.
			h.source.setSHA(pushedSHA)
		case "./deploy.sh":
			_, _ = io.WriteString(stdout, "header was "+header+"\n")
		}
		return 0, nil
	}
	o := h.build()

	req := workflowRequest(h, "deploy")
	r := h.admitAndClaim(o, req)
	h.writeWorkspaceFile(req.ID, ".github/workflows/checks.yml", checkoutWorkflow)
	res, err := o.Execute(t.Context(), r)
	if err != nil {
		t.Fatal(err)
	}
	if res.State != protocol.StateSucceeded {
		t.Fatalf("got %s/%s: %s", res.State, res.ErrorCode, res.ErrorMsg)
	}

	steps := h.env.executed()
	if len(steps) != 5 {
		t.Fatalf("executed %d steps, want 5: %+v", len(steps), steps)
	}
	const remote = "https://github.com/acme/app.git"
	const creds = "/tmp/latem/temp/" + checkoutCredentialsFile
	for i, want := range []map[string]string{
		0: {"LATEM_CHECKOUT_SHA": pinned, "LATEM_CHECKOUT_BRANCH": "main", "LATEM_CHECKOUT_TRACKING": "",
			"LATEM_CHECKOUT_PACK": "", "LATEM_CHECKOUT_CLEAN": "true",
			"LATEM_CHECKOUT_REMOTE": remote, "LATEM_CHECKOUT_INCLUDE": creds},
		2: {"LATEM_CHECKOUT_SHA": pushedSHA, "LATEM_CHECKOUT_BRANCH": "main", "LATEM_CHECKOUT_TRACKING": "true",
			"LATEM_CHECKOUT_PACK":   "/tmp/latem/temp/" + checkoutPackFile,
			"LATEM_CHECKOUT_REMOTE": remote, "LATEM_CHECKOUT_INCLUDE": creds},
		3: {"LATEM_CHECKOUT_SHA": pushedSHA, "LATEM_CHECKOUT_BRANCH": "",
			"LATEM_CHECKOUT_REMOTE": remote, "LATEM_CHECKOUT_INCLUDE": ""},
	} {
		if want == nil {
			continue
		}
		st := steps[i]
		if st.Run != source.SwitchScript || st.Shell != executor.ShellGitHubSh || st.Name != "Run actions/checkout@v5" {
			t.Errorf("step %d is not the checkout script: %q, shell %q", i, st.Name, st.Shell)
		}
		for k, v := range want {
			if st.Env[k] != v {
				t.Errorf("step %d: %s = %q, want %q", i, k, st.Env[k], v)
			}
		}
	}

	if pack, _ := h.env.put(checkoutPackFile); string(pack) != "PACK "+pushedSHA {
		t.Errorf("pack = %q", pack)
	}
	if cfg, ok := h.env.put(checkoutCredentialsFile); !ok || len(cfg) != 0 {
		t.Errorf("the credential file is %q; persist-credentials: false should have emptied it", cfg)
	}
	for i, st := range steps {
		for k, v := range st.Env {
			if strings.Contains(v, testToken) || strings.Contains(v, header) {
				t.Errorf("step %d's %s carries the token", i, k)
			}
		}
	}

	log := h.logContents(req.ID)
	for _, want := range []string{
		"checkout: the run's commit " + shortSHA(pinned) + " (refs/heads/main)",
		"checkout: ref main → refs/heads/main at " + shortSHA(pushedSHA) + " (the run's commit is " +
			shortSHA(pinned) + "; admitted by refs/heads/main)",
	} {
		if !strings.Contains(log, want) {
			t.Errorf("the log does not say %q:\n%s", want, log)
		}
	}
	// The redactor would catch the token inside its base64 on its own, but
	// only partly; registered whole, none of the header survives.
	if !strings.Contains(log, "header was "+logstream.Marker) {
		t.Errorf("the credential's header form was not redacted whole:\n%s", log)
	}
}

// A commit only a branch the policy does not name holds is code the policy
// does not trust. The step fails the job — it cannot park — and nothing of
// the credential is written.
func TestCheckout_RefusesWhatThePolicyDoesNotAdmit(t *testing.T) {
	for name, c := range map[string]struct {
		ref  string
		want string
	}{
		"a commit on an unadmitted branch": {"${{ steps.pick.outputs.sha }}", "not in the history of any ref allowed_refs admits"},
		"a branch that does not exist":     {"nope", `ref "nope" matches neither`},
		"a commit the mirror lacks":        {"1111111111111111111111111111111111111111", "is not in app"},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			h.source.registry = ""
			h.cfg.Repos["app"].GitHub = "acme/app"
			h.source.commits = map[string]bool{pushedSHA: false}
			h.source.containing = map[string][]string{pushedSHA: {"refs/heads/feature/x"}}
			h.env.outputs = func(step executor.Step) map[string]string {
				return map[string]string{"sha": pushedSHA}
			}
			o := h.build()

			req := workflowRequest(h, "deploy")
			r := h.admitAndClaim(o, req)
			h.writeWorkspaceFile(req.ID, ".github/workflows/checks.yml", `
on: [push]
jobs:
  deploy:
    runs-on: [linux]
    steps:
      - id: pick
        run: ./pick.sh
      - uses: actions/checkout@v5
        with:
          ref: `+c.ref+`
      - run: ./never.sh
`)
			res, err := o.Execute(t.Context(), r)
			if err != nil {
				t.Fatal(err)
			}
			if res.State != protocol.StateFailed || res.ErrorCode != protocol.ErrStep {
				t.Fatalf("got %s/%s, want failed/E_STEP", res.State, res.ErrorCode)
			}
			if !strings.Contains(res.ErrorMsg, c.want) {
				t.Errorf("message %q does not say %q", res.ErrorMsg, c.want)
			}
			if steps := h.env.executed(); len(steps) != 1 || steps[0].Run != "./pick.sh" {
				t.Errorf("executed %+v, want only the step before the checkout", steps)
			}
			if _, ok := h.env.put(checkoutCredentialsFile); ok {
				t.Error("a refused checkout wrote the credential")
			}
		})
	}
}

// A repository latem does not reach as github.com/owner/name has nowhere to
// pin origin and no host to scope a token to: the switch runs, origin is left
// as config set it, and the log says why there is no credential.
func TestCheckout_NotGitHubGetsNoCredential(t *testing.T) {
	h := newHarness(t)
	h.source.registry = ""
	o := h.build()
	req := workflowRequest(h, "deploy")
	r := h.admitAndClaim(o, req)
	h.writeWorkspaceFile(req.ID, ".github/workflows/checks.yml", `
on: [push]
jobs:
  deploy:
    runs-on: [linux]
    steps:
      - uses: actions/checkout@v5
`)
	res, err := o.Execute(t.Context(), r)
	if err != nil {
		t.Fatal(err)
	}
	if res.State != protocol.StateSucceeded {
		t.Fatalf("got %s/%s: %s", res.State, res.ErrorCode, res.ErrorMsg)
	}
	st := h.env.executed()[0]
	if st.Env["LATEM_CHECKOUT_REMOTE"] != "" || st.Env["LATEM_CHECKOUT_INCLUDE"] != "" {
		t.Errorf("remote %q, include %q; want neither", st.Env["LATEM_CHECKOUT_REMOTE"], st.Env["LATEM_CHECKOUT_INCLUDE"])
	}
	if log := h.logContents(req.ID); !strings.Contains(log, "is not a GitHub repository, so the workspace gets no credential") {
		t.Errorf("the log does not say why:\n%s", log)
	}
}
