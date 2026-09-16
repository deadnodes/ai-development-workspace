// Package runtimeobserver performs bounded, read-only Kubernetes/Flux observation.
package runtimeobserver

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"releasecontrol/internal/delivery"
	"strings"
	"time"
)

type Target struct {
	Kubeconfig     string `json:"kubeconfig,omitempty"`
	Context        string `json:"context,omitempty"`
	ProductID      string `json:"product_id"`
	EnvironmentID  string `json:"environment_id"`
	ApplicationID  string `json:"application_id"`
	APIURL         string `json:"api_url"`
	TokenFile      string `json:"token_file"`
	CAFile         string `json:"ca_file"`
	ClientCertFile string `json:"client_cert_file"`
	ClientKeyFile  string `json:"client_key_file"`
	FluxNamespace  string `json:"flux_namespace"`
	Kustomization  string `json:"kustomization"`
	Namespace      string `json:"namespace"`
	Deployment     string `json:"deployment"`
	Container      string `json:"container"`
}
type Result = delivery.RuntimeEvidence
type Observer struct {
	Target Target
	client *http.Client
	token  string
}

func Load(path string) ([]*Observer, error) {
	b, e := os.ReadFile(path)
	if e != nil {
		return nil, e
	}
	var targets []Target
	if e = json.Unmarshal(b, &targets); e != nil {
		return nil, e
	}
	out := []*Observer{}
	for _, t := range targets {
		o, e := New(t)
		if e != nil {
			return nil, e
		}
		out = append(out, o)
	}
	return out, nil
}
func New(t Target) (*Observer, error) {
	var material kubeMaterial
	if t.Kubeconfig != "" || t.Context != "" || t.APIURL == "" {
		var err error
		material, err = resolveKubeconfig(&t)
		if err != nil {
			return nil, err
		}
	}
	u, e := url.Parse(t.APIURL)
	if e != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("observer requires HTTPS Kubernetes API URL")
	}
	for _, s := range []string{t.ProductID, t.EnvironmentID, t.ApplicationID, t.Namespace, t.Deployment, t.Container} {
		if strings.TrimSpace(s) == "" {
			return nil, fmt.Errorf("observer target identifiers are required")
		}
	}
	if (t.FluxNamespace == "") != (t.Kustomization == "") {
		return nil, fmt.Errorf("Flux namespace and kustomization must be configured together")
	}
	tc := &tls.Config{MinVersion: tls.VersionTLS12}
	tc.ServerName = material.serverName
	if t.CAFile != "" || len(material.ca) > 0 {
		b := material.ca
		var e error
		if len(b) == 0 {
			b, e = os.ReadFile(t.CAFile)
		}
		if e != nil {
			return nil, e
		}
		tc.RootCAs = x509.NewCertPool()
		if !tc.RootCAs.AppendCertsFromPEM(b) {
			return nil, fmt.Errorf("invalid observer CA")
		}
	}
	if t.ClientCertFile != "" || t.ClientKeyFile != "" || len(material.cert) > 0 || len(material.key) > 0 {
		cert, key := material.cert, material.key
		var e error
		if len(cert) == 0 {
			cert, e = os.ReadFile(t.ClientCertFile)
			if e != nil {
				return nil, fmt.Errorf("client certificate unavailable")
			}
		}
		if len(key) == 0 {
			key, e = os.ReadFile(t.ClientKeyFile)
			if e != nil {
				return nil, fmt.Errorf("client key unavailable")
			}
		}
		c, e := tls.X509KeyPair(cert, key)
		if e != nil {
			return nil, e
		}
		tc.Certificates = []tls.Certificate{c}
	}
	return &Observer{Target: t, token: material.token, client: &http.Client{Timeout: 15 * time.Second, Transport: &http.Transport{TLSClientConfig: tc}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}
func (o *Observer) get(ctx context.Context, path string, out any) error {
	req, e := http.NewRequestWithContext(ctx, "GET", strings.TrimRight(o.Target.APIURL, "/")+path, nil)
	if e != nil {
		return e
	}
	if o.token != "" {
		req.Header.Set("Authorization", "Bearer "+o.token)
	}
	if o.Target.TokenFile != "" {
		b, e := os.ReadFile(o.Target.TokenFile)
		if e != nil {
			return fmt.Errorf("observer token unavailable")
		}
		req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(b)))
	}
	r, e := o.client.Do(req)
	if e != nil {
		return fmt.Errorf("Kubernetes API unavailable")
	}
	defer r.Body.Close()
	if r.StatusCode != 200 {
		return fmt.Errorf("Kubernetes API returned %d", r.StatusCode)
	}
	return json.NewDecoder(io.LimitReader(r.Body, 4<<20)).Decode(out)
}

type condition struct {
	Type, Status       string
	ObservedGeneration int64
}
type metadata struct {
	Generation        int64
	UID               string
	DeletionTimestamp *string
}

func ready(cs []condition) bool {
	for _, c := range cs {
		if c.Type == "Ready" && c.Status == "True" {
			return true
		}
	}
	return false
}
func (o *Observer) Observe(ctx context.Context, commit, digest string) Result {
	result := Result{}
	evidence := map[string]any{"source": "kubernetes-api", "expected_gitops_commit": commit, "expected_digest": digest, "flux": "UNKNOWN", "runtime": "UNKNOWN"}
	finish := func() Result { b, _ := json.Marshal(evidence); result.Details = string(b); return result }
	if !regexp.MustCompile(`^([0-9a-f]{40}|[0-9a-f]{64})$`).MatchString(commit) || !regexp.MustCompile(`^sha256:[0-9a-f]{64}$`).MatchString(digest) {
		evidence["error"] = "full expected GitOps commit and artifact digest required"
		return finish()
	}
	var flux struct {
		Metadata metadata
		Status   struct {
			ObservedGeneration  int64
			LastAppliedRevision string
			Conditions          []condition
		}
	}
	t := o.Target
	if e := o.get(ctx, "/apis/kustomize.toolkit.fluxcd.io/v1/namespaces/"+url.PathEscape(t.FluxNamespace)+"/kustomizations/"+url.PathEscape(t.Kustomization), &flux); e != nil {
		evidence["error"] = e.Error()
		return finish()
	}
	evidence["flux_revision"] = flux.Status.LastAppliedRevision
	matched := flux.Status.LastAppliedRevision == commit || strings.HasSuffix(flux.Status.LastAppliedRevision, "@sha1:"+commit) || strings.HasSuffix(flux.Status.LastAppliedRevision, "@sha256:"+commit)
	fluxReady := flux.Metadata.Generation > 0 && flux.Status.ObservedGeneration >= flux.Metadata.Generation && ready(flux.Status.Conditions) && matched
	evidence["flux"] = "PENDING"
	if fluxReady {
		evidence["flux"] = "RECONCILED"
	}
	var dep struct {
		Metadata metadata
		Spec     struct {
			Replicas *int
			Selector struct {
				MatchLabels      map[string]string
				MatchExpressions []any
			}
		}
		Status struct {
			ObservedGeneration                                          int64
			Replicas, UpdatedReplicas, AvailableReplicas, ReadyReplicas int
		}
	}
	if e := o.get(ctx, "/apis/apps/v1/namespaces/"+url.PathEscape(t.Namespace)+"/deployments/"+url.PathEscape(t.Deployment), &dep); e != nil {
		evidence["error"] = e.Error()
		return finish()
	}
	if len(dep.Spec.Selector.MatchLabels) == 0 || len(dep.Spec.Selector.MatchExpressions) > 0 {
		evidence["error"] = "unsupported or empty workload selector"
		return finish()
	}
	labels := []string{}
	for k, v := range dep.Spec.Selector.MatchLabels {
		labels = append(labels, k+"="+v)
	}
	var pods struct {
		Items []struct {
			Metadata metadata
			Status   struct {
				Phase             string
				Conditions        []condition
				ContainerStatuses []struct {
					Name, ImageID string
					Ready         bool
				}
			}
		}
	}
	if e := o.get(ctx, "/api/v1/namespaces/"+url.PathEscape(t.Namespace)+"/pods?labelSelector="+url.QueryEscape(strings.Join(labels, ",")), &pods); e != nil {
		evidence["error"] = e.Error()
		return finish()
	}
	replicas := 1
	if dep.Spec.Replicas != nil {
		replicas = *dep.Spec.Replicas
	}
	healthy := replicas > 0 && dep.Metadata.Generation > 0 && dep.Status.ObservedGeneration >= dep.Metadata.Generation && dep.Status.UpdatedReplicas == replicas && dep.Status.AvailableReplicas == replicas && dep.Status.ReadyReplicas == replicas && dep.Status.Replicas == replicas && len(pods.Items) == replicas
	images := []string{}
	for _, p := range pods.Items {
		found := false
		healthy = healthy && p.Metadata.DeletionTimestamp == nil && p.Status.Phase == "Running" && ready(p.Status.Conditions)
		for _, c := range p.Status.ContainerStatuses {
			if c.Name == t.Container {
				found = true
				images = append(images, c.ImageID)
				healthy = healthy && c.Ready && strings.HasSuffix(c.ImageID, "@"+digest)
			}
		}
		healthy = healthy && found
	}
	evidence["runtime_images"] = images
	evidence["runtime"] = "NOT_READY"
	if healthy {
		evidence["runtime"] = "HEALTHY"
	}
	result.Healthy = fluxReady && healthy
	return finish()
}

func (o *Observer) TargetIdentity() delivery.RuntimeTarget {
	return delivery.RuntimeTarget{ProductID: o.Target.ProductID, EnvironmentID: o.Target.EnvironmentID, ApplicationID: o.Target.ApplicationID}
}
