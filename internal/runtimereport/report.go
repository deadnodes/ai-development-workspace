// Package runtimereport defines the bounded push observation contract, outside domain.State.
package runtimereport

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

var ErrTooLarge = errors.New("runtime workload storage limit")
var ErrRateLimit = errors.New("runtime report rate limit")
var label = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)
var workloadName = regexp.MustCompile(`^[a-z0-9]([a-z0-9.-]*[a-z0-9])?$`)
var digest = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)

type Container struct {
	Name        string `json:"name"`
	Image       string `json:"image"`
	ImageDigest string `json:"image_digest"`
	ReadyPods   int    `json:"ready_pods"`
	TotalPods   int    `json:"total_pods"`
}
type Workload struct {
	Namespace          string      `json:"namespace"`
	Kind               string      `json:"kind"`
	Name               string      `json:"name"`
	Generation         int64       `json:"generation"`
	ObservedGeneration int64       `json:"observed_generation"`
	Replicas           int         `json:"replicas"`
	ReadyReplicas      int         `json:"ready_replicas"`
	UpdatedReplicas    int         `json:"updated_replicas"`
	AvailableReplicas  int         `json:"available_replicas"`
	Containers         []Container `json:"containers"`
	Absent             bool        `json:"absent,omitempty"`
}
type Report struct {
	ProductID     string     `json:"product_id"`
	EnvironmentID string     `json:"environment_id"`
	Cluster       string     `json:"cluster"`
	ReportedAt    time.Time  `json:"reported_at"`
	Workloads     []Workload `json:"workloads"`
}
type Record struct {
	AbsentSince time.Time `json:"absent_since,omitempty"`
	Document    Workload  `json:"document"`
	ObservedAt  time.Time `json:"observed_at"`
	ChangedAt   time.Time `json:"changed_at"`
}
type Cluster struct {
	ProductID        string    `json:"product_id"`
	EnvironmentID    string    `json:"environment_id"`
	Cluster          string    `json:"cluster"`
	ReportedAt       time.Time `json:"reported_at"`
	AcceptedAt       time.Time `json:"-"`
	Workloads        int       `json:"workloads"`
	StalenessSeconds int64     `json:"staleness_seconds"`
	Stale            bool      `json:"stale"`
	Records          []Record  `json:"workload_records"`
}

// AcceptedAt is persisted separately from the reporter-controlled clock.
type Snapshot struct {
	Cluster    Cluster       `json:"cluster"`
	AcceptedAt time.Time     `json:"accepted_at"`
	Journal    JournalBudget `json:"journal"`
}
type Change struct {
	Old *Workload `json:"old,omitempty"`
	New Workload  `json:"new"`
}
type Store interface {
	SaveRuntimeReport(context.Context, Report, time.Time) (JournalResult, error)
	ReadRuntimeReports(context.Context, string, string, string, ...ReadWindow) ([]Cluster, error)
}

func Key(w Workload) string           { return w.Namespace + "/" + w.Kind + "/" + w.Name }
func Scope(p, e, c string) string     { return p + "\x00" + e + "\x00" + c }
func ValidLabel(s string, n int) bool { return len(s) > 0 && len(s) <= n && label.MatchString(s) }
func Validate(r Report, now time.Time) error {
	for _, s := range []string{r.ProductID, r.EnvironmentID} {
		if !ValidLabel(s, 128) {
			return fmt.Errorf("invalid product/environment identifier")
		}
	}
	if !ValidLabel(r.Cluster, 64) || r.ReportedAt.IsZero() || r.ReportedAt.After(now.Add(5*time.Minute)) || len(r.Workloads) > 1000 {
		return fmt.Errorf("invalid cluster, timestamp or workload count")
	}
	seen := map[string]bool{}
	for _, w := range r.Workloads {
		if !ValidLabel(w.Namespace, 63) || !(len(w.Name) > 0 && len(w.Name) <= 253 && workloadName.MatchString(w.Name)) || (w.Kind != "Deployment" && w.Kind != "StatefulSet" && w.Kind != "DaemonSet") || w.Absent || len(w.Containers) > 20 || seen[Key(w)] {
			return fmt.Errorf("invalid or duplicate workload")
		}
		seen[Key(w)] = true
		if w.Generation < 0 || w.ObservedGeneration < 0 || w.Replicas < 0 || w.ReadyReplicas < 0 || w.UpdatedReplicas < 0 || w.AvailableReplicas < 0 {
			return fmt.Errorf("negative workload count")
		}
		names := map[string]bool{}
		for _, c := range w.Containers {
			if !ValidLabel(c.Name, 63) || len(c.Image) == 0 || len(c.Image) > 1024 || (c.ImageDigest != "" && !digest.MatchString(c.ImageDigest)) || c.ReadyPods < 0 || c.TotalPods < 0 || c.ReadyPods > c.TotalPods || names[c.Name] {
				return fmt.Errorf("invalid container")
			}
			names[c.Name] = true
		}
	}
	return nil
}
func same(a, b any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}

// Signal excludes rollout bookkeeping: journal only images, digests, readiness and absence.
func signal(w Workload) any {
	return struct {
		Absent           bool
		Ready, Available int
		Containers       []Container
	}{w.Absent, w.ReadyReplicas, w.AvailableReplicas, w.Containers}
}
func Apply(old Snapshot, r Report, now time.Time) (Snapshot, []Change, error) {
	if !old.AcceptedAt.IsZero() && (now.Sub(old.AcceptedAt) < 20*time.Second || !r.ReportedAt.After(old.Cluster.ReportedAt)) {
		return old, nil, ErrRateLimit
	}
	previous := map[string]Record{}
	for _, v := range old.Cluster.Records {
		previous[Key(v.Document)] = v
	}
	next := Snapshot{Cluster: Cluster{ProductID: r.ProductID, EnvironmentID: r.EnvironmentID, Cluster: r.Cluster, ReportedAt: r.ReportedAt, Workloads: len(r.Workloads)}, AcceptedAt: now}
	changes := []Change{}
	add := func(w Workload) {
		w.Containers = append([]Container{}, w.Containers...)
		sort.Slice(w.Containers, func(i, j int) bool { return w.Containers[i].Name < w.Containers[j].Name })
		v, exists := previous[Key(w)]
		changed := !exists || !same(v.Document, w)
		if !exists || !same(signal(v.Document), signal(w)) {
			c := Change{New: w}
			if exists {
				copy := v.Document
				c.Old = &copy
			}
			changes = append(changes, c)
		}
		at := v.ChangedAt
		if changed {
			at = r.ReportedAt
		}
		next.Cluster.Records = append(next.Cluster.Records, Record{Document: w, ObservedAt: r.ReportedAt, ChangedAt: at, AbsentSince: func() time.Time {
			if !w.Absent {
				return time.Time{}
			}
			if !v.AbsentSince.IsZero() {
				return v.AbsentSince
			}
			return now
		}()})
		delete(previous, Key(w))
	}
	for _, w := range r.Workloads {
		add(w)
	}
	for _, v := range previous {
		if v.Document.Absent && !v.AbsentSince.IsZero() && now.Sub(v.AbsentSince) > 7*24*time.Hour {
			continue
		}
		w := v.Document
		w.Absent = true
		add(w)
	}
	sort.Slice(next.Cluster.Records, func(i, j int) bool {
		a, b := next.Cluster.Records[i], next.Cluster.Records[j]
		if a.Document.Absent != b.Document.Absent {
			return a.Document.Absent
		}
		if !a.AbsentSince.Equal(b.AbsentSince) {
			return a.AbsentSince.Before(b.AbsentSince)
		}
		return Key(a.Document) < Key(b.Document)
	})
	for len(next.Cluster.Records) > 2000 && next.Cluster.Records[0].Document.Absent {
		next.Cluster.Records = next.Cluster.Records[1:]
	}
	if len(next.Cluster.Records) > 2000 {
		return old, nil, ErrTooLarge
	}
	sort.Slice(next.Cluster.Records, func(i, j int) bool {
		return Key(next.Cluster.Records[i].Document) < Key(next.Cluster.Records[j].Document)
	})
	return next, changes, nil
}
func Fresh(c Cluster, now time.Time) Cluster {
	c.StalenessSeconds = int64(now.Sub(c.ReportedAt).Seconds())
	if c.StalenessSeconds < 0 {
		c.StalenessSeconds = 0
	}
	c.Stale = now.Sub(c.ReportedAt) > 5*time.Minute
	return c
}

// MemoryStore is a concurrency-safe test/in-memory implementation of the same transaction.
type MemoryStore struct {
	mu        sync.Mutex
	snapshots map[string]Snapshot
}

func (m *MemoryStore) SaveRuntimeReport(ctx context.Context, r Report, now time.Time) (JournalResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return JournalResult{}, err
	}
	if m.snapshots == nil {
		m.snapshots = map[string]Snapshot{}
	}
	for _, snap := range m.snapshots {
		if snap.Cluster.ProductID == r.ProductID && snap.Cluster.EnvironmentID == r.EnvironmentID && now.Sub(snap.AcceptedAt) < 20*time.Second {
			return JournalResult{}, ErrRateLimit
		}
	}
	key := Scope(r.ProductID, r.EnvironmentID, r.Cluster)
	v, c, e := Apply(m.snapshots[key], r, now)
	result := JournalResult{}
	if e == nil {
		v.Journal, result = ReserveJournal(m.snapshots[key].Journal, c, now)
	}
	if e == nil {
		raw, err := json.Marshal(v)
		if err != nil {
			return JournalResult{}, err
		}
		var detached Snapshot
		if err = json.Unmarshal(raw, &detached); err != nil {
			return JournalResult{}, err
		}
		m.snapshots[key] = detached
	}
	return result, e
}
func (m *MemoryStore) ReadRuntimeReports(ctx context.Context, p, e, c string, windows ...ReadWindow) ([]Cluster, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []Cluster{}
	for _, v := range m.snapshots {
		if v.Cluster.ProductID == p && v.Cluster.EnvironmentID == e && (c == "" || v.Cluster.Cluster == c) {
			b, _ := json.Marshal(v.Cluster)
			var copy Cluster
			_ = json.Unmarshal(b, &copy)
			out = append(out, copy)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Cluster < out[j].Cluster })
	return WindowClusters(out, windows...), ctx.Err()
}

// ParseScopes rejects broad product/environment grants and malformed cluster entries.
func ParseScopes(raw string) (map[string]bool, error) {
	out := map[string]bool{}
	if strings.TrimSpace(raw) == "" {
		return out, nil
	}
	for _, entry := range strings.Split(raw, ",") {
		parts := strings.Split(strings.TrimSpace(entry), ":")
		if len(parts) != 3 || !ValidLabel(parts[0], 128) || !ValidLabel(parts[1], 128) || !ValidLabel(parts[2], 64) {
			return nil, fmt.Errorf("runtime reporter scope must be product:environment:cluster")
		}
		out[strings.Join(parts, ":")] = true
	}
	return out, nil
}

type PageRequest struct {
	Limit  int    `json:"limit,omitempty"`
	Cursor string `json:"cursor,omitempty"`
}

// ReadWindow bounds storage results including one lookahead item for pagination.
type ReadWindow struct {
	After string
	Limit int
}

func WindowClusters(clusters []Cluster, windows ...ReadWindow) []Cluster {
	if len(windows) == 0 {
		return clusters
	}
	window := windows[0]
	out := []Cluster{}
	count := 0
	sort.Slice(clusters, func(i, j int) bool { return clusters[i].Cluster < clusters[j].Cluster })
	for _, c := range clusters {
		records := c.Records
		c.Records = []Record{}
		sort.Slice(records, func(i, j int) bool { return Key(records[i].Document) < Key(records[j].Document) })
		if len(records) == 0 {
			if c.Cluster+"!" > window.After && count < window.Limit {
				out = append(out, c)
				count++
			}
		} else {
			for _, r := range records {
				if c.Cluster+"!"+Key(r.Document) > window.After && count < window.Limit {
					c.Records = append(c.Records, r)
					count++
				}
			}
			if len(c.Records) > 0 {
				out = append(out, c)
			}
		}
		if count >= window.Limit {
			break
		}
	}
	return out
}
