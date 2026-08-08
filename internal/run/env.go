package run

import (
	"github.com/Abyssinia-Labs/latem/internal/registry"
)

// LatemEnvVar is the name of §9's between-step channel. Its *value* comes from
// [executor.Env.EnvFile] and is never constructed here: the path is a container
// path for docker, a guest path for tart and a host path for native, and only
// the executor knows which (amendment A9).
const LatemEnvVar = "LATEM_ENV"

// buildEnv assembles the run environment (RUNNER_SPEC §9, §15).
//
// Three layers, in this order, and the order is the contract:
//
//  1. Resolved secrets, by declared name. This is the one place in the entire
//     system where a secret becomes a Go string. §15 keeps values as []byte
//     inside internal/secrets so they can be zeroed when the run ends, which a
//     string cannot be — the conversion happens here, at the last moment, into
//     a map that is dropped with the run (finding F10).
//  2. LATEM_* from the workspace, plus the job name. They are the agent's own
//     variables and they win, so a job cannot make LATEM_SHA mean something
//     other than the commit it is building.
//  3. steps[].env, which is per step and therefore not here — it is merged by
//     the executor over this map for one step only. Its ${input.NAME}
//     references were substituted by Registry.Admit; nothing in this package
//     interpolates anything.
//
// Secrets arrive by name because the job declared them. They are never
// interpolated, into steps[].run or anywhere else — §9 leaves no syntax that
// could.
func (x *execution) buildEnv() map[string]string {
	env := make(map[string]string, x.secretCount()+8)

	if x.sec != nil {
		for _, name := range x.sec.Names() {
			if v, ok := x.sec.Value(name); ok {
				env[name] = string(v)
			}
		}
	}

	for k, v := range x.ws.Env() {
		env[k] = v
	}
	env["LATEM_JOB"] = x.resolved.JobName

	return env
}

// secretNames is [executor.Spec.Secrets] (amendment A10).
//
// The executor needs it to enforce §9's rule that $LATEM_ENV cannot overwrite a
// declared secret. Before the field existed the executor had to protect every
// key it was handed — safe, but it silently refused legal overrides of ordinary
// variables like NODE_ENV, and the job author had no way to tell why.
//
// An executor handed an empty list with a non-empty environment falls back to
// protecting everything, so a job with no secrets loses nothing by this being
// nil.
func (x *execution) secretNames() []string {
	if x.sec == nil {
		return nil
	}
	return x.sec.Names()
}

func (x *execution) secretCount() int {
	if x.sec == nil {
		return 0
	}
	return x.sec.Len()
}

// stepEnv is what is merged over the run environment for one step.
//
// steps[].env comes from the registry with ${input.NAME} already substituted.
// $LATEM_ENV is added last and therefore wins: it is the agent's variable, and
// a job redefining it would break the channel §9 promises rather than redirect
// it somewhere useful.
func (x *execution) stepEnv(s registry.ResolvedStep) map[string]string {
	env := make(map[string]string, len(s.Env)+1)
	for k, v := range s.Env {
		env[k] = v
	}
	if x.env != nil {
		if p := x.env.EnvFile(); p != "" {
			env[LatemEnvVar] = p
		}
	}
	return env
}
