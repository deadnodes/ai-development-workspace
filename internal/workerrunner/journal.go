package workerrunner

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"releasecontrol/internal/domain"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type ownedResource struct {
	Name        string `json:"name"`
	ID          string `json:"id,omitempty"`
	CreateState string `json:"create_state"`
}
type journal struct {
	Owner         string               `json:"owner"`
	ParentPID     int                  `json:"parent_pid"`
	DockerContext string               `json:"docker_context"`
	DockerConfig  string               `json:"docker_config"`
	Deadline      time.Time            `json:"deadline"`
	LeaseUntil    time.Time            `json:"lease_until"`
	Resources     []ownedResource      `json:"resources"`
	Done          bool                 `json:"done"`
	SourceSHA     string               `json:"source_sha"`
	Report        *domain.WorkerReport `json:"report,omitempty"`
}

var ownerPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)
var containerIDPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)
var errUncertainCreate = errors.New("container create outcome uncertain; explicit reconciliation required")

func nonce() string { var b [16]byte; _, _ = rand.Read(b[:]); return hex.EncodeToString(b[:]) }
func saveJournal(file string, j journal) error {
	b, e := json.Marshal(j)
	if e != nil {
		return e
	}
	temp := file + ".tmp"
	f, e := os.OpenFile(temp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
	if e != nil {
		return e
	}
	_, e = f.Write(b)
	if e == nil {
		e = f.Sync()
	}
	ce := f.Close()
	if e != nil {
		return e
	}
	if ce != nil {
		return ce
	}
	if e = os.Rename(temp, file); e != nil {
		return e
	}
	d, e := os.Open(filepath.Dir(file))
	if e != nil {
		return e
	}
	defer d.Close()
	return d.Sync()
}
func readJournal(file string) (journal, error) {
	var j journal
	f, e := os.Open(file)
	if e != nil {
		return j, e
	}
	defer f.Close()
	stat, e := f.Stat()
	if e != nil || !stat.Mode().IsRegular() || stat.Size() > 8192 {
		return j, errors.New("invalid journal")
	}
	dec := json.NewDecoder(f)
	dec.DisallowUnknownFields()
	e = dec.Decode(&j)
	if e != nil || !ownerPattern.MatchString(j.Owner) || j.ParentPID < 1 || j.Deadline.IsZero() || j.LeaseUntil.IsZero() {
		return j, errors.New("invalid journal")
	}
	if len(j.Resources) > 2 {
		return j, errors.New("too many owned resources")
	}
	seen := map[string]bool{}
	for _, resource := range j.Resources {
		if resource.Name != "rcp-"+j.Owner+"-check" && resource.Name != "rcp-"+j.Owner+"-buildkit" || resource.ID != "" && !containerIDPattern.MatchString(resource.ID) || resource.CreateState != "CREATING" && resource.CreateState != "CREATED" && resource.CreateState != "CREATE_UNCERTAIN" && resource.CreateState != "REMOVED" || (resource.CreateState == "CREATED" || resource.CreateState == "REMOVED") && resource.ID == "" {
			return j, errors.New("invalid owned resource")
		}
		if seen[resource.Name] || j.Done && resource.CreateState != "REMOVED" {
			return j, errors.New("invalid resource journal completion")
		}
		seen[resource.Name] = true
	}
	return j, nil
}
func updateJournal(file string, fn func(*journal) error) (journal, error) {
	var j journal
	lock, e := os.OpenFile(file+".lock", os.O_CREATE|os.O_RDWR, 0600)
	if e != nil {
		return j, e
	}
	defer lock.Close()
	if e = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); e != nil {
		return j, e
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	j, e = readJournal(file)
	if e != nil {
		return j, e
	}
	if e = fn(&j); e != nil {
		return j, e
	}
	return j, saveJournal(file, j)
}

type dockerControl interface {
	output(context.Context, io.Reader, int64, ...string) ([]byte, error)
}

func cleanup(file string) error {
	j, e := readJournal(file)
	if e != nil {
		return e
	}
	return cleanupWith(file, docker{context: j.DockerContext, config: j.DockerConfig})
}
func cleanupWith(file string, d dockerControl) error {
	j, e := readJournal(file)
	if e != nil {
		return e
	}
	change := func(name string, fn func(*ownedResource)) error {
		_, err := updateJournal(file, func(latest *journal) error {
			for i := range latest.Resources {
				if latest.Resources[i].Name == name {
					fn(&latest.Resources[i])
					return nil
				}
			}
			return errors.New("owned resource vanished from journal")
		})
		return err
	}
	for _, resource := range j.Resources {
		if resource.CreateState == "REMOVED" {
			continue
		}
		if resource.CreateState == "CREATING" {
			if e = change(resource.Name, func(r *ownedResource) {
				if r.CreateState == "CREATING" {
					r.CreateState = "CREATE_UNCERTAIN"
				}
			}); e != nil {
				return e
			}
			resource.CreateState = "CREATE_UNCERTAIN"
		}
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		if resource.ID == "" {
			found, err := d.output(ctx, nil, 256, "container", "ls", "--all", "--no-trunc", "--filter", "name=^/"+resource.Name+"$", "--format", `{{.ID}} {{.Label "rcp.worker.owner"}}`)
			if err != nil {
				cancel()
				return err
			}
			fields := strings.Fields(string(found))
			if len(fields) == 0 {
				cancel()
				continue
			}
			if len(fields) != 2 || !containerIDPattern.MatchString(fields[0]) || fields[1] != j.Owner {
				cancel()
				return errors.New("pending resource ownership not proven")
			}
			resource.ID = fields[0]
			if e = change(resource.Name, func(r *ownedResource) {
				if r.ID == "" {
					r.ID = resource.ID
				}
			}); e != nil {
				cancel()
				return e
			}
		}
		present, e := d.output(ctx, nil, 128, "container", "ls", "--all", "--no-trunc", "--filter", "id="+resource.ID, "--format", "{{.ID}}")
		if e != nil {
			cancel()
			return e
		}
		if stringTrim(present) == "" {
			cancel()
			if e = change(resource.Name, func(r *ownedResource) {
				if r.CreateState == "CREATED" {
					r.CreateState = "REMOVED"
				}
			}); e != nil {
				return e
			}
			continue
		}
		label, e := d.output(ctx, nil, 256, "inspect", "--format", `{{.Id}} {{ index .Config.Labels "rcp.worker.owner" }}`, resource.ID)
		if e != nil || stringTrim(present) != resource.ID || stringTrim(label) != resource.ID+" "+j.Owner {
			cancel()
			return errors.New("resource ownership not proven")
		}
		_, e = d.output(ctx, nil, 1024, "rm", "--force", resource.ID)
		cancel()
		if e != nil {
			return e
		}
		if e = change(resource.Name, func(r *ownedResource) {
			if r.CreateState == "CREATED" {
				r.CreateState = "REMOVED"
			}
		}); e != nil {
			return e
		}
	}
	_, e = updateJournal(file, func(latest *journal) error {
		for _, r := range latest.Resources {
			if r.CreateState != "REMOVED" {
				return errUncertainCreate
			}
		}
		latest.Done = true
		return nil
	})
	return e
}
func startWatchdog(file string, j journal) error {
	exe, e := os.Executable()
	if e != nil {
		return e
	}
	cmd := exec.Command(exe, "watchdog", file, j.Owner)
	cmd.Env = cleanEnv()
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if e = cmd.Start(); e != nil {
		return e
	}
	return cmd.Process.Release()
}
func parentDead(pid int) bool {
	if syscall.Kill(pid, 0) != nil {
		return true
	}
	// A killed supervisor may remain a zombie until its owner reaps it. Signal
	// zero alone treats that as alive, so inspect only its bounded process state.
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/bin/ps", "-p", strconv.Itoa(pid), "-o", "stat=")
	cmd.Env = cleanEnv()
	out := &boundedBuffer{remaining: 128}
	cmd.Stdout = out
	if cmd.Run() != nil {
		return false
	}
	return strings.HasPrefix(strings.TrimSpace(out.String()), "Z")
}

// Watchdog is an independent process, deliberately without worker credentials.
// It terminates exactly labelled resources after lease/deadline or parent death.
func Watchdog(file, owner string) error {
	if !ownerPattern.MatchString(owner) {
		return errors.New("invalid watchdog owner")
	}
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for range ticker.C {
		j, e := readJournal(file)
		if e != nil {
			return e
		}
		if j.Owner != owner {
			return errors.New("journal owner changed")
		}
		if j.Done {
			return nil
		}
		dead := parentDead(j.ParentPID)
		now := time.Now()
		if dead || !now.Before(j.LeaseUntil) || !now.Before(j.Deadline) {
			if cleanup(file) == nil {
				return nil
			}
		}
	}
	return nil
}

// Recover stops orphaned resources, never resumes uncertain execution.
func Recover(stateDir string) error {
	entries, e := os.ReadDir(stateDir)
	if os.IsNotExist(e) {
		return nil
	}
	if e != nil {
		return e
	}
	for _, entry := range entries {
		if !entry.IsDir() || !ownerPattern.MatchString(entry.Name()) {
			continue
		}
		file := filepath.Join(stateDir, entry.Name(), "journal.json")
		j, e := readJournal(file)
		if e != nil {
			return e
		}
		if j.Owner != entry.Name() {
			return errors.New("journal path owner mismatch")
		}
		if !j.Done {
			if e = cleanup(file); e != nil {
				return e
			}
		}
	}
	return nil
}
