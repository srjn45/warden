//go:build !unix

package agentstore

import "os"

// CheckRepairAuthority on non-unix platforms only checks the directory exists.
func CheckRepairAuthority(dataDir string) error {
	_, err := os.Stat(dataDir)
	return err
}
