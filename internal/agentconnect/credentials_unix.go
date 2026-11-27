//go:build !windows

package agentconnect

import (
	"fmt"
	"os"
)

func makeCredentialDir(root *os.Root, name string) error {
	return root.Mkdir(name, 0700)
}

func checkCredentialAccess(_ *os.Root, name string, info os.FileInfo) error {
	if info.Mode().Perm()&0077 != 0 {
		return fmt.Errorf("credential path %s must not grant group or other permissions", name)
	}
	return nil
}
