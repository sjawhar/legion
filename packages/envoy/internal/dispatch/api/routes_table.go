package api

import (
	"net/http"
	"sort"
)

// routeAuth names who may call a route. It describes the check the handler performs; the
// handler stays the enforcement point.
type routeAuth string

const (
	// authPublic needs no credential.
	authPublic routeAuth = "public"
	// authAny accepts any authenticated caller: a browser cookie or a bearer token.
	authAny routeAuth = "any"
	// authHuman accepts a browser cookie only; bearers get 403 HUMAN_ONLY.
	authHuman routeAuth = "human"
	// authBearer accepts a bearer token only.
	authBearer routeAuth = "bearer"
)

// apiRoute is one mounted route. Register mounts the table and GET /api/v1 lists it, so the
// two can never disagree about what the server serves.
type apiRoute struct {
	Method      string
	Pattern     string
	Auth        routeAuth
	Description string
	Handler     http.HandlerFunc
}

// routeIndexEntry is the public shape of a route in GET /api/v1.
type routeIndexEntry struct {
	Method      string    `json:"method"`
	Path        string    `json:"path"`
	Auth        routeAuth `json:"auth"`
	Description string    `json:"description"`
}

// routeIndexDocs names the agent-facing guide for the routes below.
const routeIndexDocs = "skills/dispatch/SKILL.md"

// routes lists every native-workspace route. Add a row here, never a mux.HandleFunc line.
func (s *server) routes() []apiRoute {
	routes := []apiRoute{
		{http.MethodGet, "/api/v1", authPublic, "This index: every route with its method, auth, and purpose.", s.getRouteIndex},
		{http.MethodGet, "/api/v1/schema/blocks", authPublic, "The typed document block schema as JSON.", s.getBlockSchema},
		{http.MethodGet, "/api/v1/projects", authAny, "List projects.", s.listProjects},
		{http.MethodPost, "/api/v1/projects", authHuman, "Create a project {key, name}.", s.createProject},
		{http.MethodGet, "/api/v1/projects/{key}/artifacts", authAny, "List a project's documents.", s.listProjectArtifacts},
		{http.MethodPost, "/api/v1/projects/{key}/artifacts", authAny, "Upload a project document (JSON or multipart).", s.uploadProjectArtifact},
		{http.MethodGet, "/api/v1/settings/repo-projects", authHuman, "List GitHub repository to project mappings.", s.listRepoProjects},
		{http.MethodPut, "/api/v1/settings/repo-projects/{owner}/{repo}", authHuman, "Map a GitHub repository to a project.", s.putRepoProject},
		{http.MethodDelete, "/api/v1/settings/repo-projects/{owner}/{repo}", authHuman, "Remove a repository mapping.", s.deleteRepoProject},
		{http.MethodGet, "/api/v1/me/agent-tokens", authHuman, "List the caller's personal agent tokens.", s.listAgentTokens},
		{http.MethodPost, "/api/v1/me/agent-tokens", authHuman, "Mint a personal agent token.", s.createAgentToken},
		{http.MethodDelete, "/api/v1/me/agent-tokens/{id}", authHuman, "Revoke a personal agent token.", s.revokeAgentToken},
		{http.MethodGet, "/api/v1/users", authHuman, "The humans who may sign in; the assignee picker's options.", s.listUsers},
		{http.MethodGet, "/api/v1/whoami", authAny, "Who the server takes the caller for: {kind: user, login} or {kind: agent, owner} (owner is a personal token's lowercase login, null for the shared token).", s.whoami},
		{http.MethodGet, "/api/v1/issues", authAny, "List issues; filters project, status, parent, label, open, updated_since; ?pinned=true is human-only.", s.listIssues},
		{http.MethodPost, "/api/v1/issues", authAny, "Create an issue (native, or from a GitHub owner/repo#n ref); assignee defaults to the creating human, the personal token's owner, or the parent's assignee.", s.createIssue},
		{http.MethodGet, "/api/v1/issues/resolve", authAny, "Resolve ?ref=<KEY | owner/repo#n> to an issue key.", s.resolveIssue},
		{http.MethodGet, "/api/v1/issues/{key}", authAny, "Read an issue with its open asks and primary document.", s.getIssue},
		{http.MethodPatch, "/api/v1/issues/{key}", authAny, "Update title, status, priority, labels, route, parent, rank, or assignee (an allowlisted login or null; anyone may reassign).", s.patchIssue},
		{http.MethodGet, "/api/v1/issues/{key}/events", authAny, "Page an issue's event log (after, before, ids, order, limit).", s.listIssueEvents},
		{http.MethodGet, "/api/v1/issues/{key}/references", authAny, "Cross-references to and from an issue.", s.getIssueReferences},
		{http.MethodGet, "/api/v1/references", authAny, "Edges of one node in the reference graph: ?to=<dispatch ref> backlinks or ?from= links; ?kind=, ?since=<events.id> (mentions only).", s.getReferences},
		{http.MethodGet, "/api/v1/issues/{key}/subscribers", authHuman, "Sessions subscribed to an issue's topics.", s.listIssueSubscribers},
		{http.MethodDelete, "/api/v1/issues/{key}/subscribers/{session_id}", authHuman, "Unsubscribe a session from an issue.", s.unsubscribeIssueSession},
		{http.MethodGet, "/api/v1/issues/{key}/artifacts", authAny, "List an issue's documents and files.", s.listArtifacts},
		{http.MethodPost, "/api/v1/issues/{key}/artifacts", authAny, "Upload an issue document or file (JSON or multipart).", s.uploadArtifact},
		{http.MethodPost, "/api/v1/issues/{key}/messages", authAny, "Post an issue message; target and delivery address a session or role.", s.createMessage},
		{http.MethodGet, "/api/v1/issues/{key}/messages/{id}", authAny, "Read one message with its deliveries and replies.", s.getMessage},
		{http.MethodPost, "/api/v1/messages/{id}/deliveries", authAny, "Retry delivering a targeted message.", s.createDelivery},
		{http.MethodPost, "/api/v1/messages/{id}/reply", authBearer, "The targeted session's reply to a delivery; the session names itself in actor.", s.replyMessage},
		{http.MethodGet, "/api/v1/inbox", authHuman, "Open asks waiting on the caller, grouped by whose turn it is; ?project= and ?assignee=me|unassigned|<login> filter (unassigned includes document asks; an unlisted login is 400 ASSIGNEE_NOT_ALLOWED).", s.listInbox},
		{http.MethodGet, "/api/v1/search", authAny, "Full-text search ?q= across issues, documents, asks, and comments.", s.search},
		{http.MethodGet, "/api/v1/agents", authAny, "Live sessions with roles, capabilities, open asks, and last activity.", s.listAgents},
		{http.MethodGet, "/api/v1/agents/{session_id}/messages", authHuman, "A session's targeted messages, newest first.", s.listAgentMessages},
		{http.MethodPost, "/api/v1/agents/{session_id}/messages", authHuman, "Send an issue-less targeted message to a session.", s.createAgentMessage},
		{http.MethodPost, "/api/v1/issues/{key}/asks", authAny, "Open an ask (question or action) on an issue.", s.createAsk},
		{http.MethodGet, "/api/v1/issues/{key}/asks", authAny, "List an issue's asks; ?state= filters.", s.listIssueAsks},
		{http.MethodGet, "/api/v1/asks/open", authAny, "Open asks in one scope, with counts: ?author_session= for one session's own, or ?project= for every open ask on a project's issues and documents; exactly one is required.", s.listOpenAsks},
		{http.MethodGet, "/api/v1/asks/{id}", authAny, "Read one ask with its replies and edit history.", s.getAsk},
		{http.MethodGet, "/api/v1/asks/{id}/followers", authAny, "List the sessions an ask's answer and replies reach.", s.listAskFollowers},
		{http.MethodPut, "/api/v1/asks/{id}/followers/{session_id}", authAny, "Follow an ask (a bearer only for its own session; a human for any).", s.followAsk},
		{http.MethodDelete, "/api/v1/asks/{id}/followers/{session_id}", authAny, "Unfollow an ask (a bearer only for its own session; a human for any).", s.unfollowAsk},
		{http.MethodPatch, "/api/v1/asks/{id}", authAny, "Reword an open ask (its author or a human).", s.editAsk},
		{http.MethodPost, "/api/v1/asks/{id}/answer", authHuman, "Answer an ask with selected options and/or text.", s.answerAsk},
		{http.MethodPost, "/api/v1/asks/{id}/resolve", authAny, "Retract or resolve an ask without answering it.", s.resolveAsk},
		{http.MethodGet, "/api/v1/issues/{key}/comments", authAny, "List an issue's comments; ?artifact= scopes to a document.", s.listComments},
		{http.MethodPost, "/api/v1/issues/{key}/comments", authAny, "Comment on an issue, a document quote, or an ask.", s.createComment},
		{http.MethodPost, "/api/v1/comments/{id}/resolve", authAny, "Resolve a comment thread.", s.resolveComment},
		{http.MethodPost, "/api/v1/comments/{id}/reopen", authHuman, "Reopen a resolved comment thread.", s.reopenComment},
		{http.MethodGet, "/api/v1/comments/{id}", authAny, "Read one comment with its thread.", s.getComment},
		{http.MethodPost, "/api/v1/comments/{id}/accept", authHuman, "Accept a suggestion, applying its replacement.", s.acceptComment},
		{http.MethodPatch, "/api/v1/comments/{id}", authHuman, "Edit a comment's body.", s.editComment},
		{http.MethodPost, "/api/v1/comments/{id}/reject", authHuman, "Reject a suggestion.", s.rejectComment},
		{http.MethodGet, "/api/v1/artifacts/{id}/asks", authAny, "List a document's asks.", s.listArtifactAsks},
		{http.MethodPost, "/api/v1/artifacts/{id}/asks", authAny, "Open an ask on a document.", s.createArtifactAsk},
		{http.MethodGet, "/api/v1/artifacts/{id}/comments", authAny, "List a document's comments.", s.listArtifactComments},
		{http.MethodPost, "/api/v1/artifacts/{id}/comments", authAny, "Comment on a document or one of its quotes.", s.createArtifactComment},
		{http.MethodGet, "/api/v1/artifacts/{id}/events", authAny, "Page a document's event log.", s.listArtifactEvents},
		{http.MethodGet, "/api/v1/artifacts/{id}/references", authAny, "Cross-references to and from a document.", s.getArtifactReferences},
		{http.MethodGet, "/api/v1/artifacts/{id}/subscribers", authHuman, "Sessions subscribed to a document's topics.", s.listArtifactSubscribers},
		{http.MethodDelete, "/api/v1/artifacts/{id}/subscribers/{session_id}", authHuman, "Unsubscribe a session from a document.", s.unsubscribeArtifactSession},
		{http.MethodGet, "/api/v1/artifacts/{id}", authAny, "Read a document's metadata and current version.", s.getArtifact},
		{http.MethodGet, "/api/v1/artifacts/{id}/reviews", authAny, "List a document's approval reviews.", s.listArtifactReviews},
		{http.MethodPost, "/api/v1/artifacts/{id}/reviews", authHuman, "Approve or request changes on a document version.", s.createArtifactReview},
		{http.MethodPost, "/api/v1/artifacts/{id}/approval-requests", authAny, "Open (or return) the approval ask for a document.", s.requestArtifactApproval},
		{http.MethodGet, "/api/v1/artifacts/{id}/blocks", authAny, "A document's blocks with markdown ranges and reference counts.", s.getArtifactBlocks},
		{http.MethodGet, "/api/v1/artifacts/{id}/text", authAny, "A document's canonical markdown.", s.getArtifactText},
		{http.MethodGet, "/api/v1/artifacts/{id}/versions/{number}", authAny, "One named or settled document version.", s.getArtifactVersion},
		{http.MethodPost, "/api/v1/artifacts/{id}/versions", authAny, "Name the current document version.", s.createNamedVersion},
		{http.MethodPost, "/api/v1/artifacts/{id}/edits", authAny, "Apply quote-anchored edit ops to a document.", s.editArtifact},
		{http.MethodGet, "/api/v1/issues/{key}/artifacts/{slug}", authAny, "Read an issue document by slug.", s.getArtifact},
		{http.MethodGet, "/api/v1/issues/{key}/artifacts/{slug}/text", authAny, "An issue document's canonical markdown, by slug.", s.getArtifactText},
		{http.MethodGet, "/api/v1/issues/{key}/artifacts/{slug}/blocks", authAny, "An issue document's blocks, by slug.", s.getArtifactBlocks},
		{http.MethodGet, "/api/v1/issues/{key}/artifacts/{slug}/versions/{number}", authAny, "One version of an issue document, by slug.", s.getArtifactVersion},
		{http.MethodPost, "/api/v1/issues/{key}/artifacts/{slug}/versions", authAny, "Name the current version of an issue document, by slug.", s.createNamedVersion},
		{http.MethodPost, "/api/v1/issues/{key}/artifacts/{slug}/edits", authAny, "Apply edit ops to an issue document, by slug.", s.editArtifact},
		{http.MethodGet, "/api/v1/projects/{key}/artifacts/{slug}", authAny, "Read a project document by slug.", s.getArtifact},
		{http.MethodGet, "/api/v1/projects/{key}/artifacts/{slug}/text", authAny, "A project document's canonical markdown, by slug.", s.getArtifactText},
		{http.MethodGet, "/api/v1/projects/{key}/artifacts/{slug}/versions/{number}", authAny, "One version of a project document, by slug.", s.getArtifactVersion},
		{http.MethodGet, "/api/v1/projects/{key}/artifacts/{slug}/blocks", authAny, "A project document's blocks, by slug.", s.getArtifactBlocks},
		{http.MethodPost, "/api/v1/projects/{key}/artifacts/{slug}/versions", authAny, "Name the current version of a project document, by slug.", s.createNamedVersion},
		{http.MethodPost, "/api/v1/projects/{key}/artifacts/{slug}/edits", authAny, "Apply edit ops to a project document, by slug.", s.editArtifact},
		{http.MethodGet, "/api/v1/me/state", authHuman, "The caller's per-issue read state.", s.getUserState},
		{http.MethodPut, "/api/v1/me/issues/{key}/state", authHuman, "Update the caller's read state for an issue.", s.putUserState},
		{http.MethodGet, "/api/v1/me/agents/state", authHuman, "The caller's per-agent conversation state: each cleared_before cutoff.", s.getUserAgentState},
		{http.MethodPut, "/api/v1/me/agents/{session_id}/state", authHuman, "Clear an agent's conversation for the caller: hide exchanges at or before cleared_before.", s.putUserAgentState},
		{http.MethodGet, "/api/v1/events", authAny, "Server-sent event stream; Last-Event-ID or ?since= resumes.", s.streamEvents},
	}
	if s.deps.TestHooksEnabled {
		routes = append(routes, apiRoute{http.MethodPost, "/api/v1/events/_test/disconnect", authAny, "Test hook: drop every open event stream.", s.disconnectAllStreams})
	}
	return routes
}

// routeIndexEntries renders the table for GET /api/v1, sorted by path then method.
func routeIndexEntries(routes []apiRoute) []routeIndexEntry {
	entries := make([]routeIndexEntry, len(routes))
	for index, route := range routes {
		entries[index] = routeIndexEntry{
			Method:      route.Method,
			Path:        route.Pattern,
			Auth:        route.Auth,
			Description: route.Description,
		}
	}
	sort.Slice(entries, func(left, right int) bool {
		if entries[left].Path != entries[right].Path {
			return entries[left].Path < entries[right].Path
		}
		return entries[left].Method < entries[right].Method
	})
	return entries
}

func (s *server) getRouteIndex(w http.ResponseWriter, _ *http.Request) {
	WriteJSON(w, http.StatusOK, map[string]any{"routes": s.routeIndex, "docs": routeIndexDocs})
}
