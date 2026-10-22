package workerrunner

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"releasecontrol/internal/artifactspool"
	"releasecontrol/internal/domain"
	"testing"
	"time"
)

func TestCancelledExecutorSuccessCannotPersistPassedReport(t *testing.T) {
	for _, spooled := range []bool{false, true} {
		t.Run(map[bool]string{false: "local", true: "spooled"}[spooled], func(t *testing.T) {
			root := t.TempDir()
			root, _ = filepath.EvalSymlinks(root)
			c := Config{}
			if spooled {
				spool := filepath.Join(root, "spool")
				if e := os.Mkdir(spool, 0700); e != nil {
					t.Fatal(e)
				}
				c.ArtifactSpool = &artifactspool.Config{Root: spool, MaxArchiveBytes: 32, MaxTotalBytes: 64, MaxEntries: 2}
			}
			ctx, stop := context.WithCancel(context.Background())
			stop()
			report, e := prepareReport(ctx, c, root, domain.WorkerAttemptRequest{JobID: "job", AttemptID: "attempt"}, &domain.WorkerLocalOCI{}, nil)
			if !errors.Is(e, context.Canceled) || report.Result != "EXECUTOR_FAILED" || report.LocalOCI != nil {
				t.Fatal("lease stop fell through to executor PASSED", report, e)
			}
			j := journal{Owner: "0123456789abcdef0123456789abcdef", ParentPID: os.Getpid(), Deadline: time.Now().Add(time.Minute), LeaseUntil: time.Now().Add(time.Minute), Report: report}
			file := filepath.Join(root, "journal.json")
			if e = saveJournal(file, j); e != nil {
				t.Fatal(e)
			}
			stored, e := readJournal(file)
			if e != nil || stored.Report.Result == "PASSED" || stored.Report.LocalOCI != nil {
				t.Fatal("unsafe report persisted", stored, e)
			}
		})
	}
}
func TestSpoolFailureCannotProducePassedReport(t *testing.T) {
	root := t.TempDir()
	root, _ = filepath.EvalSymlinks(root)
	c := Config{ArtifactSpool: &artifactspool.Config{Root: filepath.Join(root, "unavailable"), MaxArchiveBytes: 32, MaxTotalBytes: 64, MaxEntries: 2}}
	receipt := &domain.WorkerLocalOCI{ArchiveHash: "sha256:" + "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", ArchiveBytes: 16}
	report, e := prepareReport(context.Background(), c, root, domain.WorkerAttemptRequest{}, receipt, nil)
	if e == nil || report.Result != "EXECUTOR_FAILED" || report.LocalOCI != nil {
		t.Fatal("unacknowledged spool yielded PASSED", report, e)
	}
}
