package registry

import (
	"strings"
	"testing"
)

func inputs(names ...string) map[string]*Input {
	m := make(map[string]*Input, len(names))
	for _, n := range names {
		m[n] = &Input{Type: InputString}
	}
	return m
}

func TestValidateInterpolation_Accepts(t *testing.T) {
	decl := inputs("CHANNEL", "NOTES")
	for _, v := range []string{
		"",
		"plain text",
		"${input.CHANNEL}",
		"prefix ${input.CHANNEL} suffix",
		"${input.CHANNEL}-${input.NOTES}",
		// A bare dollar is not an expansion and passes through literally. The
		// value goes into an environment map, not through a shell, so there is
		// nothing here for anything to re-expand.
		"$HOME",
		"100$",
		"$",
	} {
		if err := validateInterpolation(v, decl); err != nil {
			t.Errorf("rejected %q: %v", v, err)
		}
	}
}

func TestValidateInterpolation_Rejects(t *testing.T) {
	decl := inputs("CHANNEL")
	for name, v := range map[string]string{
		"undeclared input":   "${input.NOPE}",
		"secret reference":   "${secret.MATCH_PASSWORD}",
		"env reference":      "${env.PATH}",
		"bare name":          "${CHANNEL}",
		"empty expansion":    "${}",
		"unterminated":       "${input.CHANNEL",
		"nested":             "${input.${input.CHANNEL}}",
		"invalid input name": "${input.not-an-ident}",
	} {
		t.Run(name, func(t *testing.T) {
			if err := validateInterpolation(v, decl); err == nil {
				t.Errorf("accepted %q", v)
			}
		})
	}
}

// Secrets never arrive by substitution — they are in the environment because
// they were declared in secrets:. That rule needs no check of its own: it falls
// out of ${input.NAME} being the only legal expansion.
func TestValidateInterpolation_SecretsAreNotAddressable(t *testing.T) {
	decl := inputs("CHANNEL")
	for _, v := range []string{
		"${secret.MATCH_PASSWORD}",
		"${secrets.MATCH_PASSWORD}",
		"${MATCH_PASSWORD}",
	} {
		if err := validateInterpolation(v, decl); err == nil {
			t.Errorf("accepted a secret reference %q", v)
		}
	}
}

// GitHub Actions expression syntax gets a dedicated message. Someone writing
// ${{ }} has a specific mental model, and "invalid expansion" would not tell
// them which of their assumptions is wrong.
func TestValidateInterpolation_ActionsSyntaxIsNamed(t *testing.T) {
	err := validateInterpolation("${{ github.sha }}", inputs("X"))
	if err == nil {
		t.Fatal("accepted ${{ }}")
	}
	if !strings.Contains(err.Error(), "GitHub Actions") {
		t.Errorf("error should name the syntax it is refusing, got: %v", err)
	}
}

// steps[].run is literal. An expansion there means the author expected
// substitution that will not happen, and passing "${input.X}" through to bash
// is the confusion that leads people to reach for eval.
func TestRejectInterpolation_InRun(t *testing.T) {
	for _, v := range []string{
		"./ci/build.sh ${input.CHANNEL}",
		"echo ${{ matrix.os }}",
		"deploy ${secret.KEY}",
	} {
		if err := rejectInterpolation(v); err == nil {
			t.Errorf("accepted an expansion in run: %q", v)
		}
	}
	for _, v := range []string{
		"./ci/build.sh",
		"echo $HOME",
		`echo "$RELEASE_CHANNEL"`,
	} {
		if err := rejectInterpolation(v); err != nil {
			t.Errorf("rejected legitimate run %q: %v", v, err)
		}
	}
}

func TestSubstitute(t *testing.T) {
	values := map[string]string{"CHANNEL": "internal", "NOTES": "hello world"}
	for in, want := range map[string]string{
		"":                                "",
		"plain":                           "plain",
		"${input.CHANNEL}":                "internal",
		"channel=${input.CHANNEL}":        "channel=internal",
		"${input.CHANNEL}/${input.NOTES}": "internal/hello world",
		"$HOME ${input.CHANNEL}":          "$HOME internal",
	} {
		if got := substitute(in, values); got != want {
			t.Errorf("substitute(%q) = %q, want %q", in, got, want)
		}
	}
}

// An input declared but not supplied resolves to its default, or to empty.
// Substitution must not leave the literal expansion behind, which would put
// "${input.X}" into a build's environment.
func TestSubstitute_MissingValueBecomesEmpty(t *testing.T) {
	if got := substitute("[${input.MISSING}]", map[string]string{}); got != "[]" {
		t.Errorf("got %q, want %q", got, "[]")
	}
}
