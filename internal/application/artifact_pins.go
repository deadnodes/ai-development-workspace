package application

import (
	"regexp"
	"time"

	"releasecontrol/internal/domain"
)

var artifactPinDigest = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// Pin commands only record protection intent. They never fetch, delete, expire
// or reactivate an artifact. Exact retries return before the shared audit append.
func applyArtifactPin(st *domain.State, c domain.Command, m domain.Meta) (any, domain.Meta, bool, error) {
	if c.ProductID == "" || c.FeatureID != "" || c.IntegrationID != "" || c.GateID != "" || c.CheckID != "" || c.ResultID != "" {
		return nil, m, false, invalid("artifact pin requires exact product/component scope")
	}
	if !productExists(st, c.ProductID) {
		return nil, m, false, missing("product", c.ProductID)
	}
	if c.Action == "release_artifact_pin" {
		var in struct {
			ApplicationID string `json:"application_id"`
		}
		if err := decode(c.Data, &in); err != nil {
			return nil, m, false, err
		}
		app := applicationByID(st, in.ApplicationID)
		if c.ID == "" || app == nil || app.ProductID != c.ProductID {
			return nil, m, false, invalid("pin release requires exact product/component/ID")
		}
		for i := range st.ArtifactPins {
			p := &st.ArtifactPins[i]
			if p.ID != c.ID {
				continue
			}
			if p.ProductID != c.ProductID || p.ApplicationID != in.ApplicationID {
				return nil, m, false, invalid("artifact pin outside requested scope")
			}
			if p.ReleasedAt != nil {
				return *p, p.Meta, false, nil
			}
			now := time.Now().UTC()
			p.ReleasedAt = &now
			p.UpdatedAt = now
			p.Actor = c.Actor
			return *p, p.Meta, true, nil
		}
		return nil, m, false, missing("artifact pin", c.ID)
	}
	var in struct {
		ApplicationID   string `json:"application_id"`
		ImageRepository string `json:"image_repository"`
		Digest          string `json:"digest"`
		Purpose         string `json:"purpose"`
		RequestKey      string `json:"request_key"`
	}
	if err := decode(c.Data, &in); err != nil {
		return nil, m, false, err
	}
	if c.ID != "" || !domain.ValidArtifactPinRequestKey(in.RequestKey) || !artifactPinDigest.MatchString(in.Digest) || (in.Purpose != "manual" && in.Purpose != "rollback") {
		return nil, m, false, invalid("invalid immutable artifact pin request")
	}
	app := applicationByID(st, in.ApplicationID)
	if app == nil || app.ProductID != c.ProductID {
		return nil, m, false, invalid("component must belong to pin product")
	}
	repository, err := domain.CanonicalImageRepository(in.ImageRepository)
	if err != nil {
		return nil, m, false, invalid("invalid explicit image repository")
	}
	hash := domain.ArtifactPinRequestHash(c.ProductID, in.ApplicationID, repository, in.Digest, in.Purpose, in.RequestKey)
	for _, p := range st.ArtifactPins {
		if p.ProductID == c.ProductID && p.RequestKey == in.RequestKey {
			if p.RequestHash != hash || p.ApplicationID != in.ApplicationID || p.ImageRepository != repository || p.Digest != in.Digest || p.Purpose != in.Purpose {
				return nil, m, false, invalid("artifact pin request_key conflicts with original intent")
			}
			return p, p.Meta, false, nil
		}
	}
	pin := domain.ArtifactPin{Meta: m, ApplicationID: in.ApplicationID, ImageRepository: repository, Digest: in.Digest, Purpose: in.Purpose, RequestKey: in.RequestKey, RequestHash: hash}
	st.ArtifactPins = append(st.ArtifactPins, pin)
	return pin, m, true, nil
}
