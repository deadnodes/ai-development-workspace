package delivery

import "context"

type RuntimeTarget struct{ ProductID, EnvironmentID, ApplicationID string }
type RuntimeEvidence struct {
	Healthy bool
	Details string
}
type RuntimeObserver interface {
	TargetIdentity() RuntimeTarget
	Observe(context.Context, string, string) RuntimeEvidence
}
