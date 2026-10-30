package api

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"time"

	"github.com/sjawhar/envoy/internal/contracts"
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

// routeRow is one row of the route table: what a route is. Register mounts the table and GET
// /api/v1 lists it, so the two can never disagree about what the server serves.
type routeRow struct {
	Method      string
	Pattern     string
	Auth        routeAuth
	Description string
	Handler     http.HandlerFunc
}

// apiRoute is one mounted route: its row, and how register serves it (serveRoute).
type apiRoute struct {
	routeRow
	// TestHook marks a route TestHooksEnabled mounts: a test's own route is neither admitted nor
	// bounded by a deadline.
	TestHook bool
	// Deadline replaces the server's read deadline (defaultReadDeadline) for a read whose work is
	// bounded by the bytes it moves rather than by the database. Only a read has a deadline, so
	// only a read's row may set one (mountedRoutes).
	Deadline time.Duration
	// Weight is how many of its class's admission units a request takes (admission.Limiter): the
	// most request-pool connections it holds at once. Zero is one.
	Weight int
	// BodyLimit is the most bytes the route reads of a request body, which is also how much the
	// keyed layer spools and how much of an early-answered request's body the server drains before
	// it answers (routeBodyLimit, drainBeforeAnswer). Zero is maxJSONRequestBytes, every JSON
	// route's limit.
	BodyLimit int64
}

// pinsConnection reports whether a request of the route runs every request-pool statement on one
// connection, the one its first statement takes, held until its handler returns
// (store.PinConnection): an issue read ran twelve statements, a page of its comments four, and
// each waited for a connection of its own. Every admitted read pins but two kinds:
//   - a read of weight two (concurrentReadWeight), whose reads run side by side, each on a
//     connection of its own (store.ForConcurrentRead), which one pinned connection cannot serve;
//   - a read with a Deadline of its own, a file version download, which moves the file's bytes
//     from the file store for minutes after its last statement, holding no connection meanwhile.
//
// A write never pins: it waits for its documents' writer gates between statements, which a caller
// holding a connection is refused (docs.ErrGateWhileHoldingConnection).
func (r apiRoute) pinsConnection() bool {
	return r.Method == http.MethodGet && !r.TestHook && r.Weight <= 1 && r.Deadline == 0
}

// downloadDeadline is the deadline of the four file version downloads, which move a file up to
// files.MaxObjectSize over whatever link the client has: five minutes is what such a transfer is
// given before the server gives up on it, where a read of the database gets defaultReadDeadline.
const downloadDeadline = 5 * time.Minute

// concurrentReadWeight is the weight of a route that runs its reads side by side, at most two at
// once (readDeliveryRows, readPipelinePopulation): two request-pool connections.
const concurrentReadWeight = 2

// routeServings names the rows register serves other than as an ordinary request, by method and
// pattern. routes() refuses an entry that names no row, so a renamed route cannot silently lose
// its serving.
var routeServings = map[string]apiRoute{
	"POST /api/v1/projects/{key}/artifacts":      {BodyLimit: maxUploadRequestBytes},
	"POST /api/v1/issues/{key}/artifacts":        {BodyLimit: maxUploadRequestBytes},
	"POST /api/v1/agents/{session_id}/artifacts": {BodyLimit: maxUploadRequestBytes},

	"GET /api/v1/artifacts/{id}/versions/{number}":                       {Deadline: downloadDeadline},
	"GET /api/v1/issues/{key}/artifacts/{slug}/versions/{number}":        {Deadline: downloadDeadline},
	"GET /api/v1/projects/{key}/artifacts/{slug}/versions/{number}":      {Deadline: downloadDeadline},
	"GET /api/v1/agents/{session_id}/artifacts/{slug}/versions/{number}": {Deadline: downloadDeadline},

	// The related-item check runs its keyword read and its meaning read side by side, each on a
	// connection of its own, so the GET form cannot run on one pinned connection. The POST form
	// is a write, which never pins, and takes the same two connections at once.
	"GET /api/v1/projects/{key}/related":  {Weight: concurrentReadWeight},
	"POST /api/v1/projects/{key}/related": {Weight: concurrentReadWeight},

	"GET /api/v1/delivery/timeline": {Weight: concurrentReadWeight},
	"GET /api/v1/delivery/measures": {Weight: concurrentReadWeight},
	"GET /api/v1/delivery/pipeline": {Weight: concurrentReadWeight},
}

// mountedRoutes is rows as register serves them: each with its routeServings entry, and each
// test hook marked TestHook.
func mountedRoutes(rows, testHooks []routeRow) []apiRoute {
	routes := make([]apiRoute, 0, len(rows)+len(testHooks))
	served := 0
	for _, row := range rows {
		route, found := routeServings[row.Method+" "+row.Pattern]
		if found {
			served++
		}
		if route.Deadline != 0 && row.Method != http.MethodGet {
			panic(fmt.Sprintf("routeServings gives %s %s a deadline, and only a read has one", row.Method, row.Pattern))
		}
		route.routeRow = row
		routes = append(routes, route)
	}
	if served != len(routeServings) {
		panic(fmt.Sprintf("routeServings names %d routes, and the table mounts %d of them", len(routeServings), served))
	}
	for _, row := range testHooks {
		routes = append(routes, apiRoute{routeRow: row, TestHook: true})
	}
	return routes
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
	rows := []routeRow{
		{http.MethodGet, "/api/v1", authPublic, "This index: every route with its method, auth, and purpose.", s.getRouteIndex},
		{http.MethodGet, "/api/v1/schema/blocks", authPublic, "The typed document block schema as JSON.", s.getBlockSchema},
		{http.MethodGet, "/api/v1/projects", authAny, "List projects.", s.listProjects},
		{http.MethodPost, "/api/v1/projects", authAny, fmt.Sprintf("Create a project {key, name}: any signed-in person or agent session; a bearer names its session in actor. A key that is not a project key, an empty name, a name holding a control character or a bidi embedding, override or isolate, or a name with no visible character (a letter, number, punctuation or symbol other than the blank fillers U+115F, U+1160, U+3164, U+FFA0 and U+2800, or a code point Go's Unicode tables leave unassigned, such as an emoji newer than them, other than a noncharacter (U+FDD0-U+FDEF and each code point ending in FFFE or FFFF) or a reserved default-ignorable (U+2065, U+FFF0-U+FFF8, U+E0000-U+E0FFF); combining marks, format characters, private-use characters and whitespace don't count) is 400 INVALID_PROJECT; a name over %d characters is 400 CAP_EXCEEDED; a key that already exists is 409 PROJECT_EXISTS.", contracts.ProjectNameMax), s.createProject},
		{http.MethodGet, "/api/v1/projects/{key}/artifacts", authAny, "List a project's documents.", s.listProjectArtifacts},
		{http.MethodPost, "/api/v1/projects/{key}/artifacts", authAny, "Upload a project document or file (JSON or multipart); a file's kind is read from its bytes, and a new artifact's name held to its bound, as an issue upload's are.", s.uploadProjectArtifact},
		{http.MethodGet, "/api/v1/projects/{key}/related", authAny, fmt.Sprintf("Run the related-item check an issue create runs, over ?title= and ?text= (checked as the title, a newline and the text; either alone is enough), without writing anything: no issue, ask, suggestion or related_checks record. ?exclude_issue= leaves that issue of the project and everything it holds out of the results and out of the word statistics (400 EXCLUDE_ISSUE_INPUT when it is not an issue of the project); ?keywords_only=true skips meaning search. It waits at most 5 seconds and answers the create's advice.suggestions shape: outcome found, none or not_made (a blank text is 200 not_made, the reason in missing), search, search_note, search_query, related and decision. Title and text over %d characters together are 400 CAP_EXCEEDED; POST takes a longer text. An unknown project is 404 PROJECT_NOT_FOUND.", contracts.SearchQueryMax), s.getRelated},
		{http.MethodPost, "/api/v1/projects/{key}/related", authAny, fmt.Sprintf("The GET form of this route with its fields in a JSON body {title, text, exclude_issue, keywords_only}: the related-item check over a title and a text as long as an issue's spec (the body's 1 MiB bound; a title over %d characters is 400 CAP_EXCEEDED), writing nothing and waiting at most 5 seconds; answers the same suggestions shape.", contracts.IssueTitleMax), s.postRelated},
		{http.MethodGet, "/api/v1/settings/repo-projects", authAny, "List GitHub repository to project mappings, each marked whether it is its project's default.", s.listRepoProjects},
		{http.MethodPut, "/api/v1/settings/repo-projects/{owner}/{repo}", authAny, "Map a GitHub repository to a project {project, default?}; default true makes it the project's one default.", s.putRepoProject},
		{http.MethodDelete, "/api/v1/settings/repo-projects/{owner}/{repo}", authAny, "Remove a repository mapping no issue names as its repo.", s.deleteRepoProject},
		{http.MethodGet, "/api/v1/settings/architecture-sources", authAny, "List every project's architecture sources, by project and then in the order each project added them.", s.listArchitectureSources},
		{http.MethodGet, "/api/v1/projects/{key}/architecture-sources", authAny, "A project's architecture sources, one per repository, in the order they were added; [] when it has none. The project's component model is every source's components together.", s.listProjectArchitectureSources},
		{http.MethodGet, "/api/v1/projects/{key}/architecture-source", authAny, "Kept for dispatch clients older than this release, logged at warn: the project's first architecture source, or null when it has none. GET /api/v1/projects/{key}/architecture-sources replaces it.", s.getArchitectureSource},
		{http.MethodPut, "/api/v1/projects/{key}/architecture-sources/{owner}/{repo}", authAny, "Add a repository as an architecture source of a project, or change the branch of the source it is {branch}, after proving the GitHub App can read it and that branch is the name of a branch it has; a missing branch, a commit SHA, a relative ref (main~1, HEAD) or a refs/ path is 409 SOURCE_ACCESS naming the value and the repository, as a credential problem is, and nothing is written. A re-save resets the source's sync bookkeeping and keeps its components.", s.putArchitectureSource},
		{http.MethodDelete, "/api/v1/projects/{key}/architecture-sources/{owner}/{repo}", authAny, "Remove one architecture source of a project and the components it projected; every other source's components stay.", s.deleteArchitectureSource},
		{http.MethodPost, "/api/v1/projects/{key}/architecture-sources/{owner}/{repo}/sync", authAny, "Import one architecture source's model now; 200 carries its updated row (last_error set when the import failed), 409 SOURCE_ACCESS a credential or branch problem.", s.syncArchitectureSource},
		{http.MethodPost, "/api/v1/projects/{key}/architecture-sources/sync", authAny, "Import every architecture source of a project now, each whatever another's import did; 200 carries every updated row in the order the sources were added, each with its own last_error (an access failure included).", s.syncArchitectureSources},
		{http.MethodPost, "/api/v1/projects/{key}/architecture-source/sync", authAny, "Kept for dispatch clients older than this release, logged at warn: import every architecture source of a project now and answer one row, the first in id order whose last_error is set or else the first; 404 SOURCE_NOT_FOUND without a source. POST /api/v1/projects/{key}/architecture-sources/sync replaces it.", s.syncSingularArchitectureSource},
		{http.MethodGet, "/api/v1/projects/{key}/architecture", authAny, "A project's component tree with the work attached: its sources' sync bookkeeping, each component with its source repository, per-component counts and issue rows, plus unassigned, not-architectural, and retired-link issues; 404 SOURCE_NOT_FOUND without a source.", s.getArchitectureTree},
		{http.MethodGet, "/api/v1/settings/project-default-assignees", authHuman, "List every project that names a default assignee, by project key: the last step of a create's assignee chain. Human-only, as the default names a person.", s.listProjectDefaultAssignees},
		{http.MethodPut, "/api/v1/projects/{key}/default-assignee", authHuman, "Set a project's default assignee {assignee} (the email of a person who has signed in, or null to clear it): who an issue created with no assignee, by nobody, under no parent or under a parent with nobody on it, goes to. Human-only, as the default names a person; no agent surface sets it.", s.putProjectDefaultAssignee},
		{http.MethodGet, "/api/v1/settings/delivery", authAny, "The delivery timeline's one configuration record (deploy repository, workflow paths, production job names, optional production apply step, population authors, excluded repositories, and the Pipeline page's optional staging end-to-end, staging deploy and PR-checks roll-up job names), or null when it has not been set.", s.getDeliverySettings},
		{http.MethodPut, "/api/v1/settings/delivery", authAny, "Set the delivery timeline's configuration after proving the GitHub App can read the deploy repository; 409 DELIVERY_SETTINGS_ACCESS a credential or repository problem.", s.putDeliverySettings},
		{http.MethodGet, "/api/v1/delivery/timeline", authAny, "The delivery timeline within [from, to) (default the last 7 days), the search q (title or PR id) and the given facets (repeatable repo, parent_agent, session, issue, priority (P0-P3), component (a parent includes its children), author, rework, deployed; __no_issue__, __no_priority__, __no_component__ and __no_session__ select by cause): merges with each PR's issue title, priority and effective components, successful production deploys with every window PR they shipped first, pipeline failures with each run's production job and failing step, and merged work waiting to deploy, each PR's deployed_status (deployed, waiting, not_tracked) computed from stored facts; facet_counts (each facet counted with every other facet applied and its own ignored), color_counts (with none applied), the components and issue titles the window names, data freshness, and measures (what GET /api/v1/delivery/measures answers for the same window, search and facets); 404 DELIVERY_NOT_CONFIGURED without a delivery_settings row.", s.getDeliveryTimeline},
		{http.MethodGet, "/api/v1/delivery/runs/{id}", authAny, "One deploy-repository run with every job it ran, by start, each with its result and timings; 400 RUN_ID_INPUT for an id that is not a positive integer, 404 NOT_FOUND for no such run, 404 DELIVERY_NOT_CONFIGURED without a delivery_settings row.", s.getDeliveryRun},
		{http.MethodGet, "/api/v1/delivery/measures", authAny, "The four delivery measures and Sami's KPI targets for [from, to) (default the last 7 days) and the same facets the timeline takes: deploys a day, lead time (merge, first commit and open to production; open to merge), change failure rate per PR and per deploy (flags_source says whether stored flags fed it), time to restore, rework share, deploy-run success, a per-UTC-day series, the open P0 issues with no owner, and whether each target is met; 404 DELIVERY_NOT_CONFIGURED without a delivery_settings row.", s.getDeliveryMeasures},
		{http.MethodGet, "/api/v1/delivery/pipeline", authAny, "The Pipeline page: over the seven UTC days ending today (now, or ?at= for a fixed day), by the day each run started, the PR-checks workflow's run wall time and success, its five slowest jobs (50 timed results to rank) and the jobs that failed in the last 24 hours, the duration and result of the staging end-to-end, staging deploy and production deploy jobs the settings name in push runs on main, and merge-to-production and PR-open-to-merge per merge day against their targets; not filtered by the timeline's window or facets; 400 INVALID_QUERY for a malformed at, 404 DELIVERY_NOT_CONFIGURED without a delivery_settings row.", s.getDeliveryPipeline},
		{http.MethodPost, "/api/v1/delivery/session-groups", authAny, "Create a Delivery session group {name, sessions}: the group becomes the Parent agent of every pull request whose first session it holds; 409 SESSION_IN_GROUP for a session another group holds, 409 SESSION_GROUP_NAME_TAKEN for a name another group has, 409 SESSION_GROUP_NAME_IS_SESSION_TITLE for a name that is the title of a session outside the group. The timeline lists every group (session_groups).", s.createDeliverySessionGroup},
		{http.MethodPatch, "/api/v1/delivery/session-groups/{id}", authAny, "Rename a Delivery session group, add sessions to it and remove sessions from it {name?, add?, remove?}, with the create route's refusals; 400 SESSION_GROUP_INPUT for an edit that would leave it empty.", s.updateDeliverySessionGroup},
		{http.MethodDelete, "/api/v1/delivery/session-groups/{id}", authAny, "Delete a Delivery session group; its sessions show as themselves again.", s.deleteDeliverySessionGroup},
		{http.MethodGet, "/api/v1/me/agent-tokens", authHuman, "List the caller's personal agent tokens.", s.listAgentTokens},
		{http.MethodPost, "/api/v1/me/agent-tokens", authHuman, "Mint a personal agent token.", s.createAgentToken},
		{http.MethodDelete, "/api/v1/me/agent-tokens/{id}", authHuman, "Revoke a personal agent token.", s.revokeAgentToken},
		{http.MethodGet, "/api/v1/users", authHuman, "Everyone who has signed in, by lowercase email: the assignee picker's options.", s.listUsers},
		{http.MethodGet, "/api/v1/whoami", authAny, "Who the server takes the caller for: {kind: user, login} (login is the person's lowercase email) or {kind: agent, owner, service} (owner is a personal token's owner by lowercase email, null for the shared token; service is a verified service-account token's Kubernetes subject, null otherwise).", s.whoami},
		{http.MethodGet, "/api/v1/dashboard/builds", authAny, "Which dashboard builds are served: {serving, live}, serving the entry chunk (/assets/index-*.js) of the build this process serves (empty when none), live the distinct entry chunks of the builds every live server process serves, sorted. A tab offers its reload notice only when live holds exactly one entry and it is not the tab's own.", s.getDashboardBuilds},
		{http.MethodGet, "/api/v1/issues", authAny, fmt.Sprintf("List issues; filters project, status, parent, label, priority (repeatable: 0-3, or none for unset), open, updated_since, route_status (live, no_holder or unknown; open issues only); ?pinned=true is human-only. Each issue carries progress: {tasks, children}, its spec's task-list items and its direct children as {done, total}, each null when it has none. Pages with limit (1-%d) and offset (0 or more; alone it pages %d): a paged answer is {issues, total, limit, offset}, total counting every issue the filters match; without either it is every matching issue as an array. cursor is 400 INVALID_QUERY.", contracts.MaxIssuePageLimit, contracts.DefaultIssuePageLimit), s.listIssues},
		{http.MethodPost, "/api/v1/issues", authAny, fmt.Sprintf("Create an issue (native, or from a GitHub owner/repo#n ref); assignee defaults to the creating person, the personal token's owner, the parent's assignee, or the project's default assignee when that still names a person who has signed in. A title over %d characters is 400 CAP_EXCEEDED. After the write commits, a related-item check searches the project by meaning and by its rarest words and waits at most 5 seconds; the response's advice.suggestions says whether it found related items (outcome found, none or not_made with the reason in missing), how it searched (search, search_note), the three items most like the new issue, one per owner, with each ask item's answer state, any answered ask that settles it, words to search with by hand (search_query), and under force the title overlaps forced past (title_matches).", contracts.IssueTitleMax), s.createIssue},
		{http.MethodGet, "/api/v1/issues/resolve", authAny, "Resolve ?ref=<KEY | owner/repo#n> to an issue key.", s.resolveIssue},
		{http.MethodGet, "/api/v1/issues/{key}", authAny, "Read an issue with its open asks, primary document, children, and progress: {tasks, children}, its spec's task-list items (`- [ ]` / `- [x]`, nested lists included) and its direct children as {done, total}, each null when it has none.", s.getIssue},
		{http.MethodGet, "/api/v1/issues/{key}/summary", authAny, "What a reference to an issue shows, read without the full issue: {key, project, title, status, documents}, documents every artifact of the issue in the order the issue read lists them, each {id, slug, name, kind, latest_version, primary}, primary whether the document is the issue's spec; 404 NOT_FOUND for no such issue.", s.getIssueSummary},
		{http.MethodPatch, "/api/v1/issues/{key}", authAny, fmt.Sprintf("Update title (over %d characters is 400 CAP_EXCEEDED), status, priority, labels (or labels_add, which adds to the labels the issue holds when the write commits rather than replacing them; not with labels), route, parent, rank, assignee (the email of a person who has signed in, or null; anyone may reassign), or components ({mode: inherit|explicit|none, ids, reason}; allowed on a closed issue). Closing the issue (status done) withdraws every open ask it holds, each recorded resolved with resolution kind withdrawn naming the closer; reopening it opens exactly those asks again. expected_sequence makes the write conditional: 409 ISSUE_CHANGED when the issue's last event is no longer that sequence.", contracts.IssueTitleMax), s.patchIssue},
		{http.MethodPost, "/api/v1/issues/{key}/claim", authAny, "Claim the issue for the calling session (or human): records who is working it. The status does not move. The answer is the issue with legion_managed_status beside it: true when the issue carries the legion label in any case, or sits under one that does, which means the Legion daemon owns the status and a status another writer sets is undone. 409 ISSUE_CLAIMED names a live holder (or a human one, whose claim has no session to end); a human may pass {force: true} to take it anyway. 409 CLAIM_CONTENDED means the holder changed twice while the request ran, so nothing was applied and nobody's liveness was checked: read the issue and retry.", s.claimIssue},
		{http.MethodDelete, "/api/v1/issues/{key}/claim", authAny, "Release the issue's claim: its holder, any human, or anyone when the holding session is no longer live. The status does not move. The answer is the issue with legion_managed_status beside it: true when the issue carries the legion label in any case, or sits under one that does, which means the Legion daemon owns the status and a status another writer sets is undone. 409 ISSUE_CLAIMED names the holder this caller may not release (there is no force on this route); 409 CLAIM_CONTENDED means the holder changed twice while the request ran, so nothing was applied and nobody's liveness was checked.", s.releaseIssueClaim},
		{http.MethodGet, "/api/v1/issues/{key}/events", authAny, "Page an issue's event log (after, before, ids, order, limit).", s.listIssueEvents},
		{http.MethodGet, "/api/v1/issues/{key}/references", authAny, "The eight-hop closure of documents the issue's content references: {members, truncated}, each member a document in the closure with its depth and the node it was reached via, under an ETag (If-None-Match answers 304). ?versions=latest leaves each member's artifact.versions holding only its latest version, under its own ETag; absent, empty or all is the full answer; another value, or versions given more than once, is 400 INVALID_QUERY.", s.getIssueReferences},
		{http.MethodGet, "/api/v1/references", authAny, "Edges of one node in the reference graph: ?to=<dispatch ref> backlinks or ?from= links; ?kind=, ?since=<events.id> (mentions only).", s.getReferences},
		{http.MethodGet, "/api/v1/block-links", authAny, "Links from blocks of one document to blocks of another: ?from=<document ref>&to=<document ref>; both block nodes, the edge kind, and when and in which version the source wrote each, newest first.", s.getBlockLinks},
		{http.MethodGet, "/api/v1/issues/{key}/subscribers", authHuman, "Sessions subscribed to an issue's topics.", s.listIssueSubscribers},
		{http.MethodDelete, "/api/v1/issues/{key}/subscribers/{session_id}", authHuman, "Unsubscribe a session from an issue.", s.unsubscribeIssueSession},
		{http.MethodGet, "/api/v1/issues/{key}/artifacts", authAny, "List an issue's documents and files.", s.listArtifacts},
		{http.MethodPost, "/api/v1/issues/{key}/artifacts", authAny, fmt.Sprintf("Upload an issue document or file (JSON or multipart). A text/markdown upload is a document; any other is an image only when its bytes are a PNG, JPEG, GIF or WebP, stored under the type its bytes are whatever was declared, and otherwise a file under its declared type. A name no artifact has yet is 400 INVALID_ARTIFACT when it, or the reference key <owner>/<slug> it gives the new artifact, is over %d bytes; an upload under an existing artifact's name is never refused for its length.", maxNewArtifactKeyBytes), s.uploadArtifact},
		{http.MethodPost, "/api/v1/issues/{key}/messages", authAny, "Post an issue message; target and delivery address a session or role. A reply names the message it answers in in_reply_to, and one block of it in in_reply_to_block.", s.createMessage},
		{http.MethodGet, "/api/v1/issues/{key}/messages", authAny, "List an issue's message threads: every root message, oldest first, each with its deliveries and its replies.", s.listIssueMessages},
		{http.MethodGet, "/api/v1/issues/{key}/messages/{id}", authAny, "Read one message with its deliveries and replies.", s.getMessage},
		{http.MethodGet, "/api/v1/messages/{id}", authAny, "Read the conversation a message belongs to by any message id in it, issue-less or not: the thread root with its deliveries and every reply, oldest first. An issue thread follows the issue's rule. On a direct (issue-less) thread a bearer names its session in ?session= and reads only one whose root targets that session or that it replied in (403 THREAD_FORBIDDEN), a guard against reading another session's conversation by mistake, not an authorization boundary. Direct-conversation text also reaches every authenticated caller through that caller's events view on GET /ws/live; nothing in Dispatch restricts it by session.", s.getMessageThread},
		{http.MethodGet, "/api/v1/messages/{id}/blocks", authAny, "A message's current blocks in body order: {item, indexed, blocks: [{id, type, text, ref, from, to}]}, item the message's graph node, from and to UTF-16 code units into its stored body, ref absent for a direct message's block (it has no address). Read under GET /api/v1/messages/{id}'s rule: a bearer reading a direct message names its session in ?session= (400 SESSION_REQUIRED, 403 THREAD_FORBIDDEN). While the message is owed a block index, indexed is false and blocks empty; the read writes nothing.", s.getMessageBlocks},
		{http.MethodPost, "/api/v1/messages/{id}/deliveries", authAny, "Retry delivering a targeted message.", s.createDelivery},
		{http.MethodPost, "/api/v1/messages/{id}/deliveries/{attempt}/accept", authBearer, "The attempt's session records that it took the message as its user's own turn; the session names itself in actor (403 ACCEPT_FORBIDDEN for another's attempt). One compare-and-set: only a direct message to that session, on no issue, neither a broadcast's copy nor a reply in a broadcast's thread, in a thread whose root targets it (409 ACCEPT_NOT_DIRECT), that a person wrote (409 ACCEPT_NOT_WRITTEN_BY_PERSON), sent as a Send or an Aside (409 ACCEPT_NOT_ASIDE_OR_STEER); only the message's latest attempt (409 ACCEPT_SUPERSEDED), when no attempt of it was accepted before (409 ACCEPT_ALREADY_ACCEPTED), which a person requested (409 ACCEPT_NOT_REQUESTED_BY_PERSON), that person being the message's author (409 ACCEPT_NOT_REQUESTED_BY_AUTHOR), which did not fail (409 ACCEPT_FAILED; a pending or sent attempt is taken), within the last minute (409 ACCEPT_STALE). Answers the attempt with accepted_at and accepted_as and the message's stored body; leaves the attempt's state to the send, and appends message.accepted.", s.acceptDelivery},
		{http.MethodPost, "/api/v1/messages/{id}/reply", authBearer, "The targeted session's reply to a delivery; the session names itself in actor. Once the attempt is answered, ?follow_up=true posts other text as the session's follow-up, threaded under its first reply; with follow_up=false or absent, or with text the session already posted there, nothing is posted and the stored message comes back marked duplicate. Any other follow_up is 400 MESSAGE_INPUT.", s.replyMessage},
		{http.MethodGet, "/api/v1/inbox", authHuman, "Open asks waiting on the caller, grouped by whose turn it is; ?project= and ?assignee=me|unassigned|<email> filter (unassigned includes document asks; an email nobody has signed in with is 400 ASSIGNEE_NOT_ALLOWED).", s.listInbox},
		{http.MethodGet, "/api/v1/me/answers", authHuman, "The caller's own answers and replies on asks across issues and project documents, newest first: each row is an answer (the ask.answered event's answer, and whether it is still the ask's current one) or a reply, with the ask's question, state, owner and deep link; pages with limit (1-200, default 50) and offset; {rows, total, limit, offset}.", s.listMyAnswers},
		{http.MethodGet, "/api/v1/me/replies", authHuman, "The threads the caller wrote in (an ask, a comment thread, an issue message thread) where someone else has written since the caller last read it, newest reply first, less an open ask the Inbox lists: each row names the thread, its owner, its subject, its root's deep link and the newest reply's author, first line and time. ?issue= or ?artifact= narrows to one owner (both is 400 INVALID_QUERY); pages with limit (1-200, default 50) and offset; {rows, total, limit, offset}.", s.listMyReplies},
		{http.MethodPut, "/api/v1/me/threads/{id}/read", authHuman, "Move the caller's read mark on one thread forward to {read_through} (RFC3339; stored as no later than now; more than a minute ahead of the server is 400 INVALID_STATE). It only moves forward; a thread the caller never wrote in writes nothing; 204 either way. A non-uuid id is 400 THREAD_ID_INPUT.", s.putThreadRead},
		{http.MethodGet, "/api/v1/search", authAny, fmt.Sprintf("Full-text search ?q= across issues, documents, asks, comments, and messages, within ?project= when given: each kind ranked on its own and merged by reciprocal rank fusion, one page of ?limit= (1-50, default 20) from ?offset=, with the total match count and how many the pages can reach (each kind's best %d); a q over %d characters is 400 CAP_EXCEEDED, and a project that is not a project key is 400 INVALID_PROJECT.", contracts.SearchKindDepth, contracts.SearchQueryMax), s.search},
		{http.MethodGet, "/api/v1/search/coverage", authAny, "Meaning search's coverage {identity, place, model, total, kinds}: the configured embedder's identity, its place (bedrock or azure) and model, all three null with none configured; then, in total and for each kind (issue, document, comment, ask, message), the searchable items, how many carry a current vector from that identity, how many lack one, how many failed for good, and how many seconds the oldest lacking item has waited (null when none lacks).", s.searchCoverage},
		{http.MethodGet, "/api/v1/related-checks", authAny, "Count the related-item checks recorded when an issue, an ask or a new decision block was written, within [from, to) (RFC3339, each optional) and of ?project= and ?source_kind= (issue, ask or decision_block; any other is 400 INVALID_QUERY) when given: {project, source_kind, from, to} echo the filters (null when absent), total, by_source_kind, by_outcome (found, none, not_made), by_search (meaning_and_keywords, keywords_only, not_made), by_reason (each reason a check was not made or searched by keywords only), completed (found or none), skipped_for_time (reason timeout), and keyword_ms and total_ms each {count, p50, p95, max, over_300ms}: the checks holding that time, its discrete percentiles and largest value (null with no check), and how many took over 300 ms.", s.getRelatedChecks},
		{http.MethodGet, "/api/v1/agents", authAny, "Live sessions with roles, capabilities, open asks, and last activity, from Dispatch's copy of the Envoy listener's session list, at most 5 s old.", s.listAgents},
		{http.MethodGet, "/api/v1/agents/{session_id}/answers", authAny, "What people wrote that Dispatch delivered to one session, newest first by at, then by id: each ask's current answer (replaced when it changed an earlier answer), each comment at its current body (edited_at when edited; a resolved comment stays) and each message, once however it arrived. It reads the ask.answered, comment.created, message.created and message.answered events a person wrote whose published_destinations hold the session's own topic, the session's sent message deliveries (targeted and broadcast messages) and its sent comment deliveries (mentions); it never lists an agent's or the system's words, an ask's resolution or hand-back, or a delivery to a role: route's topic. Each item names its relation to the session (mentioned_you over replied_on_your_ask over replied_in_a_comment_thread over commented_on_a_document over commented_on_an_issue for a comment; replied_to_your_message over sent_you_a_message for a message; answered_your_ask for an answer), the ask and its whole question on an answer and on a reply in an ask's thread, its owner, dispatch:// ref and href (null owner and ref, and /agents/<session_id>/live, for a direct message). ?since= (RFC3339, default 72 hours ago; older than 7 days or in the future is 400 INVALID_QUERY) starts the window; ?max_chars= (1-100000, else 400 INVALID_QUERY) stops the list after the item whose text takes the running sum of text lengths (UTF-16 units) past it, while total counts every item in the window; {session_id, since, total, items}. A session_id that is not a session id is 400 INVALID_SESSION_ID, as on the agent artifact routes. Not an authorization boundary: direct-message text already reaches every authenticated caller through GET /api/v1/events, and a person may read any session's list.", s.listSessionAnswers},
		{http.MethodGet, "/api/v1/agents/{session_id}/messages", authHuman, "A session's targeted messages, newest first.", s.listAgentMessages},
		{http.MethodPost, "/api/v1/agents/{session_id}/messages", authHuman, "Send an issue-less targeted message to a session: {body, delivery, in_reply_to?}, answered once its own delivery attempt has settled, on past a client that gives up. Under an Idempotency-Key header (as every POST and PATCH takes one), a repeat with the same request answers 201 with the message the key made and its deliveries as they stand, Idempotency-Replayed: ?1, first sending the one attempt it still owes (none yet, or one stranded past its claim) and nothing else; a listener it cannot reach is recorded on the attempt, so a repeat is answered while the listener is down. An attempt a stopped process left owed is resumed by the server's delivery sweep whether anyone asks again or not.", s.createAgentMessage},
		{http.MethodPost, "/api/v1/agents/{session_id}/artifacts", authAny, "Upload a file or image to an agent's conversation (multipart, as an issue upload, its kind read from its bytes): ref_key agent/<session_id>/<slug>, addressed dispatch://agent/<session_id>/artifact/<slug>. It holds files and images only: a markdown document (JSON content or a text/markdown file) is 400 ARTIFACT_INPUT, since a document belongs to an issue or a project. No event is appended. A session_id that is empty or holds /, ?, #, whitespace, a control character or a character that ends a reference in text (<>\"'`[]|) is 400 INVALID_SESSION_ID on every agent artifact route.", s.uploadAgentArtifact},
		{http.MethodGet, "/api/v1/agents/{session_id}/artifacts/{slug}", authAny, "Read an agent's conversation's file or image and its versions by slug.", s.getArtifact},
		{http.MethodGet, "/api/v1/agents/{session_id}/artifacts/{slug}/versions/{number}", authAny, "Download one version of an agent's conversation's file or image, as the issue form serves it (Cache-Control: private, max-age=31536000, immutable).", s.getArtifactVersion},
		{http.MethodPost, "/api/v1/broadcasts", authHuman, "Send one message to many sessions: {body, delivery, session_ids}, one targeted message per recipient under a shared broadcast, answered once it is committed while each recipient's delivery runs behind the answer. A selected session that is not live or does not advertise the mode is excluded and named in the response, never switched to another mode. Under an Idempotency-Key header (as every POST and PATCH takes one), a repeat with the same request answers 201 at once with the broadcast the key made, every recipient's attempts as they stand and the first answer's exclusions, Idempotency-Replayed: ?1, and puts every recipient it still owes on its way behind the answer. A recipient a stopped process left owed is resumed by the server's delivery sweep whether anyone asks again or not.", s.createBroadcast},
		{http.MethodGet, "/api/v1/broadcasts", authHuman, "Recent broadcasts, newest first, with their recipient and reply counts.", s.listBroadcasts},
		{http.MethodGet, "/api/v1/broadcasts/{id}", authHuman, "One broadcast with every recipient's delivery attempts and replies, recipients in the order the send named them; a broadcast from before that order was stored falls back to created_at, id.", s.getBroadcast},
		{http.MethodPost, "/api/v1/issues/{key}/asks", authAny, "Open a question ask on an issue (options optional). After the write commits, a related-item check searches the project for the question and waits at most 5 seconds; the response's advice.suggestions says whether it found related items (outcome, with the reason in missing when not made), how it searched, the three items most like it with each ask item's answer state, and any already-answered ask that may settle the same question, with who answered and when.", s.createAsk},
		{http.MethodGet, "/api/v1/issues/{key}/asks", authAny, "List an issue's asks, each with its newest reply; ?state= filters, and ?thread=1 adds each ask's thread (replies, rewordings, answers, followers, claimant, route, route_holder).", s.listIssueAsks},
		{http.MethodGet, "/api/v1/asks/open", authAny, "Open asks in one scope, with counts: ?author_session= for one session's own, or ?project= for every open ask on a project's issues and documents; exactly one is required.", s.listOpenAsks},
		{http.MethodGet, "/api/v1/asks/{id}", authAny, "Read one ask with its replies, edit history, answers and followers, plus claimant (the session holding its issue's claim) and route (its issue's route), whom a person's answer and replies also reach, and route_holder (the live session that route reaches now: the session itself or the role's holder); each is null when absent, route_holder also when nobody holds the route or the Envoy listener did not answer, and all are null on a project document's ask.", s.getAsk},
		{http.MethodGet, "/api/v1/asks/{id}/followers", authAny, "List the sessions an ask's answer and replies reach.", s.listAskFollowers},
		{http.MethodPut, "/api/v1/asks/{id}/followers/{session_id}", authAny, "Follow an ask (a bearer only for its own session; a human for any).", s.followAsk},
		{http.MethodDelete, "/api/v1/asks/{id}/followers/{session_id}", authAny, "Unfollow an ask (a bearer only for its own session; a human for any).", s.unfollowAsk},
		{http.MethodPatch, "/api/v1/asks/{id}", authAny, "Reword an open ask (its author or a human); a block ask's `:::ask` block is rewritten with it.", s.editAsk},
		{http.MethodPost, "/api/v1/asks/{id}/answer", authHuman, "Answer an ask with selected options and/or text.", s.answerAsk},
		{http.MethodPost, "/api/v1/asks/{id}/resolve", authAny, "Retract or resolve an ask without answering it (kind retracted or resolved; withdrawn is written only by an issue's close, and an ask on a closed issue is 409 ISSUE_CLOSED); a block ask's block records it too.", s.resolveAsk},
		{http.MethodGet, "/api/v1/issues/{key}/comments", authAny, "List an issue's comments; ?artifact= scopes to a document.", s.listComments},
		{http.MethodPost, "/api/v1/issues/{key}/comments", authAny, "Comment on an issue, a document quote, or an ask. A reply names the comment it answers in reply_to, and one block of it in reply_to_block.", s.createComment},
		{http.MethodPost, "/api/v1/comments/{id}/resolve", authAny, "Resolve a comment thread.", s.resolveComment},
		{http.MethodPost, "/api/v1/comments/{id}/reopen", authHuman, "Reopen a resolved comment thread.", s.reopenComment},
		{http.MethodGet, "/api/v1/comments/{id}", authAny, "Read one comment with its thread.", s.getComment},
		{http.MethodGet, "/api/v1/comments/{id}/blocks", authAny, "A comment's current blocks in body order: {item, revision, indexed, blocks: [{id, type, text, ref, from, to, changed_revision, changed_at}]}, item the comment's graph node, revision its indexed revision, from and to UTF-16 code units into its stored body, changed_revision and changed_at the revision its block's text last changed in. While the comment is owed a block index, or was indexed from another body than it stores, indexed is false and blocks empty; the read writes nothing. A non-uuid id is 400 COMMENT_ID_INPUT.", s.getCommentBlocks},
		{http.MethodPost, "/api/v1/comments/{id}/deliveries", authAny, "Retry a comment mention delivery; target is required when it has multiple mentions.", s.createCommentDelivery},
		{http.MethodPost, "/api/v1/comments/{id}/reply", authBearer, "The mentioned session's reply to a delivery; the session names itself in actor.", s.replyComment},
		{http.MethodPost, "/api/v1/comments/{id}/accept", authHuman, "Accept a suggestion, applying its replacement.", s.acceptComment},
		{http.MethodPatch, "/api/v1/comments/{id}", authHuman, "Edit a comment's body. The dashboard editor also sends blocks ([{id, from}], from in UTF-16 code units into the new body) and base_revision, the revision GET /api/v1/comments/{id}/blocks gave, so each block keeps its id; without them the ids follow the text.", s.editComment},
		{http.MethodPost, "/api/v1/comments/{id}/reject", authHuman, "Reject a suggestion.", s.rejectComment},
		{http.MethodGet, "/api/v1/artifacts/{id}/asks", authAny, "List a document's asks.", s.listArtifactAsks},
		{http.MethodPost, "/api/v1/artifacts/{id}/asks", authAny, "Open an ask on a document. After the write commits, a related-item check searches the document's project for the question and waits at most 5 seconds; the response's advice.suggestions says whether it found related items (outcome, with the reason in missing when not made), how it searched, the three items most like it with each ask item's answer state, and any already-answered ask that may settle the same question, with who answered and when.", s.createArtifactAsk},
		{http.MethodGet, "/api/v1/artifacts/{id}/comments", authAny, "List a document's comments.", s.listArtifactComments},
		{http.MethodPost, "/api/v1/artifacts/{id}/comments", authAny, "Comment on a document or one of its quotes.", s.createArtifactComment},
		{http.MethodGet, "/api/v1/artifacts/{id}/events", authAny, "Page a document's event log.", s.listArtifactEvents},
		{http.MethodGet, "/api/v1/artifacts/{id}/references", authAny, "Cross-references to and from a document.", s.getArtifactReferences},
		{http.MethodGet, "/api/v1/artifacts/{id}/subscribers", authHuman, "Sessions subscribed to a document's topics.", s.listArtifactSubscribers},
		{http.MethodDelete, "/api/v1/artifacts/{id}/subscribers/{session_id}", authHuman, "Unsubscribe a session from a document.", s.unsubscribeArtifactSession},
		{http.MethodGet, "/api/v1/artifacts/{id}", authAny, "Read a document's metadata and current version.", s.getArtifact},
		{http.MethodPost, "/api/v1/artifacts/{id}/rebuild", authHuman, "Rebuild a document history ygo cannot load from its latest saved markdown; a live document is 409 DOCUMENT_LIVE and one that loads is 409 DOCUMENT_LOADS.", s.rebuildArtifact},
		{http.MethodGet, "/api/v1/artifacts/{id}/reviews", authAny, "List a document's approval reviews.", s.listArtifactReviews},
		{http.MethodPost, "/api/v1/artifacts/{id}/reviews", authHuman, "Approve or request changes on a document's latest settled version; answers the open approval ask when present, preserving its thread.", s.createArtifactReview},
		{http.MethodPost, "/api/v1/artifacts/{id}/approval-requests", authAny, "Open an approval ask for a document's latest version, with an optional summary. A document a human approved before carries that approval on the ask (approval.approved: version, at, by). An open ask follows later versions and waits on its agent until this route hands the same row back to a human.", s.requestArtifactApproval},
		{http.MethodGet, "/api/v1/artifacts/{id}/blocks", authAny, "A document's blocks with markdown ranges, tokens, and reference counts; a table block lists its rows and cells by the indexes delete_row and delete_column take.", s.getArtifactBlocks},
		{http.MethodGet, "/api/v1/artifacts/{id}/blocks/{block_id}", authAny, "Where one block stands in a document: its path from the top-level block down (type, id, child index), and for a table block, row or cell the table's id, the row index (0 is the header), the cell's column index, the text of the header cell drawn above it and the row's cells; 404 TARGET_NOT_FOUND for an id the live document does not hold.", s.getArtifactBlockPath},
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
		{http.MethodGet, "/api/v1/projects/{key}/artifacts/{slug}/summary", authAny, "What a reference to a project document shows, read without its versions: {id, project, slug, name, kind, latest_version}, the slug or unambiguous filename resolved as the project document read resolves it (404 ARTIFACT_NOT_FOUND, 400 ARTIFACT_REFERENCE_AMBIGUOUS).", s.getProjectDocumentSummary},
		{http.MethodGet, "/api/v1/projects/{key}/artifacts/{slug}/text", authAny, "A project document's canonical markdown and whole-document token, by slug or unambiguous filename.", s.getArtifactText},
		{http.MethodGet, "/api/v1/projects/{key}/artifacts/{slug}/versions/{number}", authAny, "One version of a project document, by slug or unambiguous filename.", s.getArtifactVersion},
		{http.MethodGet, "/api/v1/projects/{key}/artifacts/{slug}/blocks", authAny, "A project document's blocks and tokens, by slug or unambiguous filename.", s.getArtifactBlocks},
		{http.MethodPost, "/api/v1/projects/{key}/artifacts/{slug}/versions", authAny, "Name the current version of a project document, by slug or unambiguous filename.", s.createNamedVersion},
		{http.MethodPost, "/api/v1/projects/{key}/artifacts/{slug}/edits", authAny, "Apply edit ops with an optional optimistic-concurrency precondition to a project document, by slug or unambiguous filename.", s.editArtifact},
		{http.MethodGet, "/api/v1/me/state", authHuman, "The caller's per-issue read state.", s.getUserState},
		{http.MethodPut, "/api/v1/me/issues/{key}/state", authHuman, "Update the caller's read state for an issue.", s.putUserState},
		{http.MethodGet, "/api/v1/me/agents/state", authHuman, "The caller's per-agent conversation state: cleared_before, read_through, and unread_replies (the session's replies to the caller's direct messages newer than both and not read by id).", s.getUserAgentState},
		{http.MethodPut, "/api/v1/me/agents/{session_id}/state", authHuman, "Clear an agent's conversation for the caller (cleared_before hides exchanges at or before it), mark it read (read_through only moves forward), and/or mark some of its replies read by id (read_replies, the session's own messages); answers the session's state.", s.putUserAgentState},
		{http.MethodPut, "/api/v1/me/asks/{id}/snooze", authHuman, "Snooze an inbox row for the caller until snoozed_until.", s.putAskSnooze},
		{http.MethodDelete, "/api/v1/me/asks/{id}/snooze", authHuman, "Un-snooze an inbox row for the caller.", s.deleteAskSnooze},
		{http.MethodGet, "/api/v1/credential-requests", authHuman, "List credential requests the caller may decide (?approver=me only): those naming the caller, a shared secret's and a service's machine login, which anyone signed in decides; null without a configured secrets broker.", s.listCredentialPending},
		{http.MethodGet, "/api/v1/credential-requests/{id}", authHuman, "Read one credential request's facts; the broker is authoritative.", s.getCredentialRecord},
		{http.MethodPost, "/api/v1/credential-requests/{id}/approve", authHuman, "Approve a credential request as the caller (a machine login also takes its typed code); the broker decides whether the caller is its approver.", s.approveCredentialRecord},
		{http.MethodPost, "/api/v1/credential-requests/{id}/deny", authHuman, "Deny a credential request as the caller (a machine login also takes its typed code); the broker decides whether the caller is its approver.", s.denyCredentialRecord},
		{http.MethodPost, "/api/v1/credential-requests/machine-lookup", authHuman, "Resolve a pending machine login by its typed confirmation code, returning its facts.", s.lookupMachineCredential},
		{http.MethodGet, "/api/v1/credential-grants", authHuman, "List credential grants the caller may revoke (?approver=me only).", s.listCredentialGrants},
		{http.MethodPost, "/api/v1/credential-grants/{id}/revoke", authHuman, "Revoke a credential grant as the caller, its approver or its enrollment's operator.", s.revokeCredentialGrant},
		{http.MethodGet, "/api/v1/machine-logins", authHuman, "List the machine logins the caller may revoke that can still reach a secret: the secrets broker's launcher credentials for every service's login, whoever approved it, and for the caller's own machines, unexpired or expired with a session still running, each naming who approved it.", s.listMachineLogins},
		{http.MethodPost, "/api/v1/machine-logins/{id}/revoke", authHuman, "Revoke a machine login, expired or not, ending every session it enrolled, a service's pods included: anyone signed in revokes a service's login, and only the person who approved a person's machine login revokes that one.", s.revokeMachineLogin},
	}
	var testHooks []routeRow
	if s.deps.TestHooksEnabled {
		testHooks = []routeRow{
			{http.MethodPost, "/api/v1/live/_test/disconnect", authAny, "Test hook: close every open live socket with 1012, as a restart does.", s.disconnectLiveSockets},
			{http.MethodPost, "/api/v1/live/_test/rotate", authAny, "Test hook: close every open live socket with 4001 (max age), as its age does.", s.rotateLiveSockets},
			{http.MethodPost, "/api/v1/artifacts/_test/quiesce", authAny, "Test hook: close every live document and finish the settlements in flight.", s.quiesceDocuments},
			{http.MethodPost, "/api/v1/artifacts/{id}/_test/outside-schema", authAny, "Test hook: write a crafted tree outside the Proof schema.", s.injectArtifactSchemaFailure},
			{http.MethodPost, "/api/v1/agents/{session_id}/stream/_test/publish", authHuman, "Test hook: publish one frame to a session's conversation viewers.", s.publishAgentStreamFrame},
			{http.MethodPost, "/api/v1/writes/_test/slow", authAny, "Test hook: {method, path, delay_ms} delays every keyed run of that method and path by delay_ms after its key is claimed, on the handler's own context; 0 disarms.", s.armSlowWrite},
		}
	}
	return mountedRoutes(rows, testHooks)
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

// routeIndexBody is the body GET /api/v1 answers.
func routeIndexBody(entries []routeIndexEntry) map[string]any {
	return map[string]any{"routes": entries, "docs": routeIndexDocs}
}

func (s *server) getRouteIndex(w http.ResponseWriter, _ *http.Request) {
	WriteJSON(w, http.StatusOK, routeIndexBody(s.routeIndex))
}

// WriteRouteIndex writes the body GET /api/v1 answers on a server without test hooks, read
// from the table alone: no database, no listener. `envoy-dispatch routes` prints it, and the
// docs site's API reference is generated from that output.
func WriteRouteIndex(w io.Writer) error {
	return json.NewEncoder(w).Encode(routeIndexBody(routeIndexEntries((&server{}).routes())))
}
