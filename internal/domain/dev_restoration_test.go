package domain

import (
	"fmt"
	"releasecontrol/internal/delivery"
	"testing"
	"time"
)

// This pure evidence check exercises native artifact availability without any
// registry child jobs. Native provenance admission is exercised by the pipeline.
func TestRestorationNativeAvailabilityEvidence(t *testing.T) {
	at := time.Now().UTC()
	a := DeliveryArtifact{Meta: Meta{ID: "artifact", ProductID: "p"}, ApplicationID: "app", RepositoryID: "source", ImageRepository: "registry.invalid/dev/app", Digest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}
	c := DevBaselineComponent{ApplicationID: a.ApplicationID, RepositoryID: a.RepositoryID, ImageRepository: a.ImageRepository, Digest: a.Digest, ArtifactID: a.ID, ArtifactHash: checkHash(a), ObservedGitOps: &DevRuntimeGitOpsObservation{}}
	r := DevBaselineComponentResult{ApplicationID: c.ApplicationID, ImageRepository: c.ImageRepository, Digest: c.Digest, ArtifactID: c.ArtifactID, ArtifactHash: c.ArtifactHash, Availability: &delivery.ArtifactResult{Available: true, Digest: c.Digest, ObservedAt: at}}
	st := State{DeliveryArtifacts: []DeliveryArtifact{a}}
	op := ExternalOperation{Meta: Meta{ProductID: "p", CreatedAt: at.Add(-time.Minute)}}
	for _, children := range [][]string{nil, {}} {
		op.ChildIDs = children
		if !validRestorationAvailabilityEvidence(&st, op, []DevBaselineComponent{c}, []DevBaselineComponentResult{r}, at) {
			t.Fatal("current native availability rejected without registry children")
		}
	}
	for name, mutate := range map[string]func(*DevBaselineComponentResult){
		"missing":      func(v *DevBaselineComponentResult) { v.Availability = nil },
		"wrong digest": func(v *DevBaselineComponentResult) { v.Availability.Digest = "foreign" },
		"stale":        func(v *DevBaselineComponentResult) { v.Availability.ObservedAt = at.Add(-6 * time.Minute) },
		"future":       func(v *DevBaselineComponentResult) { v.Availability.ObservedAt = at.Add(time.Second) },
	} {
		t.Run(name, func(t *testing.T) {
			v := r
			proof := *r.Availability
			v.Availability = &proof
			mutate(&v)
			if validRestorationAvailabilityEvidence(&st, op, []DevBaselineComponent{c}, []DevBaselineComponentResult{v}, at) {
				t.Fatal("invalid native availability accepted")
			}
		})
	}
}

func TestRestorationOrphanNestedRetention(t *testing.T) {
	for _, quarantined := range []bool{false, true} {
		for name, put := range map[string]func(*ExternalOperation, string, string){
			"control_recovery_component": func(o *ExternalOperation, r, d string) {
				o.DevRestoration = &DevRestorationProof{Components: []DevBaselineComponent{{ApplicationID: "app", ImageRepository: r, Digest: d}}}
			},
			"control_plan_original": func(o *ExternalOperation, r, d string) {
				o.DevRestorationPlan = &DevRestorationPlan{Components: []DevRestorationMember{{ApplicationID: "app", Original: DevRuntimeGitOpsObservation{ImageRepository: r, Digest: d}}}}
			},
			"confirmation_original": func(o *ExternalOperation, r, d string) {
				o.Kind = "DEV_RESTORATION_CONFIRMATION"
				o.DevRestorationConfirmation = &DevRestorationConfirmation{Plan: DevRestorationPlan{Components: []DevRestorationMember{{ApplicationID: "app", Original: DevRuntimeGitOpsObservation{ImageRepository: r, Digest: d}}}}}
			},
			"confirmation_current": func(o *ExternalOperation, r, d string) {
				o.Kind = "DEV_RESTORATION_CONFIRMATION"
				o.DevRestorationConfirmation = &DevRestorationConfirmation{Plan: DevRestorationPlan{Components: []DevRestorationMember{{ApplicationID: "app", Current: DevRuntimeGitOpsObservation{ImageRepository: r, Digest: d}}}}}
			},
			"confirmation_group_result": func(o *ExternalOperation, r, d string) {
				o.Kind = "DEV_RESTORATION_CONFIRMATION"
				o.DevRestorationConfirmation = &DevRestorationConfirmation{Plan: DevRestorationPlan{Groups: []DevRestorationGroup{{Result: []DevRuntimeGitOpsObservation{{ImageRepository: r, Digest: d}}}}}}
			},
			"verification_component": func(o *ExternalOperation, r, d string) {
				o.Kind = "DEV_RESTORATION_VERIFICATION"
				o.DevRestorationVerification = &DevRestorationVerification{Components: []DevBaselineComponent{{ApplicationID: "app", ImageRepository: r, Digest: d}}}
			},
			"verification_component_observed": func(o *ExternalOperation, r, d string) {
				o.Kind = "DEV_RESTORATION_VERIFICATION"
				o.DevRestorationVerification = &DevRestorationVerification{Components: []DevBaselineComponent{{ApplicationID: "app", ObservedGitOps: &DevRuntimeGitOpsObservation{ImageRepository: r, Digest: d}}}}
			},
			"verification_runtime": func(o *ExternalOperation, r, d string) {
				o.Kind = "DEV_RESTORATION_VERIFICATION"
				o.DevRestorationVerification = &DevRestorationVerification{Runtime: DevRuntimeProof{Components: []DevRuntimeComponent{{ApplicationID: "app", ImageRepository: r, Digest: d}}}}
			},
			"verification_availability": func(o *ExternalOperation, r, d string) {
				o.Kind = "DEV_RESTORATION_VERIFICATION"
				o.DevRestorationVerification = &DevRestorationVerification{Availability: []DevBaselineComponentResult{{ApplicationID: "app", ImageRepository: r, Digest: d}}}
			},
			"verification_availability_gitops": func(o *ExternalOperation, r, d string) {
				o.Kind = "DEV_RESTORATION_VERIFICATION"
				o.DevRestorationVerification = &DevRestorationVerification{Availability: []DevBaselineComponentResult{{ApplicationID: "app", GitOps: DevRuntimeGitOpsObservation{ImageRepository: r, Digest: d}}}}
			},
			"release_availability": func(o *ExternalOperation, r, d string) {
				o.Kind = "DEV_RESTORATION_RELEASE"
				o.DevRestorationRelease = &DevRestorationRelease{Availability: []DevBaselineComponentResult{{ApplicationID: "app", ImageRepository: r, Digest: d}}}
			},
			"release_availability_gitops": func(o *ExternalOperation, r, d string) {
				o.Kind = "DEV_RESTORATION_RELEASE"
				o.DevRestorationRelease = &DevRestorationRelease{Availability: []DevBaselineComponentResult{{ApplicationID: "app", GitOps: DevRuntimeGitOpsObservation{ImageRepository: r, Digest: d}}}}
			},
			"recovery_effect_batch": func(o *ExternalOperation, r, d string) {
				o.DevRestoration = &DevRestorationProof{Effects: []DevRestorationEffect{{Batch: &delivery.GitOpsBatchApply{Changes: []delivery.GitOpsImageChange{{ID: "app", ImageRepository: r, Digest: d}}}}}}
			},
			"plan_group_batch": func(o *ExternalOperation, r, d string) {
				o.DevRestorationPlan = &DevRestorationPlan{Groups: []DevRestorationGroup{{Batch: &delivery.GitOpsBatchApply{Changes: []delivery.GitOpsImageChange{{ID: "app", ImageRepository: r, Digest: d}}}}}}
			},
			"runtime_result_gitops": func(o *ExternalOperation, r, d string) {
				o.DevRuntimeResult = &DevRuntimeResult{Components: []DevRuntimeComponentResult{{ApplicationID: "app", GitOps: DevRuntimeGitOpsObservation{ImageRepository: r, Digest: d}}}}
			},
			"runtime_result_pod": func(o *ExternalOperation, r, d string) {
				o.DevRuntimeResult = &DevRuntimeResult{Components: []DevRuntimeComponentResult{{ApplicationID: "app", Roles: []delivery.RuntimeSnapshot{{Pods: []delivery.RuntimePod{{Image: r + "@" + d}}}}}}}
			},
			"top_availability": func(o *ExternalOperation, r, d string) {
				o.DevRestorationAvailability = []DevBaselineComponentResult{{ApplicationID: "app", ImageRepository: r, Digest: d}}
			},
		} {
			t.Run(fmt.Sprintf("quarantine_%t/%s", quarantined, name), func(t *testing.T) {
				repo := "registry.example/independent/" + name
				digest := fmt.Sprintf("sha256:%064x", 777)
				o := ExternalOperation{Meta: Meta{ID: "orphan", ProductID: "p"}, ParentID: "missing-recovery", Kind: "DEV_CANDIDATE_RESTORATION", Status: "FAILED", ReservationQuarantined: quarantined}
				put(&o, repo, digest)
				st := State{Products: []Product{{Meta: Meta{ID: "p"}}}, Operations: []ExternalOperation{o}}
				now := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
				r := EvaluateRetention(st, "p", now, nil)
				if r.DeletionAllowed {
					t.Fatal("pure incomplete inventory allowed deletion")
				}
				found := false
				for _, img := range r.Images {
					if img.ImageRepository == repo && img.Digest == digest {
						for _, reason := range img.Reasons {
							if reason == "dev_restoration:orphan" && img.Protected {
								found = true
							}
						}
					}
				}
				if !found {
					t.Errorf("known sole nested orphan root omitted: repo=%s digest=%s report=%+v", repo, digest, r)
				}
				foreign := EvaluateRetention(st, "other-product", now, nil)
				for _, img := range foreign.Images {
					if img.ImageRepository == repo && img.Digest == digest {
						t.Fatal("foreign product leaked into inventory")
					}
				}
			})
		}
	}
}
