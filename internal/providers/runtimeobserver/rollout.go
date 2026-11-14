package runtimeobserver

import (
	"context"
	"encoding/json"
	"net/url"
	"releasecontrol/internal/delivery"
	"strings"
	"sync"
	"time"
)

// RolloutVerifier shares the observer's authenticated, bounded Kubernetes GET client.
type RolloutVerifier struct{ observer *Observer }

func NewRolloutVerifier() (*RolloutVerifier, error) {
	o, err := New(Target{InCluster: true, ProductID: "rollout", EnvironmentID: "dev", ApplicationID: "rollout", Namespace: "default", Deployment: "rollout", Container: "rollout"})
	if err != nil {
		return nil, err
	}
	return &RolloutVerifier{observer: o}, nil
}

type rolloutCondition struct {
	Type, Status, Reason, Message string
	ObservedGeneration            int64
}
type rolloutDeployment struct {
	Metadata struct {
		Name       string
		Generation int64
	}
	Spec struct {
		Replicas *int
		Template struct {
			Spec struct{ Containers []struct{ Image string } }
		}
	}
	Status struct {
		ObservedGeneration                                                               int64
		Replicas, UpdatedReplicas, ReadyReplicas, AvailableReplicas, UnavailableReplicas int
		Conditions                                                                       []rolloutCondition
	}
}

func shortReason(s string) string { return delivery.SafeRolloutReason(s) }
func imageIdentity(image string) (string, string) {
	ref, digest, _ := strings.Cut(image, "@")
	if i := strings.LastIndex(ref, ":"); i > strings.LastIndex(ref, "/") {
		ref = ref[:i]
	}
	return ref, digest
}
func checkDeployment(d rolloutDeployment, t delivery.RolloutTarget) (bool, string, string) {
	matched := false
	correct := true
	for _, c := range d.Spec.Template.Spec.Containers {
		repo, digest := imageIdentity(c.Image)
		if repo == t.Repository {
			matched = true
			correct = correct && digest == t.Digest
		}
	}
	if !matched {
		return false, "PENDING", "no matching workload image repository"
	}
	if !correct {
		return true, "PENDING", "workload image digest has not converged"
	}
	if d.Status.ObservedGeneration < d.Metadata.Generation {
		return true, "PENDING", "workload has not observed the deployed generation"
	}
	for _, c := range d.Status.Conditions {
		if c.Type == "Progressing" && c.Status == "False" && c.Reason == "ProgressDeadlineExceeded" {
			return true, "FAILED", shortReason(c.Reason + ": " + c.Message)
		}
	}
	replicas := 1
	if d.Spec.Replicas != nil {
		replicas = *d.Spec.Replicas
	}
	if d.Status.ObservedGeneration < d.Metadata.Generation || d.Status.Replicas != replicas || d.Status.UpdatedReplicas != replicas || d.Status.ReadyReplicas != replicas || d.Status.AvailableReplicas != replicas || d.Status.UnavailableReplicas > 0 {
		return true, "PENDING", "workload replicas are updating"
	}
	return true, "READY", ""
}
func (v *RolloutVerifier) target(ctx context.Context, t delivery.RolloutTarget) (string, string) {
	base := "/apis/apps/v1/namespaces/" + url.PathEscape(t.Namespace) + "/deployments"
	if t.Deployment != "" {
		var d rolloutDeployment
		if err := v.observer.get(ctx, base+"/"+url.PathEscape(t.Deployment), &d); err != nil {
			return "PENDING", err.Error()
		}
		_, phase, reason := checkDeployment(d, t)
		return phase, reason
	}
	var hr struct {
		Metadata struct{ Generation int64 }
		Spec     struct{ Values json.RawMessage }
		Status   struct {
			ObservedGeneration      int64
			LastAttemptedGeneration int64
			Conditions              []rolloutCondition
		}
	}
	if err := v.observer.get(ctx, "/apis/helm.toolkit.fluxcd.io/v2/namespaces/"+url.PathEscape(t.Namespace)+"/helmreleases/"+url.PathEscape(t.HelmRelease), &hr); err != nil {
		return "PENDING", err.Error()
	}
	// Search values only: a rendered tag may be stored alone or inside a full image reference.
	if !containsReference(hr.Spec.Values, t.Reference) || hr.Status.ObservedGeneration != hr.Metadata.Generation {
		return "PENDING", "HelmRelease has not observed the deployed reference"
	}
	if hr.Status.LastAttemptedGeneration != hr.Metadata.Generation {
		return "PENDING", "HelmRelease has not attempted the deployed generation"
	}
	ready := false
	failureReason := ""
	failed := false
	for _, c := range hr.Status.Conditions {
		if c.ObservedGeneration < hr.Metadata.Generation {
			continue
		}
		failure := c.Reason == "UpgradeFailed" || c.Reason == "InstallFailed" || c.Reason == "RetriesExceeded" || c.Reason == "ArtifactFailed" || c.Reason == "ReconciliationFailed"
		if c.Type == "Stalled" && c.Status == "True" || c.Type == "Ready" && c.Status == "False" && failure {
			if !failed {
				failureReason = c.Reason + ": " + c.Message
			}
			failed = true
		}
		if c.Type == "Ready" && c.Status == "True" {
			ready = true
		}
	}
	if failed {
		// Choose the most actionable current-generation diagnostic independently
		// of the conditions that establish failure.
		for _, kind := range []string{"Ready", "Released", "Stalled"} {
			for _, c := range hr.Status.Conditions {
				if c.ObservedGeneration != hr.Metadata.Generation || c.Type != kind {
					continue
				}
				if kind == "Stalled" && c.Status == "True" || kind != "Stalled" && c.Status == "False" {
					return "FAILED", shortReason(c.Reason + ": " + c.Message)
				}
			}
		}
		return "FAILED", shortReason(failureReason)
	}
	if !ready {
		return "PENDING", "HelmRelease is not Ready"
	}
	var list struct{ Items []rolloutDeployment }
	if err := v.observer.get(ctx, base+"?labelSelector="+url.QueryEscape("helm.toolkit.fluxcd.io/name="+t.HelmRelease), &list); err != nil {
		return "PENDING", err.Error()
	}
	found := false
	phase, reason := "READY", ""
	for _, d := range list.Items {
		matched, p, r := checkDeployment(d, t)
		if !matched {
			continue
		}
		found = true
		if p == "FAILED" {
			return p, r
		}
		if p != "READY" {
			phase, reason = p, r
		}
	}
	if !found {
		return "PENDING", "no matching Helm-managed Deployments"
	}
	return phase, reason
}
func containsReference(raw json.RawMessage, reference string) bool {
	var value any
	if json.Unmarshal(raw, &value) != nil {
		return false
	}
	var walk func(any) bool
	walk = func(v any) bool {
		switch x := v.(type) {
		case string:
			return reference != "" && (x == reference || strings.HasSuffix(x, ":"+reference))
		case []any:
			for _, item := range x {
				if walk(item) {
					return true
				}
			}
		case map[string]any:
			for _, item := range x {
				if walk(item) {
					return true
				}
			}
		}
		return false
	}
	return walk(value)
}
func (v *RolloutVerifier) VerifyRollout(ctx context.Context, plan delivery.RolloutPlan) (delivery.RolloutResult, error) {
	out := delivery.RolloutResult{Phase: "READY"}
	if len(plan.Targets) == 0 {
		out.Phase = "PENDING"
		out.Reason = "no rollout targets"
	}
	out.Targets = make([]delivery.RolloutEvidence, len(plan.Targets))
	var wg sync.WaitGroup
	jobs := make(chan int)
	for range min(4, len(plan.Targets)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				t := plan.Targets[i]
				targetCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
				phase, reason := v.target(targetCtx, t)
				cancel()
				out.Targets[i] = delivery.RolloutEvidence{Target: t, Phase: phase, Reason: shortReason(reason)}
			}
		}()
	}
	for i := range plan.Targets {
		jobs <- i
	}
	close(jobs)
	wg.Wait()
	for _, evidence := range out.Targets {
		if evidence.Phase == "FAILED" || evidence.Phase == "PENDING" && out.Phase == "READY" {
			out.Phase, out.Reason = evidence.Phase, evidence.Reason
		}
	}
	return out, nil
}
