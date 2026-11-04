//go:build unix

package intelligencepatch

import (
	"os"
	"syscall"
)

func openNoFollow(parent *os.Root, name string) (*os.File, error) {
	return parent.OpenFile(name, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
}
