package ociverify

import (
	"errors"
	"releasecontrol/internal/domain"
)

func NormalizeRuntimeRootFS(_, _ string, _ int64, _ int) (domain.WorkerRuntimeRootFS, error) {
	return domain.WorkerRuntimeRootFS{}, errors.New("runtime root filesystem normalization is unsupported on Windows")
}
