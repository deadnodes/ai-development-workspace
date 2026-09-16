package delivery

import (
	"context"
	"time"
)

type RuntimeTarget struct{ ProductID, EnvironmentID, ApplicationID string }
type RuntimeEvidence struct {
	Healthy bool
	Details string
}
type RuntimeObserver interface {
	TargetIdentity() RuntimeTarget
	Observe(context.Context, string, string) RuntimeEvidence
}

// RuntimeSnapshotObserver reads configured workloads independently of deployments
// initiated by this control plane. Snapshot errors never imply a healthy runtime.
type RuntimeSnapshotObserver interface {
	TargetIdentity() RuntimeTarget
	Snapshot(context.Context) RuntimeSnapshot
}
type RuntimeSnapshot struct {
	Context            string               `json:"context,omitempty"`
	ProductID          string               `json:"product_id"`
	EnvironmentID      string               `json:"environment_id"`
	ApplicationID      string               `json:"application_id"`
	ObservedAt         time.Time            `json:"observed_at"`
	Namespace          string               `json:"namespace"`
	Deployment         string               `json:"deployment"`
	Container          string               `json:"container"`
	WorkloadImage      string               `json:"workload_image"`
	Generation         int64                `json:"generation"`
	ObservedGeneration int64                `json:"observed_generation"`
	DesiredReplicas    int                  `json:"desired_replicas"`
	Health             string               `json:"health"`
	Pods               []RuntimePod         `json:"pods"`
	Flux               *RuntimeFluxSnapshot `json:"flux,omitempty"`
	Errors             []string             `json:"errors"`
}
type RuntimePod struct {
	Name    string `json:"name"`
	UID     string `json:"uid"`
	Phase   string `json:"phase"`
	Image   string `json:"image"`
	ImageID string `json:"image_id"`
	Ready   bool   `json:"ready"`
}
type RuntimeFluxSnapshot struct {
	Revision string `json:"revision"`
	Status   string `json:"status"`
	Error    string `json:"error,omitempty"`
}
