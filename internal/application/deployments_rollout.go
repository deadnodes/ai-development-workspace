package application

import (
	"context"
	"releasecontrol/internal/buildconfig"
	"releasecontrol/internal/delivery"
	"releasecontrol/internal/domain"
	"strings"
	"time"
)

type RolloutPlan = delivery.RolloutPlan
type RolloutResult = delivery.RolloutResult
type DeploymentRuntime interface {
	VerifyRollout(context.Context, RolloutPlan) (RolloutResult, error)
}

func rolloutPlan(d domain.Deployment, p buildconfig.Profile) RolloutPlan {
	plan := RolloutPlan{}
	for _, f := range p.Deployments["dev"].Files {
		if f.Rollout == nil {
			continue
		}
		for _, image := range d.Images {
			if image.ID == f.ImageID {
				ref, _, _ := strings.Cut(image.ImageRef, "@")
				tag := ref[strings.LastIndex(ref, ":")+1:]
				reference := strings.ReplaceAll(strings.ReplaceAll(f.TagFormat, "{tag}", tag), "{digest}", image.Digest)
				if f.Format == "container_image" {
					reference = tag + "@" + image.Digest
				}
				target := delivery.RolloutTarget{Namespace: f.Rollout.Namespace, Repository: f.ImageRepository, Digest: image.Digest, Reference: reference}
				for _, name := range f.Rollout.HelmReleases {
					target.HelmRelease = name
					plan.Targets = append(plan.Targets, target)
				}
				target.HelmRelease = ""
				for _, name := range f.Rollout.Deployments {
					target.Deployment = name
					plan.Targets = append(plan.Targets, target)
				}
			}
		}
	}
	return plan
}
func (s *Service) mergedDeployment(d domain.Deployment, p buildconfig.Profile) domain.Deployment {
	now := time.Now().UTC()
	d.MergedAt = &now
	if s.deployments.runtime == nil || len(rolloutPlan(d, p).Targets) == 0 {
		return succeededDeployment(d)
	}
	deadline := now.Add(time.Duration(s.builds.config.Rollout.TimeoutSeconds) * time.Second)
	d.Status = "VERIFYING_ROLLOUT"
	d.RolloutDeadline = &deadline
	d.FailureCode = ""
	d.UncertainAction = ""
	return d
}
func (s *Service) verifyDeploymentRollout(ctx context.Context, d domain.Deployment, p buildconfig.Profile) error {
	next := s.observeDeploymentRollout(ctx, d, p)
	return s.advanceDeployment(ctx, d, &next, false)
}

// Observation is read-only; the dispatcher applies each result through the original CAS.
func (s *Service) observeDeploymentRollout(ctx context.Context, d domain.Deployment, p buildconfig.Profile) domain.Deployment {
	next := d
	if d.RolloutDeadline == nil || !time.Now().Before(*d.RolloutDeadline) {
		next = failedDeployment(next, "ROLLOUT_TIMEOUT")
	} else if s.deployments.runtime != nil {
		checkCtx, cancel := context.WithDeadline(ctx, minRolloutDeadline(*d.RolloutDeadline))
		result, err := s.deployments.runtime.VerifyRollout(checkCtx, rolloutPlan(d, p))
		cancel()
		if err != nil {
			next.RolloutFailure = "Kubernetes API unavailable"
		} else {
			next.RolloutFailure = delivery.SafeRolloutReason(result.Reason)
			switch result.Phase {
			case "READY":
				next = succeededDeployment(next)
				next.RolloutVerified = true
				next.RolloutFailure = ""
			case "FAILED":
				next = failedDeployment(next, "ROLLOUT_FAILED")
			}
		}
		if !time.Now().Before(*d.RolloutDeadline) {
			next = failedDeployment(next, "ROLLOUT_TIMEOUT")
			next.RolloutVerified = false
		}
	}
	return next
}

const rolloutTickTimeout = 25 * time.Second

func minRolloutDeadline(deadline time.Time) time.Time {
	tick := time.Now().Add(rolloutTickTimeout)
	if deadline.Before(tick) {
		return deadline
	}
	return tick
}
