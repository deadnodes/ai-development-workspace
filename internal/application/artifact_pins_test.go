package application

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"releasecontrol/internal/domain"
)

func artifactPinFixture(t *testing.T) (*Service, *memoryStore, domain.Command) {
	t.Helper()
	s, store := fixture(t)
	for _, c := range []domain.Command{{Action: "create_repository", ID: "repo", ProductID: "p", Data: map[string]any{"name": "source", "url": "https://github.com/fixture/source"}}, {Action: "create_application", ID: "app", ProductID: "p", Data: map[string]any{"name": "fixture component", "repository_id": "repo"}}, {Action: "create_product", ID: "other", Data: map[string]any{"name": "Other"}}, {Action: "create_repository", ID: "other-repo", ProductID: "other", Data: map[string]any{"name": "other", "url": "https://github.com/fixture/other"}}, {Action: "create_application", ID: "other-app", ProductID: "other", Data: map[string]any{"name": "other component", "repository_id": "other-repo"}}} {
		exec(t, s, c)
	}
	return s, store, domain.Command{Action: "pin_artifact", ProductID: "p", Data: map[string]any{"application_id": "app", "image_repository": "REGISTRY.example:443/team/image", "digest": "sha256:" + strings.Repeat("a", 64), "purpose": "rollback", "request_key": "pin-fixture-key"}}
}
func TestArtifactPinsIdempotencyScopeAndRelease(t *testing.T) {
	s, store, command := artifactPinFixture(t)
	pin := exec(t, s, command).(domain.ArtifactPin)
	if pin.ImageRepository != "registry.example/team/image" || pin.ReleasedAt != nil {
		t.Fatal("pin did not canonicalize/protect")
	}
	if err := domain.ValidateArtifactPins(store.state); err != nil {
		t.Fatal(err)
	}
	before := len(store.state.Events)
	retry := exec(t, s, command).(domain.ArtifactPin)
	if !reflect.DeepEqual(retry, pin) || len(store.state.Events) != before {
		t.Fatal("create retry changed record or audit")
	}
	for _, change := range []func(*domain.Command){func(c *domain.Command) { c.Data["digest"] = "sha256:" + strings.Repeat("b", 64) }, func(c *domain.Command) { c.Data["purpose"] = "manual" }, func(c *domain.Command) { c.Data["application_id"] = "other-app" }, func(c *domain.Command) { c.ID = pin.ID }, func(c *domain.Command) { c.FeatureID = "f" }} {
		c := command
		c.Data = clonePinData(command.Data)
		change(&c)
		reject(t, s, c)
	}
	release := domain.Command{Action: "release_artifact_pin", ProductID: "p", ID: pin.ID, Data: map[string]any{"application_id": "app"}}
	wrong := release
	wrong.ProductID = "other"
	wrong.Data = map[string]any{"application_id": "other-app"}
	reject(t, s, wrong)
	released := exec(t, s, release).(domain.ArtifactPin)
	if released.ReleasedAt == nil {
		t.Fatal("release missing")
	}
	before = len(store.state.Events)
	repeated := exec(t, s, release).(domain.ArtifactPin)
	if !reflect.DeepEqual(repeated, released) || len(store.state.Events) != before {
		t.Fatal("release replay had another state effect")
	}
	if retry = exec(t, s, command).(domain.ArtifactPin); !reflect.DeepEqual(retry, released) || len(store.state.Events) != before {
		t.Fatal("create after release reactivated pin")
	}
	if err := domain.ValidateArtifactPins(store.state); err != nil {
		t.Fatal(err)
	}
}
func clonePinData(in map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range in {
		out[k] = v
	}
	return out
}
func TestArtifactPinsRejectUnboundedFreeformAndUnknownIdentity(t *testing.T) {
	s, _, command := artifactPinFixture(t)
	for _, change := range []func(*domain.Command){func(c *domain.Command) { c.Data["request_key"] = "" }, func(c *domain.Command) { c.Data["request_key"] = strings.Repeat("a", 129) }, func(c *domain.Command) { c.Data["purpose"] = "freeform" }, func(c *domain.Command) { c.Data["notes"] = "no freeform state" }, func(c *domain.Command) { c.Data["image_repository"] = "team/image" }, func(c *domain.Command) { c.Data["image_repository"] = "registry.example/image:tag" }, func(c *domain.Command) { c.Data["digest"] = "latest" }, func(c *domain.Command) { c.ProductID = "missing" }, func(c *domain.Command) { c.Data["application_id"] = "missing" }} {
		c := command
		c.Data = clonePinData(command.Data)
		change(&c)
		reject(t, s, c)
	}
}
func TestArtifactPinsProductDeletionCascadesWithoutDanglingReferences(t *testing.T) {
	s, store, command := artifactPinFixture(t)
	pin := exec(t, s, command).(domain.ArtifactPin)
	before := len(store.state.Events)
	reject(t, s, domain.Command{Action: "delete_product", ProductID: "p", Data: map[string]any{"name": "Product"}})
	if len(store.state.Events) != before || len(store.state.ArtifactPins) != 1 {
		t.Fatal("rejected product deletion changed pin or audit")
	}
	exec(t, s, domain.Command{Action: "release_artifact_pin", ProductID: "p", ID: pin.ID, Data: map[string]any{"application_id": "app"}})
	exec(t, s, domain.Command{Action: "delete_product", ProductID: "p", Data: map[string]any{"name": "Product"}})
	if len(store.state.ArtifactPins) != 0 {
		t.Fatal("deleted product left dangling pins")
	}
	if err := domain.ValidateArtifactPins(store.state); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Query(context.Background(), "get_artifact_retention", "p"); err == nil {
		t.Fatal("deleted product inventory still readable")
	}
}
