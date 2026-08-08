//go:build !darwin && !linux

package artifact

import (
	"fmt"
	"runtime"
)

func mkfifo(string) error { return fmt.Errorf("no fifo on %s", runtime.GOOS) }
