package runtimeobserver

import "context"

// GetClient exposes only bounded Kubernetes GET requests. Credentials rotate per request.
type GetClient struct{ observer *Observer }

func NewInClusterGetClient() (*GetClient, error) {
	o, err := New(Target{InCluster: true, ProductID: "reporter", EnvironmentID: "prod", ApplicationID: "reporter", Namespace: "reporter", Deployment: "reporter", Container: "reporter"})
	if err != nil {
		return nil, err
	}
	return &GetClient{observer: o}, nil
}
func (c *GetClient) Get(ctx context.Context, path string, out any) error {
	return c.observer.get(ctx, path, out)
}
