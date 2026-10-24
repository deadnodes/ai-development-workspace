package publishrunner

import (
	"path/filepath"
	"releasecontrol/internal/artifactspool"
	"releasecontrol/internal/ocipublisher"
	"releasecontrol/internal/workerconfig"
	"strings"
	"testing"
)

func pinnedConfig(t *testing.T) Config {
	t.Helper()
	r := workerconfig.PublicationRecipe{ID: "pub", Revision: "v1", ProductID: "p", RepositoryID: "r", ComponentID: "app", Profile: "native", BuildRecipeHash: "sha256:" + strings.Repeat("a", 64), Origin: "https://registry.example.invalid", Repository: "fixture/app", MaxBytes: 32, TimeoutSeconds: 30}
	r.Hash = workerconfig.PublicationHash(r)
	return Config{Endpoint: "http://127.0.0.1:8090", TokenEnv: "RCP_PUBLISH_WORKER_TOKEN", ConfigRevision: "cfg1", Recipe: r, Publisher: ocipublisher.Config{AllowedOrigin: r.Origin, AllowedRepository: r.Repository, Archive: "/operator/fixed.oci.tar", StateDir: "/operator/private-state", MaxArchiveBytes: r.MaxBytes, TimeoutSeconds: r.TimeoutSeconds}}
}
func TestSpoolConfigurationHasSeparatePrivateState(t *testing.T) {
	c := pinnedConfig(t)
	if e := validate(c); e != nil {
		t.Fatal(e)
	}
	c.ArtifactSpool = &artifactspool.Config{Root: "/operator/private-spool", MaxArchiveBytes: 32, MaxTotalBytes: 64, MaxEntries: 2}
	if e := validate(c); e == nil {
		t.Fatal("fixed caller path accepted alongside receipt-derived spool")
	}
	c.Publisher.Archive = ""
	if e := validate(c); e != nil {
		t.Fatal(e)
	}
	before := workerconfig.Fingerprint(c)
	derived := c.Publisher
	derived.Archive = filepath.Join(c.ArtifactSpool.Root, strings.Repeat("a", 64)+".oci.tar")
	if before != workerconfig.Fingerprint(c) {
		t.Fatal("per-lease archive mutated persistent runner identity")
	}
	for _, state := range []string{c.ArtifactSpool.Root, filepath.Join(c.ArtifactSpool.Root, "state")} {
		c.Publisher.StateDir = state
		if e := validate(c); e == nil {
			t.Fatal("publisher writes state inside read-only spool")
		}
	}
	c = pinnedConfig(t)
	c.Publisher.Archive = ""
	if e := validate(c); e == nil {
		t.Fatal("fixed publisher without archive accepted")
	}
}
