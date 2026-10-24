package application

import (
	"encoding/json"
	"releasecontrol/internal/domain"
	"strings"
	"testing"
	"time"
)

func TestRetentionPolicyValidationAndWarnings(t *testing.T) {
	st := domain.State{Applications: []domain.Application{{Meta: domain.Meta{ID: "a", ProductID: "p"}}}}
	for _, input := range []string{`{"application_id":"a","keep_last":-1}`, `{"application_id":"other","keep_last":1}`} {
		_, _, err := applyRetention(&st, domain.Command{ProductID: "p", Data: retentionData(input)}, domain.Meta{})
		if err == nil {
			t.Fatal("accepted invalid policy")
		}
	}
	_, _, err := applyRetention(&st, domain.Command{ProductID: "p", Data: retentionData(`{"application_id":"a","keep_last":1,"keep_current":true,"keep_previous":2}`)}, domain.Meta{})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	st.DeliveryArtifacts = []domain.DeliveryArtifact{{Meta: domain.Meta{ID: "missing", ProductID: "p"}, ApplicationID: "a", ImageRepository: "registry.example/image", Digest: "sha256:" + strings.Repeat("a", 64), Availability: "MISSING", ObservedAt: now}}
	items := retentionAttentionAt(st, "p", now)
	found := false
	report := domain.EvaluateRetention(st, "p", now, nil)
	for _, item := range items {
		if item["input_hash"] != report.InputHash {
			t.Fatal("attention used different report")
		}
		if item["reason"] == "RETENTION_ARTIFACT_MISSING" {
			found = true
		}
	}
	if !found {
		t.Fatal(items)
	}
	if report.DeletionAllowed || len(report.Blockers) == 0 {
		t.Fatal("inventory granted deletion")
	}
}

func TestRetentionAttentionHasNoPhantomWarningsWithoutImageEvidence(t *testing.T) {
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	for _, withComponent := range []bool{false, true} {
		st := domain.EmptyState()
		st.Products = []domain.Product{{Meta: domain.Meta{ID: "p"}}}
		if withComponent {
			st.Applications = []domain.Application{{Meta: domain.Meta{ID: "a", ProductID: "p"}, Retention: &domain.ArtifactRetention{KeepLast: 3}}}
		}
		report := domain.EvaluateRetention(st, "p", now, nil)
		if report.DeletionAllowed || len(report.Blockers) == 0 || report.InputHash == "" {
			t.Fatal("report lost mandatory deletion blockers or evidence hash")
		}
		if warnings := retentionAttentionAt(st, "p", now); len(warnings) != 0 {
			t.Fatalf("no recorded image needs attention: %+v", warnings)
		}
	}
}

func retentionData(s string) map[string]any {
	v := map[string]any{}
	_ = json.Unmarshal([]byte(s), &v)
	return v
}
