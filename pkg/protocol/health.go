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
	// Repos are the repositories this machine builds, which is how a GitHub
	// event finds the machines that can plan and run it (A52).
	Repos []RepoInfo `json:"repos,omitempty"`
	// Toolchains is what the machine has for builds (A61, 2.2), sent only to
	// a control plane that says 2.2. It explains; nothing routes on it.
	Toolchains *Toolchains `json:"toolchains,omitempty"`
}

// Toolchains is what a machine has for builds, as a step on its native
// runners would find it (A61): versions and names, never a path.
type Toolchains struct {
	Xcode            []string `json:"xcode,omitempty"` // "26.0.1"
	IOSSimulator     bool     `json:"ios_simulator,omitempty"`
	JDK              []int    `json:"jdk,omitempty"` // majors
	AndroidSDK       bool     `json:"android_sdk,omitempty"`
	AndroidPlatforms []int    `json:"android_platforms,omitempty"`
	AndroidEmulator  bool     `json:"android_emulator,omitempty"`
	// Tools are the probed commands a step's PATH finds: "xcodegen", "docker".
	Tools            []string `json:"tools,omitempty"`
	PasswordlessSudo bool     `json:"passwordless_sudo,omitempty"`
}

// RepoInfo is one configured repository: the key a run or plan names it by,
// and the owner/name GitHub knows it as (A50). GitHub is empty for a
// repository that is not on GitHub.
type RepoInfo struct {
	Key    string `json:"key"`
	GitHub string `json:"github,omitempty"`
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
