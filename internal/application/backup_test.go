package application

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"reflect"
	"testing"

	"releasecontrol/internal/domain"
)

func encodeTestBackup(t *testing.T, env backupEnvelope) (string, string) {
	t.Helper()
	b, err := json.Marshal(env)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	z := gzip.NewWriter(&out)
	_, _ = z.Write(b)
	_ = z.Close()
	return base64.StdEncoding.EncodeToString(out.Bytes()), backupHash(out.Bytes())
}
func TestBackupRoundTripHistoryAndActiveOperationSafety(t *testing.T) {
	ctx := context.Background()
	source, src := fixture(t)
	exec(t, source, domain.Command{Action: "record_decision", FeatureID: "f", Data: map[string]any{"title": "Preserve history", "reason": "Migration", "body": "Full context"}})
	src.state.Operations = append(src.state.Operations, domain.ExternalOperation{Meta: domain.Meta{ID: "op", ProductID: "p", FeatureID: "f"}, IntegrationID: "i", Status: "RUNNING", Phase: "APPLY", LeaseOwner: "source-worker"})
	archive, err := source.CreateBackup(ctx, "agent")
	if err != nil {
		t.Fatal(err)
	}
	destStore := &memoryStore{domain.EmptyState()}
	dest := New(destStore)
	_, err = dest.RestoreBackup(ctx, "agent", archive.ArchiveBase64, archive.SHA256)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(src.state.Memories, destStore.state.Memories) {
		t.Fatal("memory changed")
	}
	if !reflect.DeepEqual(src.state.Events, destStore.state.Events[:len(src.state.Events)]) {
		t.Fatal("source audit changed")
	}
	if destStore.state.Operations[0].Status != "CANCELLED" || destStore.state.Operations[0].LeaseOwner != "" {
		t.Fatal("restored operation can execute")
	}
	if src.state.Operations[0].Status != "RUNNING" {
		t.Fatal("source was modified")
	}
	if len(destStore.state.OperationSteps) != 1 {
		t.Fatal("cancellation evidence missing")
	}
	before, _ := json.Marshal(destStore.state)
	if _, err = dest.RestoreBackup(ctx, "agent", archive.ArchiveBase64, archive.SHA256); err == nil {
		t.Fatal("nonempty restore allowed")
	}
	after, _ := json.Marshal(destStore.state)
	if !bytes.Equal(before, after) {
		t.Fatal("rejected restore changed destination")
	}
}
func TestBackupRejectsCorruptionVersionAndDuplicateIDs(t *testing.T) {
	ctx := context.Background()
	source, store := fixture(t)
	good, err := source.CreateBackup(ctx, "agent")
	if err != nil {
		t.Fatal(err)
	}
	dup := store.state
	dup.Products = append(dup.Products, dup.Products[0])
	invalidVersion, versionHash := encodeTestBackup(t, backupEnvelope{Format: "release-control-backup", Version: 2, State: domain.EmptyState()})
	duplicate, dupHash := encodeTestBackup(t, backupEnvelope{Format: "release-control-backup", Version: 1, State: dup})
	for _, tc := range []struct{ name, archive, hash string }{
		{"checksum", good.ArchiveBase64, "bad"}, {"base64", "!!!", good.SHA256}, {"gzip", base64.StdEncoding.EncodeToString([]byte("bad")), backupHash([]byte("bad"))}, {"version", invalidVersion, versionHash}, {"duplicate", duplicate, dupHash},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := &memoryStore{domain.EmptyState()}
			if _, err := New(m).RestoreBackup(ctx, "agent", tc.archive, tc.hash); err == nil {
				t.Fatal("invalid archive accepted")
			}
			if len(m.state.Events) != 0 {
				t.Fatal("failed restore wrote history")
			}
		})
	}
}
func TestBackupRejectsExpandedBomb(t *testing.T) {
	var b bytes.Buffer
	z := gzip.NewWriter(&b)
	block := make([]byte, 1<<20)
	for i := 0; i < 65; i++ {
		_, _ = z.Write(block)
	}
	_ = z.Close()
	m := &memoryStore{domain.EmptyState()}
	if _, err := New(m).RestoreBackup(context.Background(), "agent", base64.StdEncoding.EncodeToString(b.Bytes()), backupHash(b.Bytes())); err == nil {
		t.Fatal("expanded limit ignored")
	}
}
