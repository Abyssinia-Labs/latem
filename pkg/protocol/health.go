package protocol

// MachineHealth is published every 15 seconds. A control plane marks a machine
// offline after three missed beats (RUNNER_SPEC §20).
type MachineHealth struct {
	MachineID  string       `json:"machine_id"`
	AgentVer   string       `json:"agent_version"`
	OS         string       `json:"os"`
	Arch       string       `json:"arch"`
	Runners    []RunnerInfo `json:"runners"`
	DiskFreeGB float64      `json:"disk_free_gb"`
	// Draining means the agent is shutting down: in-flight runs continue,
	// no new ones are accepted. Heartbeats continue throughout, because a
	// machine that stops beating and a machine that is draining need to look
	// different to whoever is watching.
	Draining bool `json:"draining"`
}

// RunnerInfo describes one execution slot.
type RunnerInfo struct {
	Name        string   `json:"name"`
	Executor    string   `json:"executor"` // "docker" | "tart" | "native"
	Labels      []string `json:"labels"`
	Concurrency int      `json:"concurrency"`
	Active      int      `json:"active"`
	// Available is false when the runner failed preflight — Docker
	// unreachable, no GPU toolkit, Tart image missing, disk below its floor.
	// An unavailable runner accepts no work; recovery is automatic when the
	// check passes again (§20).
	Available bool `json:"available"`
	// Unavailable carries the reason, for display. Empty when Available.
	Unavailable string `json:"unavailable_reason,omitempty"`
}

// Resume is sent by the agent immediately after reconnecting, before anything
// else, so the control plane can replay what was missed and pick log ingestion
// back up at the right offset (RUNNER_SPEC §7.4).
type Resume struct {
	MachineID string      `json:"machine_id"`
	Runs      []RunResume `json:"runs"`
}

// RunResume is one non-terminal run the agent still owns, and where its log
// stream left off.
type RunResume struct {
	RunID string   `json:"run_id"`
	State RunState `json:"state"`
	// LastAckedSeq is the highest [LogFrame.Seq] the control plane durably
	// accepted. The agent resumes from LastAckedSeq+1. It is persisted, so the
	// resume point survives an agent restart and not merely a reconnect.
	LastAckedSeq uint64 `json:"last_acked_seq"`
}
