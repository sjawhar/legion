// routes_shapes.go is the side map that gives every row of the route table its Go types: what it
// takes, what it answers, and with which status. The table itself carries paths, auth and prose
// and no shapes (routeRow), so a document emitted from the table alone would describe no body;
// this file is the data that document is reflected out of (INFRA-1406). Nothing here changes
// behaviour: no handler reads a type declared below, and the query structs are declarations
// alone - `r.URL.Query().Get` and `.Has` differ on an absent key, so moving forty handlers'
// reads onto a struct would be a behaviour change, and is not this file's business.
package api

import (
	"maps"
	"net/http"
	"sort"

	"github.com/sjawhar/envoy/internal/broker/wire"
	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// routeAnswer is one answer a route can give: a status and a zero value of the Go type its body
// carries, nil for a body-less answer.
type routeAnswer struct {
	Status int
	Body   any
}

// routeShape is what one row of the table takes and answers, in Go types an OpenAPI document is
// reflected out of (INFRA-1406). It stands beside the table for the reason writeClasses and
// routeScopes do: routeRow is a positional literal, Go leaves no field out of one, and a sixth
// column would have to be written on all 179 rows. routes() stamps it onto the row.
type routeShape struct {
	// Status is the status the route answers when it succeeds.
	Status int
	// Response is a zero value of the Go type the success answer carries: nil for a route that
	// answers no body, and nil with Opaque set for one whose bytes Dispatch does not model.
	Response any
	// Nullable is a route whose success answer is Response or the JSON literal `null`, which is
	// a shape of the answer rather than a second answer: OpenAPI keys a response on its status,
	// and both arms are the same status.
	Nullable bool
	// Also are the route's other answers, each at a status of its own: the second success of a
	// route that answers 200 or 201 by state, and the 304 of a conditional read.
	Also []routeAnswer
	// Request is a zero value of the request body's Go type, nil for a route that takes none.
	Request any
	// Multipart is a route that takes multipart/form-data as well as JSON.
	Multipart bool
	// Params is a zero value of the route's query-parameter struct, each field tagged
	// `query:"<key>"`; nil for a route that reads none. Path parameters are not here: they are
	// read off the pattern.
	Params any
	// Advice is a zero value of the advice shape the success answer carries beside its payload
	// (withAdvice, withRawAdvice and withSuggestions merge an `advice` sibling into the payload
	// object rather than nesting it, write_advice.go:471-517); nil for an answer that carries none.
	Advice any
	// Refusals are this route's refusal bodies richer than errorBody, each with its status.
	Refusals []routeAnswer
	// Opaque is why this route's answer carries no schema, for a route that relays or pre-renders
	// its bytes rather than modelling them; empty for every other route.
	Opaque string
	// OpaqueContentType is the media type an Opaque answer is served as; empty where Opaque is.
	OpaqueContentType string
	// OpaqueRequest is why this route's body carries no schema, for a route that reads the bytes
	// whole rather than modelling them; empty for every other route, and never beside Request.
	OpaqueRequest string
}

// --- the query-parameter structs -------------------------------------------------------------
//
// One struct per route, or per set of routes reading the same keys, each field carrying the key
// it is read from. They are declarations: the handlers keep their own reads. Two keys of the API
// are named by no struct here. `session` is read by actingPerson (server.go) on every route that
// resolves a person, so it belongs to the whole API rather than to any one operation and the
// emitter adds it everywhere; the delivery facet of the same name is left out with it. `cursor`
// on GET /api/v1/issues is read only to refuse it (400 INVALID_QUERY, issue_page.go), and naming
// it would offer a client a parameter that cannot work. No struct here is ever a request body,
// and the emitter reads only each field's `query` and `enum` tags and the Go type it carries.

// projectArtifactsQuery is GET /api/v1/projects/{key}/artifacts's query.
type projectArtifactsQuery struct {
	// Unlinked, `true`, lists only the project's documents that belong to no issue.
	Unlinked bool `query:"unlinked"`
}

// relatedQuery is GET /api/v1/projects/{key}/related's query: the related-item check an issue
// create runs, over a title and a text, writing nothing.
type relatedQuery struct {
	// Title is checked as the new issue's title; either it or Text alone is enough.
	Title string `query:"title"`
	// Text is checked as the new issue's text, under the title and a newline.
	Text string `query:"text"`
	// ExcludeIssue leaves that issue of the project and everything it holds out of the results
	// and out of the word statistics; an issue of another project is 400 EXCLUDE_ISSUE_INPUT.
	ExcludeIssue string `query:"exclude_issue"`
	// KeywordsOnly, `true`, skips meaning search; any value but true or false is 400.
	KeywordsOnly bool `query:"keywords_only"`
}

// deliveryWindowQuery is the query GET /api/v1/delivery/timeline and GET /api/v1/delivery/measures
// share: the window, the search and the repeatable facets. The `session` facet is read by the
// same code (deliveryFacets) and named here by no field, since `session` is the whole API's
// parameter.
type deliveryWindowQuery struct {
	// From is the window's start, RFC3339; absent it is seven days before To.
	From string `query:"from"`
	// To is the window's end, RFC3339, exclusive; absent it is now.
	To string `query:"to"`
	// Q searches a pull request's title or its id.
	Q string `query:"q"`
	// Repo selects pull requests of these repositories.
	Repo []string `query:"repo"`
	// ParentAgent selects pull requests whose first session belongs to these parents;
	// __no_session__ selects those whose commits name no session.
	ParentAgent []string `query:"parent_agent"`
	// Issue selects pull requests of these issues; __no_issue__ selects those naming none.
	Issue []string `query:"issue"`
	// Priority selects P0-P3; __no_priority__ selects an issue with none.
	Priority []string `query:"priority"`
	// Component selects these components, a parent including its children;
	// __no_component__ selects an issue with none.
	Component []string `query:"component"`
	// Author selects these pull-request authors.
	Author []string `query:"author"`
	// Rework selects by whether the work was rework.
	Rework []string `query:"rework"`
	// Deployed selects by deployed_status: deployed, waiting or not_tracked.
	Deployed []string `query:"deployed"`
}

// deliveryPipelineQuery is GET /api/v1/delivery/pipeline's query.
type deliveryPipelineQuery struct {
	// At fixes the day the seven-day Pipeline window ends on, RFC3339; absent it is now.
	// A malformed value is 400 INVALID_QUERY.
	At string `query:"at"`
}

// issueListQuery is GET /api/v1/issues's query: the filters and the offset page.
type issueListQuery struct {
	// Pinned, `true`, lists the caller's pinned issues; it needs the person scope, the pins
	// being one person's.
	Pinned bool `query:"pinned"`
	// Project lists one project's issues.
	Project string `query:"project"`
	// Status lists issues at one status.
	Status string `query:"status"`
	// Parent lists the children of one issue.
	Parent string `query:"parent"`
	// Label lists issues carrying every label given.
	Label []string `query:"label"`
	// UpdatedSince lists issues updated at or after this RFC3339 time.
	UpdatedSince string `query:"updated_since"`
	// Priority lists issues at these priorities, 0-3, or `none` for unset.
	Priority []string `query:"priority"`
	// RouteStatus lists open issues whose route is live, no_holder or unknown.
	RouteStatus string `query:"route_status"`
	// Open, `true`, lists open issues alone.
	Open bool `query:"open"`
	// Limit is the page size, 1 to contracts.MaxIssuePageLimit; with Offset it makes the
	// answer a page rather than an array.
	Limit int `query:"limit"`
	// Offset is where the page starts, 0 or more; alone it pages at the default limit.
	Offset int `query:"offset"`
}

// issueResolveQuery is GET /api/v1/issues/resolve's query.
type issueResolveQuery struct {
	// Ref is the reference to resolve: an issue key, or owner/repo#n.
	Ref string `query:"ref"`
}

// eventListQuery is the query GET /api/v1/issues/{key}/events and GET /api/v1/artifacts/{id}/events
// share: one page of an event log.
type eventListQuery struct {
	// After reads the events above this event id, oldest first; it combines with neither
	// Before nor order=desc.
	After int `query:"after"`
	// Before reads the events below this event id.
	Before int `query:"before"`
	// IDs reads exactly these event ids, comma-separated.
	IDs string `query:"ids"`
	// Order is asc or desc.
	Order string `query:"order"`
	// Limit is how many events the page holds.
	Limit int `query:"limit"`
}

// issueReferencesQuery is GET /api/v1/issues/{key}/references's query.
type issueReferencesQuery struct {
	// Versions, `latest`, leaves each member's artifact.versions holding only its latest
	// version, under its own ETag; absent, empty or `all` is the full answer, another value or
	// a repeat is 400 INVALID_QUERY.
	Versions string `query:"versions"`
}

// referencesQuery is GET /api/v1/references's query: the edges of one node.
type referencesQuery struct {
	// To reads the backlinks of this dispatch:// reference; exactly one of To and From is
	// required.
	To string `query:"to"`
	// From reads the links out of this dispatch:// reference.
	From string `query:"from"`
	// Kind keeps only these edge kinds, comma-separated.
	Kind string `query:"kind"`
	// Since reads the mentions above this events.id, up to the event log's safe horizon.
	Since int64 `query:"since"`
}

// referenceQuestionsQuery is GET /api/v1/references/questions's query: the one source whose
// document's question blocks are read.
type referenceQuestionsQuery struct {
	// Kind is the kind of the reference source, one of artifact, ask, comment or message;
	// anything else is 400 INVALID_KIND.
	Kind string `query:"kind" enum:"artifact,ask,comment,message"`
	// ID is that source's whole uuid or a unique prefix of 8 characters or more; it is
	// required, a prefix two sources carry is 409 ID_AMBIGUOUS.
	ID string `query:"id"`
}

// blockLinksQuery is GET /api/v1/block-links's query: both documents are required.
type blockLinksQuery struct {
	// From is the document whose blocks hold the links, as a dispatch:// reference.
	From string `query:"from"`
	// To is the document whose blocks the links reach.
	To string `query:"to"`
}

// messageReplyQuery is POST /api/v1/messages/{id}/reply's query.
type messageReplyQuery struct {
	// FollowUp, `true`, posts other text as the session's follow-up under its first reply once
	// the attempt is answered; false or absent posts nothing, any other value is 400
	// MESSAGE_INPUT.
	FollowUp bool `query:"follow_up"`
}

// inboxQuery is GET /api/v1/inbox's query.
type inboxQuery struct {
	// Project keeps the open asks of one project.
	Project string `query:"project"`
	// Assignee is me, unassigned (document asks included) or an email; an email nobody has
	// signed in with is 400 ASSIGNEE_NOT_ALLOWED.
	Assignee string `query:"assignee"`
}

// myAnswersQuery is GET /api/v1/me/answers's query: the offset page it always answers.
type myAnswersQuery struct {
	// Limit is the page size, 1 to 200, default 50.
	Limit int `query:"limit"`
	// Offset is where the page starts.
	Offset int `query:"offset"`
}

// myRepliesQuery is GET /api/v1/me/replies's query: the page and the one thread owner it may be
// narrowed to.
type myRepliesQuery struct {
	// Limit is the page size, 1 to 200, default 50.
	Limit int `query:"limit"`
	// Offset is where the page starts.
	Offset int `query:"offset"`
	// Issue narrows the rows to one issue's threads; with Artifact it is 400 INVALID_QUERY.
	Issue string `query:"issue"`
	// Artifact narrows the rows to one document's threads, by document id.
	Artifact string `query:"artifact"`
}

// searchRouteQuery is GET /api/v1/search's query. The name carries `route` because search.go
// already has a searchQuery of its own, the parsed query a search runs on.
type searchRouteQuery struct {
	// Q is the text searched for, at least 2 characters and at most contracts.SearchQueryMax.
	Q string `query:"q"`
	// Project searches within one project; empty searches every project.
	Project string `query:"project"`
	// Limit is the page size, 1 to 50, default 20.
	Limit int `query:"limit"`
	// Offset is where the page starts.
	Offset int `query:"offset"`
}

// relatedChecksQuery is GET /api/v1/related-checks's query.
type relatedChecksQuery struct {
	// Project counts the checks of one project.
	Project string `query:"project"`
	// SourceKind counts the checks of one source: issue, ask or decision_block.
	SourceKind string `query:"source_kind"`
	// From is the window's start, RFC3339.
	From string `query:"from"`
	// To is the window's end, RFC3339, exclusive.
	To string `query:"to"`
}

// sessionAnswersQuery is GET /api/v1/agents/{session_id}/answers's query.
type sessionAnswersQuery struct {
	// Since is the window's start, one RFC3339 time no older than the route's bound and not
	// after now.
	Since string `query:"since"`
	// MaxChars bounds how much of each item's text the answer carries.
	MaxChars int `query:"max_chars"`
}

// issueAsksQuery is GET /api/v1/issues/{key}/asks's query.
type issueAsksQuery struct {
	// State filters the list: all, open or answered.
	State string `query:"state"`
	// Thread adds each ask's thread: replies, rewordings, answers, followers, claimant, route
	// and route_holder. It is the literal `1` or absent, never a boolean: parseAskListThread
	// (asks.go) refuses every other value with 400 INVALID_ASK_THREAD, so a client that sent
	// `true` would be refused.
	Thread string `query:"thread" enum:"1"`
}

// artifactAsksQuery is GET /api/v1/artifacts/{id}/asks's query.
type artifactAsksQuery struct {
	// State filters the list: all, open or answered.
	State string `query:"state"`
}

// openAsksQuery is GET /api/v1/asks/open's query: exactly one of the two scopes is required.
type openAsksQuery struct {
	// AuthorSession lists the open asks one session authored.
	AuthorSession string `query:"author_session"`
	// Project lists every open ask on a project's issues and documents.
	Project string `query:"project"`
	// Since lists the asks opened at or after this one RFC3339 time.
	Since string `query:"since"`
}

// issueCommentsQuery is GET /api/v1/issues/{key}/comments's query.
type issueCommentsQuery struct {
	// Artifact scopes the comments to one document of the issue.
	Artifact string `query:"artifact"`
}

// credentialApproverQuery is the query GET /api/v1/credential-requests and
// GET /api/v1/credential-grants share.
type credentialApproverQuery struct {
	// Approver must be the literal `me`: the viewer's own list, never a login they name.
	Approver string `query:"approver"`
}

// publishFrameQuery is POST /api/v1/agents/{session_id}/stream/_test/publish's query.
type publishFrameQuery struct {
	// As says what the posted bytes are: `replay` a transcript frame, `drop` an ended view,
	// `responder` the {"responding"} body; absent, one ordinary frame.
	As string `query:"as"`
}

// liveWritePauseQuery is GET /api/v1/artifacts/{id}/_test/live-write-pauses/{point}'s query.
type liveWritePauseQuery struct {
	// Wait is how long to wait for a write to park, a positive Go duration such as "10s".
	Wait string `query:"wait"`
}

// --- the shapes ------------------------------------------------------------------------------

// routeShapes is the shape of every row of the table with its test hooks in, by
// "<method> <pattern>", exactly as writeClasses is keyed. The entries are in the table's own
// order, so the two read side by side.
//
// Two conventions a reader should know. A route whose answer can carry either advice shape names
// the richer one: writeAdvice holds every field documentWriteAdvice and suggestionsOnlyAdvice
// hold, so a document write with an issue and one without differ by which of its fields are
// present, not by which type describes them. And a route whose success answer is an object this
// server does not model - a relay's bytes, a pre-rendered schema, an event stream, a file's
// bytes - carries no Response and says why in Opaque.
var routeShapes = map[string]routeShape{
	"GET /api/v1": {Status: http.StatusOK, Response: routeIndex{}},
	"GET /api/v1/schema/blocks": {
		Status:            http.StatusOK,
		Opaque:            "the block schema is pre-rendered JSON bytes (pmdoc.SchemaJSON), written as they stand",
		OpaqueContentType: "application/json",
	},
	"GET /api/v1/limits":    {Status: http.StatusOK, Response: model.DispatchLimits{}},
	"GET /api/v1/projects":  {Status: http.StatusOK, Response: []model.Project{}},
	"POST /api/v1/projects": {Status: http.StatusCreated, Response: model.Project{}, Request: createProjectInput{}},
	"GET /api/v1/projects/{key}/artifacts": {
		Status: http.StatusOK, Response: []model.Artifact{}, Params: projectArtifactsQuery{},
	},
	"POST /api/v1/projects/{key}/artifacts": {
		Status: http.StatusCreated, Response: artifactUploadResponse{},
		Request: jsonArtifactUpload{}, Multipart: true,
		// A project document has no issue state to report, so its write advice is the
		// document half alone (artifacts.go, the withRawAdvice arm).
		Advice: documentWriteAdvice{},
	},
	"GET /api/v1/projects/{key}/related": {
		Status: http.StatusOK, Response: model.Suggestions{}, Params: relatedQuery{},
	},
	"POST /api/v1/projects/{key}/related": {
		Status: http.StatusOK, Response: model.Suggestions{}, Request: relatedRequest{},
	},
	"GET /api/v1/settings/repo-projects": {Status: http.StatusOK, Response: []model.RepoProject{}},
	"PUT /api/v1/settings/repo-projects/{owner}/{repo}": {
		Status: http.StatusOK, Response: model.RepoProject{}, Request: putRepoProjectInput{},
	},
	"DELETE /api/v1/settings/repo-projects/{owner}/{repo}": {Status: http.StatusNoContent, Request: actorInput{}},
	"GET /api/v1/github/app-installation/{owner}/{repo}":   {Status: http.StatusOK, Response: appInstallationResponse{}},
	"GET /api/v1/settings/architecture-sources":            {Status: http.StatusOK, Response: []model.ArchitectureSource{}},
	"GET /api/v1/projects/{key}/architecture-sources":      {Status: http.StatusOK, Response: []model.ArchitectureSource{}},
	"GET /api/v1/projects/{key}/architecture-source": {
		Status: http.StatusOK, Response: model.ArchitectureSource{},
		// A project with no source answers 200 and the JSON literal `null`
		// (architecture_source.go).
		Nullable: true,
	},
	"PUT /api/v1/projects/{key}/architecture-sources/{owner}/{repo}": {
		Status: http.StatusOK, Response: model.ArchitectureSource{}, Request: putArchitectureSourceInput{},
	},
	"DELETE /api/v1/projects/{key}/architecture-sources/{owner}/{repo}": {
		Status: http.StatusNoContent, Request: actorInput{},
	},
	"POST /api/v1/projects/{key}/architecture-sources/{owner}/{repo}/sync": {Status: http.StatusOK, Response: model.ArchitectureSource{}},
	"POST /api/v1/projects/{key}/architecture-sources/sync":                {Status: http.StatusOK, Response: []model.ArchitectureSource{}},
	"POST /api/v1/projects/{key}/architecture-source/sync":                 {Status: http.StatusOK, Response: model.ArchitectureSource{}},
	"GET /api/v1/projects/{key}/architecture":                              {Status: http.StatusOK, Response: model.ArchitectureTree{}},
	"GET /api/v1/settings/project-default-assignees":                       {Status: http.StatusOK, Response: []projectDefaultAssignee{}},
	"PUT /api/v1/projects/{key}/default-assignee": {
		Status: http.StatusOK, Response: projectDefaultAssignee{}, Request: projectDefaultAssigneeInput{},
	},
	"GET /api/v1/settings/delivery": {
		Status: http.StatusOK, Response: delivery.DeliverySettings{},
		// A deployment that has not set them answers 200 and the JSON literal `null`
		// (settings_delivery.go).
		Nullable: true,
	},
	"PUT /api/v1/settings/delivery": {
		Status: http.StatusOK, Response: delivery.DeliverySettings{}, Request: deliverySettingsInput{},
	},
	"GET /api/v1/delivery/timeline": {
		Status: http.StatusOK, Response: deliveryTimelineWithMeasures{}, Params: deliveryWindowQuery{},
	},
	"GET /api/v1/delivery/runs/{id}": {Status: http.StatusOK, Response: delivery.DeliveryRunDetailView{}},
	"GET /api/v1/delivery/measures": {
		Status: http.StatusOK, Response: deliveryMeasuresResponse{}, Params: deliveryWindowQuery{},
	},
	"GET /api/v1/delivery/pipeline": {
		Status: http.StatusOK, Response: deliveryPipelineResponse{}, Params: deliveryPipelineQuery{},
	},
	"POST /api/v1/delivery/session-groups": {
		Status: http.StatusCreated, Response: delivery.SessionGroup{}, Request: createDeliverySessionGroupInput{},
	},
	"PATCH /api/v1/delivery/session-groups/{id}": {
		Status: http.StatusOK, Response: delivery.SessionGroup{}, Request: updateDeliverySessionGroupInput{},
	},
	"DELETE /api/v1/delivery/session-groups/{id}": {Status: http.StatusNoContent, Request: actorInput{}},
	"GET /api/v1/me/agent-tokens":                 {Status: http.StatusOK, Response: []model.AgentToken{}},
	"POST /api/v1/me/agent-tokens": {
		Status: http.StatusCreated, Response: createAgentTokenResponse{}, Request: createAgentTokenInput{},
	},
	"PATCH /api/v1/me/agent-tokens/{id}": {
		Status: http.StatusOK, Response: model.AgentToken{}, Request: updateAgentTokenScopesInput{},
	},
	"DELETE /api/v1/me/agent-tokens/{id}": {Status: http.StatusNoContent},
	"GET /api/v1/users":                   {Status: http.StatusOK, Response: listUsersResponse{}},
	"GET /api/v1/whoami": {
		Status: http.StatusOK, Response: whoamiUser{},
		// A caller the server takes for an agent is answered the other shape at the same
		// status (issue_assignee.go).
		Also: []routeAnswer{{Status: http.StatusOK, Body: whoamiAgent{}}},
	},
	"GET /api/v1/dashboard/builds": {Status: http.StatusOK, Response: dashboardBuilds{}},
	"GET /api/v1/issues": {
		Status: http.StatusOK, Response: []model.IssueSummary{}, Params: issueListQuery{},
		// A request naming limit or offset is answered the page wrapper instead
		// (issue_page.go).
		Also: []routeAnswer{{Status: http.StatusOK, Body: model.IssueSummaryPage{}}},
	},
	"POST /api/v1/issues": {
		Status: http.StatusCreated, Response: model.Issue{}, Request: createIssueInput{}, Advice: writeAdvice{},
		// A ref whose external issue already has a Dispatch issue answers that issue, 200.
		Also:     []routeAnswer{{Status: http.StatusOK, Body: model.Issue{}}},
		Refusals: []routeAnswer{{Status: http.StatusConflict, Body: issueDuplicateError{}}},
	},
	"GET /api/v1/issues/resolve":       {Status: http.StatusOK, Response: issueRefResolved{}, Params: issueResolveQuery{}},
	"GET /api/v1/issues/{key}":         {Status: http.StatusOK, Response: issueDetail{}},
	"GET /api/v1/issues/{key}/summary": {Status: http.StatusOK, Response: issueSummary{}},
	"PATCH /api/v1/issues/{key}": {
		Status: http.StatusOK, Response: model.Issue{}, Request: updateIssueInput{}, Advice: writeAdvice{},
	},
	"POST /api/v1/issues/{key}/claim": {
		Status: http.StatusOK, Response: claimAnswer{}, Request: claimIssueInput{},
		Refusals: []routeAnswer{{Status: http.StatusConflict, Body: issueClaimedError{}}},
	},
	"DELETE /api/v1/issues/{key}/claim": {
		Status: http.StatusOK, Response: claimAnswer{}, Request: actorInput{},
		Refusals: []routeAnswer{{Status: http.StatusConflict, Body: issueClaimedError{}}},
	},
	"GET /api/v1/issues/{key}/events": {Status: http.StatusOK, Response: []model.Event{}, Params: eventListQuery{}},
	"GET /api/v1/issues/{key}/references": {
		Status: http.StatusOK, Response: model.IssueReferences{}, Params: issueReferencesQuery{},
		// An If-None-Match that matches the answer's ETag is answered 304 and no body.
		Also: []routeAnswer{{Status: http.StatusNotModified}},
	},
	"GET /api/v1/references":                               {Status: http.StatusOK, Response: model.GraphReferences{}, Params: referencesQuery{}},
	"GET /api/v1/references/questions":                     {Status: http.StatusOK, Response: referenceQuestions{}, Params: referenceQuestionsQuery{}},
	"GET /api/v1/block-links":                              {Status: http.StatusOK, Response: model.BlockLinks{}, Params: blockLinksQuery{}},
	"GET /api/v1/issues/{key}/subscribers":                 {Status: http.StatusOK, Response: []subscriber{}},
	"DELETE /api/v1/issues/{key}/subscribers/{session_id}": {Status: http.StatusNoContent},
	"GET /api/v1/issues/{key}/artifacts":                   {Status: http.StatusOK, Response: []model.Artifact{}},
	"POST /api/v1/issues/{key}/artifacts": {
		Status: http.StatusCreated, Response: artifactUploadResponse{},
		Request: jsonArtifactUpload{}, Multipart: true, Advice: writeAdvice{},
	},
	"GET /api/v1/issues/{key}/results": {Status: http.StatusOK, Response: issueCheckResults{}},
	"POST /api/v1/issues/{key}/results": {
		Status: http.StatusCreated, Response: checkResultRecorded{}, Request: checkResultInput{},
	},
	"GET /api/v1/issues/{key}/coverage": {Status: http.StatusOK, Response: model.IssueCoverage{}},
	"POST /api/v1/issues/{key}/messages": {
		Status: http.StatusCreated, Response: model.Message{}, Request: createMessageInput{}, Advice: writeAdvice{},
	},
	"GET /api/v1/issues/{key}/messages":      {Status: http.StatusOK, Response: []messageRead{}},
	"GET /api/v1/issues/{key}/messages/{id}": {Status: http.StatusOK, Response: messageRead{}},
	"POST /api/v1/issues/{key}/handoffs/{phase}": {
		Status: http.StatusCreated, Response: model.HandoffSummary{}, Request: createHandoffInput{},
	},
	"GET /api/v1/issues/{key}/handoffs":                {Status: http.StatusOK, Response: []model.HandoffSummary{}},
	"GET /api/v1/issues/{key}/handoffs/{phase}/latest": {Status: http.StatusOK, Response: model.Handoff{}},
	"GET /api/v1/handoffs/{id}":                        {Status: http.StatusOK, Response: model.Handoff{}},
	"GET /api/v1/messages/{id}":                        {Status: http.StatusOK, Response: messageRead{}},
	"GET /api/v1/messages/{id}/blocks":                 {Status: http.StatusOK, Response: model.ItemBlocks{}},
	"POST /api/v1/messages/{id}/deliveries": {
		Status: http.StatusCreated, Response: model.MessageDelivery{}, Request: createDeliveryInput{},
	},
	"POST /api/v1/messages/{id}/deliveries/{attempt}/accept": {
		Status: http.StatusOK, Response: acceptedDelivery{}, Request: actorInput{},
	},
	"POST /api/v1/messages/{id}/reply": {
		Status: http.StatusCreated, Response: replyRead{}, Request: messageReplyInput{}, Params: messageReplyQuery{},
		// An attempt already answered is answered the stored reply, 200, marked duplicate
		// when nothing was posted; an error report answers the attempt itself.
		Also: []routeAnswer{
			{Status: http.StatusOK, Body: replyRead{}},
			{Status: http.StatusOK, Body: model.MessageDelivery{}},
		},
	},
	"GET /api/v1/inbox":                {Status: http.StatusOK, Response: []inboxAsk{}, Params: inboxQuery{}},
	"GET /api/v1/me/answers":           {Status: http.StatusOK, Response: model.MyAnswersResponse{}, Params: myAnswersQuery{}},
	"GET /api/v1/me/replies":           {Status: http.StatusOK, Response: model.MyRepliesResponse{}, Params: myRepliesQuery{}},
	"PUT /api/v1/me/threads/{id}/read": {Status: http.StatusNoContent, Request: threadReadInput{}},
	"GET /api/v1/search":               {Status: http.StatusOK, Response: model.SearchResponse{}, Params: searchRouteQuery{}},
	"GET /api/v1/search/coverage":      {Status: http.StatusOK, Response: model.SearchCoverage{}},
	"GET /api/v1/related-checks": {
		Status: http.StatusOK, Response: model.RelatedChecksReport{}, Params: relatedChecksQuery{},
	},
	"POST /api/v1/going/requests": {
		Status: http.StatusCreated, Response: goingRequest{}, Request: createGoingRequestInput{},
	},
	"GET /api/v1/going/requests/{id}/stream": {
		Status:            http.StatusOK,
		Opaque:            "the run's answer is a text/event-stream of hand-written id:/event:/data: frames, not one JSON body",
		OpaqueContentType: "text/event-stream",
	},
	"GET /api/v1/going/capability": {Status: http.StatusOK, Response: goingCapability{}},
	"GET /api/v1/agents":           {Status: http.StatusOK, Response: []agentResponse{}},
	"GET /api/v1/agents/{session_id}/answers": {
		Status: http.StatusOK, Response: model.SessionAnswersResponse{}, Params: sessionAnswersQuery{},
	},
	"GET /api/v1/agents/{session_id}/messages": {Status: http.StatusOK, Response: []messageRead{}},
	"POST /api/v1/agents/{session_id}/messages": {
		Status: http.StatusCreated, Response: model.Message{}, Request: createAgentMessageInput{},
	},
	"POST /api/v1/agents/{session_id}/artifacts": {
		Status: http.StatusCreated, Response: artifactUploadResponse{},
		// The upload reads JSON or multipart as the issue and project forms do, but a
		// conversation holds files and images alone, and a JSON upload is always a markdown
		// document: 400 ARTIFACT_INPUT (agent_artifacts.go). No issue, so no advice.
		Request: jsonArtifactUpload{}, Multipart: true,
	},
	"GET /api/v1/agents/{session_id}/artifacts/{slug}": {Status: http.StatusOK, Response: model.Artifact{}},
	"GET /api/v1/agents/{session_id}/artifacts/{slug}/versions/{number}": {
		Status:            http.StatusOK,
		Opaque:            "a conversation holds files and images alone, so a version download is that file's bytes under its own content type (serveFileVersion)",
		OpaqueContentType: "application/octet-stream",
	},
	"POST /api/v1/broadcasts": {
		Status: http.StatusCreated, Response: broadcastCreated{}, Request: createBroadcastInput{},
	},
	"GET /api/v1/broadcasts":      {Status: http.StatusOK, Response: []broadcastSummary{}},
	"GET /api/v1/broadcasts/{id}": {Status: http.StatusOK, Response: broadcastRead{}},
	"POST /api/v1/issues/{key}/asks": {
		Status: http.StatusCreated, Response: model.Ask{}, Request: createAskInput{}, Advice: writeAdvice{},
	},
	"GET /api/v1/issues/{key}/asks":                   {Status: http.StatusOK, Response: []issueAskRow{}, Params: issueAsksQuery{}},
	"GET /api/v1/issues/{key}/decisions":              {Status: http.StatusOK, Response: issueDecisions{}},
	"GET /api/v1/asks/open":                           {Status: http.StatusOK, Response: openAsksResponse{}, Params: openAsksQuery{}},
	"GET /api/v1/asks/{id}":                           {Status: http.StatusOK, Response: askRead{}},
	"GET /api/v1/asks/{id}/followers":                 {Status: http.StatusOK, Response: askFollowersRead{}},
	"PUT /api/v1/asks/{id}/followers/{session_id}":    {Status: http.StatusNoContent, Request: actorInput{}},
	"DELETE /api/v1/asks/{id}/followers/{session_id}": {Status: http.StatusNoContent, Request: actorInput{}},
	"PATCH /api/v1/asks/{id}":                         {Status: http.StatusOK, Response: model.Ask{}, Request: editAskInput{}},
	"POST /api/v1/asks/{id}/answer":                   {Status: http.StatusOK, Response: model.Ask{}, Request: answerAskInput{}},
	"POST /api/v1/asks/{id}/resolve":                  {Status: http.StatusOK, Response: model.Ask{}, Request: resolveAskInput{}},
	"GET /api/v1/issues/{key}/comments": {
		Status: http.StatusOK, Response: []model.Comment{}, Params: issueCommentsQuery{},
	},
	"POST /api/v1/issues/{key}/comments": {
		Status: http.StatusCreated, Response: commentWriteResponse{}, Request: commentInput{}, Advice: writeAdvice{},
	},
	"POST /api/v1/comments/{id}/resolve": {Status: http.StatusOK, Response: model.Comment{}, Request: actorInput{}},
	"POST /api/v1/comments/{id}/reopen":  {Status: http.StatusOK, Response: model.Comment{}, Request: actorInput{}},
	"GET /api/v1/comments/{id}":          {Status: http.StatusOK, Response: commentRead{}},
	"GET /api/v1/comments/{id}/blocks":   {Status: http.StatusOK, Response: model.ItemBlocks{}},
	"POST /api/v1/comments/{id}/deliveries": {
		Status: http.StatusCreated, Response: model.CommentDelivery{}, Request: createCommentDeliveryInput{},
	},
	"POST /api/v1/comments/{id}/reply": {
		Status: http.StatusCreated, Response: commentWriteResponse{}, Request: commentReplyInput{},
		// An attempt already answered is answered the stored reply, 200; an error report
		// answers the attempt itself (comment_delivery.go).
		Also: []routeAnswer{
			{Status: http.StatusOK, Body: model.Comment{}},
			{Status: http.StatusOK, Body: model.CommentDelivery{}},
		},
	},
	"POST /api/v1/comments/{id}/accept": {Status: http.StatusOK, Response: acceptedComment{}, Request: actorInput{}},
	"PATCH /api/v1/comments/{id}":       {Status: http.StatusOK, Response: model.Comment{}, Request: editCommentInput{}},
	"POST /api/v1/comments/{id}/reject": {Status: http.StatusOK, Response: model.Comment{}, Request: actorInput{}},
	"GET /api/v1/artifacts/{id}/asks":   {Status: http.StatusOK, Response: []model.Ask{}, Params: artifactAsksQuery{}},
	"POST /api/v1/artifacts/{id}/asks": {
		Status: http.StatusCreated, Response: model.Ask{}, Request: createAskInput{}, Advice: writeAdvice{},
	},
	"GET /api/v1/artifacts/{id}/comments": {Status: http.StatusOK, Response: []model.Comment{}},
	"POST /api/v1/artifacts/{id}/comments": {
		Status: http.StatusCreated, Response: commentWriteResponse{}, Request: commentInput{}, Advice: writeAdvice{},
	},
	"GET /api/v1/artifacts/{id}/events":                      {Status: http.StatusOK, Response: []model.Event{}, Params: eventListQuery{}},
	"GET /api/v1/artifacts/{id}/references":                  {Status: http.StatusOK, Response: model.ArtifactReferences{}},
	"GET /api/v1/artifacts/{id}/subscribers":                 {Status: http.StatusOK, Response: []subscriber{}},
	"DELETE /api/v1/artifacts/{id}/subscribers/{session_id}": {Status: http.StatusNoContent},
	"GET /api/v1/artifacts/{id}":                             {Status: http.StatusOK, Response: model.Artifact{}},
	"POST /api/v1/artifacts/{id}/rebuild": {
		Status: http.StatusOK, Response: docs.RebuildReport{}, Request: rebuildArtifactInput{},
	},
	"GET /api/v1/artifacts/{id}/reviews": {Status: http.StatusOK, Response: []model.ArtifactReview{}},
	"POST /api/v1/artifacts/{id}/reviews": {
		Status: http.StatusCreated, Response: model.ArtifactReview{}, Request: createArtifactReviewInput{},
	},
	"POST /api/v1/artifacts/{id}/plan-reviews": {
		Status: http.StatusCreated, Response: model.ArtifactReview{}, Request: createPlanReviewInput{},
	},
	"POST /api/v1/artifacts/{id}/approval-requests": {
		Status: http.StatusCreated, Response: approvalRequestResponse{}, Request: approvalRequestInput{},
		// An open request that already stood, or a document already approved throughout,
		// answers the same shape at 200: nothing was appended (reviews.go).
		Also: []routeAnswer{{Status: http.StatusOK, Body: approvalRequestResponse{}}},
	},
	"GET /api/v1/artifacts/{id}/blocks":            {Status: http.StatusOK, Response: []model.ArtifactBlock{}},
	"GET /api/v1/artifacts/{id}/blocks/{block_id}": {Status: http.StatusOK, Response: model.BlockPath{}},
	"POST /api/v1/artifacts/{id}/blocks/{block_id}/keep": {
		Status: http.StatusOK, Response: model.DecisionKeep{}, Request: keepDecisionInput{},
	},
	"GET /api/v1/artifacts/{id}/text": {Status: http.StatusOK, Response: artifactText{}},
	"GET /api/v1/artifacts/{id}/versions/{number}": {
		Status: http.StatusOK, Response: artifactVersionText{},
		Opaque:            "a file version answers that file's bytes under its own content type (serveFileVersion); only a document version carries the JSON above",
		OpaqueContentType: "application/octet-stream",
	},
	"POST /api/v1/artifacts/{id}/versions": {
		Status: http.StatusCreated, Response: model.Version{}, Request: createVersionInput{},
	},
	"POST /api/v1/artifacts/{id}/edits": {
		Status: http.StatusOK, Response: editArtifactResponse{}, Request: editArtifactInput{}, Advice: writeAdvice{},
		Refusals: []routeAnswer{
			{Status: http.StatusConflict, Body: editPreconditionError{}},
			{Status: http.StatusConflict, Body: editLostError{}},
			{Status: http.StatusConflict, Body: ambiguousTargetError{}},
		},
	},
	"GET /api/v1/issues/{key}/artifacts/{slug}":        {Status: http.StatusOK, Response: model.Artifact{}},
	"GET /api/v1/issues/{key}/artifacts/{slug}/text":   {Status: http.StatusOK, Response: artifactText{}},
	"GET /api/v1/issues/{key}/artifacts/{slug}/blocks": {Status: http.StatusOK, Response: []model.ArtifactBlock{}},
	"GET /api/v1/issues/{key}/artifacts/{slug}/versions/{number}": {
		Status: http.StatusOK, Response: artifactVersionText{},
		Opaque:            "a file version answers that file's bytes under its own content type (serveFileVersion); only a document version carries the JSON above",
		OpaqueContentType: "application/octet-stream",
	},
	"POST /api/v1/issues/{key}/artifacts/{slug}/versions": {
		Status: http.StatusCreated, Response: model.Version{}, Request: createVersionInput{},
	},
	"POST /api/v1/issues/{key}/artifacts/{slug}/edits": {
		Status: http.StatusOK, Response: editArtifactResponse{}, Request: editArtifactInput{}, Advice: writeAdvice{},
		Refusals: []routeAnswer{
			{Status: http.StatusConflict, Body: editPreconditionError{}},
			{Status: http.StatusConflict, Body: editLostError{}},
			{Status: http.StatusConflict, Body: ambiguousTargetError{}},
		},
	},
	"GET /api/v1/projects/{key}/artifacts/{slug}":         {Status: http.StatusOK, Response: model.Artifact{}},
	"GET /api/v1/projects/{key}/artifacts/{slug}/summary": {Status: http.StatusOK, Response: documentSummary{}},
	"GET /api/v1/projects/{key}/artifacts/{slug}/text":    {Status: http.StatusOK, Response: artifactText{}},
	"GET /api/v1/projects/{key}/artifacts/{slug}/versions/{number}": {
		Status: http.StatusOK, Response: artifactVersionText{},
		Opaque:            "a file version answers that file's bytes under its own content type (serveFileVersion); only a document version carries the JSON above",
		OpaqueContentType: "application/octet-stream",
	},
	"GET /api/v1/projects/{key}/artifacts/{slug}/blocks": {Status: http.StatusOK, Response: []model.ArtifactBlock{}},
	"POST /api/v1/projects/{key}/artifacts/{slug}/versions": {
		Status: http.StatusCreated, Response: model.Version{}, Request: createVersionInput{},
	},
	"POST /api/v1/projects/{key}/artifacts/{slug}/edits": {
		Status: http.StatusOK, Response: editArtifactResponse{}, Request: editArtifactInput{},
		// A project document has no issue state to report (artifacts.go, the withRawAdvice arm).
		Advice: documentWriteAdvice{},
		Refusals: []routeAnswer{
			{Status: http.StatusConflict, Body: editPreconditionError{}},
			{Status: http.StatusConflict, Body: editLostError{}},
			{Status: http.StatusConflict, Body: ambiguousTargetError{}},
		},
	},
	"GET /api/v1/me/state": {Status: http.StatusOK, Response: userState{}},
	"PUT /api/v1/me/issues/{key}/state": {
		Status: http.StatusOK, Response: userIssueState{}, Request: userIssueStateInput{},
		Refusals: []routeAnswer{{Status: http.StatusConflict, Body: userStateStaleError{}}},
	},
	"GET /api/v1/me/agents/state": {Status: http.StatusOK, Response: userAgentStates{}},
	"PUT /api/v1/me/agents/{session_id}/state": {
		Status: http.StatusOK, Response: userAgentState{}, Request: userAgentStateInput{},
	},
	"GET /api/v1/me/documents/{id}/read": {Status: http.StatusOK, Response: model.DocumentRead{}},
	"PUT /api/v1/me/documents/{id}/read": {
		Status: http.StatusOK, Response: model.DocumentRead{}, Request: documentReadInput{},
	},
	"PUT /api/v1/me/asks/{id}/snooze": {
		Status: http.StatusOK, Response: userAskSnooze{}, Request: askSnoozeInput{},
	},
	"DELETE /api/v1/me/asks/{id}/snooze": {Status: http.StatusNoContent},
	// The relay rows. Dispatch writes the broker's bytes unparsed (relayBrokerResponse), so each
	// Response names the shape the broker's own handler answers with, declared in
	// internal/broker/wire and matched through the agentsecrets.Client method the handler calls.
	"GET /api/v1/credential-requests": {
		// GET /v1/pending (agentsecrets.Client.Pending).
		Status: http.StatusOK, Response: wire.PendingResponse{}, Params: credentialApproverQuery{},
		// A Dispatch with no broker answers 200 and the JSON literal `null` rather than
		// 404 FEATURE_OFF, so every page of such a deployment reads this route cleanly.
		Nullable: true,
	},
	"POST /api/v1/credential-requests/{id}/approve": {
		// POST /v1/credential-requests/{record}/approve (agentsecrets.Client.Approve).
		Status: http.StatusOK, Response: wire.DecisionResponse{}, Request: credentialDecisionInput{},
	},
	"POST /api/v1/credential-requests/{id}/deny": {
		// POST /v1/credential-requests/{record}/deny (agentsecrets.Client.Deny), which answers
		// the terminal state rather than a credential.
		Status: http.StatusOK, Response: wire.StateResponse{}, Request: credentialDecisionInput{},
	},
	"POST /api/v1/credential-requests/machine-lookup": {
		// POST /v1/machine-logins/lookup (agentsecrets.Client.MachineLookup). Dispatch forwards
		// the body to the broker unparsed (readRelayBody) rather than reading it, but the broker
		// does read it, so the route takes the shape the broker declares.
		Status: http.StatusOK, Response: wire.RecordResponse{}, Request: wire.MachineLookupRequest{},
	},
	"GET /api/v1/credential-grants": {
		// GET /v1/grants (agentsecrets.Client.Grants).
		Status: http.StatusOK, Response: wire.ApproverGrantsResponse{}, Params: credentialApproverQuery{},
	},
	"POST /api/v1/credential-grants/{id}/revoke": {
		// POST /v1/grants/{id}/revoke-by-approver (agentsecrets.Client.RevokeByApprover).
		Status: http.StatusOK, Response: wire.StateResponse{},
	},
	"GET /api/v1/machine-logins": {
		// GET /v1/launcher-credentials (agentsecrets.Client.MachineLogins).
		Status: http.StatusOK, Response: wire.LauncherCredentialsResponse{},
	},
	"POST /api/v1/machine-logins/{id}/revoke": {
		// POST /v1/launcher-credentials/{id}/revoke-by-approver
		// (agentsecrets.Client.RevokeMachineLogin).
		Status: http.StatusOK, Response: wire.StateResponse{},
	},
	"GET /api/v1/signins": {
		// GET /v1/person-signins (agentsecrets.Client.SignIns).
		Status: http.StatusOK, Response: wire.PersonSignInsResponse{},
	},
	"POST /api/v1/signins/{id}/revoke": {
		// POST /v1/person-signins/{id}/revoke (agentsecrets.Client.RevokeSignIn), which says how
		// many access tokens ended with the sign-in.
		Status: http.StatusOK, Response: wire.PersonSignInRevokedResponse{},
	},
	// Not a relay: Dispatch reads one fact off the broker's metadata and answers its own shape.
	"GET /api/v1/credentials/capabilities": {Status: http.StatusOK, Response: credentialCapabilities{}},

	// The test hooks, mounted only with Deps.TestHooksEnabled (routes()).
	"POST /api/v1/live/_test/disconnect":               {Status: http.StatusOK, Response: testHookOK{}},
	"POST /api/v1/live/_test/rotate":                   {Status: http.StatusOK, Response: testHookOK{}},
	"POST /api/v1/artifacts/_test/quiesce":             {Status: http.StatusOK, Response: testHookOK{}},
	"POST /api/v1/artifacts/{id}/_test/outside-schema": {Status: http.StatusNoContent},
	"POST /api/v1/agents/{session_id}/stream/_test/publish": {
		Status: http.StatusOK, Response: testHookOK{}, Params: publishFrameQuery{},
		// The body is the frame's own bytes, read whole and published unparsed; only
		// ?as=responder reads it, as {"responding"} (publishFrameResponderInput,
		// agent_stream.go). Naming that one arm would state a contract the route does not
		// hold its caller to, so the body is an object of any shape.
		OpaqueRequest: "the frame to publish, read whole and published unparsed; under " +
			"?as=responder it is read as {\"responding\": bool} instead",
	},
	"GET /api/v1/artifacts/_test/rooms":                          {Status: http.StatusOK, Response: residentRooms{}},
	"GET /api/v1/artifacts/{id}/_test/room-counters":             {Status: http.StatusOK, Response: docs.RoomCounters{}},
	"POST /api/v1/artifacts/{id}/_test/hold-persistence":         {Status: http.StatusNoContent, Request: holdPersistenceInput{}},
	"PUT /api/v1/artifacts/{id}/_test/live-write-pauses/{point}": {Status: http.StatusNoContent},
	"GET /api/v1/artifacts/{id}/_test/live-write-pauses/{point}": {
		Status: http.StatusOK, Response: liveWritePauseState{}, Params: liveWritePauseQuery{},
	},
	"DELETE /api/v1/artifacts/{id}/_test/live-write-pauses/{point}": {Status: http.StatusOK, Response: liveWritePauseState{}},
	"POST /api/v1/writes/_test/slow":                                {Status: http.StatusOK, Response: testHookOK{}, Request: slowWriteInput{}},
}

// shapelessRoutes names every mounted row routeShapes does not name. A row with no shape is a
// row the generated document would leave out, which is how "one definition of the API" stops
// holding: a client would be given no type for it and would hand-write one. register refuses to
// mount such a table, as it refuses a write class it cannot decide (undecidableWrites) and a
// scope nothing serves (scopedRoutesNotMounted).
func shapelessRoutes(routes []apiRoute) []string {
	var shapeless []string
	for _, route := range routes {
		name := route.Method + " " + route.Pattern
		if _, shaped := routeShapes[name]; !shaped {
			shapeless = append(shapeless, name+": no shape (routeShapes)")
		}
	}
	sort.Strings(shapeless)
	return shapeless
}

// shapedRoutesNotMounted names every key of routeShapes that is no row of the table. A shape for
// a route nothing serves is a shape the document would describe and no caller could reach - a row
// renamed or deleted and its entry left behind. The walk is over the table with its test hooks
// in, for the reason scopedRoutesNotMounted gives: a hook's row is a row of the table wherever a
// deployment leaves it out.
func shapedRoutesNotMounted() []string {
	return routesNotMounted(maps.Keys(routeShapes))
}
