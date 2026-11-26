//go:build !windows

package agentconnect

import "syscall"

func writeCredentialFD(fd int, token string) error {
	_, err := syscall.Write(fd, []byte(token))
	return err
}
