// Package capacity works out how many jobs each runner on a machine should
// take at once, from what the machine has (amendment A63).
//
// It recommends and explains; it never changes a config. A runner set above
// its recommendation still runs what it is given, and doctor says why that is
// a risk: jobs that share too few CPUs or too little memory fail in ways that
// look like flaky tests — a timeout, an OOM-killed compiler, a test that
// expected to finish in a second — and never name the machine.
package capacity

import (
	"fmt"
	"math"

	"github.com/Abyssinia-Labs/latem/internal/config"
)

// Per job, what a CI job is sized for: GitHub's own Linux runners give each
// job 2 to 4 CPUs and 7 to 16 GB, and the jobs this was written against
// (bun, Go and Convex builds) need about 2 CPUs and 4 GB at their peak.
const (
	JobCPUs     = 2
	JobMemoryGB = 4.0
)

// A macOS VM under Tart: the images it boots ask for 8 GB, and the host keeps
// about 8 GB for itself, Xcode's indexer and the VM's own overhead. Apple's
// licence caps the guests at two (§13.2), as the config check does.
const (
	TartVMMemoryGB   = 8.0
	TartHostMemoryGB = 8.0
)

// Machine is what the recommendation is worked out from.
type Machine struct {
	CPUs     int
	MemoryGB float64
	// Docker is what the Docker daemon is given — Docker Desktop's VM on a
	// Mac, the host on Linux — when one answered.
	Docker *Docker
}

// Docker is what a Docker daemon says it can use.
type Docker struct {
	CPUs     int
	MemoryGB float64
}

// Advice is one runner's recommendation.
type Advice struct {
	Runner      string
	Executor    config.Executor
	Configured  int
	Recommended int
	// Why says what the number comes from, in a sentence a person can check.
	Why string
}

// Over reports a runner set to take more jobs at once than it has room for.
func (a Advice) Over() bool { return a.Configured > a.Recommended }

// Recommend works out each runner's concurrency. Docker runners share one
// daemon, so its room is divided between them.
func Recommend(m Machine, runners []*config.Runner) []Advice {
	dockers := 0
	for _, r := range runners {
		if r.Executor == config.ExecutorDocker {
			dockers++
		}
	}
	out := make([]Advice, 0, len(runners))
	for _, r := range runners {
		a := Advice{Runner: r.Name, Executor: r.Executor, Configured: r.Concurrency}
		switch r.Executor {
		case config.ExecutorNative:
			a.Recommended = 1
			a.Why = "native runs one job at a time: jobs on one host share its caches, and on a Mac its DerivedData, keychain and simulators"
		case config.ExecutorDocker:
			a.Recommended, a.Why = docker(m, dockers)
		case config.ExecutorTart:
			a.Recommended, a.Why = tart(m)
		default:
			a.Recommended, a.Why = 1, "an executor latem does not size"
		}
		out = append(out, a)
	}
	return out
}

// MaxSlots caps a recommendation: the control plane takes 1 to 64 (A63),
// and a runner taking more than 64 jobs at once is a machine latem has not
// been tried on.
const MaxSlots = 64

// Slots is how many CI-sized jobs fit in cpus and memoryGB: at least one, at
// most MaxSlots.
func Slots(cpus int, memoryGB float64) int {
	n := min(cpus/JobCPUs, int(math.Floor(memoryGB/JobMemoryGB)), MaxSlots)
	return max(n, 1)
}

func docker(m Machine, runners int) (int, string) {
	cpus, mem, whose := m.CPUs, m.MemoryGB, "this machine has"
	if m.Docker != nil {
		cpus, mem, whose = m.Docker.CPUs, m.Docker.MemoryGB, "Docker has"
	}
	slots := Slots(cpus, mem)
	why := fmt.Sprintf("%s %d CPUs and %s GB, and a job takes about %d CPUs and %s GB",
		whose, cpus, GB(mem), JobCPUs, GB(JobMemoryGB))
	if runners > 1 {
		why += fmt.Sprintf("; %d docker runners share that", runners)
		slots = max(slots/runners, 1)
	}
	return slots, why
}

func tart(m Machine) (int, string) {
	fit := int(math.Floor((m.MemoryGB - TartHostMemoryGB) / TartVMMemoryGB))
	n := min(max(fit, 1), config.TartMaxConcurrency)
	why := fmt.Sprintf("this Mac has %s GB: a macOS VM takes about %s GB and the host keeps %s, and Apple's licence allows %d VMs",
		GB(m.MemoryGB), GB(TartVMMemoryGB), GB(TartHostMemoryGB), config.TartMaxConcurrency)
	if fit < 1 {
		why = fmt.Sprintf("this Mac has %s GB, less than a macOS VM (%s GB) and the host (%s GB) need: even one VM will swap",
			GB(m.MemoryGB), GB(TartVMMemoryGB), GB(TartHostMemoryGB))
	}
	return n, why
}

// GB prints a size the way a person reads it: "8", "3.8".
func GB(v float64) string {
	r := math.Round(v*10) / 10
	if r == math.Trunc(r) {
		return fmt.Sprintf("%.0f", r)
	}
	return fmt.Sprintf("%.1f", r)
}

// BytesToGB is a byte count as GB, binary: the 8 GB a Mac's settings show is
// 8 GiB of hw.memsize.
func BytesToGB(n uint64) float64 { return math.Round(float64(n)/(1<<30)*10) / 10 }
