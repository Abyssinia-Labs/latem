package registry

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

// InputType is the declared type of a job input.
type InputType string

const (
	InputString InputType = "string"
	InputBool   InputType = "bool"
	InputInt    InputType = "int"
	InputEnum   InputType = "enum"
)

// MaxInputValueBytes bounds a single input value before it is even type
// checked. It is a cheap gate against a broker sending megabytes in a field
// the job expects to be a release note.
const MaxInputValueBytes = 4096

// Input declares one parameter a job accepts.
type Input struct {
	Type      InputType `json:"type"`
	Default   *Scalar   `json:"default,omitempty"`
	Required  bool      `json:"required,omitempty"`
	Values    []string  `json:"values,omitempty"`     // enum only
	MaxLength *int      `json:"max_length,omitempty"` // string only
	Min       *int64    `json:"min,omitempty"`        // int only
	Max       *int64    `json:"max,omitempty"`        // int only
}

// Scalar holds a YAML scalar of any type in its canonical string form.
//
// Inputs arrive over the wire as strings because JSON's type system is not the
// registry's (RUNNER_SPEC §7.1), so a default written as `default: 3` and one
// written as `default: "3"` must end up identical. Normalising at parse time
// means coercion has exactly one shape to handle.
type Scalar struct {
	Value string
	Set   bool
}

// UnmarshalJSON accepts a string, number, or boolean.
func (s *Scalar) UnmarshalJSON(b []byte) error {
	if string(b) == "null" {
		return nil
	}
	var str string
	if err := json.Unmarshal(b, &str); err == nil {
		s.Value, s.Set = str, true
		return nil
	}
	var num json.Number
	if err := json.Unmarshal(b, &num); err == nil {
		s.Value, s.Set = num.String(), true
		return nil
	}
	var bl bool
	if err := json.Unmarshal(b, &bl); err == nil {
		s.Value, s.Set = strconv.FormatBool(bl), true
		return nil
	}
	return fmt.Errorf("default must be a string, number, or boolean, got %s", b)
}

// MarshalJSON round-trips the canonical form.
func (s Scalar) MarshalJSON() ([]byte, error) {
	if !s.Set {
		return []byte("null"), nil
	}
	return json.Marshal(s.Value)
}

func (i *Input) validate() error {
	var problems []error

	switch i.Type {
	case InputString:
		if i.MaxLength != nil && *i.MaxLength <= 0 {
			problems = append(problems, errors.New("max_length must be positive"))
		}
	case InputBool:
		// no constraints
	case InputInt:
		if i.Min != nil && i.Max != nil && *i.Min > *i.Max {
			problems = append(problems, fmt.Errorf("min %d exceeds max %d", *i.Min, *i.Max))
		}
	case InputEnum:
		if len(i.Values) == 0 {
			problems = append(problems, errors.New("enum requires a non-empty values list"))
		}
		seen := map[string]bool{}
		for _, v := range i.Values {
			if seen[v] {
				problems = append(problems, fmt.Errorf("duplicate enum value %q", v))
			}
			seen[v] = true
		}
	case "":
		problems = append(problems, errors.New("type is required"))
	default:
		problems = append(problems, fmt.Errorf(
			"unknown type %q; want one of %s, %s, %s, %s",
			i.Type, InputString, InputBool, InputInt, InputEnum))
	}

	// Constraints belonging to a different type are rejected rather than
	// ignored. `type: string` with `min: 3` means the author expected
	// something that will not happen.
	if i.Type != InputString && i.MaxLength != nil {
		problems = append(problems, fmt.Errorf("max_length is only valid for %s", InputString))
	}
	if i.Type != InputInt && (i.Min != nil || i.Max != nil) {
		problems = append(problems, fmt.Errorf("min/max are only valid for %s", InputInt))
	}
	if i.Type != InputEnum && len(i.Values) > 0 {
		problems = append(problems, fmt.Errorf("values is only valid for %s", InputEnum))
	}

	// A default must itself satisfy the declaration. Catching this at parse
	// time means an unsatisfiable default fails the commit that introduced it
	// rather than the first run that omits the input.
	if i.Default != nil && i.Default.Set && len(problems) == 0 {
		if _, err := i.coerce(i.Default.Value); err != nil {
			problems = append(problems, fmt.Errorf("default is invalid: %w", err))
		}
	}
	if i.Required && i.Default != nil && i.Default.Set {
		problems = append(problems, errors.New("required inputs cannot have a default"))
	}

	return errors.Join(problems...)
}

// coerce converts a wire string to its canonical form for the declared type,
// enforcing every constraint.
func (i *Input) coerce(raw string) (string, error) {
	if len(raw) > MaxInputValueBytes {
		return "", fmt.Errorf("value is %d bytes, limit %d", len(raw), MaxInputValueBytes)
	}
	if !utf8.ValidString(raw) {
		return "", errors.New("value is not valid UTF-8")
	}

	switch i.Type {
	case InputString:
		if i.MaxLength != nil && utf8.RuneCountInString(raw) > *i.MaxLength {
			return "", fmt.Errorf("value is %d characters, max_length is %d",
				utf8.RuneCountInString(raw), *i.MaxLength)
		}
		return raw, nil

	case InputBool:
		// strconv.ParseBool also accepts "1", "t", "TRUE" and friends. Narrow
		// it: a job author reading `DEPLOY=t` in a log should not have to
		// wonder. Canonical in, canonical out.
		switch raw {
		case "true", "false":
			return raw, nil
		}
		return "", fmt.Errorf("want \"true\" or \"false\", got %q", raw)

	case InputInt:
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			return "", fmt.Errorf("want an integer, got %q", raw)
		}
		if i.Min != nil && n < *i.Min {
			return "", fmt.Errorf("%d is below the minimum of %d", n, *i.Min)
		}
		if i.Max != nil && n > *i.Max {
			return "", fmt.Errorf("%d is above the maximum of %d", n, *i.Max)
		}
		return strconv.FormatInt(n, 10), nil

	case InputEnum:
		for _, v := range i.Values {
			if raw == v {
				return raw, nil
			}
		}
		return "", fmt.Errorf("want one of [%s], got %q", strings.Join(i.Values, ", "), raw)
	}

	return "", fmt.Errorf("unknown input type %q", i.Type)
}

// CoerceInputs validates a run request's inputs against a job's declarations
// and returns the canonical values.
//
// Undeclared keys are a hard rejection, not a warning. A control plane sending
// an input the job does not declare is either out of date or wrong about which
// job it is starting, and silently dropping the value hides both cases.
func (j *Job) CoerceInputs(supplied map[string]string) (map[string]string, error) {
	out := make(map[string]string, len(j.Inputs))

	for _, k := range sortedKeys(supplied) {
		decl, ok := j.Inputs[k]
		if !ok {
			return nil, codeErr(protocol.ErrBadInput,
				"input %q is not declared by this job", k)
		}
		v, err := decl.coerce(supplied[k])
		if err != nil {
			return nil, &Error{
				Code: protocol.ErrBadInput,
				Msg:  fmt.Sprintf("input %q", k),
				Err:  err,
			}
		}
		out[k] = v
	}

	for _, k := range sortedKeys(j.Inputs) {
		if _, ok := out[k]; ok {
			continue
		}
		decl := j.Inputs[k]
		switch {
		case decl.Default != nil && decl.Default.Set:
			// Already validated at parse time, so this cannot fail.
			v, err := decl.coerce(decl.Default.Value)
			if err != nil {
				return nil, &Error{
					Code: protocol.ErrBadRegistry,
					Msg:  fmt.Sprintf("input %q has an invalid default", k),
					Err:  err,
				}
			}
			out[k] = v
		case decl.Required:
			return nil, codeErr(protocol.ErrBadInput, "input %q is required", k)
		default:
			out[k] = ""
		}
	}

	return out, nil
}
