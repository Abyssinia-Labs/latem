package main

import (
	"encoding/json"
	"io"
)

// The report as JSON, for a script (`latem workflows check --json`). Field
// names are snake_case and stable; a level is "pass", "warn" or "fail".

type checkJSON struct {
	Source     string         `json:"source"`
	Commit     string         `json:"commit,omitempty"`
	Repository string         `json:"repository,omitempty"`
	RepoKey    string         `json:"repo_key,omitempty"`
	Branch     string         `json:"branch"`
	Jobs       int            `json:"jobs"`
	Failed     int            `json:"failed"`
	Broken     int            `json:"broken"`
	Workflows  []workflowJSON `json:"workflows"`
	Suggested  map[string]any `json:"suggested,omitempty"`
	Fixed      []string       `json:"fixed,omitempty"`
}

type workflowJSON struct {
	File     string        `json:"file"`
	Name     string        `json:"name"`
	Events   []string      `json:"events,omitempty"`
	Triggers []string      `json:"triggers,omitempty"`
	Error    string        `json:"error,omitempty"`
	Findings []findingJSON `json:"findings,omitempty"`
	Jobs     []jobJSON     `json:"jobs,omitempty"`
}

type jobJSON struct {
	Key      string        `json:"key"`
	Name     string        `json:"name"`
	Level    string        `json:"level"`
	Runner   string        `json:"runner,omitempty"`
	Findings []findingJSON `json:"findings,omitempty"`
}

type findingJSON struct {
	Level string   `json:"level"`
	Text  string   `json:"text"`
	Fixes []string `json:"fixes,omitempty"`
}

func levelName(l level) string {
	switch l {
	case levelPass:
		return "pass"
	case levelWarn:
		return "warn"
	case levelFail:
		return "fail"
	default:
		return "skip"
	}
}

func findingsJSON(fs []finding) []findingJSON {
	var out []findingJSON
	for _, f := range fs {
		out = append(out, findingJSON{Level: levelName(f.level), Text: f.text, Fixes: f.fixes})
	}
	return out
}

func printCheckJSON(w io.Writer, t checkTarget, rep checkReport, fixed []string) error {
	out := checkJSON{
		Source: t.dir, Commit: t.commit, Repository: t.slug, RepoKey: t.repoKey, Branch: t.branch,
		Jobs: rep.jobs(), Failed: rep.failed(), Broken: rep.broken(), Fixed: fixed,
		Workflows: []workflowJSON{},
	}
	if t.fsys != nil {
		out.Source = t.where
	}
	if rep.hasFixes() {
		out.Suggested = map[string]any{}
		if len(rep.aliases) > 0 {
			out.Suggested["label_aliases"] = rep.aliases
		}
		if len(rep.allowed) > 0 {
			out.Suggested["actions_allowed"] = rep.allowed
		}
	}
	for _, wr := range rep.workflows {
		wj := workflowJSON{
			File: wr.file, Name: wr.name, Events: wr.events, Triggers: wr.triggers,
			Error: wr.err, Findings: findingsJSON(wr.findings),
		}
		for _, j := range wr.jobs {
			wj.Jobs = append(wj.Jobs, jobJSON{
				Key: j.key, Name: j.name, Level: levelName(j.level()), Runner: j.runner,
				Findings: findingsJSON(j.findings),
			})
		}
		out.Workflows = append(out.Workflows, wj)
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}
