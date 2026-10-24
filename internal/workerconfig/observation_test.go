package workerconfig

import (
	"encoding/json"
	"releasecontrol/internal/domain"
	"strings"
	"testing"
	"time"
)

func observationConfig() Config {
	r := RegistryObserveRecipe{ID: "observe", Revision: "v1", ProductID: "p", RepositoryID: "r", ComponentID: "a", Origin: "https://registry.example:5443", Repository: "team/image", Limits: domain.RegistryObserveLimits{MaxResponseBytes: 1 << 20, MaxTotalBytes: 8 << 20, MaxNodes: 128, MaxEdges: 256, MaxDepth: 16, MaxPages: 64, MaxRequests: 512, TimeoutSeconds: 60}, Pull: RegistryRead{BearerEnv: "REGISTRY_OBSERVER_READ"}}
	r.Hash = ObservationHash(r)
	return Config{Revision: "cfg1", LeaseSeconds: 10, Observations: []RegistryObserveRecipe{r}, Credentials: []Credential{{ID: "observer", Principal: "observer/one", TokenEnv: "OBSERVER_TOKEN", ProductIDs: []string{"p"}, RepositoryIDs: []string{"r"}, Capabilities: []string{"registry_observe"}, ExpiresAt: time.Now().Add(time.Hour)}}}
}
func TestObservationOnlyAuthorityAndNonsecretProjection(t *testing.T) {
	c := observationConfig()
	values := map[string]string{"OBSERVER_TOKEN": strings.Repeat("o", 32), "REGISTRY_OBSERVER_READ": "private-read-secret"}
	a, e := New(c, "private-control", func(k string) string { return values[k] })
	if e != nil {
		t.Fatal(e)
	}
	r, ok := a.Observation("observe")
	if !ok || r.Hash != c.Observations[0].Hash {
		t.Fatal("missing immutable recipe")
	}
	out := a.RegistryObservations(time.Now())
	if len(out) != 1 || !out[0].Current || out[0].Scope.ImageRepository != "registry.example:5443/team/image" {
		t.Fatal(out)
	}
	b, _ := json.Marshal(out)
	for _, secret := range []string{"private-read-secret", "REGISTRY_OBSERVER_READ", "OBSERVER_TOKEN", "ca_file"} {
		if strings.Contains(string(b), secret) {
			t.Fatal("secret/ref leaked")
		}
	}
	out = a.RegistryObservations(time.Now().Add(2 * time.Hour))
	if out[0].Current {
		t.Fatal("expired grant current")
	}
	values["REGISTRY_OBSERVER_READ"] = values["OBSERVER_TOKEN"]
	if a.RegistryObservations(time.Now())[0].Current {
		t.Fatal("rotated registry credential collision current")
	}
}
func TestObservationRejectsCredentialsBoundsAndAuthorityAliasing(t *testing.T) {
	for _, mutation := range []string{"mixed_grant", "shared_principal", "control_collision", "source_collision", "publication_collision", "bad_hash", "http", "urls", "zero_limits", "huge_limits", "missing_read", "duplicate_recipe"} {
		t.Run(mutation, func(t *testing.T) {
			c := observationConfig()
			values := map[string]string{"OBSERVER_TOKEN": strings.Repeat("o", 32), "REGISTRY_OBSERVER_READ": "private-read-secret", "SOURCE_TOKEN": strings.Repeat("s", 32)}
			switch mutation {
			case "mixed_grant":
				c.Credentials[0].Capabilities = append(c.Credentials[0].Capabilities, "source_check")
			case "shared_principal":
				other := c.Credentials[0]
				other.ID = "source"
				other.TokenEnv = "SOURCE_TOKEN"
				other.Capabilities = []string{"local_oci_build"}
				c.Credentials = append(c.Credentials, other)
			case "control_collision":
				values["REGISTRY_OBSERVER_READ"] = "private-control"
			case "source_collision":
				values["REGISTRY_OBSERVER_READ"] = values["OBSERVER_TOKEN"]
			case "publication_collision":
				r := PublicationRecipe{ID: "publish", Revision: "v1", ProductID: "p", RepositoryID: "r", ComponentID: "a", Profile: "runtime", BuildRecipeHash: "sha256:" + strings.Repeat("a", 64), Origin: "https://registry.example:5443", Repository: "team/image", MaxBytes: 1 << 20, TimeoutSeconds: 60, Pull: RegistryRead{BearerEnv: "PUBLISH_READ"}}
				r.Hash = PublicationHash(r)
				c.Publications = []PublicationRecipe{r}
				values["PUBLISH_READ"] = values["REGISTRY_OBSERVER_READ"]
			case "bad_hash":
				c.Observations[0].Hash = "sha256:" + strings.Repeat("a", 64)
			case "http":
				c.Observations[0].Origin = "http://registry.example"
			case "urls":
				c.Observations[0].Origin = "https://registry.example/path"
			case "zero_limits":
				c.Observations[0].Limits.MaxNodes = 0
			case "huge_limits":
				c.Observations[0].Limits.MaxRequests = 100000
			case "missing_read":
				delete(values, "REGISTRY_OBSERVER_READ")
			case "duplicate_recipe":
				c.Observations = append(c.Observations, c.Observations[0])
			}
			if mutation != "bad_hash" {
				c.Observations[0].Hash = ObservationHash(c.Observations[0])
			}
			if _, e := New(c, "private-control", func(k string) string { return values[k] }); e == nil {
				t.Fatal("unsafe observation authority accepted")
			}
		})
	}
}
func TestObservationHardMaximumNetworkBoundsValid(t *testing.T) {
	r := observationConfig().Observations[0]
	r.Limits.MaxResponseBytes = 1 << 30
	r.Limits.MaxTotalBytes = 8 << 30
	r.Limits.TimeoutSeconds = 900
	r.Hash = ObservationHash(r)
	if ValidateObservation(r) != nil {
		t.Fatal("valid observer maxima rejected")
	}
}
