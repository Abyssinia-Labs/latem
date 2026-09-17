package native

// DefaultLang is the locale a step's environment carries unless something above
// it says otherwise (RUNNER_SPEC §9, §13.3, amendment A33).
//
// # Why the agent sets this at all
//
// §9 says a step is a new process with the environment the agent hands it, and
// that environment used to have no locale. That is correct by the letter of §9
// and wrong in practice, because every shell an operator tests in has one. The
// symptom was `pod install` aborting with
//
//	Unicode Normalization not appropriate for ASCII-8BIT (Encoding::CompatibilityError)
//
// which names Unicode rather than the locale, so it reads as a corrupt file.
// The class is larger than CocoaPods: Ruby's Encoding.default_external,
// Python's filesystem encoding, and Java's file.encoding are all decided by it,
// and each of them fails differently and describes something else.
//
// # Why it is a base layer rather than an agent-owned variable
//
// It sits *below* the run environment, so `steps[].env` and the run's own
// variables both win. A job that means to build under a different locale says
// so and is obeyed; a job that says nothing gets a working one. LATEM_* is the
// opposite case — those are the agent's assertions about what is being built
// and a job may not redefine them — and conflating the two would make a
// reasonable override impossible.
//
// # Why LANG and not LC_ALL
//
// LC_ALL overrides every LC_* category at once, including ones the inherited
// environment sets deliberately. LANG is the fallback the affected tools
// actually read, so setting it fixes the defect without overruling an operator
// who has configured LC_TIME or LC_COLLATE on purpose.
//
// The cost of that choice, stated because it is a real hole: a login shell's
// profile runs *after* execve, so `export LC_ALL=C` in ~/.bash_profile still
// wins over this. `latem doctor` probes the locale through the same login shell
// a step gets, precisely so that machine says so before a build does.
//
// # Why only this executor
//
// A container brings its own locale and its image author chose it. Setting
// en_US.UTF-8 into an image that never generated that locale produces
// `setlocale: cannot change locale` from glibc and leaves the process no better
// off, so §13.1 leaves the image's own ENV alone. Here there is no image, the
// locale is guaranteed present on macOS, and the alternative is whatever
// launchd happened to hand the agent — which is nothing.
const DefaultLang = "en_US.UTF-8"

// baseEnv is the layer beneath a run's own environment.
//
// A function rather than a package-level map so that no caller can mutate the
// defaults for every subsequent run in the process.
func baseEnv() map[string]string {
	return map[string]string{"LANG": DefaultLang}
}
