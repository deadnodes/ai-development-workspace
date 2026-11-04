//go:build !unix

package intelligencepatch

import (
	"errors"
	"os"
)

func openNoFollow(_ *os.Root, _ string) (*os.File, error) {
	return nil, errors.New("small conflict patches require a Unix worker")
}
