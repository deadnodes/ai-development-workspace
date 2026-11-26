package application

import (
	"context"
	"crypto/ecdsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"releasecontrol/internal/buildconfig"
	"releasecontrol/internal/domain"
	"releasecontrol/internal/imagesigning"
)

func (s *Service) ConfigureProdPublicKey(pem string) error {
	if _, err := buildconfig.PublicKey(pem); err != nil {
		return err
	}
	s.prodPublicKey = pem
	return nil
}

type PublicProdSignature struct {
	ProdImageRef   string    `json:"prod_image_ref"`
	SignatureTag   string    `json:"signature_tag"`
	PayloadB64     string    `json:"payload_b64"`
	SignatureB64   string    `json:"signature_b64"`
	KeyFingerprint string    `json:"key_fingerprint"`
	PromotionID    string    `json:"promotion_id"`
	SignedAt       time.Time `json:"signed_at"`
}
type PublicProdSignatures struct {
	Digest string                `json:"digest"`
	Items  []PublicProdSignature `json:"items"`
}

func (s *Service) PublicProdSignatures(ctx context.Context, digest string) (PublicProdSignatures, error) {
	out := PublicProdSignatures{Digest: digest, Items: []PublicProdSignature{}}
	if !strings.HasPrefix(digest, "sha256:") || len(digest) != 71 || !regexp.MustCompile(`^sha256:[a-f0-9]{64}$`).MatchString(digest) {
		return out, invalid("invalid digest")
	}
	if s.prodPublicKey == "" {
		return out, missing("prod signature", digest)
	}
	err := s.view(ctx, func(st *domain.State) error {
		for _, p := range st.ProdPromotions {
			product := productByID(*st, p.ProductID)
			if p.Status != "SUCCEEDED" || product != nil && product.Visibility == "private" {
				continue
			}
			for _, it := range p.Items {
				if (it.Result != "SIGNED" && it.Result != "ALREADY_SIGNED") || it.Digest != digest || it.ProdSignature == nil {
					continue
				}
				signedAt := p.UpdatedAt
				if p.FinishedAt != nil {
					signedAt = *p.FinishedAt
				}
				out.Items = append(out.Items, PublicProdSignature{it.ProdImageRef, it.ProdSignature.SignatureTag, it.ProdSignature.PayloadB64, it.ProdSignature.SignatureB64, it.ProdSignature.KeyFingerprint, p.ID, signedAt})
				if len(out.Items) >= 10 {
					return nil
				}
			}
		}
		if len(out.Items) == 0 {
			return missing("prod signature", digest)
		}
		return nil
	})
	return out, err
}

func validProdSignature(it domain.ProdPromotionItem, sig *domain.ProdSignature, pem string) bool {
	if sig == nil || pem == "" || sig.SignatureTag != imagesigning.Tag(it.Digest, "sig") {
		return false
	}
	pub, e := buildconfig.PublicKey(pem)
	if e != nil || sig.KeyFingerprint != (buildconfig.SigningKey{PublicKeyPEM: pem}).Fingerprint() {
		return false
	}
	payload, e := base64.StdEncoding.DecodeString(sig.PayloadB64)
	if e != nil || len(payload) > 64<<10 {
		return false
	}
	var v struct {
		Critical struct {
			Type     string            `json:"type"`
			Image    map[string]string `json:"image"`
			Identity map[string]string `json:"identity"`
		} `json:"critical"`
	}
	identity, ok := imagesigning.RepositoryIdentity(it.ProdImageRef)
	if json.Unmarshal(payload, &v) != nil || !ok || v.Critical.Type != "cosign container image signature" || v.Critical.Image["docker-manifest-digest"] != it.Digest || v.Critical.Identity["docker-reference"] != identity {
		return false
	}
	signature, e := base64.StdEncoding.DecodeString(sig.SignatureB64)
	if e != nil || len(signature) > 1024 {
		return false
	}
	sum := sha256.Sum256(payload)
	return ecdsa.VerifyASN1(pub, sum[:], signature)
}

var promotionDigest = regexp.MustCompile(`@sha256:[a-fA-F0-9]{64}$`)

func (s *Service) RequestProdPromotion(ctx context.Context, in domain.RequestProdPromotion) (domain.ProdPromotion, error) {
	var out domain.ProdPromotion
	if err := authorize(ctx, "write", in.ProductID); err != nil {
		return out, err
	}
	if len(in.BuildIDs) < 1 || len(in.BuildIDs) > 20 || len(strings.TrimSpace(in.OwnerApproval)) == 0 || utf8.RuneCountInString(in.OwnerApproval) > 2000 || len(in.RequestKey) < 16 || len(in.RequestKey) > 128 {
		return out, invalid("INVALID_REQUEST")
	}
	if s.builds == nil {
		return out, invalid("BUILD_NOT_FOUND")
	}
	for _, id := range in.BuildIDs {
		if id == "" {
			return out, invalid("BUILD_NOT_FOUND")
		}
	}
	seen := map[string]bool{}
	for _, id := range in.BuildIDs {
		if seen[id] {
			return out, invalid("INVALID_REQUEST")
		}
		seen[id] = true
	}
	prodHost := strings.TrimSpace(os.Getenv("RCP_PROD_REGISTRY_HOST"))
	if prodHost == "" {
		prodHost = "registry.deadnodes.internal"
	}
	if !regexp.MustCompile(`^[A-Za-z0-9.-]+(?::[0-9]{1,5})?$`).MatchString(prodHost) {
		return out, invalid("INVALID_REGISTRY_HOST")
	}
	created := false
	err := s.store.Update(ctx, func(st *domain.State) error {
		for _, p := range st.ProdPromotions {
			if p.ProductID == in.ProductID && p.RequestKey == in.RequestKey {
				if p.OwnerApproval != in.OwnerApproval || !sameStrings(buildIDs(p.Items), in.BuildIDs) {
					return invalid("REQUEST_KEY_CONFLICT")
				}
				out = p
				return nil
			}
		}
		items := []domain.ProdPromotionItem{}
		for _, id := range in.BuildIDs {
			var b *domain.Build
			for i := range st.Builds {
				if st.Builds[i].ID == id && st.Builds[i].ProductID == in.ProductID {
					b = &st.Builds[i]
					break
				}
			}
			if b == nil {
				return invalid("BUILD_NOT_FOUND")
			}
			if b.Status != "SUCCEEDED" || b.SupersededBy != "" {
				return invalid("BUILD_NOT_DELIVERABLE")
			}
			if b.SigningStatus != "SIGNED" {
				return invalid("NOT_SIGNED")
			}
			p, ok := s.builds.config.Profile(b.ProfileID)
			if !ok || p.ProductID != in.ProductID {
				return invalid("NOT_PIPELINE_SIGNED")
			}
			if b.Branch != "" && b.Branch != p.DefaultBranch {
				return invalid("NOT_PIPELINE_SIGNED")
			}
			if s.builds.config.Signing == nil || len(b.Images) == 0 {
				return invalid("INVALID_IMAGE")
			}
			for _, artifactSignature := range st.ArtifactSignatures {
				_, digest, ok := strings.Cut(artifactSignature.Request.ImageRef, "@")
				if !ok {
					continue
				}
				for _, image := range b.Images {
					if digest == image.Digest {
						return invalid("NOT_PIPELINE_SIGNED")
					}
				}
			}
			if b.CIState != "" && b.CIState != "success" {
				return invalid("CI_NOT_GREEN")
			}
			for _, state := range b.CIContexts {
				if state != "" && state != "success" {
					return invalid("CI_NOT_GREEN")
				}
			}
			for _, image := range b.Images {
				hasCIProvenance := false
				for _, sig := range b.Signatures {
					if sig.ImageID != image.ID || sig.Digest != image.Digest || sig.Status != "SIGNED" || !slices.Contains(sig.Attestations, imagesigning.ProvenanceType) {
						continue
					}
					for _, rule := range s.builds.config.Signing.Rules {
						if rule.KeyID != sig.KeyID || !rule.RequireDefaultBranch || !rule.RequireStatusContexts {
							continue
						}
						for _, prefix := range rule.TagPrefixes {
							if strings.HasPrefix(chosenTag(*b), prefix) {
								hasCIProvenance = true
							}
						}
					}
				}
				if !hasCIProvenance {
					return invalid("NOT_PIPELINE_SIGNED")
				}
			}
			for _, image := range b.Images {
				for _, sec := range b.Security {
					if sec.ImageID == image.ID && (sec.Verdict == "FAIL" || sec.Verdict == "ERROR") {
						return invalid("SECURITY_GATE_FAILED")
					}
				}
				ref := image.ImageRef
				if !promotionDigest.MatchString(ref) {
					return invalid("INVALID_IMAGE")
				}
				if !strings.HasSuffix(ref, "@"+image.Digest) || !strings.HasPrefix(image.Digest, "sha256:") {
					return invalid("INVALID_IMAGE")
				}
				tagRef := strings.Split(ref, "@")[0]
				ix := strings.LastIndex(tagRef, ":")
				if ix < 0 || !strings.HasPrefix(tagRef[ix+1:], "main-") {
					return invalid("INVALID_IMAGE")
				}
				devHost := strings.SplitN(tagRef, "/", 2)
				if len(devHost) != 2 {
					return invalid("INVALID_IMAGE")
				}
				tag := tagRef[ix+1:]
				remainder := strings.TrimPrefix(tagRef, devHost[0]+"/")
				if remainder == tagRef || remainder == "" {
					return invalid("INVALID_IMAGE")
				}
				prodRef := prodHost + "/" + remainder + "@" + strings.SplitN(ref, "@", 2)[1]
				repo := tagRef[:ix]
				items = append(items, domain.ProdPromotionItem{BuildID: b.ID, ImageID: image.ID, Repository: repo, DevImageRef: ref, Digest: image.Digest, ProdImageRef: prodRef, Tag: tag, SourceSHA: b.SourceSHA, Result: "PENDING"})
			}
		}
		now := time.Now().UTC()
		actor := attributedActor(ctx, "agent")
		out = domain.ProdPromotion{Meta: domain.Meta{ID: id(), ProductID: in.ProductID, Actor: actor, CreatedAt: now, UpdatedAt: now}, EnvironmentID: "prod", Status: "QUEUED", Items: items, OwnerApproval: in.OwnerApproval, RequestedBy: actor, RequestKey: in.RequestKey}
		st.ProdPromotions = append(st.ProdPromotions, out)
		created = true
		st.Events = append(st.Events, domain.Event{ID: id(), ProductID: in.ProductID, Actor: out.RequestedBy, At: now, Action: "prod_promotion.requested", EntityID: out.ID, Data: domain.Command{Action: "prod_promotion.requested", ID: out.ID, ProductID: in.ProductID, Actor: out.RequestedBy}})
		return nil
	})
	if err == nil && created && s.prodPromotionNotify != nil {
		select {
		case s.prodPromotionNotify <- struct{}{}:
		default:
		}
	}
	return out, err
}
func buildIDs(items []domain.ProdPromotionItem) []string {
	out := make([]string, len(items))
	for i, v := range items {
		out[i] = v.BuildID
	}
	return out
}
func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	aa := append([]string(nil), a...)
	bb := append([]string(nil), b...)
	sortStrings(aa)
	sortStrings(bb)
	for i := range aa {
		if aa[i] != bb[i] {
			return false
		}
	}
	return true
}
func sortStrings(v []string) {
	for i := 1; i < len(v); i++ {
		for j := i; j > 0 && v[j] < v[j-1]; j-- {
			v[j], v[j-1] = v[j-1], v[j]
		}
	}
}

func (s *Service) GetProdPromotion(ctx context.Context, productID, id string) (domain.ProdPromotion, error) {
	var out domain.ProdPromotion
	if err := authorize(ctx, "read", productID); err != nil {
		return out, err
	}
	err := s.view(ctx, func(st *domain.State) error {
		if err := requireProductRead(ctx, st, productID); err != nil {
			return err
		}
		for _, p := range st.ProdPromotions {
			if p.ID == id && p.ProductID == productID {
				out = p
				return nil
			}
		}
		return missing("prod promotion", id)
	})
	return out, err
}
func (s *Service) ListProdPromotions(ctx context.Context, productID string, limit int) ([]domain.ProdPromotion, error) {
	out := []domain.ProdPromotion{}
	if err := authorize(ctx, "read", productID); err != nil {
		return out, err
	}
	if limit <= 0 || limit > 100 {
		limit = 25
	}
	err := s.view(ctx, func(st *domain.State) error {
		if err := requireProductRead(ctx, st, productID); err != nil {
			return err
		}
		for i := len(st.ProdPromotions) - 1; i >= 0 && len(out) < limit; i-- {
			if st.ProdPromotions[i].ProductID == productID {
				out = append(out, st.ProdPromotions[i])
			}
		}
		return nil
	})
	return out, err
}
func (s *Service) ClaimProdPromotion(ctx context.Context, in domain.ClaimProdPromotion) (*domain.ProdPromotion, error) {
	if in.Wait < 0 || in.Wait > 55 {
		return nil, invalid("invalid wait")
	}
	deadline := time.Now().Add(time.Duration(in.Wait) * time.Second)
	for {
		var out *domain.ProdPromotion
		candidate := false
		err := s.view(ctx, func(st *domain.State) error {
			now := time.Now().UTC()
			for _, p := range st.ProdPromotions {
				if p.ProductID == in.ProductID && p.EnvironmentID == in.EnvironmentID && p.Attempts < 3 &&
					(p.Status == "QUEUED" || p.Status == "CLAIMED" && p.LeaseExpiresAt != nil && now.After(*p.LeaseExpiresAt)) {
					candidate = true
					break
				}
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
		if candidate {
			err = s.store.Update(ctx, func(st *domain.State) error {
				now := time.Now().UTC()
				for i := range st.ProdPromotions {
					p := &st.ProdPromotions[i]
					if p.ProductID != in.ProductID || p.EnvironmentID != in.EnvironmentID || p.Attempts >= 3 {
						continue
					}
					eligible := p.Status == "QUEUED" || p.Status == "CLAIMED" && p.LeaseExpiresAt != nil && now.After(*p.LeaseExpiresAt)
					if !eligible {
						continue
					}
					p.Status = "CLAIMED"
					p.Attempts++
					p.ClaimID = id()
					p.ClaimedAt = &now
					lease := now.Add(10 * time.Minute)
					p.LeaseExpiresAt = &lease
					p.UpdatedAt = now
					copy := *p
					out = &copy
					st.Events = append(st.Events, domain.Event{ID: id(), ProductID: p.ProductID, Actor: "service/prod-promoter", At: now, Action: "prod_promotion.claimed", EntityID: p.ID})
					break
				}
				return nil
			})
			if err != nil || out != nil {
				return out, err
			}
			continue
		}
		if in.Wait == 0 || !time.Now().Before(deadline) {
			return nil, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-s.prodPromotionNotify:
		case <-time.After(time.Second):
		}
	}
}
func (s *Service) RecordProdPromotionResult(ctx context.Context, promotionID, productID, environmentID string, in domain.ProdPromotionResult) (domain.ProdPromotion, error) {
	var out domain.ProdPromotion
	err := s.store.Update(ctx, func(st *domain.State) error {
		for i := range st.ProdPromotions {
			p := &st.ProdPromotions[i]
			if p.ID != promotionID {
				continue
			}
			if p.ProductID != productID || p.EnvironmentID != environmentID || p.Status != "CLAIMED" || p.ClaimID != in.ClaimID || p.LeaseExpiresAt == nil || !time.Now().UTC().Before(*p.LeaseExpiresAt) {
				return invalid("STALE_CLAIM")
			}
			if len(in.Items) != len(p.Items) {
				return invalid("INVALID_RESULT")
			}
			byID := map[string]domain.ProdPromotionItem{}
			for _, it := range in.Items {
				key := it.ImageID + "/" + it.BuildID
				if _, exists := byID[key]; exists {
					return invalid("INVALID_RESULT")
				}
				byID[key] = it
			}
			all := true
			for i := range p.Items {
				v, ok := byID[p.Items[i].ImageID+"/"+p.Items[i].BuildID]
				if !ok || v.Digest != p.Items[i].Digest {
					return invalid("INVALID_RESULT")
				}
				if v.Result != "SIGNED" && v.Result != "ALREADY_SIGNED" && v.Result != "REJECTED" && v.Result != "ERROR" {
					return invalid("INVALID_RESULT")
				}
				if utf8.RuneCountInString(v.Reason) > 256 || (v.Result == "REJECTED" || v.Result == "ERROR") && v.ProdSignature != nil {
					return invalid("INVALID_RESULT")
				}
				if (v.Result == "SIGNED" || v.Result == "ALREADY_SIGNED") && !validProdSignature(p.Items[i], v.ProdSignature, s.prodPublicKey) {
					return invalid("INVALID_SIGNATURE")
				}
				p.Items[i].Result = v.Result
				p.Items[i].Reason = v.Reason
				p.Items[i].ProdSignature = v.ProdSignature
				all = all && (v.Result == "SIGNED" || v.Result == "ALREADY_SIGNED")
			}
			now := time.Now().UTC()
			p.Status = "FAILED"
			if all {
				p.Status = "SUCCEEDED"
			}
			p.UpdatedAt = now
			p.FinishedAt = &now
			out = *p
			st.Events = append(st.Events, domain.Event{ID: id(), ProductID: p.ProductID, Actor: "service/prod-promoter", At: now, Action: "prod_promotion.completed", EntityID: p.ID})
			return nil
		}
		return fmt.Errorf("%w: prod promotion", ErrNotFound)
	})
	return out, err
}

func (s *Service) HeartbeatProdPromotion(ctx context.Context, promotionID, productID, environmentID string, in domain.HeartbeatProdPromotion) (domain.ProdPromotion, error) {
	var out domain.ProdPromotion
	err := s.store.Update(ctx, func(st *domain.State) error {
		for i := range st.ProdPromotions {
			p := &st.ProdPromotions[i]
			if p.ID != promotionID {
				continue
			}
			now := time.Now().UTC()
			if p.ProductID != productID || p.EnvironmentID != environmentID || p.Status != "CLAIMED" || p.ClaimID != in.ClaimID || p.LeaseExpiresAt == nil || !now.Before(*p.LeaseExpiresAt) {
				return invalid("STALE_CLAIM")
			}
			lease := now.Add(10 * time.Minute)
			p.LeaseExpiresAt = &lease
			p.UpdatedAt = now
			out = *p
			return nil
		}
		return fmt.Errorf("%w: prod promotion", ErrNotFound)
	})
	return out, err
}
