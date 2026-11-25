package application

import (
	"context"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/url"
	"strings"
	"testing"
	"time"

	"releasecontrol/internal/buildconfig"
	"releasecontrol/internal/domain"
	"releasecontrol/internal/imagesigning"
	"releasecontrol/internal/imagesigning/signingtest"
)

func promotionFixture(t *testing.T) (*Service, *memoryStore, string) {
	t.Helper()
	s, m := fixture(t)
	f := signingtest.New(t)
	cfg := buildConfig()
	cfg.Profiles[0].RequiredStatusContexts = []string{"ci"}
	cfg.Registries = []buildconfig.Registry{f.RegistryConfig()}
	cfg.Signing = f.Config()
	cfg.Signing.Rules[0].RequireDefaultBranch = true
	cfg.Signing.Rules[0].RequireStatusContexts = true
	if e := s.ConfigureBuilds(cfg, &signingForgejo{fakeForgejo: &fakeForgejo{head: strings.Repeat("a", 40)}, state: "success"}, &fakeBuildRegistry{}); e != nil {
		t.Fatal(e)
	}
	s.builds.signer = imagesigning.NewWithClient(f.Client())
	digest := f.Image(strings.Repeat("a", 40))
	u, _ := url.Parse("http://192.168.32.242:5000")
	m.state.Builds = append(m.state.Builds, domain.Build{Meta: domain.Meta{ID: "build1", ProductID: "p", CreatedAt: time.Now().UTC()}, ProfileID: "app", Status: "SUCCEEDED", SigningStatus: "SIGNED", Branch: "main", SourceSHA: strings.Repeat("a", 40), Images: []domain.BuildImage{{ID: "app", ImageRef: u.Host + "/deadnodes/app:main-aaaaaaaa@" + digest, Digest: digest}}, Signatures: []domain.BuildSignature{{ImageID: "app", Digest: digest, KeyID: "dev", Status: "SIGNED", Attestations: []string{imagesigning.ProvenanceType}}}})
	return s, m, digest
}
func promoReq(ids ...string) domain.RequestProdPromotion {
	return domain.RequestProdPromotion{ProductID: "p", BuildIDs: ids, OwnerApproval: "go ahead with this production promotion", RequestKey: "prod-promotion-test-key-0001"}
}

func TestRequestProdPromotionRefusalCodesAtomicAndIdempotent(t *testing.T) {
	tests := []struct {
		name, code string
		mutate     func(*domain.Build)
	}{
		{"missing", "BUILD_NOT_FOUND", nil}, {"unsigned", "NOT_SIGNED", func(b *domain.Build) { b.SigningStatus = "PENDING" }}, {"bad ci", "CI_NOT_GREEN", func(b *domain.Build) { b.CIState = "failure" }}, {"bad optional ci context", "CI_NOT_GREEN", func(b *domain.Build) { b.CIContexts = map[string]string{"optional": "failure"} }}, {"wrong branch", "NOT_PIPELINE_SIGNED", func(b *domain.Build) { b.Branch = "feature" }}, {"invalid image", "INVALID_IMAGE", func(b *domain.Build) {
			b.Images[0].ImageRef = strings.Split(b.Images[0].ImageRef, "@")[0] + "@sha256:deadbeef"
		}}, {"security", "SECURITY_GATE_FAILED", func(b *domain.Build) { b.Security = []domain.BuildImageSecurity{{ImageID: "app", Verdict: "FAIL"}} }}, {"superseded", "BUILD_NOT_DELIVERABLE", func(b *domain.Build) { b.SupersededBy = "new" }},
	}
	t.Run("backfill signature cannot qualify", func(t *testing.T) {
		s, m, digest := promotionFixture(t)
		m.state.ArtifactSignatures = append(m.state.ArtifactSignatures, domain.ArtifactSignature{Request: domain.SignArtifactRequest{ProductID: "p", ImageRef: "192.168.32.242:5000/deadnodes/app:main-aaaaaaaa@" + digest}, Status: "SUCCEEDED"})
		_, err := s.RequestProdPromotion(context.Background(), promoReq("build1"))
		if err == nil || !strings.Contains(err.Error(), "NOT_PIPELINE_SIGNED") || len(m.state.ProdPromotions) != 0 {
			t.Fatalf("backfill-repaired signature accepted: %v", err)
		}
	})
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s, m, _ := promotionFixture(t)
			if tc.mutate != nil {
				tc.mutate(&m.state.Builds[0])
			}
			ids := []string{"build1"}
			if tc.name == "missing" {
				ids = []string{"missing"}
			}
			_, e := s.RequestProdPromotion(context.Background(), promoReq(ids...))
			if e == nil || !strings.Contains(e.Error(), tc.code) {
				t.Fatalf("got %v want %s", e, tc.code)
			}
			if len(m.state.ProdPromotions) != 0 {
				t.Fatal("refusal partially stored promotion")
			}
		})
	}
	t.Run("whole request atomic", func(t *testing.T) {
		s, m, _ := promotionFixture(t)
		_, e := s.RequestProdPromotion(context.Background(), promoReq("build1", "missing"))
		if e == nil || !strings.Contains(e.Error(), "BUILD_NOT_FOUND") || len(m.state.ProdPromotions) != 0 {
			t.Fatalf("partial result: %v %#v", e, m.state.ProdPromotions)
		}
	})
	t.Run("request key replay", func(t *testing.T) {
		s, m, _ := promotionFixture(t)
		a, e := s.RequestProdPromotion(context.Background(), promoReq("build1"))
		if e != nil {
			t.Fatal(e)
		}
		b, e := s.RequestProdPromotion(context.Background(), promoReq("build1"))
		if e != nil || a.ID != b.ID || len(m.state.ProdPromotions) != 1 {
			t.Fatal(a, b, e)
		}
	})
}

func TestProdPromotionClaimLeaseAndStaleClaim(t *testing.T) {
	s, m, _ := promotionFixture(t)
	p, e := s.RequestProdPromotion(context.Background(), promoReq("build1"))
	if e != nil {
		t.Fatal(e)
	}
	first, e := s.ClaimProdPromotion(context.Background(), domain.ClaimProdPromotion{ProductID: "p", EnvironmentID: "prod"})
	if e != nil || first == nil || first.Status != "CLAIMED" || first.Attempts != 1 {
		t.Fatal(first, e)
	}
	m.state.ProdPromotions[0].LeaseExpiresAt = promotionPtrTime(time.Now().Add(-time.Second))
	second, e := s.ClaimProdPromotion(context.Background(), domain.ClaimProdPromotion{ProductID: "p", EnvironmentID: "prod"})
	if e != nil || second == nil || second.ClaimID == first.ClaimID || second.Attempts != 2 {
		t.Fatal(second, e)
	}
	_, e = s.RecordProdPromotionResult(context.Background(), p.ID, "p", "prod", domain.ProdPromotionResult{ClaimID: first.ClaimID})
	if !errors.Is(e, ErrValidation) {
		t.Fatalf("stale claim accepted: %v", e)
	}
}

type claimCountingStore struct {
	*memoryStore
	updates int
}

func (s *claimCountingStore) Update(ctx context.Context, fn func(*domain.State) error) error {
	s.updates++
	return s.memoryStore.Update(ctx, fn)
}

func TestProdPromotionClaimDoesNotWriteWhileQueueIsEmpty(t *testing.T) {
	s, m := fixture(t)
	counted := &claimCountingStore{memoryStore: m}
	s.store = counted
	claim, err := s.ClaimProdPromotion(context.Background(), domain.ClaimProdPromotion{ProductID: "p", EnvironmentID: "prod", Wait: 1})
	if err != nil || claim != nil || counted.updates != 0 {
		t.Fatalf("empty queue claim=%v err=%v update calls=%d", claim, err, counted.updates)
	}
}

func promotionPtrTime(t time.Time) *time.Time { return &t }

func TestProdPromotionResultRequiresPinnedVerifiedSignature(t *testing.T) {
	s, _, _ := promotionFixture(t)
	p, err := s.RequestProdPromotion(context.Background(), promoReq("build1"))
	if err != nil {
		t.Fatal(err)
	}
	claim, err := s.ClaimProdPromotion(context.Background(), domain.ClaimProdPromotion{ProductID: "p", EnvironmentID: "prod"})
	if err != nil || claim == nil {
		t.Fatal(claim, err)
	}
	it := claim.Items[0]
	result := domain.ProdPromotionItem{BuildID: it.BuildID, ImageID: it.ImageID, Digest: it.Digest, Result: "SIGNED", ProdSignature: &domain.ProdSignature{SignatureTag: imagesigning.Tag(it.Digest, "sig"), PayloadB64: "e30=", SignatureB64: "AA==", KeyFingerprint: "unconfigured"}}
	if _, err = s.RecordProdPromotionResult(context.Background(), p.ID, "p", "prod", domain.ProdPromotionResult{ClaimID: claim.ClaimID, Items: []domain.ProdPromotionItem{result}}); err == nil || !strings.Contains(err.Error(), "INVALID_SIGNATURE") {
		t.Fatalf("accepted signature without pinned key: %v", err)
	}

	f := signingtest.New(t)
	prodKey := f.Key("prod")
	if err = s.ConfigureProdPublicKey(prodKey.PublicKeyPEM); err != nil {
		t.Fatal(err)
	}
	identity, ok := imagesigning.RepositoryIdentity(it.ProdImageRef)
	if !ok {
		t.Fatal("invalid production image ref")
	}
	payload, err := json.Marshal(map[string]any{"critical": map[string]any{"type": "cosign container image signature", "identity": map[string]string{"docker-reference": identity}, "image": map[string]string{"docker-manifest-digest": it.Digest}}})
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(payload)
	sig, err := ecdsa.SignASN1(rand.Reader, f.Keys["prod"], sum[:])
	if err != nil {
		t.Fatal(err)
	}
	result.ProdSignature = &domain.ProdSignature{SignatureTag: imagesigning.Tag(it.Digest, "sig"), PayloadB64: base64.StdEncoding.EncodeToString(payload), SignatureB64: base64.StdEncoding.EncodeToString(sig), KeyFingerprint: prodKey.Fingerprint()}
	completed, err := s.RecordProdPromotionResult(context.Background(), p.ID, "p", "prod", domain.ProdPromotionResult{ClaimID: claim.ClaimID, Items: []domain.ProdPromotionItem{result}})
	if err != nil || completed.Status != "SUCCEEDED" || completed.Items[0].Result != "SIGNED" {
		t.Fatalf("valid signed result was refused: %#v %v", completed, err)
	}
}
