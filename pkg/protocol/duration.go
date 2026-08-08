package protocol

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// leadingDays matches a day component, which Go's own parser does not support.
//
// RUNNER_SPEC writes retention in days — log_retention = "14d", retain: 7d —
// and time.ParseDuration rejects the unit outright. Supporting it here rather
// than spelling those as "336h" keeps the config files legible; a reader
// should not have to divide by 24 to learn how long logs are kept.
//
// Only a leading day component is accepted, so "14d" and "1d12h" parse while
// "12h1d" does not. Duration strings are written largest unit first, and
// accepting the reverse would mean accepting "1h1d1m" too, which reads like a
// typo because it is one.
var leadingDays = regexp.MustCompile(`^(\d+)d`)

// hoursPerDay is a plain 24. These durations are retention windows and
// timeouts, not calendar arithmetic: nothing here should shift by an hour
// twice a year because a jurisdiction changed its clocks.
const hoursPerDay = 24

func parseDuration(s string) (time.Duration, error) {
	if s == "" {
		return 0, fmt.Errorf("empty duration")
	}

	rest := s
	var total time.Duration

	if m := leadingDays.FindStringSubmatch(rest); m != nil {
		days, err := strconv.ParseInt(m[1], 10, 64)
		if err != nil {
			return 0, fmt.Errorf("day component %q: %w", m[1], err)
		}
		const maxDays = 1 << 20 // ~2870 years; anything larger overflows below
		if days > maxDays {
			return 0, fmt.Errorf("day component %d is out of range", days)
		}
		total = time.Duration(days) * hoursPerDay * time.Hour
		rest = rest[len(m[0]):]
	}

	if rest == "" {
		return total, nil
	}
	if strings.Contains(rest, "d") {
		return 0, fmt.Errorf("day component must come first, as in \"1d12h\"")
	}

	tail, err := time.ParseDuration(rest)
	if err != nil {
		return 0, err
	}
	if total != 0 && tail < 0 {
		return 0, fmt.Errorf("mixed signs in %q", s)
	}
	return total + tail, nil
}

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
//
// It accepts everything time.ParseDuration accepts, plus a leading day
// component: "14d", "1d12h".
func (d *Duration) UnmarshalText(text []byte) error {
	parsed, err := parseDuration(string(text))
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
