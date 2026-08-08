package logstream

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"sync"
)

// alwaysOn matches credential shapes regardless of whether anyone registered
// them, as one compiled alternation (RUNNER_SPEC §16).
//
// These are a backstop, not the primary defence. The primary defence is
// registration: a secret the job declared is registered before the first step
// runs, with every encoded and per-line form the §16 rules produce. Patterns
// catch the case registration cannot — a credential that was never a declared
// secret, printed by a tool that found it somewhere else.
//
// Two of these deliberately match more than the spec's minimum. "xox[baprs]-"
// alone would redact the five-character prefix of a Slack token and leave the
// token; the same argument applies to a JWT's payload. A pattern that redacts
// only the recognisable part of a credential is worse than no pattern, because
// it looks like it worked.
var alwaysOn = regexp.MustCompile(strings.Join([]string{
	`AKIA[0-9A-Z]{16}`,                      // AWS access key id
	`-----BEGIN [A-Z ]*PRIVATE KEY-----`,    // PEM header
	`-----END [A-Z ]*PRIVATE KEY-----`,      // PEM footer
	`gh[pousr]_[A-Za-z0-9]{36,}`,            // GitHub tokens
	`xox[baprs]-[A-Za-z0-9-]{10,}`,          // Slack tokens
	`eyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]+`, // JWT
}, "|"))

// Marker is the prefix every redaction carries.
const Marker = "***"

type redactor struct {
	mu       sync.RWMutex
	literals map[string]struct{}
	matcher  *acMatcher
}

// NewRedactor returns a redactor holding only the always-on patterns. Values
// are added with [Registrar.Register].
//
// It is safe for concurrent use: registration happens before a run's first
// step, and redaction runs on every line thereafter.
func NewRedactor() Redactor {
	return &redactor{literals: map[string]struct{}{}}
}

// TooShort reports whether a value is below the length worth redacting.
//
// Callers resolving secrets should warn on this — by name, never by value —
// because the operator has a secret that will appear in logs verbatim and
// nothing else will tell them. Matching values this short turns every
// occurrence of "true" or "dev" into a marker and makes logs unreadable, which
// is why the floor exists rather than being set to zero.
func TooShort(value []byte) bool {
	return len(strings.TrimSpace(string(value))) < MinRedactableLength
}

// Register adds a value and every form of it that RUNNER_SPEC §16 requires.
func (r *redactor) Register(value []byte) {
	forms := expandForms(value)
	if len(forms) == 0 {
		return
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	changed := false
	for _, f := range forms {
		if _, seen := r.literals[f]; !seen {
			r.literals[f] = struct{}{}
			changed = true
		}
	}
	if !changed {
		return
	}

	// Rebuilding is O(total literal length) and happens once per secret, before
	// any step runs. Redaction itself never rebuilds.
	all := make([]string, 0, len(r.literals))
	for f := range r.literals {
		all = append(all, f)
	}
	sort.Strings(all) // deterministic construction, for reproducible tests
	r.matcher = newACMatcher(all)
}

// expandForms produces every string that must be redacted for one secret.
//
// This implements the four rules in RUNNER_SPEC §16, and rule 2 is the one
// that matters most. The pipeline splits into lines before redacting, so
// registering only the whole value of a multi-line secret redacts NOTHING —
// the whole value never appears in any single line. APP_STORE_CONNECT_KEY_P8
// is a PEM block and is a declared secret in the spec's own example job, so
// the omission meant an echoed App Store Connect key reached the log file and
// the control plane with INV-4's test passing (finding F3).
func expandForms(value []byte) []string {
	var out []string
	seen := map[string]struct{}{}

	add := func(s string) {
		if len(s) < MinRedactableLength {
			return
		}
		if _, dup := seen[s]; dup {
			return
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}

	whole := string(value)

	// 1 — the whole value, verbatim.
	add(whole)

	// 2 — every non-empty line, individually.
	if strings.ContainsAny(whole, "\r\n") {
		for _, line := range strings.FieldsFunc(whole, func(r rune) bool {
			return r == '\n' || r == '\r'
		}) {
			add(strings.TrimRight(line, " \t"))
		}
	}

	// 3 — the value with all whitespace stripped. Tooling re-emits PEMs as one
	// long line often enough that this is not hypothetical.
	stripped := strings.Join(strings.Fields(whole), "")
	add(stripped)

	// 4 — encoded forms of 1 and 3. CI tooling re-encodes credentials
	// constantly, and a base64'd token in a log is still a leaked token.
	for _, base := range []string{whole, stripped} {
		if base == "" {
			continue
		}
		b := []byte(base)
		add(base64.StdEncoding.EncodeToString(b))
		add(base64.RawStdEncoding.EncodeToString(b))
		add(base64.URLEncoding.EncodeToString(b))
		add(base64.RawURLEncoding.EncodeToString(b))
		add(url.QueryEscape(base))
		add(url.PathEscape(base))
	}

	return out
}

// Redact replaces every registered value and matched pattern.
//
// Overlapping matches are merged into maximal spans rather than resolved
// leftmost-longest. That choice is about safety: with leftmost-longest, a
// short match starting before a longer one truncates it, and the tail of the
// longer secret survives into the log. Merging guarantees no byte of anything
// matched is emitted.
func (r *redactor) Redact(line string) string {
	if line == "" {
		return line
	}

	r.mu.RLock()
	matcher := r.matcher
	r.mu.RUnlock()

	var found []span

	if !matcher.empty() {
		matcher.findAll(line, func(start, end int) {
			found = append(found, span{start, end})
		})
	}
	for _, loc := range alwaysOn.FindAllStringIndex(line, -1) {
		found = append(found, span{loc[0], loc[1]})
	}

	if len(found) == 0 {
		return line
	}

	var b strings.Builder
	prev := 0
	for _, s := range mergeSpans(found) {
		b.WriteString(line[prev:s.start])
		b.WriteString(Marker)
		b.WriteString(fingerprint(line[s.start:s.end]))
		prev = s.end
	}
	b.WriteString(line[prev:])
	return b.String()
}

type span struct{ start, end int }

// mergeSpans sorts and unions overlapping or touching ranges.
func mergeSpans(in []span) []span {
	sort.Slice(in, func(i, j int) bool {
		if in[i].start != in[j].start {
			return in[i].start < in[j].start
		}
		return in[i].end > in[j].end
	})

	out := in[:0:0]
	cur := in[0]
	for _, s := range in[1:] {
		if s.start <= cur.end {
			if s.end > cur.end {
				cur.end = s.end
			}
			continue
		}
		out = append(out, cur)
		cur = s
	}
	return append(out, cur)
}

// fingerprint is a stable six-character tag derived from what was matched, so
// an operator can correlate occurrences of the same secret across a log
// without learning it. Six hex characters is 24 bits: ample to distinguish the
// handful of secrets in one build, far too little to attack the value.
func fingerprint(matched string) string {
	sum := sha256.Sum256([]byte(matched))
	return hex.EncodeToString(sum[:3])
}
