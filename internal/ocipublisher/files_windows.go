package ocipublisher

import (
	"errors"
	"os"
)

func publicationPlatformError() error {
	return errors.New("native OCI publication is unsupported on Windows")
}

func openPublicationInput(_ string) (*os.File, error) { return nil, publicationPlatformError() }
func openPublicationLock(_ string) (*os.File, func(), error) {
	return nil, nil, publicationPlatformError()
}
