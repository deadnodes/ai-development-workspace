package ocipublisher

import (
	"releasecontrol/internal/domain"
	"strings"
	"testing"
)

func TestRuntimeReceiptPinsAreTypedAndBounded(t *testing.T) {
	c := fixture(t, "https://registry.example.invalid")
	spec := domain.WorkerRuntimeRootFS{Hash: "sha256:" + strings.Repeat("a", 64), InputsHash: "sha256:" + strings.Repeat("b", 64), ArchiveBytes: 10240, MaxExpandedBytes: 1 << 20, MaxEntries: 32}
	c.Plan.Receipt.RuntimeRootFS = &spec
	if _, e := c.validate(); e != nil {
		t.Fatal(e)
	}
	for _, mutate := range []func(*domain.WorkerRuntimeRootFS){func(s *domain.WorkerRuntimeRootFS) { s.Hash = "../path" }, func(s *domain.WorkerRuntimeRootFS) { s.InputsHash = "missing" }, func(s *domain.WorkerRuntimeRootFS) { s.ArchiveBytes = 129 << 20 }, func(s *domain.WorkerRuntimeRootFS) { s.MaxExpandedBytes = 0 }, func(s *domain.WorkerRuntimeRootFS) { s.MaxEntries = 20001 }} {
		bad := spec
		mutate(&bad)
		c.Plan.Receipt.RuntimeRootFS = &bad
		if _, e := c.validate(); e == nil {
			t.Fatal("publisher accepted invalid runtime receipt")
		}
	}
}
