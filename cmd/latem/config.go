package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/Abyssinia-Labs/latem/internal/config"
)

// printf writes terminal output, discarding the write error.
//
// Every use is a line for a human. The only way one of these fails is a closed
// pipe — `latem cache ls | head` — and by then the process has already been
// signalled; checking each of forty format calls would obscure the code for a
// condition nothing can act on. Anything whose delivery actually matters
// returns an error from its command instead.
func printf(w io.Writer, format string, args ...any) {
	_, _ = fmt.Fprintf(w, format, args...)
}

// loadConfig reads and validates the agent config.
//
// Every command that touches state goes through here rather than through
// config.Load directly, so --config-dir applies uniformly. §8's promise is
// that one flag relocates the lock, the socket, the sqlite database, the work
// directory, and the caches together; a command that resolved its own path
// would break that promise for exactly one file.
func loadConfig() (*config.Config, error) {
	return config.Load(config.Options{Dir: global.configDir})
}

// configDir resolves the config directory without reading the config.
//
// `latem doctor` needs it on the machine where config.toml is missing,
// unreadable, or invalid — which is the machine doctor exists for. The
// resolution order matches internal/config: the flag beats the environment
// variable, which beats ~/.latem. A flag beating an environment variable is
// the ordering people expect when they are debugging a second agent by hand.
//
// This duplicates six lines of internal/config because that package exposes no
// way to derive [config.Paths] for a directory whose config did not parse:
// config.Parse returns a nil *Config on any validation failure, so the paths
// go with it. Reported as a contract gap rather than worked around silently.
func configDir() string {
	if global.configDir != "" {
		return expandHome(global.configDir)
	}
	if dir := os.Getenv(config.EnvConfigDir); dir != "" {
		return expandHome(dir)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return config.DirName
	}
	return filepath.Join(home, config.DirName)
}

// configFile is the config.toml inside the resolved config directory.
func configFile() string { return filepath.Join(configDir(), config.FileName) }

func expandHome(p string) string {
	if p == "~" || len(p) > 1 && p[:2] == "~/" {
		home, err := os.UserHomeDir()
		if err != nil {
			return p
		}
		if p == "~" {
			return home
		}
		return filepath.Join(home, p[2:])
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return p
	}
	return abs
}
