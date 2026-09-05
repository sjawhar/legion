// Package mcp serves the Streamable HTTP MCP endpoint for the dispatch tool.
//
// Authentication is per-call: the Authorization header of the HTTP request
// carrying each tools/call is forwarded verbatim to GitHub. There is no
// fallback to a server-stored token.
//
// The endpoint is stateless. The shim holds one MCP session id for the life
// of its process and never re-initializes, so a server that validated
// session ids would answer every call from a shim older than the last
// restart with 404 until that shim's session was restarted. Dispatch is a
// single request/response tool with no server-initiated messages, which is
// all stateless mode gives up.
package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/google/go-github/v66/github"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/sjawhar/envoy/internal/dispatch/core"
	"github.com/sjawhar/envoy/internal/dispatch/githubapi"
)

// Server wires the per-request bearer middleware around an MCP Streamable HTTP
// handler that exposes a single tool: dispatch.
type Server struct {
	handler   http.Handler
	newClient func(ctx context.Context, token string) *github.Client
}

// New returns the dispatch MCP server.
func New() *Server { return newServer(githubapi.NewClient) }

// newServer builds the server around newClient, which constructs the GitHub
// client for each call's bearer. New wires githubapi.NewClient; tests
// substitute a recorder.
func newServer(newClient func(ctx context.Context, token string) *github.Client) *Server {
	mcpServer := mcpsdk.NewServer(&mcpsdk.Implementation{
		Name:    "dispatch",
		Version: "0.1.0",
	}, nil)

	s := &Server{newClient: newClient}

	mcpsdk.AddTool(mcpServer, &mcpsdk.Tool{
		Name:        "dispatch",
		Description: "Create a Dispatch thread — a GitHub issue, optionally linked as a sub-issue of a parent. Use for durable questions, decisions, FYIs, or blocking asks that need human attention.",
	}, s.dispatchHandler)

	streamable := mcpsdk.NewStreamableHTTPHandler(func(*http.Request) *mcpsdk.Server {
		return mcpServer
	}, &mcpsdk.StreamableHTTPOptions{Stateless: true})
	s.handler = bearerMiddleware(streamable)
	return s
}

// Handler returns the http.Handler to mount at /mcp.
func (s *Server) Handler() http.Handler { return s.handler }

// bearerMiddleware rejects requests without a bearer before they reach the MCP
// handler. The bearer used for GitHub is read from each tools/call's own
// request in dispatchHandler.
func bearerMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if extractBearer(r.Header) == "" {
			http.Error(w, `{"error":"missing bearer"}`, http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func extractBearer(header http.Header) string {
	value := header.Get("Authorization")
	const prefix = "Bearer "
	if len(value) < len(prefix) || !strings.EqualFold(value[:len(prefix)], prefix) {
		return ""
	}
	return strings.TrimSpace(value[len(prefix):])
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
	// The bearer is this call's own HTTP header, which the Streamable HTTP
	// transport sets on every request. Never take it from ctx: under a stateful
	// transport ctx descends from the initialize request and would pin the
	// session to the first token the shim sent; the shim mints a fresh one per
	// call.
	token := extractBearer(req.Extra.Header)
	if token == "" {
		return nil, nil, fmt.Errorf("missing bearer token")
	}
	urgency := core.Urgency(input.Urgency)
	if urgency == "" {
		urgency = core.UrgencyMed
	}
	client := s.newClient(ctx, token)
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
