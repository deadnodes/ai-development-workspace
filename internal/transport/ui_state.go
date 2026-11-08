package transport

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"releasecontrol/internal/domain"
	"strings"
)

type uiStateService interface {
	UIState(context.Context, string, string) (domain.UIState, error)
}

func uiStateSnapshot(service Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		reader, ok := service.(uiStateService)
		if !ok {
			write(w, 501, map[string]string{"error": "bounded browser reader unavailable"})
			return
		}
		for k, v := range r.URL.Query() {
			if (k != "product_id" && k != "feature_id") || len(v) != 1 {
				write(w, 400, map[string]string{"error": "invalid UI scope"})
				return
			}
		}
		v, e := reader.UIState(r.Context(), r.URL.Query().Get("product_id"), r.URL.Query().Get("feature_id"))
		if e != nil {
			respond(w, nil, e)
			return
		}
		b, e := json.Marshal(v)
		if e != nil {
			respond(w, nil, e)
			return
		}
		if len(b) > 8<<20 {
			write(w, 422, map[string]string{"error": "browser projection exceeds response budget"})
			return
		}
		h := sha256.Sum256(b)
		tag := `"` + hex.EncodeToString(h[:]) + `"`
		w.Header().Set("ETag", tag)
		w.Header().Set("Vary", "Cookie, Authorization")
		for _, candidate := range strings.Split(r.Header.Get("If-None-Match"), ",") {
			candidate = strings.TrimSpace(candidate)
			if candidate == "*" || strings.TrimPrefix(candidate, "W/") == tag {
				w.WriteHeader(http.StatusNotModified)
				return
			}
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(b)
	}
}
