package run

import (
	"context"
	"strings"
	"testing"

	"github.com/Abyssinia-Labs/latem/internal/secrets"
	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

// stoppedVault is OpenVault with its service not running.
var stoppedVault = &secrets.OpenVaultError{Op: "list", NotRunning: true, Socket: "/home/tester/.openvault/service.sock"}

// A86: a secret OpenVault cannot be asked for fails its job as a backend
// down, retryable, and not as a secret nobody set.
func TestExecute_ASecretWhileTheVaultIsDownIsARetryableBackendFailure(t *testing.T) {
	h := newHarness(t)
	h.source.registry = ""
	h.provider = secrets.NewChain(downStore{}, h.secret)
	o := h.build()
	req := h.request("deploy")
	r := h.admitAndClaim(o, req)
	h.writeWorkspaceFile(req.ID, ".github/workflows/deploy.yml", `
on: [workflow_dispatch]
jobs:
  deploy:
    runs-on: [linux]
    steps:
      - run: ./deploy.sh
        env:
          TOKEN: ${{ secrets.TOKEN }}
`)
	res, err := o.Execute(t.Context(), r)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.ErrorCode != protocol.ErrSecretBackend || !res.ErrorCode.Retryable() {
		t.Fatalf("got %s/%s (%s), want a retryable E_SECRET_BACKEND", res.State, res.ErrorCode, res.ErrorMsg)
	}
	if strings.HasPrefix(res.ErrorMsg, string(protocol.ErrMissingSecret)) {
		t.Errorf("message = %q reads as a missing secret", res.ErrorMsg)
	}
}

// downStore is OpenVault with its service stopped, as a link of the chain.
type downStore struct{}

func (downStore) Name() string { return "openvault" }
func (downStore) Resolve(ctx context.Context, name string) (secrets.Value, error) {
	return downStore{}.ResolveIn(ctx, nil, name)
}

func (downStore) ResolveIn(context.Context, []string, string) (secrets.Value, error) {
	return nil, stoppedVault
}
