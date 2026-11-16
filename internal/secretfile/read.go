// Package secretfile reads bounded operator secrets confined to RCP_SECRET_DIR.
package secretfile

import (
	"errors"
	"io"
	"os"
	"path/filepath"
)

func Read(ref string) ([]byte, error) {
	bad := errors.New("credential unavailable")
	if ref == "" || ref == "." || ref == ".." || filepath.Base(ref) != ref {
		return nil, bad
	}
	dir := os.Getenv("RCP_SECRET_DIR")
	if dir == "" {
		dir = "/run/secrets"
	}
	// os.Root confines symlinks even across concurrent rename/symlink changes.
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, bad
	}
	defer root.Close()
	f, err := root.Open(ref)
	if err != nil {
		return nil, bad
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, 65537))
	if err != nil || len(b) > 65536 {
		return nil, bad
	}
	return b, nil
}
