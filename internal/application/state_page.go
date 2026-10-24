package application

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"releasecontrol/internal/auth"
	"releasecontrol/internal/domain"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
)

const StatePageMaxBytes = 32 << 10
const StatePageMaxLabelBytes = 256

type stateMetadataReader interface {
	ReadStateMetadata(context.Context, domain.StateMetadataRequest) (domain.StateMetadata, error)
}
type stateCursor struct {
	Version    int    `json:"v"`
	Collection string `json:"collection"`
	ProductID  string `json:"product_id"`
	FeatureID  string `json:"feature_id"`
	LastID     string `json:"last_id"`
}

func stateID(s string) bool {
	return s != "" && len(s) <= 128 && utf8.ValidString(s) && strings.TrimSpace(s) == s && !strings.ContainsRune(s, 0) && !strings.ContainsFunc(s, unicode.IsSpace)
}
func (s *Service) StatePage(ctx context.Context, in domain.StatePageRequest) (domain.StatePage, error) {
	var out domain.StatePage
	if err := authorize(ctx, "read", in.ProductID); err != nil {
		return out, err
	}
	if (in.ProductID != "" && !stateID(in.ProductID)) || (in.FeatureID != "" && !stateID(in.FeatureID)) {
		return out, invalid("get_state scope IDs must be exact nonblank UTF-8 IDs of at most 128 bytes")
	}
	if in.Limit == 0 {
		in.Limit = 25
	}
	if in.Limit < 1 || in.Limit > 100 {
		return out, invalid("get_state limit must be 1..100")
	}
	q := domain.StateMetadataRequest{ProductID: in.ProductID, FeatureID: in.FeatureID, Collection: "products", Limit: in.Limit + 1}
	if principal, ok := auth.FromContext(ctx); ok && !instanceAdmin(principal) && !slices.Contains(principal.Products, "*") {
		q.RestrictProducts = true
		q.AllowedProductIDs = append([]string{}, principal.Products...)
	}
	if in.ProductID != "" {
		q.Collection = "features"
	}
	if in.FeatureID != "" {
		q.Collection = "integrations"
	}
	if in.Cursor != "" {
		if len(in.Cursor) > 2048 {
			return out, invalid("get_state cursor exceeds 2048 bytes")
		}
		raw, e := base64.RawURLEncoding.Strict().DecodeString(in.Cursor)
		if e != nil || base64.RawURLEncoding.EncodeToString(raw) != in.Cursor {
			return out, invalid("invalid get_state cursor")
		}
		var c stateCursor
		d := json.NewDecoder(bytes.NewReader(raw))
		d.DisallowUnknownFields()
		if d.Decode(&c) != nil || d.Decode(new(any)) != io.EOF {
			return out, invalid("invalid get_state cursor")
		}
		canon, _ := json.Marshal(c)
		last, lastErr := base64.RawURLEncoding.Strict().DecodeString(c.LastID)
		if !bytes.Equal(canon, raw) || c.Version != 1 || c.Collection != q.Collection || c.ProductID != encodeCursorID(in.ProductID) || c.FeatureID != encodeCursorID(in.FeatureID) || lastErr != nil || encodeCursorID(string(last)) != c.LastID || !stateID(string(last)) {
			return out, invalid("get_state cursor is invalid or belongs to another scope")
		}
		q.AfterID = string(last)
	}
	reader, ok := s.store.(stateMetadataReader)
	if !ok {
		return out, invalid("get_state requires a bounded metadata reader; update the store implementation")
	}
	meta, e := reader.ReadStateMetadata(ctx, q)
	if e != nil {
		return out, e
	}
	if in.ProductID != "" && (meta.Product == nil || meta.Product.ID != in.ProductID) {
		return out, invalid("get_state product not found")
	}
	if in.FeatureID != "" && (meta.Feature == nil || meta.Feature.ID != in.FeatureID || meta.Product == nil || meta.Feature.ProductID != meta.Product.ID || (in.ProductID != "" && meta.Feature.ProductID != in.ProductID)) {
		return out, invalid("get_state feature not found in requested product")
	}
	out = domain.StatePage{Projection: "compact_navigation", Scope: domain.StateScope{Collection: q.Collection, Product: meta.Product, Feature: meta.Feature}, Items: []domain.StateCard{}, Consistency: "live_id_keyset: not a snapshot; concurrent inserts before the cursor require a fresh traversal", Guidance: "Navigation only; recorded_status is not gate/runtime/release acceptance. Context, requirements, histories, results, logs, operations, connections and credentials are omitted. Use get_project_context, get_integration_context or scoped query tools for details; resume includes history and is not a compact export. Use the operator backup/export mechanism for a full backup."}
	validate := func(c *domain.StateCard) error {
		product := c.ProductID
		if product == "" {
			product = c.ID
		}
		if err := authorize(ctx, "read", product); err != nil {
			return err
		}
		if !stateID(c.ID) || (c.ProductID != "" && !stateID(c.ProductID)) || (c.FeatureID != "" && !stateID(c.FeatureID)) {
			return invalid("get_state encountered an oversized or invalid record identity; repair the record through the owning operator")
		}
		if len(c.RecordedStatus) > 64 || (c.FeatureID != "" && !domain.ValidStatus("integration", c.RecordedStatus)) || (c.ProductID != "" && c.FeatureID == "" && !domain.ValidStatus("feature", c.RecordedStatus)) {
			return invalid("get_state encountered an invalid recorded status")
		}
		c.Name, c.Truncated = truncateStateLabel(c.Name, c.Truncated)
		c.Title, c.Truncated = truncateStateLabel(c.Title, c.Truncated)
		return nil
	}
	if out.Scope.Product != nil {
		if e := validate(out.Scope.Product); e != nil {
			return domain.StatePage{}, e
		}
	}
	if out.Scope.Feature != nil {
		if e := validate(out.Scope.Feature); e != nil {
			return domain.StatePage{}, e
		}
	}
	if len(meta.Items) > q.Limit {
		return domain.StatePage{}, invalid("bounded metadata reader exceeded requested record limit")
	}
	last := q.AfterID
	for i, c := range meta.Items {
		if e := validate(&c); e != nil {
			return domain.StatePage{}, e
		}
		if c.ID <= last {
			return domain.StatePage{}, invalid("bounded metadata reader returned unordered identities")
		}
		if q.Collection == "features" && c.ProductID != in.ProductID || q.Collection == "integrations" && (c.FeatureID != in.FeatureID || meta.Product == nil || c.ProductID != meta.Product.ID) {
			return domain.StatePage{}, invalid("bounded metadata reader returned a record outside requested scope")
		}
		if i >= in.Limit {
			out.Pagination.HasMore = true
			break
		}
		out.Items = append(out.Items, c)
		out.Pagination.Returned = len(out.Items)
		// Reserve the exact possible continuation envelope before accepting a card.
		out.Pagination.HasMore = true
		out.Pagination.NextCursor = encodeStateCursor(q, c.ID)
		b, _ := json.Marshal(out)
		if len(b) > StatePageMaxBytes {
			out.Items = out.Items[:len(out.Items)-1]
			out.Pagination.Returned = len(out.Items)
			out.Pagination.HasMore = true
			break
		}
		last = c.ID
		out.Pagination.HasMore = false
		out.Pagination.NextCursor = ""
	}
	if out.Pagination.HasMore {
		if len(out.Items) == 0 {
			return domain.StatePage{}, invalid("get_state metadata exceeds navigation budget")
		}
		out.Pagination.NextCursor = encodeStateCursor(q, out.Items[len(out.Items)-1].ID)
	}
	return out, nil
}
func truncateStateLabel(s string, truncated bool) (string, bool) {
	if !utf8.ValidString(s) {
		s = strings.ToValidUTF8(s, "�")
		truncated = true
	}
	if len(s) <= 256 {
		return s, truncated
	}
	n := 256
	for !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n], true
}
func encodeStateCursor(q domain.StateMetadataRequest, last string) string {
	// Encode each exact ID before JSON so control characters cannot expand a
	// 128-byte identity sixfold and exceed the cursor's accepted input limit.
	b, _ := json.Marshal(stateCursor{1, q.Collection, encodeCursorID(q.ProductID), encodeCursorID(q.FeatureID), encodeCursorID(last)})
	return base64.RawURLEncoding.EncodeToString(b)
}

func encodeCursorID(id string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(id))
}
