package application

import "releasecontrol/internal/domain"

func applyRetention(st *domain.State, c domain.Command, m domain.Meta) (any, domain.Meta, error) {
	var input struct {
		ApplicationID string `json:"application_id"`
		KeepLast      int    `json:"keep_last"`
		KeepCurrent   bool   `json:"keep_current"`
		KeepPrevious  int    `json:"keep_previous"`
	}
	if err := decode(c.Data, &input); err != nil {
		return nil, m, err
	}
	app := applicationByID(st, input.ApplicationID)
	if app == nil || app.ProductID != c.ProductID {
		return nil, m, invalid("component must belong to product")
	}
	if input.KeepLast < 0 || input.KeepPrevious < 0 || input.KeepLast > 10000 || input.KeepPrevious > 10000 {
		return nil, m, invalid("retention counts must be between 0 and 10000")
	}
	app.Retention = &domain.ArtifactRetention{KeepLast: input.KeepLast, KeepCurrent: input.KeepCurrent, KeepPrevious: input.KeepPrevious}
	app.UpdatedAt = m.CreatedAt
	app.Actor = c.Actor
	return *app, m, nil
}
func retentionAttention(st domain.State, product string) []map[string]any {
	out := []map[string]any{}
	for _, e := range domain.EvaluateRetention(st, product) {
		if !e.ExpectedAvailable || e.Availability == "PRESENT" {
			continue
		}
		reason := "RETENTION_AVAILABILITY_UNKNOWN"
		if e.Availability == "MISSING" {
			reason = "RETENTION_ARTIFACT_MISSING"
		}
		out = append(out, map[string]any{"id": "retention:" + e.ArtifactID, "reason": reason, "application_id": e.ApplicationID, "artifact_id": e.ArtifactID, "digest": e.Digest, "detail": "Retention expects this image to remain available. Verify registry availability; rebuild from recorded provenance if missing. No artifact is deleted automatically.", "retention_reasons": e.Reasons})
	}
	return out
}
