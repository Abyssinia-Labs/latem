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
	// Capacity is what the machine has to run jobs on and what each runner's
	// concurrency should be (A63, 2.3), sent only to a control plane that
	// says 2.3. Like Toolchains it explains: the config decides.
	Capacity *Capacity `json:"capacity,omitempty"`
	// LabelAliases is the machine's [label_aliases] (A68, 2.4), sent only to
	// a control plane that says 2.4: how a job's runs_on reads on this
	// machine, which is how it will compile here.
	LabelAliases map[string][]string `json:"label_aliases,omitempty"`
	// Readiness is what `latem setup` says of this machine (A70, 2.5), sent
	// only to a control plane that says 2.5. Like Toolchains it explains:
	// nothing routes on it, and nothing on the far side can act on it.
	Readiness *Readiness `json:"readiness,omitempty"`
}

// Readiness is each kind of build in a machine's repositories, where it
// stands here, and what is in the way, in a person's words. It carries no
// path, no action, no secret's name and nothing of a job's steps.
type Readiness struct {
	Kinds []ReadinessKind `json:"kinds"`
	// Unread counts the repositories this machine could not read, and
	// Unclassified the jobs whose kind could not be read from them.
	Unread       int `json:"unread,omitempty"`
	Unclassified int `json:"unclassified,omitempty"`
}

// ReadinessKind is one kind of build on one machine.
type ReadinessKind struct {
	Kind  string `json:"kind"`  // "android"
	Title string `json:"title"` // "Android"
	// State is ready, missing, off or impossible.
	State string `json:"state"`
	// Why says what is not obvious from State: "needs a Mac".
	Why string `json:"why,omitempty"`
	// Runner is the runner that takes it, or would.
	Runner string `json:"runner,omitempty"`
	// Repos are the keys of the repositories with jobs of this kind, and
	// Jobs how many jobs that is.
	Repos []string `json:"repos,omitempty"`
	Jobs  int      `json:"jobs,omitempty"`
	// Elsewhere counts the jobs a label here would still not bring, because
	// their workflows ask for another machine's labels.
	Elsewhere int             `json:"elsewhere,omitempty"`
	Items     []ReadinessItem `json:"items,omitempty"`
	// More counts what is in the way and not in Items.
	More int `json:"more,omitempty"`
}

// ReadinessItem is one thing in the way.
type ReadinessItem struct {
	Title string `json:"title"`
	// Have is what the machine has instead: "JDK [27]".
	Have string `json:"have,omitempty"`
	// Soft is worth saying and not worth failing for: a job already running
	// here has got by without it.
	Soft bool `json:"soft,omitempty"`
}

// Capacity is a machine's size and the concurrency latem recommends for each
// of its runners (A63).
type Capacity struct {
	CPUs     int     `json:"cpus"`
	MemoryGB float64 `json:"memory_gb"`
	// Docker is what the Docker daemon is given — Docker Desktop's VM on a
	// Mac, the host itself on Linux — when a daemon answered.
	Docker  *DockerCapacity  `json:"docker,omitempty"`
	Runners []RunnerCapacity `json:"runners,omitempty"`
}

// DockerCapacity is what a Docker daemon says it can use.
type DockerCapacity struct {
	CPUs     int     `json:"cpus"`
	MemoryGB float64 `json:"memory_gb"`
}

// RunnerCapacity is one runner's recommended concurrency, and why.
type RunnerCapacity struct {
	Name        string `json:"name"`
	Recommended int    `json:"recommended"`
	Why         string `json:"why"`
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
	// Vars is a digest of each of the repository's variables, by name (A68,
	// 2.4), sent only to a control plane that says 2.4: enough to tell two
	// machines' answers apart. An unset variable is an absent name.
	Vars map[string]string `json:"vars,omitempty"`
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
