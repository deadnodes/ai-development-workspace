package agentconnect

import "errors"

func writeCredentialFD(_ int, _ string) error {
	return errors.New("--stdout-to-fd is unsupported on Windows")
}
