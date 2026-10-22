package workerconfig

import (
	"releasecontrol/internal/domain"
	"strings"
	"testing"
)

func TestRuntimeRootFSPinnedRecipeAndBounds(t *testing.T) {
	r := Recipe{Trust: "trusted_source", Capability: "local_oci_build", Execution: &domain.WorkerExecution{Kind: "local_oci_build", ToolchainImage: "golang@sha256:" + strings.Repeat("a", 64), BuildKitImage: "moby/buildkit@sha256:" + strings.Repeat("b", 64), Platform: "linux/arm64", CheckArgv: []string{"make", "check"}, BuildArgv: []string{"make", "build"}, OutputFile: "server", ImagePath: "usr/local/bin/server", Entrypoint: []string{"/usr/local/bin/server"}, Network: "none", TimeoutSeconds: 60, MemoryMB: 256, StorageMB: 64, CPUs: 1, PIDs: 64, SourceBytes: 1 << 20, OutputBytes: 4 << 20, LogBytes: 4096}}
	if e := ValidateExecution(r); e != nil {
		t.Fatal(e)
	}
	scratch := RecipeHash(r)
	spec := domain.WorkerRuntimeRootFS{Hash: "sha256:" + strings.Repeat("c", 64), InputsHash: "sha256:" + strings.Repeat("d", 64), ArchiveBytes: 10240, MaxExpandedBytes: 1 << 20, MaxEntries: 32}
	r.Execution.RuntimeRootFS = &spec
	if e := ValidateExecution(r); e != nil || RecipeHash(r) == scratch {
		t.Fatal("runtime not bound into recipe", e)
	}
	pinned := RecipeHash(r)
	for _, mutate := range []func(*domain.WorkerRuntimeRootFS){func(s *domain.WorkerRuntimeRootFS) { s.Hash = "wrong" }, func(s *domain.WorkerRuntimeRootFS) { s.InputsHash = "wrong" }, func(s *domain.WorkerRuntimeRootFS) { s.ArchiveBytes = 0 }, func(s *domain.WorkerRuntimeRootFS) { s.ArchiveBytes = 129 << 20 }, func(s *domain.WorkerRuntimeRootFS) { s.MaxExpandedBytes = 0 }, func(s *domain.WorkerRuntimeRootFS) { s.MaxExpandedBytes = 129 << 20 }, func(s *domain.WorkerRuntimeRootFS) { s.MaxEntries = 0 }, func(s *domain.WorkerRuntimeRootFS) { s.MaxEntries = 20001 }} {
		bad := spec
		mutate(&bad)
		r.Execution.RuntimeRootFS = &bad
		if ValidateExecution(r) == nil || RecipeHash(r) == pinned {
			t.Fatal("invalid runtime pins accepted")
		}
	}
	r.Execution.RuntimeRootFS = &spec
	r.Capability = "source_check"
	r.Execution.Kind = "source_check"
	r.Execution.BuildKitImage = ""
	r.Execution.BuildArgv = nil
	r.Execution.OutputFile = ""
	r.Execution.ImagePath = ""
	r.Execution.Entrypoint = nil
	if ValidateExecution(r) == nil {
		t.Fatal("source-only job declared runtime rootfs")
	}
}
