package config_test

import (
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/Abyssinia-Labs/latem/internal/config"
)

// The reference config in RUNNER_SPEC §8 must actually load.
//
// It did not. Amendment A31 promoted the `[runner.native]` block from an
// example nobody copied to the default `latem init` writes, and the block
// carried `shell = "bash"` — a key no runner schema has ever had. Decoding is
// strict, so the config a new operator is told to start from would have been
// rejected at load with an unknown-key error, on the one path where they have
// the least context to diagnose it.
//
// The key was wrong from the moment it was written; what changed is that it
// went from documentation nobody executed to the shipped default. Nothing could
// have caught it, because no test had ever read the spec. This one does: it
// extracts the fenced TOML from §8 and puts it through the same decode and
// validation an agent runs.
//
// [config.Parse] rather than [config.Load] because the spec's example names
// paths that do not exist on the machine running this test — the schema is what
// is being checked here, not a real installation.
func TestSpecReferenceConfigParses(t *testing.T) {
	const specPath = "../../docs/RUNNER_SPEC.md"

	src, err := os.ReadFile(specPath)
	if err != nil {
		t.Fatalf("reading the spec: %v", err)
	}

	toml := specSection8TOML(t, string(src))

	// The tart runner ships commented out (A31) and the native one enabled.
	// Assert that, because "the reference config parses" would also be true of
	// a file where somebody had commented out the wrong one, and which executor
	// an operator gets by default is the substance of A31 rather than a detail
	// of it.
	if !strings.Contains(toml, `executor    = "native"`) {
		t.Error("§8 does not enable a native runner; A31 makes it the macOS default")
	}
	if regexp.MustCompile(`(?m)^\s*executor\s*=\s*"tart"`).MatchString(toml) {
		t.Error("§8 enables a tart runner; A31 ships it commented out")
	}

	// The default config directory, not a temp one: §8's example writes
	// "~/.latem/work" and friends, and the same-directory rule that catches two
	// agents sharing state would reject those against any other Dir. Using the
	// default is what an operator copying this file actually does.
	cfg, err := config.Parse([]byte(toml), config.Options{})
	if err != nil {
		t.Fatalf("the spec's own reference config does not load:\n%v", err)
	}

	// A config that parses but has no runners would pass everything above and
	// be useless, which is the failure mode of asserting only on the error.
	if len(cfg.Runners) == 0 {
		t.Fatal("parsed, but no runners — the extraction probably matched the wrong block")
	}
}

// specSection8TOML returns the first fenced ```toml block after the §8 heading.
//
// Deliberately literal. A markdown parser here would be a second thing that can
// be wrong, and the failure mode of this helper is loud: an extraction that
// grabs the wrong block produces a config that does not parse or has no
// runners, both of which the caller fails on.
func specSection8TOML(t *testing.T, src string) string {
	t.Helper()

	heading := regexp.MustCompile(`(?m)^## 8\. `)
	loc := heading.FindStringIndex(src)
	if loc == nil {
		t.Fatal("no §8 heading in RUNNER_SPEC.md — has the spec been renumbered?")
	}
	rest := src[loc[1]:]

	open := strings.Index(rest, "```toml\n")
	if open < 0 {
		t.Fatal("no fenced toml block after the §8 heading")
	}
	rest = rest[open+len("```toml\n"):]

	end := strings.Index(rest, "```")
	if end < 0 {
		t.Fatal("unterminated toml fence in §8")
	}
	return rest[:end]
}
