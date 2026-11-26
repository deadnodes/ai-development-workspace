package transport

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"releasecontrol/internal/application"
	"releasecontrol/internal/domain"
)

type prodPromotionService interface {
	RequestProdPromotion(context.Context, domain.RequestProdPromotion) (domain.ProdPromotion, error)
	GetProdPromotion(context.Context, string, string) (domain.ProdPromotion, error)
	ListProdPromotions(context.Context, string, int) ([]domain.ProdPromotion, error)
	ClaimProdPromotion(context.Context, domain.ClaimProdPromotion) (*domain.ProdPromotion, error)
	RecordProdPromotionResult(context.Context, string, string, string, domain.ProdPromotionResult) (domain.ProdPromotion, error)
	HeartbeatProdPromotion(context.Context, string, string, string, domain.HeartbeatProdPromotion) (domain.ProdPromotion, error)
	PublicProdSignatures(context.Context, string) (application.PublicProdSignatures, error)
}

func registerProdPromotions(mux routeRegistrar, server *mcp.Server, service Service) {
	mux.HandleFunc("GET /api/public/prod-signatures/{digest}", func(w http.ResponseWriter, r *http.Request) {
		s, ok := service.(prodPromotionService)
		if !ok {
			write(w, 503, nil)
			return
		}
		v, e := s.PublicProdSignatures(r.Context(), r.PathValue("digest"))
		if e != nil {
			respond(w, nil, e)
			return
		}
		b, e := json.Marshal(v)
		if e != nil {
			respond(w, nil, e)
			return
		}
		if len(b) > 1<<20 {
			write(w, 413, map[string]string{"error": "signature response exceeds size limit"})
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "public, max-age=30")
		w.WriteHeader(200)
		_, _ = w.Write(b)
	})
	mux.HandleFunc("POST /api/prod-promotions", func(w http.ResponseWriter, r *http.Request) {
		s, ok := service.(prodPromotionService)
		if !ok {
			write(w, 503, map[string]string{"error": "promotions unavailable"})
			return
		}
		if r.Header.Get("Content-Type") != "application/json" {
			write(w, 415, map[string]string{"error": "application/json required"})
			return
		}
		var in domain.RequestProdPromotion
		d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 32<<10))
		d.DisallowUnknownFields()
		if d.Decode(&in) != nil || d.Decode(new(any)) != io.EOF {
			write(w, 400, map[string]string{"error": "invalid request"})
			return
		}
		v, e := s.RequestProdPromotion(r.Context(), in)
		respond(w, v, e)
	})
	mux.HandleFunc("GET /api/prod-promotions", func(w http.ResponseWriter, r *http.Request) {
		s, ok := service.(prodPromotionService)
		if !ok {
			write(w, 503, nil)
			return
		}
		limit := 25
		if r.URL.Query().Has("limit") {
			var e error
			limit, e = strconv.Atoi(r.URL.Query().Get("limit"))
			if e != nil || limit < 1 || limit > 100 {
				write(w, 422, map[string]string{"error": "limit must be 1..100"})
				return
			}
		}
		v, e := s.ListProdPromotions(r.Context(), r.URL.Query().Get("product_id"), limit)
		respond(w, v, e)
	})
	mux.HandleFunc("GET /api/prod-promotions/{id}", func(w http.ResponseWriter, r *http.Request) {
		s, ok := service.(prodPromotionService)
		if !ok {
			write(w, 503, nil)
			return
		}
		v, e := s.GetProdPromotion(r.Context(), r.URL.Query().Get("product_id"), r.PathValue("id"))
		respond(w, v, e)
	})
	mcp.AddTool(server, &mcp.Tool{Name: "request_prod_promotion", Description: "The only way to obtain cosign-prod signatures. Requires the owner's explicit go quoted verbatim in owner_approval. This requests signing only; it does not merge or deploy anything."}, func(ctx context.Context, _ *mcp.CallToolRequest, in domain.RequestProdPromotion) (*mcp.CallToolResult, any, error) {
		s, ok := service.(prodPromotionService)
		if !ok {
			return nil, nil, io.ErrUnexpectedEOF
		}
		v, e := s.RequestProdPromotion(ctx, in)
		return nil, v, e
	})
	mcp.AddTool(server, &mcp.Tool{Name: "get_prod_promotion", Description: "Read one Product-scoped production promotion and its per-image outcomes."}, func(ctx context.Context, _ *mcp.CallToolRequest, in struct {
		ProductID   string `json:"product_id"`
		PromotionID string `json:"promotion_id"`
	}) (*mcp.CallToolResult, any, error) {
		s, ok := service.(prodPromotionService)
		if !ok {
			return nil, nil, io.ErrUnexpectedEOF
		}
		v, e := s.GetProdPromotion(ctx, in.ProductID, in.PromotionID)
		return nil, v, e
	})
	mcp.AddTool(server, &mcp.Tool{Name: "list_prod_promotions", Description: "List recent Product-scoped production promotions; limit is 1..100."}, func(ctx context.Context, _ *mcp.CallToolRequest, in struct {
		ProductID string `json:"product_id"`
		Limit     int    `json:"limit,omitempty"`
	}) (*mcp.CallToolResult, any, error) {
		s, ok := service.(prodPromotionService)
		if !ok {
			return nil, nil, io.ErrUnexpectedEOF
		}
		v, e := s.ListProdPromotions(ctx, in.ProductID, in.Limit)
		return nil, v, e
	})
}

func prodPromoterRoutes(service Service, options Options) http.Handler {
	mux := http.NewServeMux()
	s, ok := service.(prodPromotionService)
	if !ok {
		mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) { write(w, 503, nil) })
		return mux
	}
	product, environment, _ := strings.Cut(options.ProdPromoterScope, ":")
	mux.HandleFunc("POST /api/prod-promotions/claim", func(w http.ResponseWriter, r *http.Request) {
		if product == "" || environment == "" {
			write(w, 403, map[string]string{"error": "promoter scope unavailable"})
			return
		}
		var in struct {
			Wait *int `json:"wait,omitempty"`
		}
		d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
		d.DisallowUnknownFields()
		if d.Decode(&in) != nil || d.Decode(new(any)) != io.EOF {
			write(w, 400, map[string]string{"error": "invalid claim request"})
			return
		}
		wait := 50
		if in.Wait != nil {
			wait = *in.Wait
		}
		if wait >= 0 && wait <= 55 {
			_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(time.Duration(wait+15) * time.Second))
		}
		v, e := s.ClaimProdPromotion(r.Context(), domain.ClaimProdPromotion{ProductID: product, EnvironmentID: environment, Wait: wait})
		if e != nil {
			respond(w, nil, e)
			return
		}
		if v == nil {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		respond(w, v, nil)
	})
	mux.HandleFunc("POST /api/prod-promotions/{id}/result", func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, 2<<20)
		d := json.NewDecoder(r.Body)
		d.DisallowUnknownFields()
		var in domain.ProdPromotionResult
		if d.Decode(&in) != nil || d.Decode(new(any)) != io.EOF {
			write(w, 400, map[string]string{"error": "invalid result"})
			return
		}
		v, e := s.RecordProdPromotionResult(r.Context(), r.PathValue("id"), product, environment, in)
		respond(w, v, e)
	})
	mux.HandleFunc("POST /api/prod-promotions/{id}/heartbeat", func(w http.ResponseWriter, r *http.Request) {
		var in domain.HeartbeatProdPromotion
		d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
		d.DisallowUnknownFields()
		if d.Decode(&in) != nil || d.Decode(new(any)) != io.EOF || in.ClaimID == "" {
			write(w, 400, map[string]string{"error": "invalid heartbeat"})
			return
		}
		v, e := s.HeartbeatProdPromotion(r.Context(), r.PathValue("id"), product, environment, in)
		respond(w, v, e)
	})
	return mux
}
