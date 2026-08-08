package registry

import (
	"strings"
	"testing"

	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

func ptr[T any](v T) *T { return &v }

func TestInput_CoerceString(t *testing.T) {
	in := &Input{Type: InputString, MaxLength: ptr(10)}

	if got, err := in.coerce("hello"); err != nil || got != "hello" {
		t.Errorf("got %q, %v", got, err)
	}
	if _, err := in.coerce(strings.Repeat("x", 11)); err == nil {
		t.Error("accepted a value over max_length")
	}
	// max_length counts characters, not bytes. A 10-emoji note is 10
	// characters to whoever wrote it, and counting bytes would reject it for
	// reasons no author could predict.
	if _, err := in.coerce(strings.Repeat("é", 10)); err != nil {
		t.Errorf("rejected 10 multi-byte characters: %v", err)
	}
	if _, err := in.coerce("\xff\xfe"); err == nil {
		t.Error("accepted invalid UTF-8")
	}
}

func TestInput_CoerceBool(t *testing.T) {
	in := &Input{Type: InputBool}
	for _, ok := range []string{"true", "false"} {
		if got, err := in.coerce(ok); err != nil || got != ok {
			t.Errorf("%q: got %q, %v", ok, got, err)
		}
	}
	// strconv.ParseBool would accept all of these. Narrowing means a log line
	// reading DEPLOY=t never makes anyone wonder.
	for _, bad := range []string{"1", "0", "t", "f", "TRUE", "False", "yes", "no", ""} {
		if _, err := in.coerce(bad); err == nil {
			t.Errorf("accepted %q as a bool", bad)
		}
	}
}

func TestInput_CoerceInt(t *testing.T) {
	in := &Input{Type: InputInt, Min: ptr(int64(1)), Max: ptr(int64(10))}

	for _, ok := range []string{"1", "5", "10"} {
		if _, err := in.coerce(ok); err != nil {
			t.Errorf("%q: %v", ok, err)
		}
	}
	for _, bad := range []string{"0", "11", "-1", "abc", "1.5", "", " 5", "0x10"} {
		if _, err := in.coerce(bad); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
	// Canonicalised: leading zeros and plus signs normalise away, so the same
	// value cannot reach a build spelled two different ways.
	if got, _ := (&Input{Type: InputInt}).coerce("+007"); got != "7" {
		t.Errorf("got %q, want %q", got, "7")
	}
}

func TestInput_CoerceEnum(t *testing.T) {
	in := &Input{Type: InputEnum, Values: []string{"internal", "external"}}

	if _, err := in.coerce("internal"); err != nil {
		t.Errorf("rejected a declared value: %v", err)
	}
	for _, bad := range []string{"Internal", "INTERNAL", "beta", "", "internal "} {
		if _, err := in.coerce(bad); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}

func TestInput_CoerceRejectsOversizedValues(t *testing.T) {
	in := &Input{Type: InputString}
	if _, err := in.coerce(strings.Repeat("x", MaxInputValueBytes+1)); err == nil {
		t.Error("accepted a value over the byte ceiling")
	}
}

func TestInput_Validate(t *testing.T) {
	valid := []*Input{
		{Type: InputString},
		{Type: InputString, MaxLength: ptr(500), Default: &Scalar{Value: "", Set: true}},
		{Type: InputBool, Default: &Scalar{Value: "true", Set: true}},
		{Type: InputInt, Min: ptr(int64(1)), Max: ptr(int64(10))},
		{Type: InputEnum, Values: []string{"a", "b"}, Default: &Scalar{Value: "a", Set: true}},
		{Type: InputString, Required: true},
	}
	for i, in := range valid {
		if err := in.validate(); err != nil {
			t.Errorf("valid[%d]: %v", i, err)
		}
	}

	invalid := map[string]*Input{
		"no type":                  {},
		"unknown type":             {Type: "float"},
		"enum without values":      {Type: InputEnum},
		"enum duplicate values":    {Type: InputEnum, Values: []string{"a", "a"}},
		"min above max":            {Type: InputInt, Min: ptr(int64(10)), Max: ptr(int64(1))},
		"max_length on int":        {Type: InputInt, MaxLength: ptr(5)},
		"min on string":            {Type: InputString, Min: ptr(int64(1))},
		"values on string":         {Type: InputString, Values: []string{"a"}},
		"negative max_length":      {Type: InputString, MaxLength: ptr(0)},
		"default outside enum":     {Type: InputEnum, Values: []string{"a"}, Default: &Scalar{Value: "z", Set: true}},
		"default fails max_length": {Type: InputString, MaxLength: ptr(2), Default: &Scalar{Value: "long", Set: true}},
		"default is not a bool":    {Type: InputBool, Default: &Scalar{Value: "yes", Set: true}},
		"required with a default":  {Type: InputString, Required: true, Default: &Scalar{Value: "x", Set: true}},
	}
	for name, in := range invalid {
		t.Run(name, func(t *testing.T) {
			if err := in.validate(); err == nil {
				t.Error("accepted an invalid declaration")
			}
		})
	}
}

// Constraints belonging to another type are rejected rather than ignored:
// `type: string` with `min: 3` means the author expected something that will
// not happen, and silence would let that ship.
func TestInput_MismatchedConstraintsAreRejected(t *testing.T) {
	if err := (&Input{Type: InputString, Min: ptr(int64(3))}).validate(); err == nil {
		t.Error("min on a string input should be rejected")
	}
}

func TestJob_CoerceInputs(t *testing.T) {
	job := &Job{Inputs: map[string]*Input{
		"CHANNEL": {Type: InputEnum, Values: []string{"internal", "external"},
			Default: &Scalar{Value: "internal", Set: true}},
		"NOTES": {Type: InputString, MaxLength: ptr(500), Default: &Scalar{Value: "", Set: true}},
		"COUNT": {Type: InputInt},
	}}

	got, err := job.CoerceInputs(map[string]string{"CHANNEL": "external"})
	if err != nil {
		t.Fatal(err)
	}
	if got["CHANNEL"] != "external" {
		t.Errorf("CHANNEL = %q", got["CHANNEL"])
	}
	// Defaults fill in, and every declared input is present so a step's
	// environment never depends on whether the caller mentioned it.
	if got["NOTES"] != "" {
		t.Errorf("NOTES = %q", got["NOTES"])
	}
	if _, ok := got["COUNT"]; !ok {
		t.Error("COUNT missing from the coerced map")
	}
}

// An undeclared key is a hard rejection. A control plane sending one is either
// out of date or wrong about which job it is starting; dropping it hides both.
func TestJob_CoerceInputs_RejectsUndeclared(t *testing.T) {
	job := &Job{Inputs: map[string]*Input{"CHANNEL": {Type: InputString}}}

	_, err := job.CoerceInputs(map[string]string{"CHANNEL": "x", "SNEAKY": "y"})
	if err == nil {
		t.Fatal("accepted an undeclared input")
	}
	if CodeOf(err) != protocol.ErrBadInput {
		t.Errorf("code = %s, want %s", CodeOf(err), protocol.ErrBadInput)
	}
}

func TestJob_CoerceInputs_RequiredMustBeSupplied(t *testing.T) {
	job := &Job{Inputs: map[string]*Input{"VERSION": {Type: InputString, Required: true}}}

	if _, err := job.CoerceInputs(nil); CodeOf(err) != protocol.ErrBadInput {
		t.Errorf("want E_BAD_INPUT for a missing required input, got %v", err)
	}
	if _, err := job.CoerceInputs(map[string]string{"VERSION": "1.2"}); err != nil {
		t.Errorf("rejected a supplied required input: %v", err)
	}
}

// A job with no declared inputs accepts none. This is the common case and the
// one where an unnoticed extra key would be most surprising.
func TestJob_CoerceInputs_NoDeclarationsMeansNoInputs(t *testing.T) {
	job := &Job{}
	if _, err := job.CoerceInputs(map[string]string{"ANY": "thing"}); err == nil {
		t.Error("a job declaring no inputs accepted one")
	}
	if got, err := job.CoerceInputs(nil); err != nil || len(got) != 0 {
		t.Errorf("got %v, %v", got, err)
	}
}

func TestScalar_AcceptsYAMLScalarTypes(t *testing.T) {
	for in, want := range map[string]string{
		`"internal"`: "internal",
		`3`:          "3",
		`3.5`:        "3.5",
		`true`:       "true",
		`false`:      "false",
		`""`:         "",
	} {
		var s Scalar
		if err := s.UnmarshalJSON([]byte(in)); err != nil {
			t.Errorf("%s: %v", in, err)
			continue
		}
		if !s.Set || s.Value != want {
			t.Errorf("%s decoded to %q (set=%v), want %q", in, s.Value, s.Set, want)
		}
	}

	var s Scalar
	if err := s.UnmarshalJSON([]byte(`["a"]`)); err == nil {
		t.Error("accepted a list as a default")
	}
}
