package workerconfig

import (
	"releasecontrol/internal/domain"
	"strings"
	"testing"
)

func sourceFetchRecipe() Recipe {
	return Recipe{ID: "remote", Revision: "v1", ProductID: "p", RepositoryID: "r", RepositoryURL: "https://github.com/fixture/source", Capability: "source_check", Trust: "trusted_source", Execution: &domain.WorkerExecution{Kind: "source_check", ToolchainImage: "golang@sha256:" + strings.Repeat("a", 64), Platform: "linux/arm64", CheckArgv: []string{"/bin/true"}, Network: "none", TimeoutSeconds: 30, MemoryMB: 128, StorageMB: 16, CPUs: 1, PIDs: 64, SourceBytes: 1 << 20, OutputBytes: 1 << 20, LogBytes: 4096, SourceFetch: &domain.WorkerSourceFetch{Provider: "github", RepositoryID: 42, TimeoutSeconds: 20, MaxDownloadBytes: 2 << 20, MaxRequests: 100, MaxEntries: 100, MaxDepth: 10}}}
}
func TestSourceFetchRecipeAndInputIdentity(t *testing.T) {
	r := sourceFetchRecipe()
	r.Hash = RecipeHash(r)
	if err := ValidateExecution(r); err != nil {
		t.Fatal(err)
	}
	inputs := domain.WorkerInputs{Execution: r.Execution, RecipeHash: r.Hash}
	oldRecipe, oldInputs := r.Hash, Fingerprint(inputs)
	r.Execution.SourceFetch.RepositoryID++
	if RecipeHash(r) == oldRecipe || Fingerprint(inputs) == oldInputs {
		t.Fatal("source identity omitted from recipe/input hash")
	}
	for _, url := range []string{"https://github.com/fixture/source.git", "https://github.com/fixture/source/", "https://api.github.com/fixture/source", "http://github.com/fixture/source", "https://github.com/fixture/source?", "https://github.com/fixture/%73ource", "https://secret@github.com/fixture/source", "https://github.com:443/fixture/source", "https://github.com/./source"} {
		r = sourceFetchRecipe()
		r.RepositoryURL = url
		if ValidateExecution(r) == nil {
			t.Fatal("noncanonical remote repository accepted", url)
		}
	}
	r = sourceFetchRecipe()
	r.Execution.SourceFetch.MaxRequests = 4097
	if ValidateExecution(r) == nil {
		t.Fatal("unsafe remote bound accepted")
	}
	r = sourceFetchRecipe()
	r.Execution.SourceFetch = nil
	r.RepositoryURL = "https://git.example.test/fixture/source"
	if err := ValidateExecution(r); err != nil {
		t.Fatal("mirror recipe compatibility changed", err)
	}
}
