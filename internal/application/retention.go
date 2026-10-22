package application

import (
	"releasecontrol/internal/domain"
	"time"
)

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
	return retentionAttentionAt(st, product, time.Now().UTC())
}
func retentionAttentionAt(st domain.State, product string, now time.Time) []map[string]any {
	return retentionAttentionReport(domain.EvaluateRetention(st, product, now, nil))
}
func retentionAttentionReport(report domain.RetentionReport) []map[string]any {
	out := []map[string]any{}
	for _, e := range report.Images {
		if !e.Protected || (e.Availability == "PRESENT" && e.Freshness == "FRESH" && len(e.Blockers) == 0) {
			continue
		}
		reason := "RETENTION_AVAILABILITY_UNKNOWN"
		if e.Availability == "MISSING" {
			reason = "RETENTION_ARTIFACT_MISSING"
		}
		out = append(out, map[string]any{"id": "retention:" + e.Identity, "reason": reason, "application_ids": e.ApplicationIDs, "artifact_ids": e.ArtifactIDs, "image_repository": e.ImageRepository, "digest": e.Digest, "detail": "A protected image has missing, stale, unknown or incomplete stored evidence. No registry query or deletion was performed.", "retention_reasons": e.Reasons, "blockers": e.Blockers, "freshness": e.Freshness, "input_hash": report.InputHash})
	}
	return out
}
