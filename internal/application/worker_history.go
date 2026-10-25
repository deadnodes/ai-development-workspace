package application

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"releasecontrol/internal/domain"
)

type workerHistoryReader interface {
	ReadWorkerHistory(context.Context, domain.WorkerHistoryMetadataRequest) (domain.WorkerHistoryMetadata, error)
}

type workerHistoryCursor struct {
	Version    int    `json:"v"`
	Product    string `json:"product"`
	Feature    string `json:"feature"`
	Job        string `json:"job"`
	Collection string `json:"collection"`
	Last       string `json:"last"`
}

func historyCursor(q domain.WorkerHistoryMetadataRequest, last string) string {
	raw, _ := json.Marshal(workerHistoryCursor{1, encodeCursorID(q.ProductID), encodeCursorID(q.FeatureID), encodeCursorID(q.JobID), q.Collection, encodeCursorID(last)})
	return base64.RawURLEncoding.EncodeToString(raw)
}

func (s *Service) WorkerHistory(ctx context.Context, in domain.WorkerHistoryRequest) (domain.WorkerHistoryPage, error) {
	var out domain.WorkerHistoryPage
	// Permission and explicit Product scope are checked before any store access.
	if e := authorize(ctx, "read", in.ProductID); e != nil {
		return out, e
	}
	if !stateID(in.ProductID) || in.FeatureID != "" && !stateID(in.FeatureID) || in.JobID != "" && !stateID(in.JobID) {
		return out, invalid("worker history requires exact Product, Feature and job identities")
	}
	if in.Collection == "" {
		in.Collection = "jobs"
	}
	if in.Collection != "jobs" && in.Collection != "attempts" && in.Collection != "links" || in.Collection != "jobs" && in.JobID == "" {
		return out, invalid("worker history collection must be jobs, or attempts/links with job_id")
	}
	if in.Limit == 0 {
		in.Limit = 25
	}
	if in.Limit < 1 || in.Limit > 100 {
		return out, invalid("worker history limit must be 1..100")
	}
	q := domain.WorkerHistoryMetadataRequest{ProductID: in.ProductID, FeatureID: in.FeatureID, JobID: in.JobID, Collection: in.Collection, Limit: in.Limit + 1}
	if in.Cursor != "" {
		if len(in.Cursor) > 2048 {
			return out, invalid("invalid worker history cursor")
		}
		raw, e := base64.RawURLEncoding.Strict().DecodeString(in.Cursor)
		if e != nil {
			return out, invalid("invalid worker history cursor")
		}
		var c workerHistoryCursor
		if json.Unmarshal(raw, &c) != nil {
			return out, invalid("invalid worker history cursor")
		}
		canon, _ := json.Marshal(c)
		last, e := base64.RawURLEncoding.Strict().DecodeString(c.Last)
		if e != nil || !bytes.Equal(raw, canon) || c.Version != 1 || !stateID(string(last)) || historyCursor(q, string(last)) != in.Cursor {
			return out, invalid("worker history cursor is invalid or belongs to another scope")
		}
		q.AfterID = string(last)
	}
	reader, ok := s.store.(workerHistoryReader)
	if !ok {
		return out, invalid("worker history requires a bounded metadata reader")
	}
	meta, e := reader.ReadWorkerHistory(ctx, q)
	if e != nil {
		return out, e
	}
	if !meta.ProductFound || in.FeatureID != "" && !meta.FeatureFound || in.JobID != "" && !meta.JobFound {
		return out, invalid("worker history scope not found")
	}
	out = domain.WorkerHistoryPage{Projection: "native_worker_history", ProductID: in.ProductID, FeatureID: in.FeatureID, JobID: in.JobID, Collection: in.Collection, Items: []domain.WorkerHistoryItem{}, Consistency: "live_id_keyset: not a snapshot; concurrent inserts before the cursor require a fresh traversal", Guidance: "Recorded states and provenance are historical metadata, not verified success or runtime readiness. Native worker history only; Actions history is separate. Follow attempts and links collections for complete per-job records."}
	if len(meta.Items) > q.Limit {
		return out, invalid("worker history reader exceeded limit")
	}
	last := q.AfterID
	for i, c := range meta.Items {
		if !stateID(c.ID) || c.ID <= last || c.ProductID != q.ProductID || q.JobID != "" && c.JobID != q.JobID && !(q.Collection == "jobs" && c.ID == q.JobID) {
			return domain.WorkerHistoryPage{}, invalid("worker history reader returned invalid identity or scope")
		}
		for _, value := range []string{c.DeliveryID, c.ParentID, c.SnapshotHash, c.ParentHash, c.JobID, c.OperationID, c.IntegrationID, c.FeatureID, c.RevisionID, c.RepositoryID, c.SourceSHA, c.RecipeHash, c.InputHash, c.Capability, c.RecordedStatus, c.CreatedAt, c.UpdatedAt, c.Provenance} {
			if len(value) > 128 {
				return domain.WorkerHistoryPage{}, invalid("worker history reader returned oversized metadata")
			}
		}
		if q.Collection == "links" && (!stateID(c.FeatureID) || !stateID(c.IntegrationID) || !stateID(c.RevisionID) || !stateID(c.RepositoryID) || c.RevisionID != c.ID) {
			return domain.WorkerHistoryPage{}, invalid("worker history reader returned invalid frozen link")
		}
		// Every projected field is bounded; identity values are never truncated.
		b, _ := json.Marshal(c)
		if len(b) > 8192 {
			return domain.WorkerHistoryPage{}, invalid("worker history record exceeds metadata budget")
		}
		if i >= in.Limit {
			out.Pagination.HasMore = true
			break
		}
		out.Items = append(out.Items, c)
		out.Pagination.Returned = len(out.Items)
		out.Pagination.HasMore = true
		out.Pagination.NextCursor = historyCursor(q, c.ID)
		b, _ = json.Marshal(out)
		if len(b) > StatePageMaxBytes {
			out.Items = out.Items[:len(out.Items)-1]
			out.Pagination.Returned = len(out.Items)
			break
		}
		last = c.ID
		out.Pagination.HasMore = false
		out.Pagination.NextCursor = ""
	}
	if out.Pagination.HasMore {
		if len(out.Items) == 0 {
			return domain.WorkerHistoryPage{}, invalid("worker history exceeds metadata budget")
		}
		out.Pagination.NextCursor = historyCursor(q, out.Items[len(out.Items)-1].ID)
	}
	return out, nil
}
