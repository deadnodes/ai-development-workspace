package registryobserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"syscall"
	"time"

	"releasecontrol/internal/domain"
	"releasecontrol/internal/workerclient"
	"releasecontrol/internal/workerconfig"
)

type Config struct {
	Endpoint       string                             `json:"endpoint"`
	TokenEnv       string                             `json:"token_env"`
	ConfigRevision string                             `json:"config_revision"`
	StateDir       string                             `json:"state_dir"`
	Recipe         workerconfig.RegistryObserveRecipe `json:"recipe"`
}
type journal struct {
	ConfigHash string               `json:"config_hash"`
	Lease      domain.WorkerLease   `json:"lease"`
	StartedAt  time.Time            `json:"started_at"`
	Report     *domain.WorkerReport `json:"report,omitempty"`
	Done       bool                 `json:"done"`
}

func decode(b []byte, v any) error {
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if d.Decode(v) != nil || d.Decode(new(any)) != io.EOF {
		return errScan
	}
	return nil
}
func Load(file string) (Config, error) {
	var c Config
	f, e := os.Open(file)
	if e != nil {
		return c, errScan
	}
	b, e := io.ReadAll(io.LimitReader(f, 1<<20+1))
	f.Close()
	if e != nil || len(b) > 1<<20 || decode(b, &c) != nil || validate(c) != nil {
		return c, errScan
	}
	return c, nil
}
func validate(c Config) error {
	if workerconfig.ValidateObservation(c.Recipe) != nil || !workerconfig.ValidIdentifier(c.ConfigRevision) || !filepath.IsAbs(c.StateDir) || !regexp.MustCompile(`^[A-Z_][A-Z0-9_]*$`).MatchString(c.TokenEnv) {
		return errScan
	}
	token := os.Getenv(c.TokenEnv)
	if len(token) < 32 {
		return errScan
	}
	for _, ref := range []string{c.Recipe.Pull.UsernameEnv, c.Recipe.Pull.PasswordEnv, c.Recipe.Pull.BearerEnv} {
		if ref != "" && (ref == c.TokenEnv || os.Getenv(ref) == "" || os.Getenv(ref) == token) {
			return errScan
		}
	}
	return nil
}
func durable(path string, j journal) error {
	b, e := json.Marshal(j)
	if e != nil {
		return errScan
	}
	f, e := os.CreateTemp(filepath.Dir(path), ".observe-")
	if e != nil {
		return errScan
	}
	name := f.Name()
	defer os.Remove(name)
	if e = f.Chmod(0600); e == nil {
		_, e = f.Write(b)
	}
	if e == nil {
		e = f.Sync()
	}
	ce := f.Close()
	if e != nil || ce != nil {
		return errScan
	}
	if e = os.Rename(name, path); e != nil {
		return errScan
	}
	d, e := os.Open(filepath.Dir(path))
	if e != nil {
		return errScan
	}
	defer d.Close()
	if d.Sync() != nil {
		return errScan
	}
	return nil
}
func matches(c Config, l domain.WorkerLease) bool {
	j, a := l.Job, l.Attempt
	r := j.Inputs.Registry
	if r == nil {
		return false
	}
	scope := r.Scope
	recipe := c.Recipe
	u := strings.TrimPrefix(strings.TrimPrefix(recipe.Origin, "https://"), "http://")
	image, e := domain.CanonicalImageRepository(u + "/" + recipe.Repository)
	return e == nil && j.Capability == "registry_observe" && j.ProductID == recipe.ProductID && j.Inputs.RepositoryID == recipe.RepositoryID && j.Inputs.SourceSHA == "" && j.Inputs.RevisionID == "" && j.Inputs.SourceProof == nil && j.Inputs.Check == nil && j.Inputs.Trust == "registry_observation" && len(j.Inputs.Parameters) == 0 && domain.ValidateRegistryScope(scope) == nil && len(r.Roots) <= 256 && j.Inputs.Execution == nil && j.Inputs.Publication == nil && j.Inputs.RecipeID == recipe.ID && j.Inputs.RecipeRevision == recipe.Revision && j.Inputs.RecipeHash == recipe.Hash && j.Inputs.ConfigRevision == c.ConfigRevision && workerconfig.Fingerprint(j.Inputs) == j.InputHash && scope == (domain.RegistryScope{ProductID: recipe.ProductID, RepositoryID: recipe.RepositoryID, ComponentID: recipe.ComponentID, Origin: recipe.Origin, Repository: recipe.Repository, ImageRepository: image}) && r.Limits == recipe.Limits && domain.RegistryRootSetHash(scope, recipe.ID, recipe.Revision, recipe.Hash, c.ConfigRevision, r.Roots) == r.RootSetHash && a.JobID == j.ID && a.InputHash == j.InputHash && a.Fence == j.Fence && !j.CancellationRequested
}

// RunOnce keeps one durable lease/report. Read-only recovery reuses the earliest
// observation start and its absolute timeout; completed reports are never rescanned.
func RunOnce(parent context.Context, c Config) (*domain.WorkerLease, error) {
	if validate(c) != nil {
		return nil, errScan
	}
	resolved, e := filepath.EvalSymlinks(c.StateDir)
	if e != nil || resolved != filepath.Clean(c.StateDir) {
		return nil, errScan
	}
	st, e := os.Stat(c.StateDir)
	if e != nil || !st.IsDir() || st.Mode().Perm()&0077 != 0 {
		return nil, errScan
	}
	lockPath := filepath.Join(c.StateDir, "observer.lock")
	lock, e := os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_RDWR|syscall.O_NOFOLLOW, 0600)
	if os.IsExist(e) {
		lock, e = os.OpenFile(lockPath, os.O_RDWR|syscall.O_NOFOLLOW, 0)
	}
	if e != nil {
		return nil, errScan
	}
	defer lock.Close()
	info, e := lock.Stat()
	pathInfo, pe := os.Lstat(lockPath)
	if e != nil || pe != nil || !os.SameFile(info, pathInfo) || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return nil, errScan
	}
	if syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) != nil {
		return nil, errScan
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	path := filepath.Join(c.StateDir, "observer.json")
	var j journal
	f, e := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if e == nil {
		info, se := f.Stat()
		if se != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() > 1<<20 {
			f.Close()
			return nil, errScan
		}
		b, re := io.ReadAll(io.LimitReader(f, 1<<20+1))
		f.Close()
		if re != nil || decode(b, &j) != nil || j.ConfigHash != workerconfig.Fingerprint(c) || !matches(c, j.Lease) {
			return nil, errScan
		}
	} else if !os.IsNotExist(e) {
		return nil, errScan
	}
	client, e := workerclient.NewRegistryObserver(c.Endpoint, os.Getenv(c.TokenEnv))
	if e != nil {
		return nil, errScan
	}
	if j.Done {
		if durable(filepath.Join(c.StateDir, "completed-"+strings.TrimPrefix(workerconfig.Fingerprint(j.Lease.Job.ID), "sha256:")+".json"), j) != nil {
			return nil, errScan
		}
		j = journal{}
	}
	if j.ConfigHash == "" {
		l, e := client.Claim(parent, domain.WorkerClaim{ProductID: c.Recipe.ProductID, RepositoryID: c.Recipe.RepositoryID, Capability: "registry_observe", RecipeID: c.Recipe.ID, RecipeHash: c.Recipe.Hash})
		if e != nil {
			return nil, errScan
		}
		if l == nil {
			return nil, nil
		}
		if !matches(c, *l) {
			return nil, errScan
		}
		j = journal{ConfigHash: workerconfig.Fingerprint(c), Lease: *l}
		if durable(path, j) != nil {
			return nil, errScan
		}
	}
	req := domain.WorkerAttemptRequest{JobID: j.Lease.Job.ID, AttemptID: j.Lease.Attempt.ID, Fence: j.Lease.Attempt.Fence, InputHash: j.Lease.Job.InputHash}
	if j.Report != nil {
		if j.Report.WorkerAttemptRequest != req || j.Report.Registry == nil || !j.Report.Registry.StartedAt.Equal(j.StartedAt) {
			return nil, errScan
		}
		out, e := client.Report(parent, *j.Report)
		if e != nil {
			return nil, errScan
		}
		if !matches(c, out) || out.Job.Status != "SUCCEEDED" || out.Attempt.Status != "SUCCEEDED" || !reflect.DeepEqual(out.Attempt.Report, j.Report) {
			return nil, errScan
		}
		j.Lease = out
		j.Done = true
		return &out, durable(path, j)
	}
	lease, e := client.Start(parent, req)
	if e != nil {
		return nil, errScan
	}
	if !matches(c, lease) {
		return nil, errScan
	}
	j.Lease = lease
	if j.StartedAt.IsZero() {
		j.StartedAt = time.Now().UTC()
	}
	if durable(path, j) != nil {
		return nil, errScan
	}
	deadline := j.StartedAt.Add(time.Duration(c.Recipe.Limits.TimeoutSeconds) * time.Second)
	if j.Lease.Job.Deadline.Before(deadline) {
		deadline = j.Lease.Job.Deadline
	}
	ctx, cancel := context.WithDeadline(parent, deadline)
	defer cancel()
	heartbeat := make(chan error, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		tick := time.NewTicker(time.Second)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				if _, e := client.Heartbeat(ctx, req); e != nil {
					select {
					case heartbeat <- errScan:
					default:
					}
					cancel()
					return
				}
			}
		}
	}()
	report, e := Observe(ctx, c.Recipe, *lease.Job.Inputs.Registry, j.StartedAt)
	interrupted := ctx.Err() != nil
	cancel()
	<-done
	if e != nil || interrupted {
		return nil, errScan
	}
	select {
	case <-heartbeat:
		return nil, errScan
	default:
	}
	report.ConfigRevision = c.ConfigRevision
	if domain.ValidateRegistryGraph(report.Graph, c.Recipe.Limits, lease.Job.Inputs.Registry.Roots) != nil {
		return nil, errScan
	}
	j.Report = &domain.WorkerReport{WorkerAttemptRequest: req, Result: "PASSED", Registry: report}
	if b, _ := json.Marshal(j.Report); len(b) > domain.RegistryReportMaxBytes {
		return nil, errScan
	}
	if durable(path, j) != nil {
		return nil, errScan
	}
	out, e := client.Report(parent, *j.Report)
	if e != nil {
		return nil, errScan
	}
	if !matches(c, out) || out.Job.Status != "SUCCEEDED" || out.Attempt.Status != "SUCCEEDED" || out.Job.ID != lease.Job.ID || !reflect.DeepEqual(out.Attempt.Report, j.Report) {
		return nil, errors.New("registry_report_rejected")
	}
	j.Lease = out
	j.Done = true
	return &out, durable(path, j)
}
