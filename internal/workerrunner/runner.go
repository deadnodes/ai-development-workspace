package workerrunner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"

	"releasecontrol/internal/artifactspool"
	"releasecontrol/internal/domain"
	"releasecontrol/internal/ociverify"
	"releasecontrol/internal/workerclient"
	"releasecontrol/internal/workerconfig"
)

type SourceGitHubAccess struct {
	Anonymous bool   `json:"anonymous"`
	TokenEnv  string `json:"token_env,omitempty"`
}
type Config struct {
	SourceGitHub *SourceGitHubAccess `json:"source_github,omitempty"`

	ArtifactSpool           *artifactspool.Config `json:"artifact_spool,omitempty"`
	Endpoint                string                `json:"endpoint"`
	TokenEnv                string                `json:"token_env"`
	ProductID               string                `json:"product_id"`
	RepositoryID            string                `json:"repository_id"`
	ConfigRevision          string                `json:"config_revision"`
	Recipe                  workerconfig.Recipe   `json:"recipe"`
	SourceMirror            string                `json:"source_mirror,omitempty"`
	StateDir                string                `json:"state_dir"`
	DockerContext           string                `json:"docker_context,omitempty"`
	DockerConfig            string                `json:"docker_config,omitempty"`
	AllowRootlessUnconfined bool                  `json:"allow_rootless_unconfined"`
}

func Load(file string) (Config, error) {
	var c Config
	f, e := os.Open(file)
	if e != nil {
		return c, e
	}
	defer f.Close()
	d := json.NewDecoder(io.LimitReader(f, 1<<20))
	d.DisallowUnknownFields()
	if e = d.Decode(&c); e != nil {
		return c, errors.New("invalid worker configuration")
	}
	if d.Decode(new(any)) != io.EOF {
		return c, errors.New("invalid worker configuration")
	}
	return c, validateConfig(c)
}
func validateConfig(c Config) error {
	if c.ArtifactSpool != nil && artifactspool.Validate(*c.ArtifactSpool) != nil {
		return errors.New("invalid artifact spool configuration")
	}
	if c.Recipe.Hash != workerconfig.RecipeHash(c.Recipe) || c.Recipe.Execution == nil || workerconfig.ValidateExecution(c.Recipe) != nil || c.Recipe.Trust != "trusted_source" || c.Recipe.ProductID != c.ProductID || c.Recipe.RepositoryID != c.RepositoryID || c.ConfigRevision == "" || !filepath.IsAbs(c.StateDir) || c.SourceMirror != "" && !filepath.IsAbs(c.SourceMirror) || !regexp.MustCompile(`^[A-Z_][A-Z0-9_]*$`).MatchString(c.TokenEnv) || c.DockerContext != "" && !regexp.MustCompile(`^[A-Za-z0-9_.-]{1,64}$`).MatchString(c.DockerContext) {
		return errors.New("invalid pinned worker configuration")
	}
	if err := validateSourceStrategy(c); err != nil {
		return err
	}
	if c.Recipe.Capability == "local_oci_build" && !c.AllowRootlessUnconfined {
		return errors.New("rootless BuildKit security exceptions require explicit operator opt-in")
	}
	return nil
}

type journalState struct {
	sync.Mutex
	path  string
	value journal
}

func (j *journalState) update(fn func(*journal)) error {
	j.Lock()
	defer j.Unlock()
	latest, err := updateJournal(j.path, func(v *journal) error { fn(v); return nil })
	if err == nil {
		j.value = latest
	}
	return err
}
func (j *journalState) snapshot() journal {
	j.Lock()
	defer j.Unlock()
	if latest, err := readJournal(j.path); err == nil {
		j.value = latest
	}
	return j.value
}
func (j *journalState) beginResource(name string) error {
	j.Lock()
	defer j.Unlock()
	latest, err := updateJournal(j.path, func(v *journal) error {
		now := time.Now()
		if v.Done || !now.Before(v.Deadline) || !now.Before(v.LeaseUntil) {
			return errors.New("resource creation after lease/deadline forbidden")
		}
		for _, r := range v.Resources {
			if r.Name == name {
				return errors.New("resource creation cannot be replayed")
			}
		}
		v.Resources = append(v.Resources, ownedResource{Name: name, CreateState: "CREATING"})
		return nil
	})
	if err == nil {
		j.value = latest
	}
	return err
}
func (j *journalState) completeCreate(name, id string, createErr error) error {
	return j.update(func(v *journal) {
		for i := range v.Resources {
			if v.Resources[i].Name == name {
				if createErr != nil {
					v.Resources[i].CreateState = "CREATE_UNCERTAIN"
				} else {
					v.Resources[i].ID = id
					v.Resources[i].CreateState = "CREATED"
				}
			}
		}
	})
}

// RunOne claims and executes one real isolated job. No source command receives
// control-plane credentials, host sockets, Git metadata or registry credentials.
func RunOne(parent context.Context, c Config) (*domain.WorkerReport, error) {
	return runOne(parent, c, nil)
}

// Tests inject transport for the production API URL, not an operator host option.
func runOne(parent context.Context, c Config, sourceClient *http.Client) (*domain.WorkerReport, error) {
	if c.DockerConfig == "" {
		home, e := os.UserHomeDir()
		if e != nil {
			return nil, e
		}
		c.DockerConfig = filepath.Join(home, ".docker")
	}
	if e := validateConfig(c); e != nil {
		return nil, e
	}
	if e := os.MkdirAll(c.StateDir, 0700); e != nil {
		return nil, e
	}
	stat, e := os.Stat(c.StateDir)
	if e != nil || stat.Mode().Perm()&0077 != 0 {
		return nil, errors.New("worker state directory must be private")
	}
	lock, e := os.OpenFile(filepath.Join(c.StateDir, ".lock"), os.O_CREATE|os.O_RDWR, 0600)
	if e != nil {
		return nil, e
	}
	defer lock.Close()
	if syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) != nil {
		return nil, errors.New("another supervisor owns worker state")
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	if e := Recover(c.StateDir); e != nil {
		return nil, e
	}
	workerToken := os.Getenv(c.TokenEnv)
	sourceCredential := sourceCredential(c, workerToken)
	client, e := workerclient.New(c.Endpoint, workerToken)
	if e != nil {
		return nil, e
	}
	// A persisted result may be replayed after cleanup/restart, but execution
	// itself is never resumed. The server still validates lease and fencing.
	entries, _ := os.ReadDir(c.StateDir)
	for _, entry := range entries {
		if !entry.IsDir() || !ownerPattern.MatchString(entry.Name()) {
			continue
		}
		j, err := readJournal(filepath.Join(c.StateDir, entry.Name(), "journal.json"))
		if err == nil && j.Report != nil {
			_, _ = client.Report(parent, *j.Report)
		}
	}
	if c.Recipe.Execution.SourceFetch != nil {
		if _, err := sourceCredential(); err != nil {
			return nil, err
		}
	}
	lease, e := client.Claim(parent, domain.WorkerClaim{ProductID: c.ProductID, RepositoryID: c.RepositoryID, Capability: c.Recipe.Capability, RecipeID: c.Recipe.ID, RecipeHash: c.Recipe.Hash})
	if e != nil || lease == nil {
		return nil, e
	}
	if lease.Job.Inputs.RecipeHash != c.Recipe.Hash || lease.Job.Inputs.RecipeRevision != c.Recipe.Revision || lease.Job.Inputs.ConfigRevision != c.ConfigRevision || lease.Job.Inputs.RepositoryURL != c.Recipe.RepositoryURL || lease.Job.Inputs.Trust != "trusted_source" || workerconfig.Fingerprint(lease.Job.Inputs.Execution) != workerconfig.Fingerprint(c.Recipe.Execution) {
		return nil, errors.New("claimed job differs from operator execution configuration")
	}
	if lease.Job.ProductID != c.ProductID || lease.Job.Inputs.RepositoryID != c.RepositoryID || lease.Job.Capability != c.Recipe.Capability || lease.Job.InputHash != workerconfig.Fingerprint(lease.Job.Inputs) || lease.Attempt.InputHash != lease.Job.InputHash || lease.Attempt.JobID != lease.Job.ID || lease.Attempt.Fence != lease.Job.Fence {
		return nil, errors.New("claimed job immutable identity mismatch")
	}
	request := domain.WorkerAttemptRequest{JobID: lease.Job.ID, AttemptID: lease.Attempt.ID, Fence: lease.Attempt.Fence, InputHash: lease.Job.InputHash}
	spec := c.Recipe.Execution
	deadline := time.Now().Add(time.Duration(spec.TimeoutSeconds) * time.Second)
	if deadline.After(lease.Job.Deadline) {
		deadline = lease.Job.Deadline
	}
	ctx, cancel := context.WithDeadline(parent, deadline)
	defer cancel()
	owner := nonce()
	root := filepath.Join(c.StateDir, owner)
	if e = os.Mkdir(root, 0700); e != nil {
		return nil, e
	}
	js := &journalState{path: filepath.Join(root, "journal.json"), value: journal{Owner: owner, ParentPID: os.Getpid(), DockerContext: c.DockerContext, DockerConfig: c.DockerConfig, Deadline: deadline, LeaseUntil: lease.Attempt.LeaseUntil, SourceSHA: lease.Job.Inputs.SourceSHA, Resources: []ownedResource{}}}
	if e = saveJournal(js.path, js.value); e != nil {
		return nil, e
	}
	if e = startWatchdog(js.path, js.value); e != nil {
		return nil, e
	}
	defer func() { _ = cleanup(js.path) }()
	var started domain.WorkerLease
	for tries := 0; tries < 3; tries++ {
		started, e = client.Start(ctx, request)
		if e == nil {
			break
		}
	}
	if e != nil {
		return nil, e
	}
	if e = js.update(func(j *journal) { j.LeaseUntil = started.Attempt.LeaseUntil }); e != nil {
		return nil, e
	}
	stopHeartbeat := superviseWorkerHeartbeat(ctx, cancel, client, request, js)
	defer stopHeartbeat()
	result, runErr := execute(ctx, c, root, js, sourceClient, sourceCredential)
	report, runErr := prepareReport(ctx, c, root, request, result, runErr)
	// The terminal report must not race a heartbeat rejected after its own
	// acceptance. Join supervision first, then honor any lease failure observed
	// during execution/export before persisting the final report.
	stopHeartbeat()
	if ctx.Err() != nil {
		report, runErr = prepareReport(ctx, c, root, request, nil, ctx.Err())
	}
	if report.LocalOCI != nil {
		result.JobID = lease.Job.ID
		result.AttemptID = lease.Attempt.ID
		result.InputHash = lease.Job.InputHash
		result.SourceSHA = lease.Job.Inputs.SourceSHA
		result.RecipeHash = c.Recipe.Hash
		result.ConfigRevision = c.ConfigRevision
		result.ToolchainImage = spec.ToolchainImage
	}
	if e = js.update(func(j *journal) { j.Report = report }); e != nil {
		return nil, e
	}
	// Repeating the identical result is safe even if the first response is lost.
	for tries := 0; tries < 3; tries++ {
		_, e = client.Report(ctx, *report)
		if e == nil {
			break
		}
	}
	if e != nil {
		return report, e
	}
	return report, runErr
}

// superviseWorkerHeartbeat keeps lease failures authoritative until execution
// has a durable terminal result. Stopping joins an in-flight bounded HTTP call;
// it never cancels the parent/report context or ignores a known lease failure.
func superviseWorkerHeartbeat(ctx context.Context, cancel context.CancelFunc, client *workerclient.Client, request domain.WorkerAttemptRequest, js *journalState) func() {
	stopHeartbeat := make(chan struct{})
	heartbeatDone := make(chan struct{})
	go func() {
		defer close(heartbeatDone)
		for {
			remaining := time.Until(js.snapshot().LeaseUntil)
			interval := remaining / 3
			if interval > time.Second {
				interval = time.Second
			}
			if interval < 20*time.Millisecond {
				interval = 20 * time.Millisecond
			}
			select {
			case <-stopHeartbeat:
				return
			case <-ctx.Done():
				return
			case <-time.After(interval):
			}
			// A blocked renewal cannot remain authoritative beyond the lease
			// it is trying to renew. This also bounds finalization's join by
			// the current lease and preserves the execution/parent deadline.
			heartbeatCtx, finishHeartbeat := context.WithDeadline(ctx, js.snapshot().LeaseUntil)
			renewed, err := client.Heartbeat(heartbeatCtx, request)
			finishHeartbeat()
			if err != nil || js.update(func(j *journal) { j.LeaseUntil = renewed.Attempt.LeaseUntil }) != nil {
				cancel()
				return
			}
		}
	}()
	var once sync.Once
	return func() { once.Do(func() { close(stopHeartbeat) }); <-heartbeatDone }
}

// prepareReport is the durability boundary between a verified local output and
// an admissible report. A known lease stop always overrides executor success.
func prepareReport(ctx context.Context, c Config, root string, request domain.WorkerAttemptRequest, result *domain.WorkerLocalOCI, runErr error) (*domain.WorkerReport, error) {
	report := &domain.WorkerReport{WorkerAttemptRequest: request, Result: "EXECUTOR_FAILED"}
	if runErr == nil && result != nil && c.ArtifactSpool != nil {
		runErr = artifactspool.Export(ctx, *c.ArtifactSpool, filepath.Join(root, "image.oci.tar"), *result)
	}
	if errors.Is(runErr, errCheckFailed) {
		report.Result = "CHECK_FAILED"
	}
	if ctx.Err() != nil {
		runErr = ctx.Err()
		report.Result = "EXECUTOR_FAILED"
	}
	if runErr == nil {
		report.Result = "PASSED"
		report.LocalOCI = result
	}
	return report, runErr
}
func execute(ctx context.Context, c Config, root string, js *journalState, sourceClient *http.Client, sourceCredential func() (string, error)) (*domain.WorkerLocalOCI, error) {
	e := c.Recipe.Execution
	source, err := materializeSource(ctx, c, root, jsSource(js), sourceClient, sourceCredential)
	if err != nil {
		return nil, err
	}
	d := docker{context: c.DockerContext, config: c.DockerConfig}
	info, err := d.output(ctx, nil, 128, "info", "--format", "{{.OSType}}/{{.Architecture}}")
	native := strings.ReplaceAll(stringTrim(info), "aarch64", "arm64")
	native = strings.ReplaceAll(native, "x86_64", "amd64")
	if err != nil || native != e.Platform {
		return nil, errors.New("native platform not proven")
	}
	// Never pull images implicitly. Operator-approved immutable images must be cached.
	for _, image := range []string{e.ToolchainImage, e.BuildKitImage} {
		if image == "" {
			continue
		}
		identity, err := d.output(ctx, nil, 128, "image", "inspect", "--format", "{{.Os}}/{{.Architecture}}", image)
		if err != nil || stringTrim(identity) != e.Platform {
			return nil, errors.New("pinned native image unavailable")
		}
	}

	checkName := "rcp-" + js.snapshot().Owner + "-check"
	if err = js.beginResource(checkName); err != nil {
		return nil, err
	}
	checkID, createErr := d.createCheck(ctx, checkName, js.snapshot().Owner, source, e)
	if err = js.completeCreate(checkName, checkID, createErr); err != nil {
		return nil, err
	}
	if createErr != nil {
		return nil, fmt.Errorf("create compiler: %w", createErr)
	}
	if err = d.startOwned(ctx, checkID, js.snapshot().Owner); err != nil {
		return nil, err
	}
	if err = d.verifyLimits(ctx, checkID, e); err != nil {
		return nil, fmt.Errorf("compiler isolation: %w", err)
	}
	if err = d.execCheck(ctx, checkID, e.CheckArgv, e.LogBytes); err != nil {
		return nil, errCheckFailed
	}
	if e.Kind == "source_check" {
		return nil, nil
	}
	if err = d.execCheck(ctx, checkID, e.BuildArgv, e.LogBytes); err != nil {
		return nil, fmt.Errorf("compile output: %w", err)
	}
	file := "/output/" + e.OutputFile
	kind, err := d.output(ctx, nil, 128, "exec", checkID, "stat", "-c", "%F", file)
	if err != nil || stringTrim(kind) != "regular file" {
		return nil, errors.New("output must be a regular file")
	}
	binary, err := d.output(ctx, nil, e.OutputBytes, "exec", checkID, "cat", file)
	if err != nil || len(binary) == 0 {
		return nil, errors.New("compiled output unavailable or exceeds bound")
	}
	var runtime []ociverify.RuntimeEntry
	if e.RuntimeRootFS != nil {
		kind, statErr := d.output(ctx, nil, 128, "exec", checkID, "stat", "-c", "%F", "/opt", "/opt/rcp", ociverify.RuntimeBundlePath)
		if statErr != nil || stringTrim(kind) != "directory\ndirectory\nregular file" {
			return nil, errors.New("runtime bundle must be immutable regular image content")
		}
		bundle, readErr := d.output(ctx, nil, e.RuntimeRootFS.ArchiveBytes, "exec", checkID, "cat", ociverify.RuntimeBundlePath)
		if readErr != nil {
			return nil, errors.New("pinned runtime bundle unavailable")
		}
		runtime, err = ociverify.RuntimeBundle(bundle, e.RuntimeRootFS)
		if err != nil {
			return nil, err
		}
	}
	contextTar, err := buildContextRuntime(e, binary, runtime)
	if err != nil {
		return nil, err
	}
	buildName := "rcp-" + js.snapshot().Owner + "-buildkit"
	if err = js.beginResource(buildName); err != nil {
		return nil, err
	}
	buildID, createErr := d.createBuildKit(ctx, buildName, js.snapshot().Owner, e)
	if err = js.completeCreate(buildName, buildID, createErr); err != nil {
		return nil, err
	}
	if createErr != nil {
		return nil, fmt.Errorf("create BuildKit: %w", createErr)
	}
	if err = d.startOwned(ctx, buildID, js.snapshot().Owner); err != nil {
		return nil, err
	}
	if err = d.waitBuildKit(ctx, buildID); err != nil {
		return nil, err
	}
	if err = d.verifyLimits(ctx, buildID, e); err != nil {
		return nil, fmt.Errorf("BuildKit isolation: %w", err)
	}
	if _, err = d.output(ctx, bytes.NewReader(contextTar), 1024, "exec", "-i", buildID, "tar", "-xf", "-", "-C", "/context"); err != nil {
		return nil, fmt.Errorf("BuildKit context: %w", err)
	}
	if _, err = d.output(ctx, nil, e.LogBytes, "exec", buildID, "buildctl", "--addr", "unix:///run/user/1000/buildkit/buildkitd.sock", "build", "--frontend", "dockerfile.v0", "--local", "context=/context", "--local", "dockerfile=/context", "--opt", "platform="+e.Platform, "--opt", "build-arg:SOURCE_DATE_EPOCH=0", "--output", "type=oci,dest=/output/image.tar,rewrite-timestamp=true"); err != nil {
		return nil, fmt.Errorf("BuildKit export: %w", err)
	}
	archive, err := d.output(ctx, nil, e.OutputBytes, "exec", buildID, "cat", "/output/image.tar")
	if err != nil {
		return nil, err
	}
	archivePath := filepath.Join(root, "image.oci.tar")
	if err = os.WriteFile(archivePath, archive, 0600); err != nil {
		return nil, err
	}
	result, err := ociverify.VerifyWithRuntime(archivePath, e.OutputBytes, e.Platform, e, binary, runtime)
	return &result, err
}
func jsSource(j *journalState) string { return j.snapshot().SourceSHA }
