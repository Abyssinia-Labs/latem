package capacity

import (
	"strings"
	"testing"

	"github.com/Abyssinia-Labs/latem/internal/config"
)

func runner(name string, ex config.Executor, n int) *config.Runner {
	return &config.Runner{Name: name, Executor: ex, Concurrency: n}
}

// The Mac mini: 8 CPUs, 8 GB, Docker Desktop given 6 CPUs and 3.8 GB.
func TestRecommend_TheMacMini(t *testing.T) {
	m := Machine{CPUs: 8, MemoryGB: 8, Docker: &Docker{CPUs: 6, MemoryGB: 3.8}}
	got := Recommend(m, []*config.Runner{
		runner("macos", config.ExecutorNative, 1),
		runner("linux", config.ExecutorDocker, 2),
	})
	if got[0].Recommended != 1 || got[0].Over() {
		t.Errorf("native = %+v, want 1 and not over", got[0])
	}
	// 3.8 GB holds less than one 4 GB job; one is still the floor.
	if got[1].Recommended != 1 || !got[1].Over() {
		t.Errorf("docker = %+v, want 1 and over at 2", got[1])
	}
	if !strings.Contains(got[1].Why, "Docker has 6 CPUs and 3.8 GB") {
		t.Errorf("why = %q, want Docker's own numbers", got[1].Why)
	}
}

// Memory, not CPUs, is usually what runs out: 16 CPUs and 16 GB is four jobs.
func TestRecommend_DockerTakesTheSmallerOfCPUsAndMemory(t *testing.T) {
	cases := []struct {
		cpus int
		mem  float64
		want int
	}{
		{16, 16, 4},
		{4, 64, 2},
		{1, 2, 1},
		{12, 7.7, 1},
	}
	for _, c := range cases {
		got := Recommend(Machine{CPUs: c.cpus, MemoryGB: c.mem}, []*config.Runner{runner("l", config.ExecutorDocker, 1)})
		if got[0].Recommended != c.want {
			t.Errorf("%d CPUs, %v GB = %d, want %d", c.cpus, c.mem, got[0].Recommended, c.want)
		}
	}
}

// Two docker runners share one daemon.
func TestRecommend_DockerRunnersShareTheDaemon(t *testing.T) {
	got := Recommend(Machine{CPUs: 16, MemoryGB: 32}, []*config.Runner{
		runner("a", config.ExecutorDocker, 4),
		runner("b", config.ExecutorDocker, 4),
	})
	if got[0].Recommended != 4 || got[1].Recommended != 4 || !strings.Contains(got[0].Why, "2 docker runners share") {
		t.Errorf("got %+v", got)
	}
}

func TestRecommend_Tart(t *testing.T) {
	cases := []struct {
		mem  float64
		want int
		says string
	}{
		{8, 1, "even one VM will swap"},
		{16, 1, "licence allows 2"},
		{24, 2, "licence allows 2"},
		{64, 2, "licence allows 2"},
	}
	for _, c := range cases {
		got := Recommend(Machine{CPUs: 8, MemoryGB: c.mem}, []*config.Runner{runner("vm", config.ExecutorTart, 1)})
		if got[0].Recommended != c.want || !strings.Contains(got[0].Why, c.says) {
			t.Errorf("%v GB = %+v, want %d and %q", c.mem, got[0], c.want, c.says)
		}
	}
}

func TestGB(t *testing.T) {
	for in, want := range map[float64]string{8: "8", 3.84: "3.8", 15.96: "16", 0.5: "0.5"} {
		if got := GB(in); got != want {
			t.Errorf("GB(%v) = %q, want %q", in, got, want)
		}
	}
	if got := BytesToGB(8589934592); got != 8 {
		t.Errorf("BytesToGB(8 GiB) = %v, want 8", got)
	}
}
