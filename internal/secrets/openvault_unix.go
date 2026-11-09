//go:build unix

package secrets

import (
	"errors"
	"io/fs"
	"os"
	"syscall"
)

// checkOpenVaultSocket refuses a socket that is not one, or that another user
// owns.
//
// The service checks that its peer is its own user; this is the other
// direction. OpenVault's directory is 0700, so the case it covers is an
// OPENVAULT_HOME pointing somewhere shared, where anyone could listen at that
// path and hand a build whatever values they liked.
func checkOpenVaultSocket(path string) error {
	fi, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if fi.Mode().Type() != fs.ModeSocket {
		return errors.New("its socket path is not a socket")
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok && int(st.Uid) != os.Getuid() {
		return errors.New("its socket belongs to another user")
	}
	return nil
}
