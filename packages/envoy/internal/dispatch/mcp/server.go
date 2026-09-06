// Package mcp serves the Streamable HTTP MCP endpoint for the dispatch tool.
//
// Authentication is per-call: the Authorization header of the HTTP request
// carrying each tools/call is forwarded verbatim to GitHub. There is no
// fallback to a server-stored token.
//
// The endpoint is stateless. Clients — the `dispatch` tool inside each host
// plugin, via envoy-client's dispatch-client — send exactly one tools/call
// POST per invocation and never initialize or hold a session, so a server
// that validated session ids would refuse them. Dispatch is a single
// request/response tool with no server-initiated messages, which is all
// stateless mode gives up.
package mcp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

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
		Description: "Raise a durable question to the human as a Dispatch thread (a GitHub issue), or continue an existing thread with a follow-up question. Open with subject; continue with thread.",
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
		started := time.Now()
		bearer := extractBearer(r.Header)
		response := &statusResponseWriter{ResponseWriter: w}
		if bearer == "" {
			http.Error(response, `{"error":"missing bearer"}`, http.StatusUnauthorized)
		} else {
			next.ServeHTTP(response, r)
		}
		slog.Info(
			"dispatch request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", response.statusCode(),
			"duration_ms", time.Since(started).Milliseconds(),
			"bearer", bearerFingerprint(bearer),
		)
	})
}

// statusResponseWriter records the status the handler wrote so the request
// log can report it. The MCP Streamable HTTP transport streams SSE and flushes
// through http.NewResponseController, which reaches the real writer via
// Unwrap; without it every Flush would report ErrNotSupported and events
// would sit in the buffer until the response ended.
type statusResponseWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusResponseWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

func (w *statusResponseWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *statusResponseWriter) Write(body []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(body)
}

func (w *statusResponseWriter) statusCode() int {
	if w.status == 0 {
		return http.StatusOK
	}
	return w.status
}

func bearerFingerprint(bearer string) string {
	if bearer == "" {
		return "-"
	}
	sum := sha256.Sum256([]byte(bearer))
	return hex.EncodeToString(sum[:4])
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
// description string; required-ness is conveyed by absence of omitempty:
// only context and question are required, because `subject` opens a thread
// and `thread` continues one. core.Dispatch enforces that exactly one is
// present with its arguments.
type dispatchInput struct {
	Subject  string                  `json:"subject,omitempty" jsonschema:"Open a thread: one line, the decision needed. Omit when continuing a thread."`
	Thread   string                  `json:"thread,omitempty" jsonschema:"Continue a thread: <n> | owner/name#<n>. Omit subject, urgency, and parent."`
	Context  string                  `json:"context" jsonschema:"What you are doing, what you found, why you are stuck (at most 1200 characters). The reader has NOT seen your transcript."`
	Question string                  `json:"question" jsonschema:"The ask (at most 800 characters): current state → desired state → your recommendation and why; options go in ask."`
	Ask      []core.DispatchQuestion `json:"ask,omitempty" jsonschema:"Optional structured questions attached to this turn"`
	Urgency  string                  `json:"urgency,omitempty" jsonschema:"Urgency: low | med | high | blocking (default med). Opening a thread only."`
	Repo     string                  `json:"repo,omitempty" jsonschema:"owner/name. Filled by the calling plugin from the session's working directory when the call does not name a qualified parent or thread."`
	Parent   string                  `json:"parent,omitempty" jsonschema:"<n> | owner/name#<n>[#<commentId>]. Opening a thread only: link it as a sub-issue and append a breadcrumb to the comment."`
	Origin   *core.Origin            `json:"origin,omitempty" jsonschema:"Filled by the calling plugin from the session; leave unset."`
}

func (s *Server) dispatchHandler(ctx context.Context, req *mcpsdk.CallToolRequest, input dispatchInput) (*mcpsdk.CallToolResult, any, error) {
	started := time.Now()
	// The bearer is this call's own HTTP header, which the Streamable HTTP
	// transport sets on every request. Never take it from ctx: under a stateful
	// transport ctx descends from the initialize request and would pin the
	// session to the first token a client sent; plugins mint a fresh one per
	// call.
	token := extractBearer(req.Extra.Header)
	call := core.DispatchInput{
		Repo:     input.Repo,
		Parent:   input.Parent,
		Thread:   input.Thread,
		Subject:  input.Subject,
		Context:  input.Context,
		Question: input.Question,
		Origin:   input.Origin,
		Ask:      input.Ask,
		Urgency:  core.Urgency(input.Urgency),
	}
	if token == "" {
		err := fmt.Errorf("missing bearer token")
		logToolCall(call, token, loggedThread(input.Thread), started, err)
		return nil, nil, err
	}
	client := s.newClient(ctx, token)
	result, err := core.Dispatch(ctx, client, call)
	if err != nil {
		logToolCall(call, token, loggedThread(input.Thread), started, err)
		return nil, nil, err
	}
	logToolCall(call, token, strconv.Itoa(result.Thread), started, nil)
	data, _ := json.Marshal(result)
	return &mcpsdk.CallToolResult{
		Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: string(data)}},
	}, result, nil
}

// logToolCall writes the one line per tools/call that the service keeps. repo
// is the repository the call resolved to (a qualified thread or parent names
// its own), so a cross-repository continuation logs the repo it posted to
// rather than the empty input.Repo the plugin sent.
func logToolCall(call core.DispatchInput, bearer, thread string, started time.Time, err error) {
	session, host := "-", "-"
	if call.Origin != nil {
		if call.Origin.SessionID != "" {
			session = call.Origin.SessionID
		}
		if call.Origin.Host != "" {
			host = call.Origin.Host
		}
	}
	result, message := "ok", ""
	if err != nil {
		result, message = "error", err.Error()
	}
	slog.Info(
		"dispatch tool call",
		"tool", "dispatch",
		"session", session,
		"host", host,
		"repo", core.ResolveRepo(call),
		"thread", thread,
		"upstream_status", upstreamStatus(err),
		"result", result,
		"error", message,
		"duration_ms", time.Since(started).Milliseconds(),
		"bearer", bearerFingerprint(bearer),
	)
}

func loggedThread(thread string) string {
	if thread == "" {
		return "new"
	}
	parsed, err := core.ParseThread(thread)
	if err != nil {
		return thread
	}
	return strconv.Itoa(parsed.IssueNumber)
}

// upstreamStatus is GitHub's HTTP status behind a failed call, or 0 when the
// error did not come from a GitHub response. go-github returns four types that
// carry the response: ErrorResponse, TwoFactorAuthError, and the primary and
// secondary rate-limit errors for 403/429.
func upstreamStatus(err error) int {
	var response *github.ErrorResponse
	if errors.As(err, &response) && response.Response != nil {
		return response.Response.StatusCode
	}
	var twoFactor *github.TwoFactorAuthError
	if errors.As(err, &twoFactor) && twoFactor.Response != nil {
		return twoFactor.Response.StatusCode
	}
	var rateLimit *github.RateLimitError
	if errors.As(err, &rateLimit) && rateLimit.Response != nil {
		return rateLimit.Response.StatusCode
	}
	var abuse *github.AbuseRateLimitError
	if errors.As(err, &abuse) && abuse.Response != nil {
		return abuse.Response.StatusCode
	}
	return 0
}
