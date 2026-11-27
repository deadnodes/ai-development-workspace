package application

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"slices"
	"strings"
	"time"

	"releasecontrol/internal/auth"
	"releasecontrol/internal/domain"
	"releasecontrol/internal/workspace"
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
	if err := authorize(ctx, "admin", ""); err != nil {
		return Backup{}, err
	}
	actor = attributedActor(ctx, actor)
	if strings.TrimSpace(actor) == "" {
		return Backup{}, invalid("actor required")
	}
	// Raw persisted state, not derived views. Read uses a consistent DB transaction.
	st, err := s.store.Read(ctx)
	if err != nil {
		return Backup{}, err
	}
	for _, product := range st.Products {
		if !productVisibleTo(ctx, &product) {
			return Backup{}, auth.ErrForbidden
		}
	}
	if _, restricted := auth.PrivateProductViewer(ctx); restricted {
		principal, _ := auth.FromContext(ctx)
		st, err = filterScopedStateForContext(ctx, st, principal)
		if err != nil {
			return Backup{}, err
		}
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
	if err := authorize(ctx, "admin", ""); err != nil {
		return nil, err
	}
	actor = attributedActor(ctx, actor)
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
	reconciliationRequired := []string{}
	quarantinedBuilds := []string{}
	// PostgreSQL write callbacks receive projected events; test the full journal
	// under the same writer lock before importing. Full-state stores use the guard below.
	update := s.store.Update
	if scoped, ok := s.store.(interface {
		UpdateScoped(context.Context, domain.StateMutationScope, func(*domain.State) error) error
	}); ok {
		update = func(ctx context.Context, fn func(*domain.State) error) error {
			return scoped.UpdateScoped(ctx, domain.StateMutationScope{AllCollections: true, RequireEmptyJournal: true}, fn)
		}
	}
	err = update(ctx, func(current *domain.State) error {
		b, _ := json.Marshal(current)
		var groups map[string][]json.RawMessage
		_ = json.Unmarshal(b, &groups)
		for _, rows := range groups {
			if len(rows) > 0 {
				return invalid("restore requires an empty instance; existing state is never replaced")
			}
		}
		restored := envelope.State
		for _, p := range restored.Products {
			if !nativeQuarantined(&restored, p.ID) {
				restored.NativeQuarantines = append(restored.NativeQuarantines, domain.NativeQuarantine{Meta: domain.Meta{ID: id(), ProductID: p.ID, Actor: actor, CreatedAt: now, UpdatedAt: now}, RestoreID: restoreID})
			}
		}
		evidenceCounts := map[string]int{}
		evidence := func(collection string, meta domain.Meta, record any) error {
			if meta.ProductID == "" {
				return invalid("restore evidence for %s requires a product scope", collection)
			}
			evidenceCounts[collection]++
			restored.Events = append(restored.Events, domain.Event{ID: id(), Action: "restore_backup_evidence", Actor: actor, At: now, EntityID: meta.ID, ProductID: meta.ProductID, FeatureID: meta.FeatureID, Data: domain.Command{Action: "restore_backup_evidence", Actor: actor, ProductID: meta.ProductID, FeatureID: meta.FeatureID, Data: map[string]any{"restore_event_id": restoreID, "collection": collection, "record": record}}})
			return nil
		}
		restored.Operations = slices.Clone(restored.Operations)
		originalOperations := make(map[string]int, len(restored.Operations))
		for index, operation := range envelope.State.Operations {
			originalOperations[operation.ID] = index
		}
		recordedOperations := map[string]bool{}
		operationEvidence := func(operation domain.ExternalOperation) error {
			if recordedOperations[operation.ID] {
				return nil
			}
			recordedOperations[operation.ID] = true
			return evidence("interrupted_operations", operation.Meta, envelope.State.Operations[originalOperations[operation.ID]])
		}
		for i := range restored.Operations {
			if domain.IsRestorationOperation(restored.Operations[i]) || restored.Operations[i].DevActivationGroupID != "" || restored.Operations[i].DevActivationBatch != nil || restored.Operations[i].DevActivationIntentHash != "" || restored.Operations[i].DevActivationPRHash != "" || restored.Operations[i].DevActivationResultHash != "" || restored.Operations[i].DevActivation != nil || restored.Operations[i].DevRuntime != nil || restored.Operations[i].DevBaseline != nil {
				restored.Operations[i].ReservationQuarantined = true
			}
			if active := activeDevReservation(&restored, restored.Operations[i].EnvironmentID); restored.Operations[i].Kind == "DEV_RESERVATION" && active != nil && active.ID == restored.Operations[i].ID {
				restored.Operations[i].ReservationQuarantined = true
			}
		}
		// Quarantine malformed orphans and every referenced child of imported recovery.
		recoveryIDs := map[string]bool{}
		for _, o := range restored.Operations {
			if domain.IsRestorationOperation(o) {
				recoveryIDs[o.ID] = true
				for _, id := range o.ChildIDs {
					recoveryIDs[id] = true
				}
				if o.DevRestoration != nil {
					recoveryIDs[o.DevRestoration.ReservationOperationID] = true
					recoveryIDs[o.DevRestoration.BaselineCaptureID] = true
				}
				if o.DevRestorationConfirmation != nil {
					recoveryIDs[o.DevRestorationConfirmation.Request.RestorationOperationID] = true
				}
				if o.DevRestorationVerification != nil {
					recoveryIDs[o.DevRestorationVerification.Request.RestorationOperationID] = true
					recoveryIDs[o.DevRestorationVerification.ConfirmationID] = true
				}
				if o.DevRestorationRelease != nil {
					recoveryIDs[o.DevRestorationRelease.ReservationOperationID] = true
					recoveryIDs[o.DevRestorationRelease.Request.RestorationOperationID] = true
					recoveryIDs[o.DevRestorationRelease.Request.RestorationVerificationOperationID] = true
					recoveryIDs[o.DevRestorationRelease.ConfirmationID] = true
				}
			}
		}
		for i := range restored.Operations {
			if recoveryIDs[restored.Operations[i].ID] || recoveryIDs[restored.Operations[i].ParentID] {
				restored.Operations[i].ReservationQuarantined = true
			}
		}
		for i := range restored.Operations {
			op := &restored.Operations[i]
			if op.Kind == domain.WorkerOperationKind || op.Kind == domain.RegistryObserveOperationKind {
				continue
			}
			if op.Status == "PENDING" || op.Status == "RUNNING" {
				if err := operationEvidence(*op); err != nil {
					return err
				}
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
		// Restored external work must never compete with the source installation.
		for i := range restored.WorkerJobs {
			j := &restored.WorkerJobs[i]
			if !workerTerminal(j.Status) {
				if op := operationByID(&restored, j.OperationID); op != nil {
					if err := operationEvidence(*op); err != nil {
						return err
					}
					reconciliationRequired = append(reconciliationRequired, op.ID)
				}
				j.Status = "RECONCILIATION_REQUIRED"
				j.CancellationRequested = true
				if a := currentWorkerAttempt(&restored, j); a != nil {
					a.Status = "RECONCILIATION_REQUIRED"
					a.LeaseUntil = now
					a.UpdatedAt = now
				}
				workerAudit(&restored, j, actor, "worker_restore_requires_reconciliation", now)
				if op := operationByID(&restored, j.OperationID); op != nil {
					restored.OperationSteps = append(restored.OperationSteps, domain.OperationStep{Meta: domain.Meta{ID: id(), ProductID: op.ProductID, FeatureID: op.FeatureID, Actor: actor, CreatedAt: now, UpdatedAt: now}, OperationID: op.ID, Phase: "RESTORE_RECONCILIATION_REQUIRED", Status: op.Status, Detail: op.Detail})
				}
			}
		}
		for i := range restored.Deployments {
			d := &restored.Deployments[i]
			if !deploymentTerminal(*d) {
				if err := evidence("quarantined_deployments", d.Meta, *d); err != nil {
					return err
				}
				d.Status = "RECONCILIATION_REQUIRED"
				d.UncertainAction = "restored"
				d.FailureCode = "DEPLOYMENT_RESTORED"
				d.UpdatedAt = now
			}
		}
		for i := range restored.ArtifactSignatures {
			v := &restored.ArtifactSignatures[i]
			if v.Status == "PENDING" {
				if err := evidence("quarantined_signatures", v.Meta, *v); err != nil {
					return err
				}
				v.Status = "FAILED"
				v.UpdatedAt = now
			}
		}
		for i := range restored.Builds {
			b := &restored.Builds[i]
			quarantine := b.Status == "QUEUED" || buildActive(*b) || b.RestoreQuarantined
			if b.SigningStatus == "PENDING" || quarantine {
				if err := evidence("quarantined_builds", b.Meta, *b); err != nil {
					return err
				}
			}
			if b.SigningStatus == "PENDING" {
				b.SigningStatus = "FAILED"
				b.UpdatedAt = now
			}
			if quarantine {
				quarantinedBuilds = append(quarantinedBuilds, b.ID)
				b.RestoreQuarantined = true
				b.Status = "RECONCILIATION_REQUIRED"
				b.FailureCode = "BUILD_RESTORED"
				b.FailureMessage = "Quarantined during restore; inspect external systems and submit a new request_key"
				b.UpdatedAt = now
			}
		}
		// Preserve pre-restore operation state as audit evidence; destination workers
		// must not race the source instance or repeat CI/GitOps mutations.
		restored.Events = append(restored.Events, domain.Event{ID: restoreID, Action: "restore_backup", Actor: actor, At: now, EntityID: restoreID, Data: domain.Command{Action: "restore_backup", Actor: actor, Data: map[string]any{"sha256": expected, "source_created_at": envelope.CreatedAt, "evidence_counts": evidenceCounts}}})
		*current = restored
		return nil
	})
	if errors.Is(err, domain.ErrJournalNotEmpty) {
		return nil, invalid("restore requires an empty instance; existing state is never replaced")
	}
	if err != nil {
		return nil, err
	}
	return map[string]any{"restored": true, "sha256": expected, "restore_event_id": restoreID, "quarantined_build_ids": quarantinedBuilds, "cancelled_operation_ids": cancelled, "reconciliation_required_operation_ids": reconciliationRequired}, nil
}
func validateBackupState(st domain.State) error {
	for _, repo := range st.Repositories {
		if workspace.CloneURLHasCredentials(repo.URL) {
			return invalid("backup contains an unsafe repository clone URL")
		}
	}
	for _, repo := range st.RegisteredRepositories {
		if workspace.CloneURLHasCredentials(repo.URL) {
			return invalid("backup contains an unsafe registered repository clone URL")
		}
	}
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
