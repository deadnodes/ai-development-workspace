package workerrunner

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"releasecontrol/internal/testdocker"
	"strings"
	"syscall"
	"testing"
	"time"

	"releasecontrol/internal/domain"
)

type raceDocker struct {
	owner, id, replacement                                 string
	present, replaced, deletedOriginal, deletedReplacement bool
}

func (f *raceDocker) output(_ context.Context, _ io.Reader, _ int64, args ...string) ([]byte, error) {
	if args[0] == "container" {
		if !f.present {
			return nil, nil
		}
		for _, arg := range args {
			if arg == "id="+f.id {
				return []byte(f.id + "\n"), nil
			}
		}
		return []byte(f.id + " " + f.owner + "\n"), nil
	}
	if args[0] == "inspect" {
		f.replaced = true
		return []byte(f.id + " " + f.owner), nil
	}
	if args[0] == "rm" {
		target := args[len(args)-1]
		if target == f.id {
			f.deletedOriginal = true
			f.present = false
			return []byte(f.id), nil
		}
		f.deletedReplacement = true
		return nil, errors.New("unsafe name deletion")
	}
	return nil, errors.New("unexpected fake Docker command")
}
func identityJournal(t *testing.T, state string, id string) (string, journal) {
	t.Helper()
	owner := nonce()
	root := filepath.Join(t.TempDir(), owner)
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(root, "journal.json")
	j := journal{Owner: owner, ParentPID: os.Getpid(), Deadline: time.Now().Add(time.Minute), LeaseUntil: time.Now().Add(time.Second), Resources: []ownedResource{{Name: "rcp-" + owner + "-check", ID: id, CreateState: state}}}
	if err := saveJournal(file, j); err != nil {
		t.Fatal(err)
	}
	return file, j
}
func TestCleanupUsesImmutableIDAcrossNameReplacement(t *testing.T) {
	id := strings.Repeat("a", 64)
	file, j := identityJournal(t, "CREATED", id)
	fake := &raceDocker{owner: j.Owner, id: id, replacement: strings.Repeat("b", 64), present: true}
	if err := cleanupWith(file, fake); err != nil {
		t.Fatal(err)
	}
	if !fake.replaced || !fake.deletedOriginal || fake.deletedReplacement {
		t.Fatal("replacement container was mutated")
	}
	after, err := readJournal(file)
	if err != nil || !after.Done || after.Resources[0].ID != id || after.Resources[0].CreateState != "REMOVED" {
		t.Fatal("exact-ID cleanup not persisted", err)
	}
}
func TestUnknownCreateEmptyObservationIsNotClean(t *testing.T) {
	file, j := identityJournal(t, "CREATING", "")
	fake := &raceDocker{owner: j.Owner, id: strings.Repeat("a", 64)}
	if err := cleanupWith(file, fake); !errors.Is(err, errUncertainCreate) {
		t.Fatal("empty observation manufactured certainty", err)
	}
	after, _ := readJournal(file)
	if after.Done || after.Resources[0].CreateState != "CREATE_UNCERTAIN" {
		t.Fatal("uncertain create marked done")
	}
	fake.present = true
	if err := cleanupWith(file, fake); !errors.Is(err, errUncertainCreate) {
		t.Fatal("unknown late create must still require reconciliation", err)
	}
	after, _ = readJournal(file)
	if after.Done || after.Resources[0].ID != fake.id || !fake.deletedOriginal || fake.deletedReplacement {
		t.Fatal("late exact-ID discovery/deletion lost uncertainty")
	}
}

// Real daemon windows: the parent dies (or its lease expires) before CREATE
// returns. A late commit is discovered and removed by exact ID, while its
// unresolved intent remains durable. START uses only an already-persisted ID.
func TestRealCreateStartSupervisionWindows(t *testing.T) {
	if os.Getenv("RCP_TEST_DOCKER") != "1" {
		t.Skip("real boundary tests require explicit opt-in")
	}
	fixtureDocker := testdocker.Load(t)
	root := t.TempDir()
	binary := filepath.Join(root, "worker")
	build := exec.Command("go", "build", "-o", binary, "../../cmd/worker")
	if b, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build watchdog %v %s", err, b)
	}
	for _, scenario := range []string{"kill-during-create", "timeout-during-create", "kill-during-start"} {
		t.Run(scenario, func(t *testing.T) {
			dir := t.TempDir()
			owner := nonce()
			attempt := filepath.Join(dir, owner)
			if err := os.Mkdir(attempt, 0700); err != nil {
				t.Fatal(err)
			}
			source := filepath.Join(attempt, "source")
			if err := os.Mkdir(source, 0755); err != nil {
				t.Fatal(err)
			}
			parent := exec.Command("/bin/sleep", "60")
			parent.Env = cleanEnv()
			if err := parent.Start(); err != nil {
				t.Fatal(err)
			}
			defer func() { _ = parent.Process.Kill(); _ = parent.Wait() }()
			home, _ := os.UserHomeDir()
			d := docker{context: fixtureDocker.Context, config: filepath.Join(home, ".docker")}
			name := "rcp-" + owner + "-check"
			lease := time.Now().Add(30 * time.Second)
			if scenario == "timeout-during-create" {
				lease = time.Now().Add(300 * time.Millisecond)
			}
			file := filepath.Join(attempt, "journal.json")
			j := journal{Owner: owner, ParentPID: parent.Process.Pid, DockerContext: d.context, DockerConfig: d.config, Deadline: time.Now().Add(time.Minute), LeaseUntil: lease, Resources: []ownedResource{{Name: name, CreateState: "CREATING"}}}
			if err := saveJournal(file, j); err != nil {
				t.Fatal(err)
			}
			watchdog := exec.Command(binary, "watchdog", file, owner)
			watchdog.Env = cleanEnv()
			if err := watchdog.Start(); err != nil {
				t.Fatal(err)
			}
			defer func() { _ = watchdog.Process.Kill(); _ = watchdog.Wait() }()
			e := &domain.WorkerExecution{Platform: fixtureDocker.Platform, ToolchainImage: "golang@sha256:b305420a68d0f229d91eb3b3ed9e519fcf2cf5461da4bef997bf927e8c0bfd2b", MemoryMB: 128, StorageMB: 16, CPUs: 1, PIDs: 64}
			if scenario == "kill-during-create" {
				_ = parent.Process.Kill()
			}
			if scenario != "kill-during-start" {
				// Let the watchdog observe an empty provider state before the late create.
				until := time.Now().Add(5 * time.Second)
				for {
					observed, _ := readJournal(file)
					if observed.Resources[0].CreateState == "CREATE_UNCERTAIN" {
						if observed.Done {
							t.Fatal("empty CREATE window marked done")
						}
						break
					}
					if time.Now().After(until) {
						t.Fatal("watchdog failed to preserve CREATE uncertainty")
					}
					time.Sleep(50 * time.Millisecond)
				}
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			id, err := d.createCheck(ctx, name, owner, source, e)
			cancel()
			if err != nil {
				t.Fatal("late real create", err)
			}
			// A deterministic test-owned fallback prevents leaking resources on failure;
			// it checks the same exact ID/label and never touches unrelated containers.
			defer func() {
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				identity, _ := d.output(ctx, nil, 256, "inspect", "--format", `{{.Id}} {{ index .Config.Labels "rcp.worker.owner" }}`, id)
				if stringTrim(identity) == id+" "+owner {
					_, _ = d.output(ctx, nil, 1024, "rm", "--force", id)
				}
			}()
			if scenario == "kill-during-start" {
				if _, err = updateJournal(file, func(v *journal) error { v.Resources[0].ID = id; v.Resources[0].CreateState = "CREATED"; return nil }); err != nil {
					t.Fatal(err)
				}
				ctx, cancel = context.WithTimeout(context.Background(), 10*time.Second)
				err = d.startOwned(ctx, id, owner)
				cancel()
				if err != nil {
					t.Fatal(err)
				}
				_ = parent.Process.Kill()
			}
			until := time.Now().Add(8 * time.Second)
			for {
				ctx, cancel = context.WithTimeout(context.Background(), time.Second)
				b, err := d.output(ctx, nil, 128, "container", "ls", "--all", "--no-trunc", "--filter", "id="+id, "--format", "{{.ID}}")
				cancel()
				if err == nil && stringTrim(b) == "" {
					observed, _ := readJournal(file)
					if scenario != "kill-during-start" || observed.Done {
						break
					}
				}
				if time.Now().After(until) {
					t.Fatal("watchdog left a late-created/running exact container")
				}
				time.Sleep(50 * time.Millisecond)
			}
			after, err := readJournal(file)
			if err != nil {
				t.Fatal(err)
			}
			if after.Resources[0].ID != id {
				t.Fatal("recovered exact ID not persisted")
			}
			if scenario == "kill-during-start" {
				if !after.Done || after.Resources[0].CreateState != "REMOVED" {
					t.Fatal("known START result not reconciled")
				}
			} else {
				if after.Done || after.Resources[0].CreateState != "CREATE_UNCERTAIN" {
					t.Fatal("late CREATE silently resolved uncertainty")
				}
				// Container absence does not mean the still-running watchdog has
				// released journal.lock. Retry only that nonblocking lock conflict;
				// the eventual unknown-create proof remains mandatory below.
				until := time.Now().Add(5 * time.Second)
				for {
					err = Recover(dir)
					if !errors.Is(err, syscall.EAGAIN) && !errors.Is(err, syscall.EWOULDBLOCK) {
						break
					}
					if !time.Now().Before(until) {
						t.Fatal("watchdog journal contention did not clear", err)
					}
					time.Sleep(10 * time.Millisecond)
				}
				if !errors.Is(err, errUncertainCreate) {
					t.Fatal("restart did not stop at unknown create", err)
				}
			}
			ctx, cancel = context.WithTimeout(context.Background(), time.Second)
			err = d.startOwned(ctx, id, owner)
			cancel()
			if err == nil {
				t.Fatal("removed ID restarted after supervision")
			}
			t.Logf("actual %s: late container %s removed; journal done=%t", scenario, id, after.Done)
		})
	}
}
