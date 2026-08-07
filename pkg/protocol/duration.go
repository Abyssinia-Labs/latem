package protocol

import (
	"encoding/json"
	"fmt"
	"time"
)

// Duration wraps [time.Duration] with text and JSON codecs that accept Go
// duration strings such as "30m" or "1h30m".
//
// It exists because both configuration paths need durations and neither
// provides them. TOML has no duration type, and sigs.k8s.io/yaml routes YAML
// through JSON, which has no duration type either. Defining it once here is
// what stops internal/config and internal/registry from growing two subtly
// different parsers (RUNNER_SPEC §3).
type Duration time.Duration

// Duration returns the wrapped value.
func (d Duration) Duration() time.Duration { return time.Duration(d) }

// String implements [fmt.Stringer].
func (d Duration) String() string { return time.Duration(d).String() }

// UnmarshalText implements [encoding.TextUnmarshaler]. This is the method
// BurntSushi/toml uses, so it covers config.toml.
func (d *Duration) UnmarshalText(text []byte) error {
	parsed, err := time.ParseDuration(string(text))
	if err != nil {
		return fmt.Errorf("protocol: invalid duration %q: %w", string(text), err)
	}
	*d = Duration(parsed)
	return nil
}

// MarshalText implements [encoding.TextMarshaler].
func (d Duration) MarshalText() ([]byte, error) {
	return []byte(time.Duration(d).String()), nil
}

// UnmarshalJSON accepts a duration string, and covers jobs.yaml by way of
// sigs.k8s.io/yaml.
//
// A bare number is rejected rather than interpreted. Go would read `30` as 30
// nanoseconds while every human writing `timeout: 30` in a CI file means
// minutes, and silently choosing either reading is worse than refusing.
func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("protocol: duration must be a string such as \"30m\": %w", err)
	}
	return d.UnmarshalText([]byte(s))
}

// MarshalJSON implements [json.Marshaler].
func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(time.Duration(d).String())
}
