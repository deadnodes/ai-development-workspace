package application

import (
	"context"
	"errors"
	"releasecontrol/internal/domain"
	"releasecontrol/internal/imagesigning"
	"time"
)

func securityEvent(st *domain.State, b domain.Build) {
	now := time.Now().UTC()
	action := "build.security_observed"
	st.Events = append(st.Events, domain.Event{ID: id(), Action: action, Actor: b.Actor, At: now, EntityID: b.ID, ProductID: b.ProductID, Data: domain.Command{Action: action, ID: b.ID, ProductID: b.ProductID, Actor: b.Actor, Data: map[string]any{"security_status": b.SecurityStatus, "security": b.Security}}})
}

func (s *Service) observeBuildSecurity(ctx context.Context) error {
	a := s.builds
	if a == nil || a.config == nil || a.signer == nil {
		return nil
	}
	snap, err := s.buildSnapshot(ctx)
	if err != nil {
		return err
	}
	for _, b := range snap.Builds {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		p, ok := a.config.Profile(b.ProfileID)
		if b.Status != "SUCCEEDED" || b.RestoreQuarantined || !ok || !p.RequireSecurityGate || (b.SecurityStatus != "" && b.SecurityStatus != "PENDING") {
			continue
		}
		next := copyBuild(b)
		next.Security = nil
		missing, transient, failed, errored := false, false, false, false
		allPass := true
		credential := ""
		if a.config.Signing != nil {
			credential = a.config.Signing.RegistryCredentials[p.RegistryID]
		}
		for _, img := range b.Images {
			ev, e := a.signer.SecurityEvidence(ctx, a.config.Registry(p.RegistryID), signingRepository(p, img.ID), credential, img.Digest)
			row := domain.BuildImageSecurity{ImageID: img.ID, Digest: img.Digest, ObservedAt: time.Now().UTC()}
			if errors.Is(e, imagesigning.ErrEvidenceNotFound) {
				missing = true
				allPass = false
				row.Verdict = "PENDING"
			} else if e != nil {
				transient = true
				allPass = false
				row.Verdict = "PENDING"
			} else {
				row.Verdict = ev.Verdict
				row.SummaryDigest = ev.SummaryDigest
				row.SBOMDigest = ev.SBOM.Digest
				row.SourceSHA = ev.SHA
				row.GitleaksFindings = ev.Gitleaks.Findings
				row.Critical = ev.Trivy.Critical
				row.HighFixable = ev.Trivy.HighFixable
				row.Blocking = ev.Trivy.Blocking
				row.BlockingIDs = ev.Trivy.BlockingIDs
				row.Reasons = ev.Reasons
				if t, e := time.Parse(time.RFC3339, ev.CreatedAt); e == nil {
					row.CreatedAt = t
				}
				if ev.SHA != b.SourceSHA {
					row.Verdict = "ERROR"
					row.Reasons = append(row.Reasons, "source SHA does not match build")
				}
				if row.Verdict == "FAIL" {
					failed = true
					allPass = false
				}
				if row.Verdict == "ERROR" {
					errored = true
					allPass = false
				}
				if row.Verdict != "PASS" {
					allPass = false
				}
			}
			next.Security = append(next.Security, row)
		}
		old := next.SecurityStatus
		switch {
		case failed:
			next.SecurityStatus = "FAIL"
		case errored:
			next.SecurityStatus = "ERROR"
		case allPass && len(next.Security) > 0:
			next.SecurityStatus = "PASS"
		case missing && !transient:
			started := b.FinishedAt
			if started == nil {
				t := b.UpdatedAt
				started = &t
			}
			if time.Since(*started) >= 30*time.Minute {
				next.SecurityStatus = "MISSING"
			} else {
				next.SecurityStatus = "PENDING"
			}
		default:
			next.SecurityStatus = "PENDING"
		}
		if next.SecurityStatus == old && len(next.Security) == len(b.Security) {
			continue
		}
		next.UpdatedAt = time.Now().UTC()
		if err = s.store.Update(ctx, func(st *domain.State) error {
			for i, v := range st.Builds {
				if v.ID == b.ID && v.UpdatedAt.Equal(b.UpdatedAt) {
					st.Builds[i] = next
					if old != next.SecurityStatus {
						securityEvent(st, next)
					}
					return nil
				}
			}
			return invalid("build security changed")
		}); err != nil {
			return err
		}
	}
	return nil
}
