package workflow

import (
	"fmt"
	"sort"
	"strings"

	"github.com/Abyssinia-Labs/latem/internal/workflow/actions"
)

// checkoutAction is the one action every workflow opens with.
const checkoutAction = "actions/checkout"

// satisfiedCheckoutInputs are the `with:` values latem's own checkout already
// delivers, so a step setting only these is satisfied by having run at all.
//
// Everything absent from this map is refused by name rather than ignored,
// which is the whole point. `fetch-depth: 0` is the case that decides the
// design: it asks for full history, latem checks out one commit (§12), and a
// build that silently got a shallow tree fails later in `git describe` or a
// version derived from tags — a failure nobody traces back to a checkout step
// that appeared to succeed.
var satisfiedCheckoutInputs = map[string]func(string) bool{
	// We never leave a credential in the workspace, so either answer is true.
	"persist-credentials": func(string) bool { return true },
	// The workspace is a fresh clone per run (§12), so it is always clean.
	"clean": truthy,
	// One commit is what §12 checks out, which is this default.
	"fetch-depth": func(v string) bool { return v == "" || v == "1" },
	"submodules":  falsey,
	"lfs":         falsey,
	// The default is the workspace root, which is where we already are.
	"path": func(v string) bool { return v == "" || v == "." },
	// Absent or empty means "this repository at this commit", which is what
	// gate 9 resolved and gate 10 checked out.
	"repository":         func(v string) bool { return v == "" },
	"ref":                func(v string) bool { return v == "" },
	"show-progress":      func(string) bool { return true },
	"set-safe-directory": func(string) bool { return true },
}

func truthy(v string) bool { return v == "" || strings.EqualFold(v, "true") }
func falsey(v string) bool { return v == "" || strings.EqualFold(v, "false") }

// checkoutShim reports whether a `uses:` is a checkout latem has already
// performed, and refuses one that asks for something else.
//
// It runs before the allowlist, and deliberately: nothing is fetched and
// nothing third-party executes, so there is no decision for an operator to
// make. Requiring `actions/checkout` to be allowlisted would make every
// workflow fail on a machine that had opted into no actions at all, for a step
// that is a no-op.
//
// INV-5 is the reason this is sound rather than a convenience. Gate 9 resolved
// the ref, gate 10 checked out that commit, and gate 11 read the registry from
// it. A real `actions/checkout` would re-clone over the top and resolve the
// ref a second time — a second answer, in the window an approval exists to
// close. Not running it is the more correct behaviour, not merely the cheaper
// one.
func checkoutShim(st Step) (satisfied bool, err error) {
	ref, parseErr := actions.ParseRef(st.Uses)
	if parseErr != nil || !strings.EqualFold(ref.Slug(), checkoutAction) || ref.Path != "" {
		return false, nil
	}

	with, err := withStrings(st.With)
	if err != nil {
		return false, err
	}
	var refused []string
	for _, name := range sortedNames(with) {
		ok, known := satisfiedCheckoutInputs[strings.ToLower(name)]
		if !known || !ok(strings.TrimSpace(with[name])) {
			refused = append(refused, fmt.Sprintf("%s: %s", name, with[name]))
		}
	}
	if len(refused) > 0 {
		sort.Strings(refused)
		return false, fmt.Errorf(
			"uses: %s — latem has already checked this repository out at the commit it resolved "+
				"(INV-5), so the step is normally a no-op. It cannot be for %s. %s",
			st.Uses, strings.Join(refused, ", "), checkoutAdvice(with))
	}
	return true, nil
}

// checkoutAdvice says what to do instead, which depends on what was asked for.
//
// Worth splitting, because the honest answer to `ref:` is not the answer to
// anything else. A checkout at a ref other than the one the run named would
// resolve it a second time — a second answer, in exactly the window INV-5's
// approval exists to close — so the fix is to name the ref when starting the
// run, not to re-checkout inside it. This project's own release.yml is the
// case: it passes `ref: ${{ github.event.inputs.ref || github.ref }}`, which on
// latem is `latem run <repo> <job> --ref <tag>`.
func checkoutAdvice(with map[string]string) string {
	for _, k := range []string{"ref", "repository"} {
		if v, ok := with[k]; ok && strings.TrimSpace(v) != "" {
			return "A run builds one commit of one repository, named when it starts — " +
				"`latem run <repo> <job> --ref <ref>` — because resolving a ref a second time " +
				"inside the run is the window INV-5 exists to close."
		}
	}
	return "Do that with a run: step, which is explicit about happening after the checkout " +
		"rather than instead of it."
}
