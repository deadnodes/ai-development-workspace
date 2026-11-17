package application

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"releasecontrol/internal/domain"
	"releasecontrol/internal/imagesigning"
	"strings"
	"testing"
	"time"
)

func evidenceFor(b domain.Build, verdict string) imagesigning.SecurityEvidence {
	return imagesigning.SecurityEvidence{Schema: "deadnodes.security-gate/v1", Verdict: verdict, Digest: b.Images[0].Digest, SHA: b.SourceSHA, CreatedAt: time.Now().UTC().Format(time.RFC3339), SummaryDigest: "sha256:" + strings.Repeat("1", 64), SBOM: struct {
		Digest string `json:"digest"`
	}{Digest: "sha256:" + strings.Repeat("2", 64)}}
}

func TestObserveBuildSecurityAggregationAndGracePeriod(t *testing.T) {
	for _, tc := range []struct {
		name, verdict string
		missing       bool
		age           time.Duration
		want          string
	}{
		{"all pass", "PASS", false, 0, "PASS"}, {"one fails", "FAIL", false, 0, "FAIL"}, {"error", "ERROR", false, 0, "ERROR"},
		{"missing pending", "", true, 0, "PENDING"}, {"missing expired", "", true, 31 * time.Minute, "MISSING"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, m, fixture, b := gatedSigningFixture(t)
			// Two image results exercise aggregation across the complete build.
			second := b.Images[0]
			second.ID = "other"
			b.Images = append(b.Images, second)
			m.state.Builds[0].Images = b.Images
			if tc.age > 0 {
				finished := time.Now().Add(-tc.age)
				m.state.Builds[0].FinishedAt = &finished
			}
			digest := b.Images[0].Digest
			f := &securityEvidenceSigner{Client: imagesigning.NewWithClient(fixture.Client()), evidence: map[string]imagesigning.SecurityEvidence{}, errors: map[string]error{}}
			f.evidence[digest] = evidenceFor(b, "PASS")
			if tc.verdict != "" {
				ev := evidenceFor(b, tc.verdict)
				f.evidence[digest] = ev
				f.evidence[b.Images[1].Digest] = ev
			}
			if tc.missing {
				f.errors[digest] = imagesigning.ErrEvidenceNotFound
				f.errors[b.Images[1].Digest] = imagesigning.ErrEvidenceNotFound
			}
			s.builds.signer = f
			if err := s.observeBuildSecurity(context.Background()); err != nil {
				t.Fatal(err)
			}
			got := m.state.Builds[0].SecurityStatus
			if got != tc.want {
				t.Fatalf("status=%s want=%s", got, tc.want)
			}
		})
	}
}

func TestObserveBuildSecuritySourceMismatchAndUngatedProfile(t *testing.T) {
	s, m, fixture, b := gatedSigningFixture(t)
	f := &securityEvidenceSigner{Client: imagesigning.NewWithClient(fixture.Client()), evidence: map[string]imagesigning.SecurityEvidence{}, errors: map[string]error{}}
	ev := evidenceFor(b, "PASS")
	ev.SHA = strings.Repeat("9", 40)
	f.evidence[b.Images[0].Digest] = ev
	s.builds.signer = f
	if err := s.observeBuildSecurity(context.Background()); err != nil {
		t.Fatal(err)
	}
	if m.state.Builds[0].SecurityStatus != "ERROR" {
		t.Fatal(m.state.Builds[0].SecurityStatus)
	}
	s2, m2, fixture2, b2 := gatedSigningFixture(t)
	s2.builds.config.Profiles[0].RequireSecurityGate = false
	s2.builds.signer = &securityEvidenceSigner{Client: imagesigning.NewWithClient(fixture2.Client()), evidence: map[string]imagesigning.SecurityEvidence{}, errors: map[string]error{b2.Images[0].Digest: errors.New("must not be queried")}}
	if err := s2.observeBuildSecurity(context.Background()); err != nil {
		t.Fatal(err)
	}
	if m2.state.Builds[0].SecurityStatus != "" {
		t.Fatal("ungated profile was touched", m2.state.Builds[0].SecurityStatus)
	}
}

func TestSigningSecurityGateOutcomesAndByproduct(t *testing.T) {
	for _, tc := range []struct {
		name, status, wantCode string
		wait                   bool
	}{
		{"fail", "FAIL", "SECURITY_GATE_FAILED", false}, {"missing", "MISSING", "SECURITY_GATE_MISSING", false}, {"pending", "PENDING", "", true}, {"pass", "PASS", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, m, fixture, b := gatedSigningFixture(t)
			ev := evidenceFor(b, "PASS")
			if tc.status == "PASS" {
				secondDigest := fixture.Image(strings.Repeat("8", 40))
				b.Images = append(b.Images, domain.BuildImage{ID: "other", Digest: secondDigest, ImageRef: "registry.test/o/other:main-bbbbbbbb@" + secondDigest})
				m.state.Builds[0].Images = b.Images
				ev2 := ev
				ev2.Digest = secondDigest
				ev2.SummaryDigest = "sha256:" + strings.Repeat("3", 64)
				fakeEvidence := map[string]imagesigning.SecurityEvidence{b.Images[0].Digest: ev, secondDigest: ev2}
				fake := &securityEvidenceSigner{Client: imagesigning.NewWithClient(fixture.Client()), evidence: fakeEvidence, errors: map[string]error{}}
				s.builds.signer = fake
				if err := m.Update(context.Background(), func(st *domain.State) error {
					st.Builds[0].SigningStatus = "PENDING"
					st.Builds[0].SigningStartedAt = time.Now().UTC()
					return nil
				}); err != nil {
					t.Fatal(err)
				}
				if err := s.SignBuildsOnce(context.Background()); err != nil {
					t.Fatal(err)
				}
				got := m.state.Builds[0]
				if fixture.Signs == 0 || len(got.Signatures) != 2 {
					t.Fatal(got.Signatures, fixture.Signs)
				}
				for i, image := range b.Images {
					var manifest imagesigning.Manifest
					if err := json.Unmarshal(fixture.Manifests[imagesigning.Tag(image.Digest, "att")], &manifest); err != nil {
						t.Fatal(err)
					}
					var envelope imagesigning.Envelope
					if err := json.Unmarshal(fixture.Blobs[manifest.Layers[0].Digest], &envelope); err != nil {
						t.Fatal(err)
					}
					statement, err := base64.StdEncoding.DecodeString(envelope.Payload)
					if err != nil {
						t.Fatal(err)
					}
					var doc map[string]any
					if err = json.Unmarshal(statement, &doc); err != nil {
						t.Fatal(err)
					}
					products := doc["predicate"].(map[string]any)["runDetails"].(map[string]any)["byproducts"].([]any)
					var scan map[string]any
					for _, item := range products {
						p := item.(map[string]any)
						if p["name"] == "security_scan" {
							scan = p
						}
					}
					wantDigest := ev.SummaryDigest
					if i == 1 {
						wantDigest = "sha256:" + strings.Repeat("3", 64)
					}
					if scan == nil || scan["digest"].(map[string]any)["sha256"] != strings.TrimPrefix(wantDigest, "sha256:") {
						t.Fatalf("image %s byproducts: %v", image.ID, products)
					}
				}
				return
			}
			fake := &securityEvidenceSigner{Client: imagesigning.NewWithClient(fixture.Client()), evidence: map[string]imagesigning.SecurityEvidence{b.Images[0].Digest: ev}, errors: map[string]error{}}
			s.builds.signer = fake
			if tc.status == "FAIL" {
				ev.Verdict = "FAIL"
				fake.evidence[b.Images[0].Digest] = ev
			}
			if tc.status == "ERROR" {
				ev.Verdict = "ERROR"
				fake.evidence[b.Images[0].Digest] = ev
			}
			if tc.status == "MISSING" {
				fake.errors[b.Images[0].Digest] = imagesigning.ErrEvidenceNotFound
				finished := time.Now().Add(-31 * time.Minute)
				m.state.Builds[0].FinishedAt = &finished
			}
			if tc.status == "PENDING" {
				fake.errors[b.Images[0].Digest] = imagesigning.ErrEvidenceNotFound
			}
			if err := m.Update(context.Background(), func(st *domain.State) error {
				st.Builds[0].SigningStatus = "PENDING"
				st.Builds[0].SigningStartedAt = time.Now().UTC()
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if err := s.SignBuildsOnce(context.Background()); err != nil {
				t.Fatal(err)
			}
			got := m.state.Builds[0]
			if tc.wait {
				if len(got.Signatures) != 0 || fixture.Signs != 0 {
					t.Fatal("pending gate signed", got.Signatures)
				}
				return
			}
			if tc.wantCode != "" {
				if len(got.Signatures) != 1 || got.Signatures[0].Status != "SKIPPED" || got.Signatures[0].FailureCode != tc.wantCode || fixture.Signs != 0 {
					t.Fatal(got.Signatures, fixture.Signs)
				}
				return
			}
			if fixture.Signs == 0 || len(got.Signatures) != 1 || got.Signatures[0].Status != "SIGNED" {
				t.Fatal(got.Signatures, fixture.Signs)
			}
			var manifest imagesigning.Manifest
			if err := json.Unmarshal(fixture.Manifests[imagesigning.Tag(b.Images[0].Digest, "att")], &manifest); err != nil {
				t.Fatal(err)
			}
			var envelope imagesigning.Envelope
			if err := json.Unmarshal(fixture.Blobs[manifest.Layers[0].Digest], &envelope); err != nil {
				t.Fatal(err)
			}
			statement, err := base64.StdEncoding.DecodeString(envelope.Payload)
			if err != nil {
				t.Fatal(err)
			}
			var doc map[string]any
			if err = json.Unmarshal(statement, &doc); err != nil {
				t.Fatal(err)
			}
			predicate := doc["predicate"].(map[string]any)
			details := predicate["runDetails"].(map[string]any)
			products := details["byproducts"].([]any)
			found := false
			for _, item := range products {
				p := item.(map[string]any)
				if p["name"] == "security_scan" {
					found = true
					if p["digest"].(map[string]any)["sha256"] != strings.TrimPrefix(ev.SummaryDigest, "sha256:") {
						t.Fatal(p)
					}
				}
			}
			if !found {
				t.Fatalf("security_scan missing: %s", fmt.Sprint(products))
			}
		})
	}
}

func TestDeployBuildSecurityGateRejectsNonPass(t *testing.T) {
	for _, status := range []string{"", "PENDING", "FAIL", "ERROR", "MISSING"} {
		t.Run(fmt.Sprintf("%q", status), func(t *testing.T) {
			s, m, _, in := deploymentFixture(t)
			s.builds.config.Profiles[0].RequireSecurityGate = true
			m.state.Builds[0].SecurityStatus = status
			_, err := s.DeployBuild(context.Background(), in)
			if err == nil || !strings.Contains(err.Error(), "SECURITY_GATE_NOT_PASSED") {
				t.Fatalf("error=%v", err)
			}
		})
	}
}

func TestAutoDeployWaitsForPendingSecurityGate(t *testing.T) {
	s, m := autoDeployFixture(t)
	s.builds.config.Profiles[0].RequireSecurityGate = true
	m.state.Builds[0].SecurityStatus = "PENDING"
	s.autoDeployBuilds(context.Background())
	if len(m.state.Deployments) != 0 {
		t.Fatal("auto deploy proposed while security gate was pending")
	}
}
