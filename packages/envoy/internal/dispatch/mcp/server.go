// Package mcp serves the Streamable HTTP MCP endpoint for the dispatch tool.
//
// Authentication is per-request: the Authorization header is forwarded verbatim
// to GitHub for every call made on behalf of the agent. There is no fallback
// to a server-stored token.
package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/sjawhar/envoy/internal/dispatch/core"
	"github.com/sjawhar/envoy/internal/dispatch/githubapi"
)

// Server wires the per-request bearer middleware around an MCP Streamable HTTP
// handler that exposes a single tool: dispatch.
type Server struct {
	handler http.Handler
}

type bearerKey struct{}

func bearerFromContext(ctx context.Context) string {
	v, _ := ctx.Value(bearerKey{}).(string)
	return v
}

// New returns the dispatch MCP server.
func New() *Server {
	mcpServer := mcpsdk.NewServer(&mcpsdk.Implementation{
		Name:    "dispatch",
		Version: "0.1.0",
	}, nil)

	s := &Server{}

	mcpsdk.AddTool(mcpServer, &mcpsdk.Tool{
		Name:        "dispatch",
		Description: "Create a Dispatch thread — a GitHub issue, optionally linked as a sub-issue of a parent. Use for durable questions, decisions, FYIs, or blocking asks that need human attention.",
	}, s.dispatchHandler)

	streamable := mcpsdk.NewStreamableHTTPHandler(func(*http.Request) *mcpsdk.Server {
		return mcpServer
	}, nil)
	s.handler = bearerMiddleware(streamable)
	return s
}

// Handler returns the http.Handler to mount at /mcp.
func (s *Server) Handler() http.Handler { return s.handler }

func bearerMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := extractBearer(r)
		if token == "" {
			http.Error(w, `{"error":"missing bearer"}`, http.StatusUnauthorized)
			return
		}
		ctx := context.WithValue(r.Context(), bearerKey{}, token)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func extractBearer(r *http.Request) string {
	header := r.Header.Get("Authorization")
	if header == "" {
		return ""
	}
	const prefix = "Bearer "
	if len(header) < len(prefix) {
		return ""
	}
	if !strings.EqualFold(header[:len(prefix)], prefix) {
		return ""
	}
	return strings.TrimSpace(header[len(prefix):])
}

// dispatchInput mirrors core.DispatchInput. The jsonschema tag is a plain
// description string; required-ness is conveyed by absence of omitempty.
type dispatchInput struct {
	Subject  string              `json:"subject" jsonschema:"One line: the decision needed."`
	Context  string              `json:"context" jsonschema:"What you are doing, what you found, why you are stuck. The reader has NOT seen your transcript — never reference 'the list above' or 'those items'."`
	Question string              `json:"question" jsonschema:"The ask: current state → desired state → proposed change, options with tradeoffs, your recommendation."`
	Ask      []core.QuestionInfo `json:"ask,omitempty" jsonschema:"Optional structured questions attached to the thread"`
	Urgency  string              `json:"urgency,omitempty" jsonschema:"Urgency: low | med | high | blocking (default med)"`
	Repo     string              `json:"repo,omitempty" jsonschema:"owner/name. Defaults to the repo of the session's working directory (the shim fills this)."`
	Parent   string              `json:"parent,omitempty" jsonschema:"<n> | owner/name#<n>[#<commentId>]. When given, the thread is linked as a sub-issue and a breadcrumb is appended to the comment."`
	Origin   *core.Origin        `json:"origin,omitempty" jsonschema:"Injected by the dispatch shim; leave unset."`
}

func (s *Server) dispatchHandler(ctx context.Context, req *mcpsdk.CallToolRequest, input dispatchInput) (*mcpsdk.CallToolResult, any, error) {
	token := bearerFromContext(ctx)
	if token == "" {
		return nil, nil, fmt.Errorf("missing bearer token")
	}
	urgency := core.Urgency(input.Urgency)
	if urgency == "" {
		urgency = core.UrgencyMed
	}
	client := githubapi.NewClient(ctx, token)
	result, err := core.CreateThread(ctx, client, core.DispatchInput{
		Repo:     input.Repo,
		Parent:   input.Parent,
		Subject:  input.Subject,
		Context:  input.Context,
		Question: input.Question,
		Origin:   input.Origin,
		Ask:      input.Ask,
		Urgency:  urgency,
	})
	if err != nil {
		return nil, nil, err
	}
	data, _ := json.Marshal(result)
	return &mcpsdk.CallToolResult{
		Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: string(data)}},
	}, result, nil
}
