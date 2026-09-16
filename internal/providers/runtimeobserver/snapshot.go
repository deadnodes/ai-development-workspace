package runtimeobserver

import (
	"context"
	"net/url"
	"releasecontrol/internal/delivery"
	"sort"
	"strings"
	"time"
)

type snapshotMeta struct {
	Name, UID         string
	Generation        int64
	DeletionTimestamp *string
	OwnerReferences   []struct {
		UID, Kind  string
		Controller *bool
	}
}

func owned(m snapshotMeta, kind, uid string) bool {
	if uid == "" {
		return false
	}
	for _, r := range m.OwnerReferences {
		if r.Kind == kind && r.UID == uid && r.Controller != nil && *r.Controller {
			return true
		}
	}
	return false
}

// Snapshot requests only explicitly mapped Kubernetes resources and retains only
// image/health metadata, never pod environment variables or mounted credentials.
func (o *Observer) Snapshot(ctx context.Context) delivery.RuntimeSnapshot {
	t := o.Target
	r := delivery.RuntimeSnapshot{Context: t.Context, ProductID: t.ProductID, EnvironmentID: t.EnvironmentID, ApplicationID: t.ApplicationID, ObservedAt: time.Now().UTC(), Namespace: t.Namespace, Deployment: t.Deployment, Container: t.Container, Health: "UNKNOWN", Pods: []delivery.RuntimePod{}, Errors: []string{}}
	if t.Kustomization != "" {
		f := &delivery.RuntimeFluxSnapshot{Status: "UNKNOWN"}
		r.Flux = f
		var x struct {
			Metadata snapshotMeta
			Status   struct {
				ObservedGeneration  int64
				LastAppliedRevision string
				Conditions          []condition
			}
		}
		if err := o.get(ctx, "/apis/kustomize.toolkit.fluxcd.io/v1/namespaces/"+url.PathEscape(t.FluxNamespace)+"/kustomizations/"+url.PathEscape(t.Kustomization), &x); err != nil {
			f.Error = err.Error()
		} else {
			f.Revision = x.Status.LastAppliedRevision
			f.Status = "PENDING"
			if x.Metadata.Generation > 0 && x.Status.ObservedGeneration >= x.Metadata.Generation && ready(x.Status.Conditions) {
				f.Status = "READY"
			}
		}
	}
	var d struct {
		Metadata snapshotMeta
		Spec     struct {
			Replicas *int
			Selector struct {
				MatchLabels      map[string]string
				MatchExpressions []any
			}
			Template struct {
				Spec struct {
					Containers []struct{ Name, Image string }
				}
			}
		}
		Status struct {
			ObservedGeneration                                          int64
			Replicas, UpdatedReplicas, AvailableReplicas, ReadyReplicas int
		}
	}
	fail := func(s string) delivery.RuntimeSnapshot { r.Errors = append(r.Errors, s); return r }
	if err := o.get(ctx, "/apis/apps/v1/namespaces/"+url.PathEscape(t.Namespace)+"/deployments/"+url.PathEscape(t.Deployment), &d); err != nil {
		return fail("deployment: " + err.Error())
	}
	r.Generation = d.Metadata.Generation
	r.ObservedGeneration = d.Status.ObservedGeneration
	r.DesiredReplicas = 1
	if d.Spec.Replicas != nil {
		r.DesiredReplicas = *d.Spec.Replicas
	}
	for _, c := range d.Spec.Template.Spec.Containers {
		if c.Name == t.Container {
			r.WorkloadImage = c.Image
		}
	}
	if d.Metadata.UID == "" || r.WorkloadImage == "" || d.Metadata.Generation <= 0 {
		return fail("deployment missing UID, generation or configured container image")
	}
	if len(d.Spec.Selector.MatchLabels) == 0 || len(d.Spec.Selector.MatchExpressions) > 0 {
		return fail("unsupported or empty workload selector")
	}
	labels := []string{}
	for k, v := range d.Spec.Selector.MatchLabels {
		labels = append(labels, k+"="+v)
	}
	sort.Strings(labels)
	query := "?labelSelector=" + url.QueryEscape(strings.Join(labels, ",")) + "&limit=500"
	var sets struct {
		Metadata struct{ Continue string }
		Items    []struct{ Metadata snapshotMeta }
	}
	if err := o.get(ctx, "/apis/apps/v1/namespaces/"+url.PathEscape(t.Namespace)+"/replicasets"+query, &sets); err != nil {
		return fail("replicasets: " + err.Error())
	}
	if sets.Metadata.Continue != "" {
		return fail("replicasets inventory exceeds bounded page")
	}
	owners := map[string]bool{}
	for _, rs := range sets.Items {
		if owned(rs.Metadata, "Deployment", d.Metadata.UID) && rs.Metadata.UID != "" {
			owners[rs.Metadata.UID] = true
		}
	}
	var pods struct {
		Metadata struct{ Continue string }
		Items    []struct {
			Metadata snapshotMeta
			Status   struct {
				Phase             string
				Conditions        []condition
				ContainerStatuses []struct {
					Name, Image, ImageID string
					Ready                bool
				}
			}
		}
	}
	if err := o.get(ctx, "/api/v1/namespaces/"+url.PathEscape(t.Namespace)+"/pods"+query, &pods); err != nil {
		return fail("pods: " + err.Error())
	}
	if pods.Metadata.Continue != "" {
		return fail("pod inventory exceeds bounded page")
	}
	n := r.DesiredReplicas
	healthy := n > 0 && d.Metadata.DeletionTimestamp == nil && d.Status.ObservedGeneration >= d.Metadata.Generation && d.Status.Replicas == n && d.Status.UpdatedReplicas == n && d.Status.AvailableReplicas == n && d.Status.ReadyReplicas == n
	for _, p := range pods.Items {
		belongs := false
		for uid := range owners {
			if owned(p.Metadata, "ReplicaSet", uid) {
				belongs = true
				break
			}
		}
		if !belongs {
			continue
		}
		out := delivery.RuntimePod{Name: p.Metadata.Name, UID: p.Metadata.UID, Phase: p.Status.Phase}
		for _, c := range p.Status.ContainerStatuses {
			if c.Name == t.Container {
				out.Image = c.Image
				out.ImageID = c.ImageID
				out.Ready = c.Ready && c.ImageID != "" && c.Image != "" && p.Status.Phase == "Running" && p.Metadata.DeletionTimestamp == nil && ready(p.Status.Conditions)
			}
		}
		healthy = healthy && out.Ready
		r.Pods = append(r.Pods, out)
	}
	sort.Slice(r.Pods, func(i, j int) bool { return r.Pods[i].Name < r.Pods[j].Name })
	healthy = healthy && len(r.Pods) == n
	r.Health = "NOT_READY"
	if healthy {
		r.Health = "HEALTHY"
	} else if n == 0 && len(r.Pods) == 0 && d.Status.ObservedGeneration >= d.Metadata.Generation {
		r.Health = "SCALED_ZERO"
	}
	return r
}
