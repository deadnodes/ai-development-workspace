package workerconfig

import (
	"releasecontrol/internal/domain"
	"strings"
	"testing"
)

func TestOperatorCheckBindingRestrictionsAndCanonicalOwnership(t *testing.T) {
	base := testConfig()
	r := base.Recipes[0]
	r.Capability = "local_oci_build"
	r.Trust = "trusted_source"
	r.Parameters = map[string]Parameter{}
	r.Check = &domain.WorkerCheckBinding{Name: "RCP / checks and build", ExpectedAppID: 7, FinalizeSeconds: 300}
	r.Execution = &domain.WorkerExecution{Kind: "local_oci_build", ToolchainImage: "go@sha256:" + strings.Repeat("a", 64), BuildKitImage: "buildkit@sha256:" + strings.Repeat("b", 64), Platform: "linux/arm64", CheckArgv: []string{"make", "check"}, BuildArgv: []string{"make", "build"}, OutputFile: "app", ImagePath: "app", Entrypoint: []string{"/app"}, Network: "none", TimeoutSeconds: 60, MemoryMB: 256, StorageMB: 64, CPUs: 1, PIDs: 64, SourceBytes: 1 << 20, OutputBytes: 4 << 20, LogBytes: 4096}
	r.Hash = RecipeHash(r)
	for _, change := range []func(*Recipe){func(r *Recipe) { r.Check.Name = "arbitrary provider context" }, func(r *Recipe) { r.Check.ExpectedAppID = 0 }, func(r *Recipe) { r.Check.FinalizeSeconds = 59 }, func(r *Recipe) { r.Check.FinalizeSeconds = 601 }, func(r *Recipe) { r.Parameters = map[string]Parameter{"race": {Type: "boolean"}} }, func(r *Recipe) { r.Trust = "untrusted_source" }, func(r *Recipe) { r.Capability = "source_check" }} {
		copy := r
		binding := *r.Check
		copy.Check = &binding
		change(&copy)
		if ValidateExecution(copy) == nil {
			t.Fatal("unsafe check binding accepted")
		}
	}
	base.Recipes = []Recipe{r}
	base.Credentials[0].Capabilities = []string{"local_oci_build"}
	if _, e := New(base, "control", func(string) string { return strings.Repeat("w", 32) }); e != nil {
		t.Fatal(e)
	}
	for _, url := range []string{"https://GITHUB.com/test/repo/", "https://github.com/test/repo.git", "https://github.com:443/TEST/REPO.git/"} {
		other := r
		other.ID = "other"
		other.ProductID = "p2"
		other.RepositoryID = "r2"
		other.RepositoryURL = url
		other.Hash = RecipeHash(other)
		base.Recipes = []Recipe{r, other}
		if _, e := New(base, "control", func(string) string { return strings.Repeat("w", 32) }); e == nil {
			t.Fatal("canonical physical owner conflict accepted", url)
		}
	}
	legacy := testConfig()
	legacy.Recipes[0].Check = nil
	if _, e := New(legacy, "control", func(string) string { return strings.Repeat("w", 32) }); e != nil {
		t.Fatal("disabled parameterized legacy recipe changed", e)
	}
}
