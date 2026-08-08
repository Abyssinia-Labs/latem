package system

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
)

// PathMode is the permission state of one path the agent owns.
type PathMode struct {
	Path   string
	Exists bool
	IsDir  bool
	// Mode is the permission bits as found.
	Mode fs.FileMode
	// Want is the mode RUNNER_SPEC §23 requires.
	Want fs.FileMode
	// OK reports whether the path is no more permissive than Want.
	//
	// "No more permissive" rather than "equal": a 0400 token file is stricter
	// than the 0600 the spec names and is not a fault. What matters is that
	// nothing is reachable by group or other, because a world-readable
	// ~/.latem holding a machine token and every secret is the whole threat
	// model failing quietly (§8, §23).
	OK bool
	// Err is why the path could not be inspected. A missing path is not an
	// error — many of these appear only after the agent has run once.
	Err error
}

// FixCommand is the chmod that would repair this path.
func (p PathMode) FixCommand() string {
	return fmt.Sprintf("chmod %04o %s", p.Want.Perm(), p.Path)
}

// InspectMode reports whether path is no more permissive than want.
func InspectMode(path string, want fs.FileMode) PathMode {
	p := PathMode{Path: path, Want: want}
	if path == "" {
		return p
	}
	fi, err := os.Stat(path)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			p.Err = err
		}
		return p
	}
	p.Exists = true
	p.IsDir = fi.IsDir()
	p.Mode = fi.Mode().Perm()
	p.OK = p.Mode&^want.Perm() == 0
	return p
}
