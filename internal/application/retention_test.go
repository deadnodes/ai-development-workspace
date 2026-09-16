package application

import (
	"encoding/json"
	"releasecontrol/internal/domain"
	"testing"
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
	st.DeliveryArtifacts = []domain.DeliveryArtifact{{Meta: domain.Meta{ID: "missing", ProductID: "p"}, ApplicationID: "a", Digest: "sha256:x", Availability: "MISSING"}}
	items := retentionAttention(st, "p")
	if len(items) != 1 || items[0]["reason"] != "RETENTION_ARTIFACT_MISSING" {
		t.Fatal(items)
	}
	st.DeliveryArtifacts[0].Availability = "PRESENT"
	if len(retentionAttention(st, "p")) != 0 {
		t.Fatal("present artifact warning")
	}
}

func retentionData(s string) map[string]any {
	v := map[string]any{}
	_ = json.Unmarshal([]byte(s), &v)
	return v
}
