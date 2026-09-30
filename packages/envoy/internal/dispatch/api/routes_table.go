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
		{http.MethodGet, "/api/v1/settings/architecture-sources", authHuman, "List every project's architecture source.", s.listArchitectureSources},
		{http.MethodGet, "/api/v1/projects/{key}/architecture-source", authAny, "A project's architecture source, or null when it has none.", s.getArchitectureSource},
		{http.MethodPut, "/api/v1/projects/{key}/architecture-source", authHuman, "Set a project's architecture source {repo, branch} after proving the GitHub App can read it.", s.putArchitectureSource},
		{http.MethodDelete, "/api/v1/projects/{key}/architecture-source", authHuman, "Remove a project's architecture source.", s.deleteArchitectureSource},
		{http.MethodPost, "/api/v1/projects/{key}/architecture-source/sync", authAny, "Import a project's architecture model from its source now; 200 carries the updated row (last_error set when the import failed), 409 SOURCE_ACCESS a credential or branch problem.", s.syncArchitectureSource},
		{http.MethodGet, "/api/v1/projects/{key}/architecture", authAny, "A project's component tree with the work attached: per-component counts and issue rows, plus unassigned, not-architectural, and retired-link issues; 404 SOURCE_NOT_FOUND without a source.", s.getArchitectureTree},
		{http.MethodGet, "/api/v1/me/agent-tokens", authHuman, "List the caller's personal agent tokens.", s.listAgentTokens},
		{http.MethodPost, "/api/v1/me/agent-tokens", authHuman, "Mint a personal agent token.", s.createAgentToken},
		{http.MethodDelete, "/api/v1/me/agent-tokens/{id}", authHuman, "Revoke a personal agent token.", s.revokeAgentToken},
		{http.MethodGet, "/api/v1/users", authHuman, "The humans who may sign in; the assignee picker's options.", s.listUsers},
		{http.MethodGet, "/api/v1/whoami", authAny, "Who the server takes the caller for: {kind: user, login} or {kind: agent, owner, service} (owner is a personal token's lowercase login, null for the shared token; service is a verified service-account token's Kubernetes subject, null otherwise).", s.whoami},
		{http.MethodGet, "/api/v1/issues", authAny, "List issues, unpaginated: every matching issue, and limit, offset or cursor is 400 INVALID_QUERY (the dispatch_issues tool pages). Filters project, status, parent, label, priority (repeatable: 0-3, or none for unset), open, updated_since, route_status (live, no_holder or unknown; open issues only); ?pinned=true is human-only.", s.listIssues},
		{http.MethodPost, "/api/v1/issues", authAny, "Create an issue (native, or from a GitHub owner/repo#n ref); assignee defaults to the creating human, the personal token's owner, or the parent's assignee.", s.createIssue},
		{http.MethodGet, "/api/v1/issues/resolve", authAny, "Resolve ?ref=<KEY | owner/repo#n> to an issue key.", s.resolveIssue},
		{http.MethodGet, "/api/v1/issues/{key}", authAny, "Read an issue with its open asks and primary document.", s.getIssue},
		{http.MethodPatch, "/api/v1/issues/{key}", authAny, "Update title, status, priority, labels, route, parent, rank, assignee (an allowlisted login or null; anyone may reassign), or components ({mode: inherit|explicit|none, ids, reason}; allowed on a closed issue).", s.patchIssue},
		{http.MethodPost, "/api/v1/issues/{key}/claim", authAny, "Claim the issue for the calling session (or human): records who is working it. The status does not move. 409 ISSUE_CLAIMED names a live holder (or a human one, whose claim has no session to end); a human may pass {force: true} to take it anyway. 409 CLAIM_CONTENDED means the holder changed twice while the request ran, so nothing was applied and nobody's liveness was checked: read the issue and retry.", s.claimIssue},
		{http.MethodDelete, "/api/v1/issues/{key}/claim", authAny, "Release the issue's claim: its holder, any human, or anyone when the holding session is no longer live. The status does not move. 409 ISSUE_CLAIMED names the holder this caller may not release (there is no force on this route); 409 CLAIM_CONTENDED means the holder changed twice while the request ran, so nothing was applied and nobody's liveness was checked.", s.releaseIssueClaim},
		{http.MethodGet, "/api/v1/issues/{key}/events", authAny, "Page an issue's event log (after, before, ids, order, limit).", s.listIssueEvents},
		{http.MethodGet, "/api/v1/issues/{key}/references", authAny, "Cross-references to and from an issue.", s.getIssueReferences},
		{http.MethodGet, "/api/v1/references", authAny, "Edges of one node in the reference graph: ?to=<dispatch ref> backlinks or ?from= links; ?kind=, ?since=<events.id> (mentions only).", s.getReferences},
		{http.MethodGet, "/api/v1/issues/{key}/subscribers", authHuman, "Sessions subscribed to an issue's topics.", s.listIssueSubscribers},
		{http.MethodDelete, "/api/v1/issues/{key}/subscribers/{session_id}", authHuman, "Unsubscribe a session from an issue.", s.unsubscribeIssueSession},
		{http.MethodGet, "/api/v1/issues/{key}/artifacts", authAny, "List an issue's documents and files.", s.listArtifacts},
		{http.MethodPost, "/api/v1/issues/{key}/artifacts", authAny, "Upload an issue document or file (JSON or multipart).", s.uploadArtifact},
		{http.MethodPost, "/api/v1/issues/{key}/messages", authAny, "Post an issue message; target and delivery address a session or role.", s.createMessage},
		{http.MethodGet, "/api/v1/issues/{key}/messages/{id}", authAny, "Read one message with its deliveries and replies.", s.getMessage},
		{http.MethodGet, "/api/v1/messages/{id}", authAny, "Read the conversation a message belongs to by any message id in it, issue-less or not: the thread root with its deliveries and every reply, oldest first. An issue thread follows the issue's rule. On a direct (issue-less) thread a bearer names its session in ?session= and reads only one whose root targets that session or that it replied in (403 THREAD_FORBIDDEN), a guard against reading another session's conversation by mistake, not an authorization boundary. Direct-conversation text also reaches every authenticated caller through GET /api/v1/events; nothing in Dispatch restricts it by session.", s.getMessageThread},
		{http.MethodPost, "/api/v1/messages/{id}/deliveries", authAny, "Retry delivering a targeted message.", s.createDelivery},
		{http.MethodPost, "/api/v1/messages/{id}/reply", authBearer, "The targeted session's reply to a delivery; the session names itself in actor. Once the attempt is answered, ?follow_up=true posts other text as the session's follow-up, threaded under its first reply; with follow_up=false or absent, or with text the session already posted there, nothing is posted and the stored message comes back marked duplicate. Any other follow_up is 400 MESSAGE_INPUT.", s.replyMessage},
		{http.MethodGet, "/api/v1/inbox", authHuman, "Open asks waiting on the caller, grouped by whose turn it is; ?project= and ?assignee=me|unassigned|<login> filter (unassigned includes document asks; an unlisted login is 400 ASSIGNEE_NOT_ALLOWED).", s.listInbox},
		{http.MethodGet, "/api/v1/search", authAny, "Full-text search ?q= across issues, documents, asks, and comments.", s.search},
		{http.MethodGet, "/api/v1/agents", authAny, "Live sessions with roles, capabilities, open asks, and last activity.", s.listAgents},
		{http.MethodGet, "/api/v1/agents/{session_id}/messages", authHuman, "A session's targeted messages, newest first.", s.listAgentMessages},
		{http.MethodPost, "/api/v1/agents/{session_id}/messages", authHuman, "Send an issue-less targeted message to a session.", s.createAgentMessage},
		{http.MethodGet, "/api/v1/agents/{session_id}/stream", authHuman, "Server-sent stream of a live session's own conversation, relayed from the session itself: an SSE `replay` event with what the session can still replay, then a `frame` event per turn, tool call, and streamed update. Nothing is stored.", s.streamAgentConversation},
		{http.MethodPost, "/api/v1/broadcasts", authHuman, "Send one message to many sessions: one targeted message per recipient under a shared broadcast. A selected session that is not live or does not advertise the mode is excluded and named in the response, never switched to another mode.", s.createBroadcast},
		{http.MethodGet, "/api/v1/broadcasts", authHuman, "Recent broadcasts, newest first, with their recipient and reply counts.", s.listBroadcasts},
		{http.MethodGet, "/api/v1/broadcasts/{id}", authHuman, "One broadcast with every recipient's delivery attempts and replies, recipients in the order the send named them; a broadcast from before that order was stored falls back to created_at, id.", s.getBroadcast},
		{http.MethodPost, "/api/v1/issues/{key}/asks", authAny, "Open a question ask on an issue (options optional).", s.createAsk},
		{http.MethodGet, "/api/v1/issues/{key}/asks", authAny, "List an issue's asks; ?state= filters.", s.listIssueAsks},
		{http.MethodGet, "/api/v1/asks/open", authAny, "Open asks in one scope, with counts: ?author_session= for one session's own, or ?project= for every open ask on a project's issues and documents; exactly one is required.", s.listOpenAsks},
		{http.MethodGet, "/api/v1/asks/{id}", authAny, "Read one ask with its replies and edit history.", s.getAsk},
		{http.MethodGet, "/api/v1/asks/{id}/followers", authAny, "List the sessions an ask's answer and replies reach.", s.listAskFollowers},
		{http.MethodPut, "/api/v1/asks/{id}/followers/{session_id}", authAny, "Follow an ask (a bearer only for its own session; a human for any).", s.followAsk},
		{http.MethodDelete, "/api/v1/asks/{id}/followers/{session_id}", authAny, "Unfollow an ask (a bearer only for its own session; a human for any).", s.unfollowAsk},
		{http.MethodPatch, "/api/v1/asks/{id}", authAny, "Reword an open ask (its author or a human); a block ask's `:::ask` block is rewritten with it.", s.editAsk},
		{http.MethodPost, "/api/v1/asks/{id}/answer", authHuman, "Answer an ask with selected options and/or text.", s.answerAsk},
		{http.MethodPost, "/api/v1/asks/{id}/resolve", authAny, "Retract or resolve an ask without answering it; a block ask's block records it too.", s.resolveAsk},
		{http.MethodGet, "/api/v1/issues/{key}/comments", authAny, "List an issue's comments; ?artifact= scopes to a document.", s.listComments},
		{http.MethodPost, "/api/v1/issues/{key}/comments", authAny, "Comment on an issue, a document quote, or an ask.", s.createComment},
		{http.MethodPost, "/api/v1/comments/{id}/resolve", authAny, "Resolve a comment thread.", s.resolveComment},
		{http.MethodPost, "/api/v1/comments/{id}/reopen", authHuman, "Reopen a resolved comment thread.", s.reopenComment},
		{http.MethodGet, "/api/v1/comments/{id}", authAny, "Read one comment with its thread.", s.getComment},
		{http.MethodPost, "/api/v1/comments/{id}/deliveries", authAny, "Retry a comment mention delivery; target is required when it has multiple mentions.", s.createCommentDelivery},
		{http.MethodPost, "/api/v1/comments/{id}/reply", authBearer, "The mentioned session's reply to a delivery; the session names itself in actor.", s.replyComment},
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
		{http.MethodPost, "/api/v1/artifacts/{id}/reviews", authHuman, "Approve or request changes on a document's latest settled version; answers the approval ask open at that version, and retracts one naming an older version.", s.createArtifactReview},
		{http.MethodPost, "/api/v1/artifacts/{id}/approval-requests", authAny, "Open the approval ask for a document's latest version, with an optional summary; a repeated request returns the ask open at that version and replaces a stale one, which names an older version.", s.requestArtifactApproval},
		{http.MethodGet, "/api/v1/artifacts/{id}/blocks", authAny, "A document's blocks with markdown ranges, tokens, and reference counts.", s.getArtifactBlocks},
		{http.MethodGet, "/api/v1/artifacts/{id}/text", authAny, "A document's canonical markdown and whole-document token.", s.getArtifactText},
		{http.MethodGet, "/api/v1/artifacts/{id}/versions/{number}", authAny, "One named or settled document version.", s.getArtifactVersion},
		{http.MethodPost, "/api/v1/artifacts/{id}/versions", authAny, "Name the current document version.", s.createNamedVersion},
		{http.MethodPost, "/api/v1/artifacts/{id}/edits", authAny, "Apply quote-anchored edit ops with an optional optimistic-concurrency precondition.", s.editArtifact},
		{http.MethodGet, "/api/v1/issues/{key}/artifacts/{slug}", authAny, "Read an issue document by slug or unambiguous filename.", s.getArtifact},
		{http.MethodGet, "/api/v1/issues/{key}/artifacts/{slug}/text", authAny, "An issue document's canonical markdown and whole-document token, by slug or unambiguous filename.", s.getArtifactText},
		{http.MethodGet, "/api/v1/issues/{key}/artifacts/{slug}/blocks", authAny, "An issue document's blocks and tokens, by slug or unambiguous filename.", s.getArtifactBlocks},
		{http.MethodGet, "/api/v1/issues/{key}/artifacts/{slug}/versions/{number}", authAny, "One version of an issue document, by slug or unambiguous filename.", s.getArtifactVersion},
		{http.MethodPost, "/api/v1/issues/{key}/artifacts/{slug}/versions", authAny, "Name the current version of an issue document, by slug or unambiguous filename.", s.createNamedVersion},
		{http.MethodPost, "/api/v1/issues/{key}/artifacts/{slug}/edits", authAny, "Apply edit ops with an optional optimistic-concurrency precondition to an issue document, by slug or unambiguous filename.", s.editArtifact},
		{http.MethodGet, "/api/v1/projects/{key}/artifacts/{slug}", authAny, "Read a project document by slug or unambiguous filename.", s.getArtifact},
		{http.MethodGet, "/api/v1/projects/{key}/artifacts/{slug}/text", authAny, "A project document's canonical markdown and whole-document token, by slug or unambiguous filename.", s.getArtifactText},
		{http.MethodGet, "/api/v1/projects/{key}/artifacts/{slug}/versions/{number}", authAny, "One version of a project document, by slug or unambiguous filename.", s.getArtifactVersion},
		{http.MethodGet, "/api/v1/projects/{key}/artifacts/{slug}/blocks", authAny, "A project document's blocks and tokens, by slug or unambiguous filename.", s.getArtifactBlocks},
		{http.MethodPost, "/api/v1/projects/{key}/artifacts/{slug}/versions", authAny, "Name the current version of a project document, by slug or unambiguous filename.", s.createNamedVersion},
		{http.MethodPost, "/api/v1/projects/{key}/artifacts/{slug}/edits", authAny, "Apply edit ops with an optional optimistic-concurrency precondition to a project document, by slug or unambiguous filename.", s.editArtifact},
		{http.MethodGet, "/api/v1/me/state", authHuman, "The caller's per-issue read state.", s.getUserState},
		{http.MethodPut, "/api/v1/me/issues/{key}/state", authHuman, "Update the caller's read state for an issue.", s.putUserState},
		{http.MethodGet, "/api/v1/me/agents/state", authHuman, "The caller's per-agent conversation state: cleared_before, read_through, and unread_replies (the session's replies to the caller's direct messages newer than both).", s.getUserAgentState},
		{http.MethodPut, "/api/v1/me/agents/{session_id}/state", authHuman, "Clear an agent's conversation for the caller (cleared_before hides exchanges at or before it) and/or mark it read (read_through only moves forward); answers the session's state.", s.putUserAgentState},
		{http.MethodPut, "/api/v1/me/asks/{id}/snooze", authHuman, "Snooze an inbox row for the caller until snoozed_until.", s.putAskSnooze},
		{http.MethodDelete, "/api/v1/me/asks/{id}/snooze", authHuman, "Un-snooze an inbox row for the caller.", s.deleteAskSnooze},
		{http.MethodGet, "/api/v1/events", authAny, "Server-sent event stream; Last-Event-ID or ?since= resumes.", s.streamEvents},
		{http.MethodGet, "/api/v1/credential-requests", authHuman, "List credential requests pending the caller's own decision (?approver=me only); 404 FEATURE_OFF without a configured secrets broker.", s.listCredentialPending},
		{http.MethodGet, "/api/v1/credential-requests/{id}", authHuman, "Read one credential request's facts and, while pending, its WebAuthn challenges; the broker is authoritative.", s.getCredentialRecord},
		{http.MethodPost, "/api/v1/credential-requests/{id}/approve", authHuman, "Approve a credential request with a WebAuthn assertion; relayed verbatim, the broker decides.", s.approveCredentialRecord},
		{http.MethodPost, "/api/v1/credential-requests/{id}/deny", authHuman, "Deny a credential request with a WebAuthn assertion; relayed verbatim, the broker decides.", s.denyCredentialRecord},
		{http.MethodPost, "/api/v1/credential-requests/machine-lookup", authHuman, "Resolve a pending machine login by its typed confirmation code, returning its facts and challenges.", s.lookupMachineCredential},
		{http.MethodGet, "/api/v1/credential-keys/{login}", authHuman, "List a login's registered approver keys.", s.getCredentialKeys},
		{http.MethodPost, "/api/v1/credential-keys/{login}/{kind}/{step}", authHuman, "Drive one step of a key ceremony (kind register|endorse, step begin|finish; anything else 404).", s.credentialKeyCeremony},
		{http.MethodGet, "/api/v1/credential-grants", authHuman, "List credential grants the caller may revoke (?approver=me only).", s.listCredentialGrants},
		{http.MethodPost, "/api/v1/credential-grants/{id}/revoke", authHuman, "Revoke a credential grant with a WebAuthn assertion.", s.revokeCredentialGrant},
	}
	if s.deps.TestHooksEnabled {
		routes = append(routes,
			apiRoute{http.MethodPost, "/api/v1/events/_test/disconnect", authAny, "Test hook: drop every open event stream.", s.disconnectAllStreams},
			apiRoute{http.MethodPost, "/api/v1/artifacts/_test/quiesce", authAny, "Test hook: close every live document and finish the settlements in flight.", s.quiesceDocuments},
			apiRoute{http.MethodPost, "/api/v1/agents/{session_id}/stream/_test/publish", authHuman, "Test hook: publish one frame to a session's conversation viewers.", s.publishAgentStreamFrame},
		)
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
