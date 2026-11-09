//go:build !unix

package secrets

import "os"

// checkOpenVaultSocket only asks that the path exists: OpenVault has no named
// pipe yet off Unix (service.md, not in v1).
func checkOpenVaultSocket(path string) error {
	_, err := os.Lstat(path)
	return err
}
