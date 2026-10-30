package main

import (
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/Abyssinia-Labs/latem/internal/needs"
)

// How `latem workflows check` and `latem setup` say what jobs need (A59).

var kindNames = map[string]string{
	needs.KindIOS:         "iOS",
	needs.KindMacOSApp:    "a macOS app",
	needs.KindXcode:       "Xcode",
	needs.KindAndroid:     "Android",
	needs.KindLinuxDocker: "Linux with a Docker daemon",
	needs.KindLinux:       "Linux",
	needs.KindMacOS:       "macOS",
	needs.KindUnknown:     "not known",
}

// needsLine is a job's needs in one line: its kind, then each requirement.
func needsLine(r *needs.Report) string {
	parts := []string{kindNames[r.Kind]}
	for _, q := range r.Requires {
		parts = append(parts, requirementLabel(q))
	}
	if n := len(r.Unknown); n > 0 {
		parts = append(parts, fmt.Sprintf("%d thing%s not read", n, map[bool]string{true: "", false: "s"}[n == 1]))
	}
	return strings.Join(parts, " · ")
}

// requirementLabel is xcode 27, or xcode 27? when the version is only
// inferred, and the action that provides it.
func requirementLabel(q needs.Requirement) string {
	s := q.Key
	if q.Version != "" {
		s += " " + q.Version
		if q.VersionStrength == needs.Inferred {
			s += "?"
		}
	}
	if q.ProvidedBy != "" {
		s += " (" + q.ProvidedBy + ")"
	}
	return s
}

func evidenceText(e needs.Evidence) string {
	where := e.File
	switch {
	case e.Line > 0:
		where = fmt.Sprintf("%s:%d", e.File, e.Line)
	case e.Where != "":
		where = e.File + ", " + e.Where
	}
	return where + ": " + e.Snippet
}

// printNeedsEvidence says where each requirement, and each thing not read,
// was found: --verbose.
func printNeedsEvidence(w io.Writer, st styler, indent string, r *needs.Report) {
	for _, q := range r.Requires {
		printf(w, "%s%s %s\n", indent, st.dim(fmt.Sprintf("%-18s", requirementLabel(q))), evidenceText(q.Evidence))
	}
	for _, e := range r.Unknown {
		printf(w, "%s%s %s\n", indent, st.dim(fmt.Sprintf("%-18s", "not read")), evidenceText(e))
	}
}

// kindsSummary counts a report's jobs by kind, and names the Xcode versions
// the Apple ones need: "7 Linux, 2 iOS (Xcode 27)".
func kindsSummary(rep checkReport) string {
	count := map[string]int{}
	xcode := map[string]bool{}
	for _, wr := range rep.workflows {
		for _, j := range wr.jobs {
			if j.needs == nil {
				continue
			}
			count[j.needs.Kind]++
			if q, ok := j.needs.Get(needs.Xcode); ok && q.Version != "" {
				xcode[q.Version] = true
			}
		}
	}
	order := []string{needs.KindLinux, needs.KindLinuxDocker, needs.KindMacOS, needs.KindIOS, needs.KindMacOSApp,
		needs.KindXcode, needs.KindAndroid, needs.KindUnknown}
	var parts []string
	for _, k := range order {
		if n := count[k]; n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", n, kindNames[k]))
		}
	}
	if len(parts) == 0 {
		return ""
	}
	s := strings.Join(parts, ", ")
	if len(xcode) > 0 {
		vs := slices.Sorted(func(yield func(string) bool) {
			for v := range xcode {
				if !yield(v) {
					return
				}
			}
		})
		s += " — Xcode " + strings.Join(vs, ", ")
	}
	return s
}
