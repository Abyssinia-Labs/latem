package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/Abyssinia-Labs/latem/internal/executor/native"
)

// A33. This check and [doctor.checkSchemes] are the same shape: a property that
// is true on every developer's machine and false on every runner, so neither
// failure can be found by testing the agent against itself.

// checkLocale reports the locale a step's shell actually resolves to (A33).
//
// The interesting part is that this cannot be answered by reading the executor's
// code. §13.3 runs each step as `bash -lc`, and a *login* shell sources
// /etc/profile and ~/.bash_profile **after** execve — so an `export LC_ALL=C`
// in a profile overrules the LANG the executor sets, and the only honest way to
// know what a step gets is to ask the same shell the same way.
//
// `env(1)` is argv[0] so that LANG is in the child's environment before bash
// starts, which is the order a real step sees.
func (d *doctor) checkLocale(ctx context.Context) check {
	const id, title = "macos.locale", "step locale"

	out, err := d.sys.Exec(ctx, "env", "LANG="+native.DefaultLang, native.DefaultShell, "-lc",
		`printf '%s\t%s\t%s\n' "${LANG-}" "${LC_ALL-}" "$(locale charmap 2>/dev/null)"`)
	if err != nil {
		return warn(id, title, fmt.Sprintf("could not probe %s: %v", native.DefaultShell, err))
	}

	fields := strings.Split(strings.TrimSpace(firstLineOf(strings.TrimSpace(string(out)))), "\t")
	for len(fields) < 3 {
		fields = append(fields, "")
	}
	lang, lcAll, charmap := fields[0], fields[1], fields[2]

	if isUTF8(charmap) {
		detail := fmt.Sprintf("LANG=%s → %s", lang, charmap)
		if lcAll != "" {
			detail += fmt.Sprintf(", LC_ALL=%s", lcAll)
		}
		return pass(id, title, detail)
	}

	// Two distinguishable causes, and they have different fixes. A profile
	// setting LC_ALL is the operator's own machine overruling the agent; no
	// UTF-8 at all is the locale being unavailable or unset.
	if lcAll != "" && !isUTF8(lcAll) {
		return fail(id, title,
			fmt.Sprintf("LC_ALL=%s in the login shell's profile overrides LANG=%s (charmap %s)",
				lcAll, native.DefaultLang, orUnknown(charmap)),
			`grep -rn "LC_ALL" ~/.bash_profile ~/.bashrc ~/.profile /etc/profile`,
			"# then remove it, or set it to a UTF-8 locale").
			withNotes("The agent sets LANG in every step's environment (A33), but a step is `bash "+
				"-lc` and a login shell reads its profile after execve — so LC_ALL there wins, and "+
				"it overrides every LC_* category at once.",
				"The symptom is `pod install` aborting with \"Unicode Normalization not appropriate "+
					"for ASCII-8BIT\", which names Unicode rather than the locale. Ruby, Python and "+
					"Java tools all read this and each fails describing something else.")
	}

	return fail(id, title,
		fmt.Sprintf("a step's shell resolves to %s, not UTF-8 (LANG=%s)", orUnknown(charmap), orUnset(lang)),
		fmt.Sprintf("locale -a | grep -i utf   # confirm %s exists on this machine", native.DefaultLang)).
		withNotes("The agent sets LANG=" + native.DefaultLang + " for every step (A33). If the shell " +
			"still reports a non-UTF-8 charmap, something in the profile is unsetting it or the locale " +
			"is not installed.")
}

func isUTF8(s string) bool {
	up := strings.ToUpper(s)
	return strings.Contains(up, "UTF-8") || strings.Contains(up, "UTF8")
}

func orUnknown(s string) string {
	if s == "" {
		return "an unknown charmap"
	}
	return s
}

func orUnset(s string) string {
	if s == "" {
		return "unset"
	}
	return s
}
