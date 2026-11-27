package transport

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type productWorkQuery struct {
	ProductID string `json:"product_id,omitempty"`
}

type queueWorkQuery struct {
	ProductID string `json:"product_id,omitempty"`
	Limit     int    `json:"limit,omitempty"`
}

type questionListQuery struct {
	ProductID   string `json:"product_id,omitempty"`
	FeatureID   string `json:"feature_id,omitempty"`
	AddressedTo string `json:"addressed_to,omitempty"`
	Status      string `json:"status,omitempty"`
	Kind        string `json:"kind,omitempty"`
	Limit       int    `json:"limit,omitempty"`
	Cursor      string `json:"cursor,omitempty"`
}

type dependencyGraphQuery struct {
	ProductID       string `json:"product_id"`
	IncludeProposed bool   `json:"include_proposed,omitempty"`
}

func registerWorkRoutes(mux routeRegistrar, service Service) {
	for pattern, tool := range map[string]string{
		"GET /api/questions":                      "list_questions",
		"GET /api/questions/{id}":                 "get_question",
		"GET /api/products/{id}/dependency-graph": "get_dependency_graph",
		"GET /api/next-work":                      "next_work",
		"GET /api/status-overview":                "status_overview",
	} {
		mux.HandleFunc(pattern, func(w http.ResponseWriter, request *http.Request) {
			if tool == "get_dependency_graph" {
				w.Header().Set("Cache-Control", "no-store")
			}
			input := request.PathValue("id")
			if tool != "get_question" {
				query := request.URL.Query()
				productID := query.Get("product_id")
				if input != "" {
					productID = input
				}
				limit := 0
				if raw := query.Get("limit"); raw != "" {
					parsed, err := strconv.Atoi(raw)
					if err != nil || parsed <= 0 {
						limit = -1
					} else {
						limit = parsed
					}
				}
				payload, _ := json.Marshal(map[string]any{"product_id": productID, "feature_id": query.Get("feature_id"), "status": query.Get("status"), "kind": query.Get("kind"), "addressed_to": query.Get("addressed_to"), "cursor": query.Get("cursor"), "limit": limit, "include_proposed": query.Get("include_proposed") == "true"})
				input = string(payload)
			}
			value, err := service.Query(request.Context(), tool, input)
			respond(w, value, err)
		})
	}
}

func registerWorkTool[Input any](server *mcp.Server, service Service, tool *mcp.Tool) {
	mcp.AddTool(server, tool, func(ctx context.Context, _ *mcp.CallToolRequest, input Input) (*mcp.CallToolResult, any, error) {
		payload, err := json.Marshal(input)
		if err != nil {
			return nil, nil, err
		}
		value, err := service.Query(ctx, tool.Name, string(payload))
		return nil, value, err
	})
}

func registerWorkTools(server *mcp.Server, service Service) {
	registerWorkTool[questionListQuery](server, service, &mcp.Tool{Name: "list_questions", Description: "List visible owner questions and actions, defaulting to open and addressed to me. Filter by kind and follow next_cursor."})
	mcp.AddTool(server, &mcp.Tool{Name: "get_question", Description: "Read a visible question or owner action with options, recommendation, action instructions, resolved reference titles and any durable answer."}, func(ctx context.Context, _ *mcp.CallToolRequest, input struct {
		ID string `json:"question_id"`
	}) (*mcp.CallToolResult, any, error) {
		value, err := service.Query(ctx, "get_question", input.ID)
		return nil, value, err
	})
	registerWorkTool[dependencyGraphQuery](server, service, &mcp.Tool{Name: "get_dependency_graph", Description: "Read accepted and optionally proposed dependencies between visible features; requires edges gate execution."})
	registerWorkTool[queueWorkQuery](server, service, &mcp.Tool{Name: "next_work", Description: "Read the caller's ordered eligible integrations and features to plan, with bounded blocker explanations. Claim separately."})
	registerWorkTool[productWorkQuery](server, service, &mcp.Tool{Name: "status_overview", Description: "Read personal questions and execution counts, product ideas, ready features and recent completion."})
}
