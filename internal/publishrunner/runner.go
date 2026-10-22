// Package publishrunner is a separate, operator-pinned publication executor.
package publishrunner

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
	"releasecontrol/internal/domain"
	"releasecontrol/internal/ocipublisher"
	"releasecontrol/internal/workerclient"
	"releasecontrol/internal/workerconfig"
	"strings"
	"syscall"
	"time"
)

type Config struct {
	Endpoint       string                         `json:"endpoint"`
	TokenEnv       string                         `json:"token_env"`
	ConfigRevision string                         `json:"config_revision"`
	Recipe         workerconfig.PublicationRecipe `json:"recipe"`
	Publisher      ocipublisher.Config            `json:"publisher"`
}
type journal struct {
	ConfigHash string               `json:"config_hash"`
	Lease      domain.WorkerLease   `json:"lease"`
	Report     *domain.WorkerReport `json:"report,omitempty"`
	Done       bool                 `json:"done"`
}

var invalid = errors.New("invalid pinned native publisher configuration or journal")

func Load(file string) (Config, error) {
	var c Config
	b, e := os.ReadFile(file)
	if e != nil || len(b) > 65536 {
		return c, invalid
	}
	if e = decode(b, &c); e != nil {
		return c, e
	}
	return c, validate(c)
}
func decode(b []byte, v any) error {
	d := json.NewDecoder(newReader(b))
	d.DisallowUnknownFields()
	if d.Decode(v) != nil || d.Decode(new(any)) != io.EOF {
		return invalid
	}
	return nil
}

// Using bytes.Reader keeps decoding independent of filesystem paths supplied by RCP.
func newReader(b []byte) *bytes.Reader { return bytes.NewReader(b) }
func validate(c Config) error {
	if workerconfig.ValidatePublication(c.Recipe) != nil || c.ConfigRevision == "" || !regexp.MustCompile(`^[A-Z_][A-Z0-9_]*$`).MatchString(c.TokenEnv) || c.Publisher.AllowedOrigin != c.Recipe.Origin || c.Publisher.AllowedRepository != c.Recipe.Repository || c.Publisher.MaxArchiveBytes != c.Recipe.MaxBytes || c.Publisher.TimeoutSeconds != c.Recipe.TimeoutSeconds || !filepath.IsAbs(c.Publisher.Archive) || !filepath.IsAbs(c.Publisher.StateDir) {
		return invalid
	}
	for _, ref := range []string{c.Publisher.UsernameEnv, c.Publisher.PasswordEnv, c.Publisher.BearerEnv} {
		if ref != "" {
			if ref == c.TokenEnv || os.Getenv(ref) == "" || os.Getenv(ref) == os.Getenv(c.TokenEnv) {
				return invalid
			}
			for _, pull := range []string{c.Recipe.Pull.UsernameEnv, c.Recipe.Pull.PasswordEnv, c.Recipe.Pull.BearerEnv} {
				if ref == pull || pull != "" && os.Getenv(pull) != "" && os.Getenv(ref) == os.Getenv(pull) {
					return invalid
				}
			}
		}
	}
	return nil
}
func durable(file string, j journal) error {
	b, e := json.Marshal(j)
	if e != nil {
		return e
	}
	f, e := os.CreateTemp(filepath.Dir(file), ".native-")
	if e != nil {
		return e
	}
	name := f.Name()
	defer os.Remove(name)
	if _, e = f.Write(b); e == nil {
		e = f.Sync()
	}
	ce := f.Close()
	if e != nil {
		return e
	}
	if ce != nil {
		return ce
	}
	if e = os.Rename(name, file); e != nil {
		return e
	}
	d, e := os.Open(filepath.Dir(file))
	if e != nil {
		return e
	}
	defer d.Close()
	return d.Sync()
}
func leaseMatches(c Config, l domain.WorkerLease) bool {
	j, a := l.Job, l.Attempt
	p := j.Inputs.Publication
	r := c.Recipe
	return j.Capability == "registry_publish" && j.ProductID == r.ProductID && j.Inputs.RepositoryID == r.RepositoryID && j.Inputs.RecipeID == r.ID && j.Inputs.RecipeHash == r.Hash && j.Inputs.RecipeRevision == r.Revision && j.Inputs.ConfigRevision == c.ConfigRevision && workerconfig.Fingerprint(j.Inputs) == j.InputHash && p != nil && p.ComponentID == r.ComponentID && p.Profile == r.Profile && p.Origin == r.Origin && p.Repository == r.Repository && p.Receipt.RecipeHash == r.BuildRecipeHash && j.Inputs.SourceSHA == p.Receipt.SourceSHA && a.JobID == j.ID && a.InputHash == j.InputHash && a.Fence == j.Fence && !j.CancellationRequested
}

// RunOnce durably retains one lease and pending report. Restart always retries
// the same START, immutable digest publication or REPORT; it never rebuilds.
func RunOnce(parent context.Context, c Config) (*domain.WorkerLease, error) {
	if e := validate(c); e != nil {
		return nil, e
	}
	dir := c.Publisher.StateDir
	resolved, e := filepath.EvalSymlinks(dir)
	if e != nil || resolved != filepath.Clean(dir) {
		return nil, invalid
	}
	st, e := os.Stat(dir)
	if e != nil || !st.IsDir() || st.Mode().Perm()&0077 != 0 {
		return nil, invalid
	}
	lock, e := os.OpenFile(filepath.Join(dir, "native.lock"), os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0600)
	if e != nil {
		return nil, invalid
	}
	defer lock.Close()
	lockInfo, err := lock.Stat()
	if err != nil || !lockInfo.Mode().IsRegular() || lockInfo.Mode().Perm()&0077 != 0 {
		return nil, invalid
	}
	if e = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); e != nil {
		return nil, errors.New("native publication already running")
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	file := filepath.Join(dir, "native.json")
	var j journal
	f, e := os.OpenFile(file, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if e == nil {
		st, se := f.Stat()
		if se != nil || !st.Mode().IsRegular() || st.Mode().Perm()&0077 != 0 || st.Size() > 65536 {
			f.Close()
			return nil, invalid
		}
		b, err := io.ReadAll(io.LimitReader(f, 65537))
		if err == nil {
			err = f.Sync()
		}
		f.Close()
		if err != nil || decode(b, &j) != nil || j.ConfigHash != workerconfig.Fingerprint(c) || !leaseMatches(c, j.Lease) {
			return nil, invalid
		}
		d, err := os.Open(dir)
		if err != nil {
			return nil, err
		}
		err = d.Sync()
		d.Close()
		if err != nil {
			return nil, err
		}
	} else if !os.IsNotExist(e) {
		return nil, invalid
	}
	client, e := workerclient.New(c.Endpoint, os.Getenv(c.TokenEnv))
	if e != nil {
		return nil, e
	}
	if j.Done {
		// Preserve completed provenance before allowing the same profile to claim a
		// different job. The completed current journal remains safe across a crash.
		history := filepath.Join(dir, "completed-"+strings.TrimPrefix(workerconfig.Fingerprint(j.Lease.Job.ID), "sha256:")+".json")
		if e = durable(history, j); e != nil {
			return nil, e
		}
		j = journal{}
	}
	if j.ConfigHash == "" {
		lease, e := client.Claim(parent, domain.WorkerClaim{ProductID: c.Recipe.ProductID, RepositoryID: c.Recipe.RepositoryID, Capability: "registry_publish", RecipeID: c.Recipe.ID, RecipeHash: c.Recipe.Hash})
		if e != nil || lease == nil {
			return nil, e
		}
		if !leaseMatches(c, *lease) {
			return nil, invalid
		}
		j = journal{ConfigHash: workerconfig.Fingerprint(c), Lease: *lease}
		if e = durable(file, j); e != nil {
			return nil, e
		}
	}
	req := domain.WorkerAttemptRequest{JobID: j.Lease.Job.ID, AttemptID: j.Lease.Attempt.ID, Fence: j.Lease.Attempt.Fence, InputHash: j.Lease.Job.InputHash}
	if j.Report != nil {
		if j.Report.WorkerAttemptRequest != req || j.Report.Result != "PASSED" || j.Report.LocalOCI != nil || j.Report.Publication == nil || j.Report.Publication.ManifestDigest != j.Lease.Job.Inputs.Publication.Receipt.ManifestDigest {
			return nil, invalid
		}
		ctx, stop, _ := keepAlive(parent, client, c, req, j.Lease.Job.Deadline)
		defer stop()
		out, e := client.Report(ctx, *j.Report)
		if e != nil {
			return nil, e
		}
		j.Done = true
		j.Lease = out
		return &out, durable(file, j)
	}
	lease, e := client.Start(parent, req)
	if e != nil {
		return nil, e
	}
	if !leaseMatches(c, lease) {
		return nil, invalid
	}
	j.Lease = lease
	if e = durable(file, j); e != nil {
		return nil, e
	}
	ctx, stop, hbErr := keepAlive(parent, client, c, req, lease.Job.Deadline)
	defer stop()
	p := *lease.Job.Inputs.Publication
	publisher := c.Publisher
	publisher.Plan = ocipublisher.Plan{ID: lease.Job.ID, Approved: true, ProductID: lease.Job.ProductID, ComponentID: p.ComponentID, RepositoryID: lease.Job.Inputs.RepositoryID, Origin: p.Origin, Repository: p.Repository, Receipt: p.Receipt}
	observation, e := ocipublisher.Publish(ctx, publisher)
	select {
	case err := <-hbErr:
		return nil, err
	default:
	}
	if e != nil {
		return nil, e
	}
	if observation.ManifestDigest != p.Receipt.ManifestDigest || !reflect.DeepEqual(observation.Plan.Receipt, p.Receipt) {
		return nil, invalid
	}
	report := domain.WorkerReport{WorkerAttemptRequest: req, Result: "PASSED", Publication: &domain.WorkerPublicationReport{ManifestDigest: observation.ManifestDigest}}
	j.Report = &report
	if e = durable(file, j); e != nil {
		return nil, e
	}
	out, e := client.Report(ctx, report)
	if e != nil {
		return nil, e
	}
	j.Done = true
	j.Lease = out
	return &out, durable(file, j)
}

func keepAlive(parent context.Context, client *workerclient.Client, c Config, req domain.WorkerAttemptRequest, deadline time.Time) (context.Context, func(), <-chan error) {
	ctx, cancel := context.WithDeadline(parent, deadline)
	done := make(chan struct{})
	hbErr := make(chan error, 1)
	go func() {
		defer close(done)
		ticker := time.NewTicker(500 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				l, e := client.Heartbeat(ctx, req)
				if e != nil || !leaseMatches(c, l) {
					if e == nil {
						e = invalid
					}
					hbErr <- e
					cancel()
					return
				}
			}
		}
	}()
	return ctx, func() { cancel(); <-done }, hbErr
}
