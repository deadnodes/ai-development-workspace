package application

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"strings"
	"time"

	"releasecontrol/internal/domain"
)

const MaxBackupCompressed = 8 << 20
const MaxBackupExpanded = 64 << 20

type Backup struct {
	Format        string `json:"format"`
	SHA256        string `json:"sha256"`
	Bytes         int    `json:"bytes"`
	ArchiveBase64 string `json:"archive_base64"`
}
type backupEnvelope struct {
	Format      string       `json:"format"`
	Version     int          `json:"version"`
	CreatedAt   time.Time    `json:"created_at"`
	RequestedBy string       `json:"requested_by"`
	State       domain.State `json:"state"`
}

func backupHash(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }

func (s *Service) CreateBackup(ctx context.Context, actor string) (Backup, error) {
	if strings.TrimSpace(actor) == "" {
		return Backup{}, invalid("actor required")
	}
	// Raw persisted state, not derived views. Read uses a consistent DB transaction.
	st, err := s.store.Read(ctx)
	if err != nil {
		return Backup{}, err
	}
	data, err := json.Marshal(backupEnvelope{Format: "release-control-backup", Version: 1, CreatedAt: time.Now().UTC(), RequestedBy: actor, State: st})
	if err != nil {
		return Backup{}, err
	}
	if len(data) > MaxBackupExpanded {
		return Backup{}, invalid("backup exceeds 64 MiB expanded limit; use PostgreSQL backup")
	}
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err = zw.Write(data); err != nil {
		return Backup{}, err
	}
	if err = zw.Close(); err != nil {
		return Backup{}, err
	}
	if buf.Len() > MaxBackupCompressed {
		return Backup{}, invalid("backup exceeds 8 MiB compressed limit; use PostgreSQL backup")
	}
	return Backup{Format: "rcp.json.gz", SHA256: backupHash(buf.Bytes()), Bytes: buf.Len(), ArchiveBase64: base64.StdEncoding.EncodeToString(buf.Bytes())}, nil
}

// RestoreBackup never merges databases or replays external side effects.
func (s *Service) RestoreBackup(ctx context.Context, actor, archive, expected string) (any, error) {
	if strings.TrimSpace(actor) == "" || expected == "" {
		return nil, invalid("actor and sha256 required")
	}
	if len(archive) > base64.StdEncoding.EncodedLen(MaxBackupCompressed) {
		return nil, invalid("compressed backup too large")
	}
	compressed, err := base64.StdEncoding.DecodeString(archive)
	if err != nil || len(compressed) > MaxBackupCompressed {
		return nil, invalid("invalid base64 backup")
	}
	if backupHash(compressed) != expected {
		return nil, invalid("backup checksum mismatch")
	}
	zr, err := gzip.NewReader(bytes.NewReader(compressed))
	if err != nil {
		return nil, invalid("invalid gzip backup")
	}
	data, err := io.ReadAll(io.LimitReader(zr, MaxBackupExpanded+1))
	_ = zr.Close()
	if err != nil || len(data) > MaxBackupExpanded {
		return nil, invalid("corrupt backup or expanded size limit exceeded")
	}
	var envelope backupEnvelope
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err = dec.Decode(&envelope); err != nil {
		return nil, invalid("unsupported or invalid backup JSON")
	}
	if dec.Decode(new(any)) != io.EOF {
		return nil, invalid("trailing backup data")
	}
	if envelope.Format != "release-control-backup" || envelope.Version != 1 {
		return nil, invalid("unsupported backup version")
	}
	if err = validateBackupState(envelope.State); err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	restoreID := id()
	cancelled := []string{}
	err = s.store.Update(ctx, func(current *domain.State) error {
		b, _ := json.Marshal(current)
		var groups map[string][]json.RawMessage
		_ = json.Unmarshal(b, &groups)
		for _, rows := range groups {
			if len(rows) > 0 {
				return invalid("restore requires an empty instance; existing state is never replaced")
			}
		}
		restored := envelope.State
		originalOperations := []domain.ExternalOperation{}
		for i := range restored.Operations {
			op := &restored.Operations[i]
			if op.Status == "PENDING" || op.Status == "RUNNING" {
				originalOperations = append(originalOperations, *op)
				cancelled = append(cancelled, op.ID)
				op.Status = "CANCELLED"
				op.FinishedAt = &now
				op.LeaseOwner = ""
				op.LeaseUntil = time.Time{}
				op.Error = "Cancelled during restore; inspect external systems before requesting new work"
				op.Detail = op.Error
				op.UpdatedAt = now
				restored.OperationSteps = append(restored.OperationSteps, domain.OperationStep{Meta: domain.Meta{ID: id(), ProductID: op.ProductID, FeatureID: op.FeatureID, Actor: actor, CreatedAt: now, UpdatedAt: now}, OperationID: op.ID, Phase: "RESTORE", Status: "CANCELLED", Detail: op.Error})
			}
		}
		// Preserve pre-restore operation state as audit evidence; destination workers
		// must not race the source instance or repeat CI/GitOps mutations.
		restored.Events = append(restored.Events, domain.Event{ID: restoreID, Action: "restore_backup", Actor: actor, At: now, EntityID: restoreID, Data: domain.Command{Action: "restore_backup", Actor: actor, Data: map[string]any{"sha256": expected, "source_created_at": envelope.CreatedAt, "interrupted_operations": originalOperations}}})
		*current = restored
		return nil
	})
	if err != nil {
		return nil, err
	}
	return map[string]any{"restored": true, "sha256": expected, "restore_event_id": restoreID, "cancelled_operation_ids": cancelled}, nil
}
func validateBackupState(st domain.State) error {
	if err := domain.ValidateStateStatuses(st); err != nil {
		return invalid("backup status validation: %s", err)
	}
	b, _ := json.Marshal(st)
	var groups map[string][]json.RawMessage
	if err := json.Unmarshal(b, &groups); err != nil {
		return invalid("invalid state")
	}
	ids := map[string]bool{}
	products := map[string]bool{}
	features := map[string]bool{}
	for _, v := range st.Products {
		products[v.ID] = true
	}
	for _, v := range st.Features {
		features[v.ID] = true
	}
	for kind, rows := range groups {
		for _, row := range rows {
			var ref struct {
				ID        string `json:"id"`
				ProductID string `json:"product_id"`
				FeatureID string `json:"feature_id"`
			}
			if json.Unmarshal(row, &ref) != nil || ref.ID == "" || ids[ref.ID] {
				return invalid("missing or duplicate ID in backup")
			}
			ids[ref.ID] = true
			if kind != "events" && ((ref.ProductID != "" && !products[ref.ProductID]) || (ref.FeatureID != "" && !features[ref.FeatureID])) {
				return invalid("dangling product/feature reference")
			}
		}
	}
	return nil
}
