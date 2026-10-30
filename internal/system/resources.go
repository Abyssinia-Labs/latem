package system

import (
	"bufio"
	"bytes"
	"context"
	"os"
	"strconv"
	"strings"
)

// Resources reports the machine's CPUs and memory in bytes, for capacity's
// recommendation (A63). Zero means it could not be read.
//
// A Mac answers through sysctl; Linux through getconf and /proc/meminfo, read
// under the system's root so a test never reads the machine it runs on.
func (s *System) Resources(ctx context.Context) (cpus int, memory uint64) {
	if s.IsMacOS() {
		cpus = s.intOf(ctx, "sysctl", "-n", "hw.ncpu")
		if out, err := s.Exec(ctx, "sysctl", "-n", "hw.memsize"); err == nil {
			memory, _ = strconv.ParseUint(strings.TrimSpace(string(out)), 10, 64)
		}
		return cpus, memory
	}
	cpus = s.intOf(ctx, "getconf", "_NPROCESSORS_ONLN")
	data, err := os.ReadFile(s.sysPath("proc/meminfo"))
	if err != nil {
		return cpus, 0
	}
	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() {
		// MemTotal:       16314424 kB
		fields := strings.Fields(sc.Text())
		if len(fields) >= 2 && fields[0] == "MemTotal:" {
			kb, err := strconv.ParseUint(fields[1], 10, 64)
			if err == nil {
				memory = kb * 1024
			}
			break
		}
	}
	return cpus, memory
}

func (s *System) intOf(ctx context.Context, name string, args ...string) int {
	out, err := s.Exec(ctx, name, args...)
	if err != nil {
		return 0
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil {
		return 0
	}
	return n
}
