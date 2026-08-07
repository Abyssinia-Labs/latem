//go:build deps

// This file is never built. `go mod tidy` considers every build configuration,
// so the blank imports below keep these modules in go.mod until the
// workstreams that own them start importing them for real.
//
// Each line names the workstream that will consume it, so a dependency that is
// still blank-imported long after its workstream landed is a visible signal
// that something changed.

package deps

import (
	_ "github.com/BurntSushi/toml"      // WS-04 config.toml
	_ "github.com/coder/websocket"      // WS-14 remote broker
	_ "github.com/docker/docker/client" // WS-07 docker executor
	_ "github.com/spf13/cobra"          // WS-12 CLI
	_ "modernc.org/sqlite"              // WS-05 store — cgo-free, required
	_ "sigs.k8s.io/yaml"                // WS-01 jobs.yaml
)
