// Package runtimereporter implements the read-only, outbound production reporter.
package runtimereporter

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	rr "releasecontrol/internal/runtimereport"
	"strings"
	"time"
)

type Config struct {
	URL, TokenFile, ProductID, EnvironmentID, Cluster string
	Namespaces                                        []string
	Interval                                          time.Duration
}

func Load(getenv func(string) string) (Config, error) {
	c := Config{URL: getenv("RCP_URL"), TokenFile: getenv("RCP_TOKEN_FILE"), ProductID: getenv("PRODUCT_ID"), EnvironmentID: getenv("ENVIRONMENT_ID"), Cluster: getenv("CLUSTER"), Interval: 60 * time.Second}
	if c.URL == "" {
		c.URL = "https://rcp-dev.tail9c31db.ts.net"
	}
	if c.EnvironmentID == "" {
		c.EnvironmentID = "prod"
	}
	u, e := url.Parse(c.URL)
	if e != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return c, fmt.Errorf("RCP_URL must be an HTTPS origin")
	}
	if v := getenv("INTERVAL"); v != "" {
		c.Interval, e = time.ParseDuration(v)
		if e != nil || c.Interval < 30*time.Second {
			return c, fmt.Errorf("INTERVAL must be at least 30s")
		}
	}
	seen := map[string]bool{}
	for _, n := range strings.Split(getenv("NAMESPACES"), ",") {
		n = strings.TrimSpace(n)
		if !rr.ValidLabel(n, 63) {
			return c, fmt.Errorf("NAMESPACES must be an explicit namespace allowlist")
		}
		if !seen[n] {
			c.Namespaces = append(c.Namespaces, n)
			seen[n] = true
		}
	}
	if e = rr.Validate(rr.Report{ProductID: c.ProductID, EnvironmentID: c.EnvironmentID, Cluster: c.Cluster, ReportedAt: time.Now()}, time.Now()); e != nil {
		return c, e
	}
	if c.TokenFile == "" {
		return c, fmt.Errorf("RCP_TOKEN_FILE required")
	}
	b, e := os.ReadFile(c.TokenFile)
	if e != nil || strings.TrimSpace(string(b)) == "" {
		return c, fmt.Errorf("RCP token unavailable")
	}
	return c, nil
}

type Getter interface {
	Get(context.Context, string, any) error
}
type metadata struct {
	Name, UID       string
	Generation      int64
	OwnerReferences []struct {
		UID, Kind, Name string
		Controller      bool
	}
}
type container struct{ Name, Image string }
type workload struct {
	Metadata metadata
	Spec     struct {
		Selector struct {
			MatchLabels      map[string]string
			MatchExpressions []struct {
				Key, Operator string
				Values        []string
			}
		}
		Template struct {
			Spec struct{ Containers []container }
		}
	}
	Status struct {
		ObservedGeneration                                                                                                                                                int64
		Replicas, ReadyReplicas, UpdatedReplicas, AvailableReplicas, CurrentNumberScheduled, DesiredNumberScheduled, NumberReady, NumberAvailable, UpdatedNumberScheduled int
	}
}
type pod struct {
	Metadata struct {
		Labels          map[string]string
		OwnerReferences []struct {
			UID, Kind, Name string
			Controller      bool
		}
		DeletionTimestamp *string
	}
	Spec   struct{ Containers []container }
	Status struct {
		ContainerStatuses []struct {
			Name, ImageID string
			Ready         bool
		}
	}
}

func matches(w workload, p pod) bool {
	// Match selectors, including expressions; empty selectors never capture unrelated pods.
	s := w.Spec.Selector
	if len(s.MatchLabels)+len(s.MatchExpressions) == 0 {
		return false
	}
	for k, v := range s.MatchLabels {
		if p.Metadata.Labels[k] != v {
			return false
		}
	}
	for _, e := range s.MatchExpressions {
		v, exists := p.Metadata.Labels[e.Key]
		contains := false
		for _, x := range e.Values {
			if v == x {
				contains = true
			}
		}
		switch e.Operator {
		case "In":
			if !exists || !contains {
				return false
			}
		case "NotIn":
			if contains {
				return false
			}
		case "Exists":
			if !exists {
				return false
			}
		case "DoesNotExist":
			if exists {
				return false
			}
		default:
			return false
		}
	}
	return p.Metadata.DeletionTimestamp == nil
}

var imageDigest = regexp.MustCompile(`sha256:[a-f0-9]{64}$`)

func Collect(ctx context.Context, k Getter, c Config) (rr.Report, error) {
	r := rr.Report{ProductID: c.ProductID, EnvironmentID: c.EnvironmentID, Cluster: c.Cluster, Workloads: []rr.Workload{}}
	for _, ns := range c.Namespaces {
		var pods struct {
			Items    []pod
			Metadata struct{ Continue string }
		}
		if err := k.Get(ctx, "/api/v1/namespaces/"+url.PathEscape(ns)+"/pods", &pods); err != nil {
			return r, err
		}
		if pods.Metadata.Continue != "" {
			return r, fmt.Errorf("incomplete pod list")
		}
		for _, resource := range []struct{ Plural, Kind string }{{"deployments", "Deployment"}, {"statefulsets", "StatefulSet"}, {"daemonsets", "DaemonSet"}} {
			var list struct {
				Items    []workload
				Metadata struct{ Continue string }
			}
			if err := k.Get(ctx, "/apis/apps/v1/namespaces/"+url.PathEscape(ns)+"/"+resource.Plural, &list); err != nil {
				return r, err
			}
			if list.Metadata.Continue != "" {
				return r, fmt.Errorf("incomplete workload list")
			}
			for _, w := range list.Items {
				v := rr.Workload{Namespace: ns, Kind: resource.Kind, Name: w.Metadata.Name, Generation: w.Metadata.Generation, ObservedGeneration: w.Status.ObservedGeneration, Replicas: w.Status.Replicas, ReadyReplicas: w.Status.ReadyReplicas, UpdatedReplicas: w.Status.UpdatedReplicas, AvailableReplicas: w.Status.AvailableReplicas, Containers: []rr.Container{}}
				if resource.Kind == "DaemonSet" {
					v.Replicas = w.Status.DesiredNumberScheduled
					v.ReadyReplicas = w.Status.NumberReady
					v.UpdatedReplicas = w.Status.UpdatedNumberScheduled
					v.AvailableReplicas = w.Status.NumberAvailable
				}
				for _, ct := range w.Spec.Template.Spec.Containers {
					cv := rr.Container{Name: ct.Name, Image: ct.Image}
					digests := map[string]bool{}
					for _, p := range pods.Items {
						if !matches(w, p) {
							continue
						}
						present := false
						for _, pc := range p.Spec.Containers {
							if pc.Name == ct.Name {
								present = true
							}
						}
						if !present {
							continue
						}
						cv.TotalPods++
						for _, status := range p.Status.ContainerStatuses {
							if status.Name == ct.Name {
								if status.Ready {
									cv.ReadyPods++
								}
								if d := imageDigest.FindString(status.ImageID); d != "" {
									digests[d] = true
								}
							}
						}
					}
					if len(digests) == 1 {
						for d := range digests {
							cv.ImageDigest = d
						}
					}
					v.Containers = append(v.Containers, cv)
				}
				r.Workloads = append(r.Workloads, v)
			}
		}
	}
	r.ReportedAt = time.Now().UTC()
	return r, rr.Validate(r, time.Now())
}
func Push(ctx context.Context, client *http.Client, c Config, r rr.Report) (int, error) {
	raw, err := json.Marshal(r)
	if err != nil {
		return 0, err
	}
	if len(raw) > 1<<20 {
		return 0, fmt.Errorf("report exceeds 1 MiB")
	}
	b, err := os.ReadFile(c.TokenFile)
	if err != nil || strings.TrimSpace(string(b)) == "" {
		return 0, fmt.Errorf("RCP token unavailable")
	}
	req, err := http.NewRequestWithContext(ctx, "POST", strings.TrimRight(c.URL, "/")+"/api/runtime-reports", bytes.NewReader(raw))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(b)))
	res, err := client.Do(req)
	if err != nil {
		return 0, fmt.Errorf("RCP request failed")
	}
	defer res.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, 4096))
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return res.StatusCode, fmt.Errorf("RCP HTTP %d", res.StatusCode)
	}
	return res.StatusCode, nil
}
