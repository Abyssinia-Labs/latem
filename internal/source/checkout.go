package source

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"net/url"
	"strings"

	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

// A checkout step (A53) is split across a trust boundary, and this file is
// both halves of the git it needs.
//
// The agent runs git only in the mirror, which it owns: which refs hold a
// commit, and a pack of the objects the workspace lacks. Everything that
// touches the workspace runs in the job's own environment, as the step's
// process, through [SwitchScript]. Every earlier step can write the
// workspace's .git — through the bind mount, for docker — and a host git
// reading that .git would run its hooks, its fsmonitor, its filter drivers,
// or follow a url rewrite to an ssh command. That would hand a container step
// the agent's account. Run inside the environment, the same git is the job
// acting on itself, which it could always do.

// ContainingRefs lists the refs under prefixes whose history holds sha, for
// [registry.AdmitCheckout]. Tags are peeled to the commit they name.
//
// prefixes are [registry.PolicyPrefixes]: the mirror is asked only about refs
// a policy could admit, which keeps the walk short on a repository with
// thousands of tags. No prefixes is no refs — a policy that admits nothing
// admits no checkout.
func (mi *Mirror) ContainingRefs(ctx context.Context, sha string, prefixes []string) ([]string, error) {
	if err := validateSHA(sha); err != nil {
		return nil, err
	}
	if len(prefixes) == 0 {
		return nil, nil
	}
	for _, p := range prefixes {
		if !strings.HasPrefix(p, "refs/") {
			return nil, codeErr(protocol.ErrInvalidRef, "ContainingRefs needs fully-qualified prefixes, got %q", p)
		}
	}
	args := append([]string{"for-each-ref", "--format=%(refname)", "--contains", sha, "--end-of-options"}, prefixes...)
	out, err := mi.mgr.run(ctx, mi.path, args...)
	if err != nil {
		return nil, gitErr(err, "listing the refs that hold %s in %s", sha, mi.repo.Key)
	}
	var refs []string
	for line := range strings.Lines(out) {
		if r := strings.TrimSpace(line); r != "" {
			refs = append(refs, r)
		}
	}
	return refs, nil
}

// MaxCheckoutPack bounds the objects a checkout carries into a workspace. A
// deploy job's target is a version commit or two past the run's; a pack this
// size is a checkout of unrelated history, which is refused rather than held
// in memory.
const MaxCheckoutPack = 256 << 20

// PackSince is a pack of the objects sha's history holds and have's does
// not, for a workspace cloned at have to index before it switches to sha.
// Nil when there is nothing to carry: sha is have, or an ancestor of it, and
// the workspace already holds every object it needs.
//
// The workspace was cloned from the whole mirror, so it lacks only what the
// mirror fetched since — the commit an earlier job of the same run pushed,
// typically. Excluding have's history is what keeps the pack that small; an
// object both have and the pack carry is harmless, only wasted.
func (mi *Mirror) PackSince(ctx context.Context, sha, have string) ([]byte, error) {
	if sha == have {
		return nil, nil
	}
	if within, err := mi.IsAncestor(ctx, sha, have); err != nil {
		return nil, err
	} else if within {
		return nil, nil
	}
	// --revs reads rev-list arguments from stdin, which is the only way
	// pack-objects takes them. Both are validated object names, so neither
	// can be read as an option.
	cmd := mi.mgr.command(ctx, mi.path, []string{"pack-objects", "--revs", "--stdout", "-q"})
	cmd.Stdin = strings.NewReader(sha + "\n^" + have + "\n")
	stdout := &cappedBuffer{limit: MaxCheckoutPack}
	var stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = stdout, &stderr
	err := cmd.Run()
	if stdout.over {
		return nil, codeErr(protocol.ErrSource,
			"%s carries more than %d MB of history the run's commit does not; a checkout is not a second clone",
			sha, MaxCheckoutPack>>20)
	}
	if err != nil {
		return nil, gitErr(&gitError{args: []string{"pack-objects", "--revs"}, stderr: strings.TrimSpace(stderr.String()), err: err},
			"packing %s for a checkout in %s", sha, mi.repo.Key)
	}
	return stdout.Bytes(), nil
}

// CredentialConfig is the git config a persisted checkout credential is
// (A53): the token as an Authorization header for remote's host, in the form
// GitHub's own checkout writes. It goes into a file outside the workspace,
// which the workspace's .git/config includes, so an upload of the workspace
// cannot carry it.
//
// The value is base64, so nothing a token contains can break the file's
// syntax; [CredentialSecrets] lists both forms for the redactor.
func CredentialConfig(remote, token string) ([]byte, error) {
	base, err := remoteBase(remote)
	if err != nil {
		return nil, err
	}
	return fmt.Appendf(nil, "[http %q]\n\textraheader = \"AUTHORIZATION: basic %s\"\n", base, basicAuth(token)), nil
}

// CredentialSecrets are the forms of a token [CredentialConfig] writes, each
// to be registered with the redactor before the file exists.
func CredentialSecrets(token string) []string {
	return []string{token, basicAuth(token)}
}

func basicAuth(token string) string {
	return base64.StdEncoding.EncodeToString([]byte("x-access-token:" + token))
}

// remoteBase is the scheme and host an extraheader is scoped to. The whole
// host, as GitHub's checkout scopes it, so a later step's fetch of another of
// the owner's repositories carries it too.
func remoteBase(remote string) (string, error) {
	u, err := url.Parse(remote)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.User != nil {
		return "", codeErr(protocol.ErrSource, "a checkout credential needs an http(s) remote, got %q", remote)
	}
	return u.Scheme + "://" + u.Host + "/", nil
}

// Switch is what a checkout step does to the workspace, handed to
// [SwitchScript] as its environment.
type Switch struct {
	// SHA is the commit to check out, already admitted.
	SHA string
	// Branch, when set, is checked out as a local branch at SHA with
	// origin/<Branch> as its upstream, so a bare `git push` works; empty
	// detaches. Tracking also moves refs/remotes/origin/<Branch> to SHA, for
	// a branch resolved by name as the step ran.
	Branch   string
	Tracking bool
	// Pack is the environment-visible path of a [Mirror.PackSince] pack to
	// index first, or empty.
	Pack string
	// Clean removes untracked and ignored files and discards changes first,
	// as GitHub's `git clean -ffdx`. It cannot empty a cache: both executors
	// refuse a cache target inside the workspace.
	Clean bool
	// Remote is the URL origin is pinned to, for fetch and push, or empty to
	// leave origin alone. Include is the environment-visible path of a
	// [CredentialConfig] file, or empty for no credential.
	Remote  string
	Include string
}

// Env is the switch as [SwitchScript] reads it. Nothing in it is secret: the
// credential is a file, and only its path is here.
func (s Switch) Env() (map[string]string, error) {
	if err := validateSHA(s.SHA); err != nil {
		return nil, err
	}
	if s.Remote != "" {
		if _, err := remoteBase(s.Remote); err != nil {
			return nil, err
		}
	}
	return map[string]string{
		"LATEM_CHECKOUT_SHA":      s.SHA,
		"LATEM_CHECKOUT_BRANCH":   s.Branch,
		"LATEM_CHECKOUT_TRACKING": boolEnv(s.Tracking),
		"LATEM_CHECKOUT_PACK":     s.Pack,
		"LATEM_CHECKOUT_CLEAN":    boolEnv(s.Clean),
		"LATEM_CHECKOUT_REMOTE":   s.Remote,
		"LATEM_CHECKOUT_INCLUDE":  s.Include,
	}, nil
}

func boolEnv(b bool) string {
	if b {
		return "true"
	}
	return ""
}

// SwitchScript performs a [Switch] in the job's environment, as `sh -e` in
// the workspace root. It is fixed text: every value arrives through the
// environment and is only ever expanded inside double quotes (INV-3).
//
// The order matters in two places. The pack is indexed before the checkout
// that needs its objects. And origin is pinned after the credential's
// include is in place but checked last: a push must go out as the job's
// token, never as whatever the operator's ~/.gitconfig would route it
// through — an ssh `insteadOf`, a keychain helper — because a push as a
// person re-triggers the workflow that made it, where a push as the App is
// one latem-cloud ignores (A52). So origin and its push URL are set, a
// self-mapping insteadOf outranks any shorter prefix the operator rewrites,
// the credential helpers are emptied for this repository, and the step fails
// if the push URL still comes out as anything else.
const SwitchScript = `set -e
if [ -n "$LATEM_CHECKOUT_PACK" ]; then
  git index-pack --stdin < "$LATEM_CHECKOUT_PACK" > /dev/null
fi
if [ -n "$LATEM_CHECKOUT_CLEAN" ]; then
  git clean -ffdx -q
  git reset --hard -q HEAD
fi
if [ -n "$LATEM_CHECKOUT_BRANCH" ]; then
  git checkout -q --force -B "$LATEM_CHECKOUT_BRANCH" "$LATEM_CHECKOUT_SHA" --
  git config branch."$LATEM_CHECKOUT_BRANCH".remote origin
  git config branch."$LATEM_CHECKOUT_BRANCH".merge "refs/heads/$LATEM_CHECKOUT_BRANCH"
  if [ -n "$LATEM_CHECKOUT_TRACKING" ]; then
    git update-ref "refs/remotes/origin/$LATEM_CHECKOUT_BRANCH" "$LATEM_CHECKOUT_SHA"
  fi
else
  git checkout -q --force --detach "$LATEM_CHECKOUT_SHA"
fi
git config --unset-all include.path || [ $? -eq 5 ]
if [ -n "$LATEM_CHECKOUT_INCLUDE" ]; then
  git config --add include.path "$LATEM_CHECKOUT_INCLUDE"
fi
if [ -n "$LATEM_CHECKOUT_REMOTE" ]; then
  git remote set-url origin "$LATEM_CHECKOUT_REMOTE"
  git remote set-url --push origin "$LATEM_CHECKOUT_REMOTE"
  git config --replace-all url."$LATEM_CHECKOUT_REMOTE".insteadOf "$LATEM_CHECKOUT_REMOTE"
  git config --replace-all credential.helper ''
  pushes_to=$(git remote get-url --push origin)
  if [ "$pushes_to" != "$LATEM_CHECKOUT_REMOTE" ]; then
    echo "checkout: origin pushes to $pushes_to, not $LATEM_CHECKOUT_REMOTE; refusing to leave a credential that would push elsewhere" >&2
    git config --unset-all include.path || true
    exit 1
  fi
fi
echo "HEAD is now at $(git rev-parse --short HEAD)"
`
