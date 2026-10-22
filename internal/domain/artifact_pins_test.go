package domain

import (
	"encoding/json"
	"testing"
	"time"
)

func artifactPinFixture() State {
	st := retentionBase()
	p := ArtifactPin{Meta: Meta{ID: "pin", ProductID: "p", Actor: "agent", CreatedAt: retentionNow, UpdatedAt: retentionNow}, ApplicationID: "app", ImageRepository: "ghcr.io/owner/app", Digest: retentionSHA(1), Purpose: "rollback", RequestKey: "pin:req-1"}
	p.RequestHash = ArtifactPinRequestHash(p.ProductID, p.ApplicationID, p.ImageRepository, p.Digest, p.Purpose, p.RequestKey)
	st.ArtifactPins = []ArtifactPin{p}
	return st
}
func TestArtifactPinValidationAndIdentity(t *testing.T) {
	st := artifactPinFixture()
	if e := ValidateArtifactPins(st); e != nil {
		t.Fatal(e)
	}
	for _, mode := range []string{"cross-product", "wrong-app", "hash", "digest", "purpose", "key", "duplicate-key", "duplicate-id", "other-kind-id", "release-before-created", "release-after-updated", "noncanonical", "zero-created", "feature"} {
		t.Run(mode, func(t *testing.T) {
			b, _ := json.Marshal(st)
			var bad State
			json.Unmarshal(b, &bad)
			p := &bad.ArtifactPins[0]
			switch mode {
			case "cross-product":
				p.ProductID = "other"
			case "wrong-app":
				p.ApplicationID = "other"
			case "hash":
				p.RequestHash = retentionSHA(99)
			case "digest":
				p.Digest = "latest"
			case "purpose":
				p.Purpose = "secret note"
			case "key":
				p.RequestKey = "key with spaces"
			case "duplicate-key":
				q := *p
				q.ID = "second"
				bad.ArtifactPins = append(bad.ArtifactPins, q)
			case "duplicate-id":
				bad.ArtifactPins = append(bad.ArtifactPins, *p)
			case "other-kind-id":
				bad.Applications[0].ID = p.ID
			case "release-before-created":
				at := retentionNow.Add(-time.Second)
				p.ReleasedAt = &at
			case "release-after-updated":
				at := retentionNow.Add(time.Second)
				p.ReleasedAt = &at
			case "noncanonical":
				p.ImageRepository = "GHCR.IO:443/owner/app"
			case "zero-created":
				p.CreatedAt = time.Time{}
			case "feature":
				p.FeatureID = "unscoped-feature"
			}
			if ValidateArtifactPins(bad) == nil || ValidateStateStatuses(bad) == nil {
				t.Fatal("corrupt pin accepted", mode)
			}
		})
	}
	artifactPinReleaseProof(&st)
	at := *st.ArtifactPins[0].ReleasedAt
	if e := ValidateArtifactPins(st); e != nil {
		t.Fatal(e)
	}
	r := EvaluateRetention(st, "p", at)
	if len(r.Images) != 0 {
		t.Fatal("released pin still roots image", r)
	}
	active := artifactPinFixture()
	r = EvaluateRetention(active, "p", at)
	if len(r.Images) != 1 || !r.Images[0].Protected {
		t.Fatal("active pin absent", r)
	}
}
func TestCanonicalImageRepositoryAndPinKeys(t *testing.T) {
	for raw, want := range map[string]string{"GHCR.IO:443/owner/app": "ghcr.io/owner/app", "localhost:5000/test/app": "localhost:5000/test/app", "registry.example:5443/team/app": "registry.example:5443/team/app", "[::1]:5000/team/app": "[::1]:5000/team/app"} {
		got, e := CanonicalImageRepository(raw)
		if e != nil || got != want {
			t.Fatal(raw, got, e)
		}
	}
	for _, raw := range []string{"owner/app", "https://ghcr.io/owner/app", "ghcr.io/owner/app:latest", "ghcr.io/owner/app@" + retentionSHA(1), "ghcr.io/../app", "user@ghcr.io/owner/app", "ghcr.io/Owner/app", "ghcr.io/owner//app", "ghcr.io/owner/app\n"} {
		if _, e := CanonicalImageRepository(raw); e == nil {
			t.Fatal("invalid repository", raw)
		}
	}
	for _, key := range []string{"", " spaces", "with spaces", "\n", string(make([]byte, 129))} {
		if ValidArtifactPinRequestKey(key) {
			t.Fatal("invalid request key")
		}
	}
}
func TestEmptyStatePinsCurrentSchema(t *testing.T) {
	st := EmptyState()
	if st.ArtifactPins == nil {
		t.Fatal("pin collection missing current schema")
	}
	if ValidateArtifactPins(st) != nil {
		t.Fatal("empty state invalid")
	}
}

func artifactPinReleaseProof(st *State) {
	p := &st.ArtifactPins[0]
	at := retentionNow.Add(time.Second)
	p.ReleasedAt = &at
	p.UpdatedAt = at
	p.Actor = "agent/release"
	st.Events = []Event{{ID: "release-event", Action: "release_artifact_pin", Actor: p.Actor, At: at, EntityID: p.ID, ProductID: p.ProductID, Data: Command{Action: "release_artifact_pin", Actor: p.Actor, ID: p.ID, ProductID: p.ProductID, Data: map[string]any{"application_id": p.ApplicationID}}}}
}
func TestArtifactPinReleaseRequiresExactRetainedAuditProof(t *testing.T) {
	active := artifactPinFixture()
	at := active.ArtifactPins[0].UpdatedAt
	active.ArtifactPins[0].ReleasedAt = &at
	if ValidateArtifactPins(active) == nil {
		t.Fatal("single ReleasedAt-field mutation removed protection without release command")
	}
	valid := artifactPinFixture()
	artifactPinReleaseProof(&valid)
	if e := ValidateArtifactPins(valid); e != nil {
		t.Fatal("valid logical release proof rejected", e)
	}
	for _, mode := range []string{"missing", "duplicate", "event-action", "command-action", "entity-id", "command-id", "event-product", "command-product", "app", "nonstring-app", "extra-data", "event-actor", "command-actor", "pin-actor", "event-time", "updated-time", "event-feature", "command-feature", "command-integration", "command-gate", "command-check", "command-result", "reactivation"} {
		t.Run(mode, func(t *testing.T) {
			b, _ := json.Marshal(valid)
			var bad State
			json.Unmarshal(b, &bad)
			p := &bad.ArtifactPins[0]
			e := &bad.Events[0]
			switch mode {
			case "missing":
				bad.Events = nil
			case "duplicate":
				copy := *e
				copy.ID = "second-event"
				bad.Events = append(bad.Events, copy)
			case "event-action":
				e.Action = "other"
			case "command-action":
				e.Data.Action = "other"
			case "entity-id":
				e.EntityID = "other"
			case "command-id":
				e.Data.ID = "other"
			case "event-product":
				e.ProductID = "other"
			case "command-product":
				e.Data.ProductID = "other"
			case "app":
				e.Data.Data["application_id"] = "other"
			case "nonstring-app":
				e.Data.Data["application_id"] = 42
			case "extra-data":
				e.Data.Data["released_at"] = "caller-time"
			case "event-actor":
				e.Actor = "other"
			case "command-actor":
				e.Data.Actor = "other"
			case "pin-actor":
				p.Actor = "other"
			case "event-time":
				e.At = e.At.Add(time.Nanosecond)
			case "updated-time":
				p.UpdatedAt = p.UpdatedAt.Add(time.Nanosecond)
			case "event-feature":
				e.FeatureID = "feature"
			case "command-feature":
				e.Data.FeatureID = "feature"
			case "command-integration":
				e.Data.IntegrationID = "integration"
			case "command-gate":
				e.Data.GateID = "gate"
			case "command-check":
				e.Data.CheckID = "check"
			case "command-result":
				e.Data.ResultID = "result"
			case "reactivation":
				p.ReleasedAt = nil
			}
			if ValidateArtifactPins(bad) == nil || ValidateStateStatuses(bad) == nil {
				t.Fatal("invalid logical release proof accepted", mode)
			}
		})
	}
}
