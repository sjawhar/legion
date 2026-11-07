package api

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"sort"
	"strings"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// routeAuth names who may call a route. It describes the check the handler performs; the
// handler stays the enforcement point.
type routeAuth string

const (
	// authPublic needs no credential.
	authPublic routeAuth = "public"
	// authAny accepts any authenticated caller: a browser cookie or a bearer token. What such a
	// caller may do there is the route's own scopes' to decide (routeScopes), which a person's
	// credential holds and a credential the deployment configured holds none of.
	authAny routeAuth = "any"
	// authBearer accepts a bearer token only.
	authBearer routeAuth = "bearer"
)

// writeScope is what a mutating route's write belongs to, which is what lets one check decide a
// caller whose credential names one project (project_scope.go). Unlike routeAuth, which
// describes a check its handler makes, this is the enforcement point itself: register wraps
// every mutating route in it.
type writeScope string

const (
	// scopeProject is a write to an object that belongs to one project: the issue-key rows, the
	// project-key rows, the artifact-id rows, the ask-id rows, the comment-id rows, and the
	// message rows that carry an issue. A caller whose credential names another project is
	// refused, naming both projects.
	scopeProject writeScope = "project"
	// scopeSession is a write that belongs to the calling session rather than to any project:
	// an agent conversation's own artifact, messages and stream frames, the two routes a
	// session answers its own delivery on, and the caller-private `me` rows. There is no
	// project to compare, and each row is held to its caller another way, each one read:
	//
	//   - The rows whose path names a session — the `agents/{session_id}` writes and
	//     `PUT /api/v1/me/agents/{session_id}/state` — are held to it by withinSessionScope,
	//     which refuses a credential that proved another session (project_scope.go). Only a
	//     broker-issued token proves one; `POST /api/v1/agents/{session_id}/messages`, the
	//     stream publish and the `me` state row need the `person` scope besides, since a person
	//     writing to a session is writing to someone else's conversation by design, and that
	//     scope is what lets their own credential past the session check (withinSessionScope).
	//   - `POST /api/v1/messages/{id}/deliveries/{attempt}/accept` and
	//     `POST /api/v1/messages/{id}/reply` take the session from the caller's own credential
	//     or `actor` and refuse an attempt that is not that session's, `403 ACCEPT_FORBIDDEN`
	//     and `403 REPLY_FORBIDDEN`.
	//   - The `me` token rows name no session at all: they are the calling person's, read and
	//     written by a credential holding the `person` scope and keyed on that person.
	scopeSession writeScope = "session"
	// scopeDeployment is a write that belongs to the deployment rather than to any project: a
	// credential that may write one project may not create another or change a deployment-wide
	// setting, so a project-scoped caller is refused these by name.
	scopeDeployment writeScope = "deployment"
	// scopeProjectInBody is a write that belongs to one project the request's body names rather
	// than its path: the issue create, whose project comes from the body, or from the
	// repository an external ref names. There is no object to resolve before the handler runs,
	// so createIssue makes the check itself once that project is settled
	// (refuseWriteOutsideProjectScope), and withinProjectScope passes the row through by this
	// class rather than by a name it compares.
	scopeProjectInBody writeScope = "project-in-body"
)

// writeClasses is the class of every mutating route, by "<method> <pattern>". It stands beside
// the table rather than in each row because Go leaves no field out of a positional composite
// literal: a sixth value on the row would have to be written on every row of the table, every GET row
// included, and a GET row needs no class. routes() stamps it onto the row, so apiRoute.Scope is
// what every reader of a route sees, and undecidableWrites holds the two in exact correspondence
// — a mutating row this table does not name, and a name that is no mutating row, each fail
// TestEveryMutatingRouteIsClassed and refuse to mount (register).
var writeClasses = map[string]writeScope{
	// The rows that write an object belonging to one project: its issue, its project, its
	// document, its ask, its comment, or a message on one of its issues. The issue create
	// leads them by path and is the one of them whose project its address does not name.
	"POST /api/v1/issues":                                  scopeProjectInBody,
	"PATCH /api/v1/issues/{key}":                           scopeProject,
	"POST /api/v1/issues/{key}/claim":                      scopeProject,
	"DELETE /api/v1/issues/{key}/claim":                    scopeProject,
	"DELETE /api/v1/issues/{key}/subscribers/{session_id}": scopeProject,
	"POST /api/v1/issues/{key}/artifacts":                  scopeProject,
	"POST /api/v1/issues/{key}/artifacts/{slug}/versions":  scopeProject,
	"POST /api/v1/issues/{key}/artifacts/{slug}/edits":     scopeProject,
	"POST /api/v1/issues/{key}/messages":                   scopeProject,
	"POST /api/v1/issues/{key}/asks":                       scopeProject,
	"POST /api/v1/issues/{key}/comments":                   scopeProject,
	"POST /api/v1/issues/{key}/results":                    scopeProject,
	"POST /api/v1/issues/{key}/handoffs/{phase}":           scopeProject,

	"POST /api/v1/projects/{key}/artifacts":                                scopeProject,
	"POST /api/v1/projects/{key}/artifacts/{slug}/versions":                scopeProject,
	"POST /api/v1/projects/{key}/artifacts/{slug}/edits":                   scopeProject,
	"PUT /api/v1/projects/{key}/default-assignee":                          scopeProject,
	"PUT /api/v1/projects/{key}/architecture-sources/{owner}/{repo}":       scopeProject,
	"DELETE /api/v1/projects/{key}/architecture-sources/{owner}/{repo}":    scopeProject,
	"POST /api/v1/projects/{key}/architecture-sources/{owner}/{repo}/sync": scopeProject,
	"POST /api/v1/projects/{key}/architecture-sources/sync":                scopeProject,
	"POST /api/v1/projects/{key}/architecture-source/sync":                 scopeProject,
	// The POST form of the related-item check writes nothing, but it reads the project its path
	// names, so a credential scoped to one project runs it over that project and no other.
	"POST /api/v1/projects/{key}/related": scopeProject,

	"POST /api/v1/artifacts/{id}/blocks/{block_id}/keep":            scopeProject,
	"POST /api/v1/artifacts/{id}/asks":                              scopeProject,
	"POST /api/v1/artifacts/{id}/comments":                          scopeProject,
	"DELETE /api/v1/artifacts/{id}/subscribers/{session_id}":        scopeProject,
	"POST /api/v1/artifacts/{id}/rebuild":                           scopeProject,
	"POST /api/v1/artifacts/{id}/reviews":                           scopeProject,
	"POST /api/v1/artifacts/{id}/plan-reviews":                      scopeProject,
	"POST /api/v1/artifacts/{id}/approval-requests":                 scopeProject,
	"POST /api/v1/artifacts/{id}/versions":                          scopeProject,
	"POST /api/v1/artifacts/{id}/edits":                             scopeProject,
	"POST /api/v1/artifacts/{id}/_test/outside-schema":              scopeProject,
	"POST /api/v1/artifacts/{id}/_test/hold-persistence":            scopeProject,
	"PUT /api/v1/artifacts/{id}/_test/live-write-pauses/{point}":    scopeProject,
	"DELETE /api/v1/artifacts/{id}/_test/live-write-pauses/{point}": scopeProject,

	"PUT /api/v1/asks/{id}/followers/{session_id}":    scopeProject,
	"DELETE /api/v1/asks/{id}/followers/{session_id}": scopeProject,
	"PATCH /api/v1/asks/{id}":                         scopeProject,
	"POST /api/v1/asks/{id}/answer":                   scopeProject,
	"POST /api/v1/asks/{id}/resolve":                  scopeProject,

	"POST /api/v1/comments/{id}/resolve":    scopeProject,
	"POST /api/v1/comments/{id}/reopen":     scopeProject,
	"POST /api/v1/comments/{id}/deliveries": scopeProject,
	"POST /api/v1/comments/{id}/reply":      scopeProject,
	"POST /api/v1/comments/{id}/accept":     scopeProject,
	"PATCH /api/v1/comments/{id}":           scopeProject,
	"POST /api/v1/comments/{id}/reject":     scopeProject,

	"POST /api/v1/messages/{id}/deliveries": scopeProject,

	// The question an answer run is recorded for belongs to the project its body names, as an
	// issue create's does, so a credential scoped to one project asks only of that project
	// (createGoingRequest makes the check itself).
	"POST /api/v1/going/requests": scopeProjectInBody,

	// The rows that write what belongs to the calling session: its own conversation, its answer
	// to its own delivery, and the caller-private `me` rows.
	"POST /api/v1/messages/{id}/deliveries/{attempt}/accept": scopeSession,
	"POST /api/v1/messages/{id}/reply":                       scopeSession,
	"POST /api/v1/agents/{session_id}/messages":              scopeSession,
	"POST /api/v1/agents/{session_id}/artifacts":             scopeSession,
	"POST /api/v1/agents/{session_id}/stream/_test/publish":  scopeSession,
	"PUT /api/v1/me/documents/{id}/read":                     scopeSession,
	"POST /api/v1/me/agent-tokens":                           scopeSession,
	"PATCH /api/v1/me/agent-tokens/{id}":                     scopeSession,
	"DELETE /api/v1/me/agent-tokens/{id}":                    scopeSession,
	"PUT /api/v1/me/threads/{id}/read":                       scopeSession,
	"POST /api/v1/me/inbox/read":                             scopeSession,
	"POST /api/v1/me/inbox/dismiss":                          scopeSession,
	"PUT /api/v1/me/issues/{key}/state":                      scopeSession,
	"PUT /api/v1/me/agents/{session_id}/state":               scopeSession,
	"PUT /api/v1/me/asks/{id}/snooze":                        scopeSession,
	"DELETE /api/v1/me/asks/{id}/snooze":                     scopeSession,

	// The rows that write what belongs to the deployment: a project-scoped credential may not
	// create another project, remap a repository, change a deployment-wide setting, broadcast,
	// decide a credential, or reach into this process.
	"POST /api/v1/projects":                                scopeDeployment,
	"PUT /api/v1/settings/repo-projects/{owner}/{repo}":    scopeDeployment,
	"DELETE /api/v1/settings/repo-projects/{owner}/{repo}": scopeDeployment,
	"PUT /api/v1/settings/delivery":                        scopeDeployment,
	// Which daemon a project's Legion state is read from, and the operator's relayed change to
	// that daemon's own legion.yaml: each names a project, and neither belongs to it. Pointing
	// Dispatch at a host, and changing what a daemon runs on, are the deployment's writes, so a
	// credential that may write one project's issues is refused both by name.
	"PUT /api/v1/settings/legion/{project}":           scopeDeployment,
	"DELETE /api/v1/settings/legion/{project}":        scopeDeployment,
	"PUT /api/v1/legion/{project}/settings":           scopeDeployment,
	"POST /api/v1/delivery/session-groups":            scopeDeployment,
	"PATCH /api/v1/delivery/session-groups/{id}":      scopeDeployment,
	"DELETE /api/v1/delivery/session-groups/{id}":     scopeDeployment,
	"POST /api/v1/broadcasts":                         scopeDeployment,
	"POST /api/v1/credential-requests/{id}/approve":   scopeDeployment,
	"POST /api/v1/credential-requests/{id}/deny":      scopeDeployment,
	"POST /api/v1/credential-requests/machine-lookup": scopeDeployment,
	"POST /api/v1/credential-grants/{id}/revoke":      scopeDeployment,
	"POST /api/v1/machine-logins/{id}/revoke":         scopeDeployment,
	"POST /api/v1/signins/{id}/revoke":                scopeDeployment,
	"POST /api/v1/live/_test/disconnect":              scopeDeployment,
	"POST /api/v1/live/_test/rotate":                  scopeDeployment,
	"POST /api/v1/artifacts/_test/quiesce":            scopeDeployment,
	"POST /api/v1/writes/_test/slow":                  scopeDeployment,
}

// idRoute is what the id wildcards of one route's pattern name, which is what the prefix layer
// reads before the scope checks and the handler see those values (resolveIDPrefixes).
//
// Kind is the kind the pattern's {id} names — ask, comment, message or artifact, as
// refs.ResolvePrefix reads a kind — and WithinIssue whether a prefix of it is looked for among
// the items of the issue the pattern's {key} names rather than among every item of that kind.
// Blocks is whether the pattern's {block_id} is a block of the document its {id} names, matched
// among that document's blocks once the {id} itself is resolved.
//
// NotFound is the code of the 404 this route already answers for a whole id of its kind that
// names no item, which is the answer a prefix that names none gets too: a short id resolves
// wherever a whole one does, so it fails where a whole one fails. It is read from the route
// rather than assumed, and the routes disagree — a document's read answers ARTIFACT_NOT_FOUND,
// a comment's thread NOT_FOUND and a comment's delivery COMMENT_NOT_FOUND — so it is written
// per route and not per kind. BlockNotFound is the same for the {block_id}. notFoundNone is the
// route that answers no 404 at all for an id it does not hold, which then keeps that answer.
//
// Reason is why a route looks up no prefix, written exactly where Kind is empty and Blocks is
// false: an id of a kind refs.ResolvePrefix does not read, so nothing is looked for and the
// handler makes of the value what it always made of it. Such a route is wrapped all the same,
// because its {id} is still rewritten to the canonical spelling where it is a whole uuid
// written another of uuid.Parse's ways, as it is on every route that takes an id
// (canonicalizeUUIDPath).
type idRoute struct {
	Kind          string
	WithinIssue   bool
	Blocks        bool
	NotFound      string
	BlockNotFound string
	Reason        string
}

// notFoundNone is the NotFound of a route that answers no 404 for an id it does not hold, so a
// prefix that names nothing is left as written and that route answers what it always answers.
// Only DELETE /api/v1/me/asks/{id}/snooze is one: un-snoozing a row nobody snoozed is the state
// the caller asked for, and it answers 204 whatever ask the id names.
const notFoundNone = "none"

// resolves reports whether the route looks a prefix up at all, which is what makes Reason
// required. It does not decide the wrapping: register wraps every route idRoutes names, since
// one that resolves nothing still has its {id} canonicalized there (resolveIDPrefixes).
func (route idRoute) resolves() bool { return route.Kind != "" || route.Blocks }

// idRoutes is, by "<method> <pattern>", what the {id} and {block_id} of every route that
// carries one name: the kind whose prefix is resolved into it and the 404 that route answers
// for an id of that kind it does not hold, or the reason it resolves none. It stands beside the
// table for writeClasses' reason — the columns would have to be written on every row of the table, and
// most rows carry no id at all — and it is keyed by method and pattern as writeClasses is,
// because two methods of one pattern can answer a missing row differently: PUT
// /api/v1/me/asks/{id}/snooze answers 404 and DELETE answers 204.
//
// TestEveryIDRouteIsNamed holds the two in exact correspondence: a route with an id wildcard
// this table does not name, a row that names neither a kind nor a reason, a resolving row with
// no 404 of its own, and a name that is no route the server serves, each fail it, so a route
// added later cannot silently stop resolving prefixes.
//
// The ids this layer never sees are no part of it: an id a request's body carries (reply_to,
// ask_id, in_reply_to) and the ?artifact= filters are in no path value, and the resolver that
// rewrites a body's short ids as its text is indexed is their one reader (refs.Resolver).
var idRoutes = map[string]idRoute{
	// A document's own routes, and the caller's read mark on one: the {id} is an artifact,
	// looked for among every document and file, as a whole artifact id is. Every one of them
	// reads the row through loadArtifact, which answers ARTIFACT_NOT_FOUND.
	"GET /api/v1/artifacts/{id}":                             {Kind: "artifact", NotFound: "ARTIFACT_NOT_FOUND"},
	"POST /api/v1/artifacts/{id}/_test/outside-schema":       {Kind: "artifact", NotFound: "ARTIFACT_NOT_FOUND"},
	"POST /api/v1/artifacts/{id}/approval-requests":          {Kind: "artifact", NotFound: "ARTIFACT_NOT_FOUND"},
	"GET /api/v1/artifacts/{id}/asks":                        {Kind: "artifact", NotFound: "ARTIFACT_NOT_FOUND"},
	"POST /api/v1/artifacts/{id}/asks":                       {Kind: "artifact", NotFound: "ARTIFACT_NOT_FOUND"},
	"GET /api/v1/artifacts/{id}/blocks":                      {Kind: "artifact", NotFound: "ARTIFACT_NOT_FOUND"},
	"GET /api/v1/artifacts/{id}/comments":                    {Kind: "artifact", NotFound: "ARTIFACT_NOT_FOUND"},
	"POST /api/v1/artifacts/{id}/comments":                   {Kind: "artifact", NotFound: "ARTIFACT_NOT_FOUND"},
	"POST /api/v1/artifacts/{id}/edits":                      {Kind: "artifact", NotFound: "ARTIFACT_NOT_FOUND"},
	"GET /api/v1/artifacts/{id}/events":                      {Kind: "artifact", NotFound: "ARTIFACT_NOT_FOUND"},
	"POST /api/v1/artifacts/{id}/plan-reviews":               {Kind: "artifact", NotFound: "ARTIFACT_NOT_FOUND"},
	"POST /api/v1/artifacts/{id}/rebuild":                    {Kind: "artifact", NotFound: "ARTIFACT_NOT_FOUND"},
	"GET /api/v1/artifacts/{id}/references":                  {Kind: "artifact", NotFound: "ARTIFACT_NOT_FOUND"},
	"GET /api/v1/artifacts/{id}/reviews":                     {Kind: "artifact", NotFound: "ARTIFACT_NOT_FOUND"},
	"POST /api/v1/artifacts/{id}/reviews":                    {Kind: "artifact", NotFound: "ARTIFACT_NOT_FOUND"},
	"GET /api/v1/artifacts/{id}/subscribers":                 {Kind: "artifact", NotFound: "ARTIFACT_NOT_FOUND"},
	"DELETE /api/v1/artifacts/{id}/subscribers/{session_id}": {Kind: "artifact", NotFound: "ARTIFACT_NOT_FOUND"},
	"GET /api/v1/artifacts/{id}/text":                        {Kind: "artifact", NotFound: "ARTIFACT_NOT_FOUND"},
	"POST /api/v1/artifacts/{id}/versions":                   {Kind: "artifact", NotFound: "ARTIFACT_NOT_FOUND"},
	"GET /api/v1/artifacts/{id}/versions/{number}":           {Kind: "artifact", NotFound: "ARTIFACT_NOT_FOUND"},
	"GET /api/v1/me/documents/{id}/read":                     {Kind: "artifact", NotFound: "ARTIFACT_NOT_FOUND"},
	"PUT /api/v1/me/documents/{id}/read":                     {Kind: "artifact", NotFound: "ARTIFACT_NOT_FOUND"},

	// The two routes that also name a block of that document, each answering TARGET_NOT_FOUND
	// for a block id the document does not hold.
	"GET /api/v1/artifacts/{id}/blocks/{block_id}":       {Kind: "artifact", Blocks: true, NotFound: "ARTIFACT_NOT_FOUND", BlockNotFound: "TARGET_NOT_FOUND"},
	"POST /api/v1/artifacts/{id}/blocks/{block_id}/keep": {Kind: "artifact", Blocks: true, NotFound: "ARTIFACT_NOT_FOUND", BlockNotFound: "TARGET_NOT_FOUND"},

	// An ask's own routes and the caller's snooze of one. Each reads the row through loadAsk
	// or loadAskOwner, whose missing row is writeHandlerError's NOT_FOUND; the snooze write
	// answers its own NOT_FOUND, and the un-snooze answers 204 for any ask at all.
	"GET /api/v1/asks/{id}":                           {Kind: "ask", NotFound: "NOT_FOUND"},
	"PATCH /api/v1/asks/{id}":                         {Kind: "ask", NotFound: "NOT_FOUND"},
	"POST /api/v1/asks/{id}/answer":                   {Kind: "ask", NotFound: "NOT_FOUND"},
	"POST /api/v1/asks/{id}/resolve":                  {Kind: "ask", NotFound: "NOT_FOUND"},
	"GET /api/v1/asks/{id}/followers":                 {Kind: "ask", NotFound: "NOT_FOUND"},
	"PUT /api/v1/asks/{id}/followers/{session_id}":    {Kind: "ask", NotFound: "NOT_FOUND"},
	"DELETE /api/v1/asks/{id}/followers/{session_id}": {Kind: "ask", NotFound: "NOT_FOUND"},
	"PUT /api/v1/me/asks/{id}/snooze":                 {Kind: "ask", NotFound: "NOT_FOUND"},
	"DELETE /api/v1/me/asks/{id}/snooze":              {Kind: "ask", NotFound: notFoundNone},

	// A comment's own routes. The reads and the thread writes answer writeHandlerError's
	// NOT_FOUND; the two delivery routes answer COMMENT_NOT_FOUND of their own.
	"GET /api/v1/comments/{id}":             {Kind: "comment", NotFound: "NOT_FOUND"},
	"PATCH /api/v1/comments/{id}":           {Kind: "comment", NotFound: "NOT_FOUND"},
	"GET /api/v1/comments/{id}/blocks":      {Kind: "comment", NotFound: "NOT_FOUND"},
	"POST /api/v1/comments/{id}/accept":     {Kind: "comment", NotFound: "NOT_FOUND"},
	"POST /api/v1/comments/{id}/reject":     {Kind: "comment", NotFound: "NOT_FOUND"},
	"POST /api/v1/comments/{id}/reopen":     {Kind: "comment", NotFound: "NOT_FOUND"},
	"POST /api/v1/comments/{id}/resolve":    {Kind: "comment", NotFound: "NOT_FOUND"},
	"POST /api/v1/comments/{id}/deliveries": {Kind: "comment", NotFound: "COMMENT_NOT_FOUND"},
	"POST /api/v1/comments/{id}/reply":      {Kind: "comment", NotFound: "COMMENT_NOT_FOUND"},

	// A message, by any id in its conversation. The issue form names an owner, so a prefix of
	// it is looked for among that issue's messages alone; the owner-free forms look among
	// every message, a direct conversation's included. The reads answer NOT_FOUND and the
	// three delivery routes MESSAGE_NOT_FOUND.
	"GET /api/v1/messages/{id}":                              {Kind: "message", NotFound: "NOT_FOUND"},
	"GET /api/v1/messages/{id}/blocks":                       {Kind: "message", NotFound: "NOT_FOUND"},
	"POST /api/v1/messages/{id}/deliveries":                  {Kind: "message", NotFound: "MESSAGE_NOT_FOUND"},
	"POST /api/v1/messages/{id}/deliveries/{attempt}/accept": {Kind: "message", NotFound: "MESSAGE_NOT_FOUND"},
	"POST /api/v1/messages/{id}/reply":                       {Kind: "message", NotFound: "MESSAGE_NOT_FOUND"},
	"GET /api/v1/issues/{key}/messages/{id}":                 {Kind: "message", WithinIssue: true, NotFound: "NOT_FOUND"},

	// The routes whose id names none of the five kinds a prefix shortens — an ask, a comment,
	// a message, a document, a document block — so no prefix is looked for and the handler
	// answers for the value as it always has. A whole uuid an {id} spells another of
	// uuid.Parse's ways is still rewritten to the canonical spelling, as it is on every route
	// that takes an id (canonicalizeUUIDPath).
	"GET /api/v1/broadcasts/{id}":                   {Reason: "a broadcast id: one send to many sessions"},
	"POST /api/v1/credential-grants/{id}/revoke":    {Reason: "a credential grant id"},
	"POST /api/v1/credential-requests/{id}/approve": {Reason: "a credential request id"},
	"POST /api/v1/credential-requests/{id}/deny":    {Reason: "a credential request id"},
	"GET /api/v1/delivery/runs/{id}":                {Reason: "a deploy-repository run id, a positive integer and no uuid at all"},
	"PATCH /api/v1/delivery/session-groups/{id}":    {Reason: "a Delivery session group id"},
	"DELETE /api/v1/delivery/session-groups/{id}":   {Reason: "a Delivery session group id"},
	"GET /api/v1/going/requests/{id}/stream":        {Reason: "a going request's run id, which the route reads for its own author alone and answers for itself"},
	"GET /api/v1/handoffs/{id}":                     {Reason: "a handoff id: one phase's record on an issue"},
	"POST /api/v1/machine-logins/{id}/revoke":       {Reason: "a machine login id"},
	"POST /api/v1/signins/{id}/revoke":              {Reason: "a sign-in id at the secrets broker"},
	"DELETE /api/v1/me/agent-tokens/{id}":           {Reason: "a personal agent token id"},
	"PATCH /api/v1/me/agent-tokens/{id}":            {Reason: "a personal agent token id"},
	"PUT /api/v1/me/threads/{id}/read":              {Reason: "a thread id: the root of a conversation, and no read of the message it names"},

	// The document test hooks, which address the live room this process holds under the
	// document's own id and read no row of the artifacts table, so they answer no 404 of a
	// document that does not exist and resolve no prefix: a short id written at one of them
	// reaches the hook as the caller wrote it, as every other value does.
	"POST /api/v1/artifacts/{id}/_test/hold-persistence":            {Reason: "a test hook's document id, naming a live room rather than a stored document"},
	"GET /api/v1/artifacts/{id}/_test/room-counters":                {Reason: "a test hook's document id, naming a live room rather than a stored document"},
	"PUT /api/v1/artifacts/{id}/_test/live-write-pauses/{point}":    {Reason: "a test hook's document id, naming a live room rather than a stored document"},
	"GET /api/v1/artifacts/{id}/_test/live-write-pauses/{point}":    {Reason: "a test hook's document id, naming a live room rather than a stored document"},
	"DELETE /api/v1/artifacts/{id}/_test/live-write-pauses/{point}": {Reason: "a test hook's document id, naming a live room rather than a stored document"},
}

// hasIDWildcard reports whether a pattern carries an id the prefix layer could resolve.
func hasIDWildcard(pattern string) bool {
	return strings.Contains(pattern, "{id}") || strings.Contains(pattern, "{block_id}")
}

// undecidedIDRoutes names every route whose pattern carries an id wildcard that idRoutes
// leaves undecided: one it does not name, one that resolves nothing and gives no reason, one
// that names a kind and a reason at once, one that resolves an id or a block without saying
// what 404 that route answers for one it does not hold, and one that resolves nothing and
// names such an answer anyway.
func undecidedIDRoutes(routes []apiRoute) []string {
	var undecided []string
	for _, route := range routes {
		if !hasIDWildcard(route.Pattern) {
			continue
		}
		name := route.Method + " " + route.Pattern
		entry, named := idRoutes[name]
		switch {
		case !named:
			undecided = append(undecided, name+": idRoutes says neither what its id names nor why no prefix is resolved there")
		case entry.resolves() && entry.Reason != "":
			undecided = append(undecided, name+": idRoutes names a kind to resolve and a reason not to")
		case !entry.resolves() && strings.TrimSpace(entry.Reason) == "":
			undecided = append(undecided, name+": idRoutes resolves no prefix there and gives no reason")
		case entry.Kind != "" && entry.NotFound == "":
			undecided = append(undecided, name+": idRoutes resolves an id there and does not say what 404 the route answers for one it does not hold")
		case entry.Blocks && entry.BlockNotFound == "":
			undecided = append(undecided, name+": idRoutes resolves a block id there and does not say what 404 the route answers for one the document does not hold")
		case !entry.resolves() && (entry.NotFound != "" || entry.BlockNotFound != ""):
			undecided = append(undecided, name+": idRoutes resolves nothing there and names a not-found answer anyway")
		}
	}
	sort.Strings(undecided)
	return slices.Compact(undecided)
}

// routeScopeSet is the OAuth scopes one route needs of the credential that calls it, every one
// of which that credential must hold (gateRouteScopes). An empty set is a route any authenticated
// caller may reach.
type routeScopeSet []string

// routeScopes is the set each route needs, by "<method> <pattern>", and it stands beside the
// table for the reason writeClasses does: Go leaves no field out of a positional composite
// literal, so a sixth value on the row would have to be written on every row of the table, and most rows
// need no scope. routes() stamps it onto the row, so apiRoute.OAuthScopes is what every reader
// of a route sees — GET /api/v1 and `envoy-dispatch routes` among them — and
// scopedRoutesNotMounted holds the map to the table, a name that is no row refusing to mount
// (register).
//
// These are the routes Dispatch refused every bearer token on until INFRA-1017, and two scopes
// decide them now: `person`, "this credential acts as its person", which every row here needs,
// and `approve`, the right to settle what a person settles, which the ten rows below that
// decide a credential, a sign-in, a review or a token need as well. A person's browser, their
// personal tokens and their machine logins hold both; the shared token, a pod's service-account
// token, a project-scoped session's token and a broker-issued token that names a service and no
// person hold neither.
//
// A route that takes an id written short needs no scope of its own for it: the prefix layer
// (resolveIDPrefixes, idRoutes) runs inside this gate - register wraps the gate around it, so
// the row's scopes are the first thing a request is held to and a credential that holds none is
// refused before any id is read - and it only rewrites the path value, so a short id reaches
// exactly the scopes the whole id reaches, and a row that needs none with a whole id needs none
// with a prefix.
var routeScopes = map[string]routeScopeSet{
	// The rows that read or write what belongs to the calling person: their inbox, their own
	// answers and replies, their read marks, their conversations with sessions, and the people
	// and default assignees an assignee picker reads.
	"GET /api/v1/settings/project-default-assignees":         {model.ScopePerson},
	"PUT /api/v1/projects/{key}/default-assignee":            {model.ScopePerson},
	"GET /api/v1/users":                                      {model.ScopePerson},
	"GET /api/v1/inbox":                                      {model.ScopePerson},
	"GET /api/v1/me/answers":                                 {model.ScopePerson},
	"GET /api/v1/me/replies":                                 {model.ScopePerson},
	"PUT /api/v1/me/threads/{id}/read":                       {model.ScopePerson},
	"POST /api/v1/me/inbox/read":                             {model.ScopePerson},
	"POST /api/v1/me/inbox/dismiss":                          {model.ScopePerson},
	"GET /api/v1/me/state":                                   {model.ScopePerson},
	"PUT /api/v1/me/issues/{key}/state":                      {model.ScopePerson},
	"GET /api/v1/me/agents/state":                            {model.ScopePerson},
	"PUT /api/v1/me/agents/{session_id}/state":               {model.ScopePerson},
	"POST /api/v1/artifacts/{id}/blocks/{block_id}/keep":     {model.ScopePerson},
	"GET /api/v1/me/documents/{id}/read":                     {model.ScopePerson},
	"PUT /api/v1/me/documents/{id}/read":                     {model.ScopePerson},
	"PUT /api/v1/me/asks/{id}/snooze":                        {model.ScopePerson},
	"DELETE /api/v1/me/asks/{id}/snooze":                     {model.ScopePerson},
	"GET /api/v1/agents/{session_id}/messages":               {model.ScopePerson},
	"POST /api/v1/agents/{session_id}/messages":              {model.ScopePerson},
	"POST /api/v1/broadcasts":                                {model.ScopePerson},
	"GET /api/v1/broadcasts":                                 {model.ScopePerson},
	"GET /api/v1/broadcasts/{id}":                            {model.ScopePerson},
	"GET /api/v1/issues/{key}/subscribers":                   {model.ScopePerson},
	"DELETE /api/v1/issues/{key}/subscribers/{session_id}":   {model.ScopePerson},
	"GET /api/v1/artifacts/{id}/subscribers":                 {model.ScopePerson},
	"DELETE /api/v1/artifacts/{id}/subscribers/{session_id}": {model.ScopePerson},
	"POST /api/v1/artifacts/{id}/rebuild":                    {model.ScopePerson},
	"POST /api/v1/asks/{id}/answer":                          {model.ScopePerson},
	"POST /api/v1/comments/{id}/reopen":                      {model.ScopePerson},
	"POST /api/v1/comments/{id}/accept":                      {model.ScopePerson},
	"PATCH /api/v1/comments/{id}":                            {model.ScopePerson},
	"POST /api/v1/comments/{id}/reject":                      {model.ScopePerson},
	"GET /api/v1/me/agent-tokens":                            {model.ScopePerson},
	"GET /api/v1/credential-requests":                        {model.ScopePerson},
	"GET /api/v1/credential-grants":                          {model.ScopePerson},
	"GET /api/v1/machine-logins":                             {model.ScopePerson},
	"GET /api/v1/signins":                                    {model.ScopePerson},
	"GET /api/v1/credentials/capabilities":                   {model.ScopePerson},
	"POST /api/v1/agents/{session_id}/stream/_test/publish":  {model.ScopePerson},

	// The rows that settle what a person settles: a document's review, a credential's fate, and
	// a personal token's own scopes, which a credential may not widen past its own.
	"POST /api/v1/artifacts/{id}/reviews":             {model.ScopePerson, model.ScopeApprove},
	"POST /api/v1/credential-requests/{id}/approve":   {model.ScopePerson, model.ScopeApprove},
	"POST /api/v1/credential-requests/{id}/deny":      {model.ScopePerson, model.ScopeApprove},
	"POST /api/v1/credential-requests/machine-lookup": {model.ScopePerson, model.ScopeApprove},
	"POST /api/v1/credential-grants/{id}/revoke":      {model.ScopePerson, model.ScopeApprove},
	"POST /api/v1/machine-logins/{id}/revoke":         {model.ScopePerson, model.ScopeApprove},
	"POST /api/v1/signins/{id}/revoke":                {model.ScopePerson, model.ScopeApprove},
	"POST /api/v1/me/agent-tokens":                    {model.ScopePerson, model.ScopeApprove},
	"PATCH /api/v1/me/agent-tokens/{id}":              {model.ScopePerson, model.ScopeApprove},
	"DELETE /api/v1/me/agent-tokens/{id}":             {model.ScopePerson, model.ScopeApprove},
}

// scopedRoutesNotMounted names every route routeScopes gives scopes that the table does not
// hold. A scope named on no row is a scope nothing enforces, which is what register refuses to
// mount, as it refuses a write class it cannot decide (undecidableWrites). The walk is over the
// table with its test hooks in, since a hook's row is a row of the table wherever a deployment
// leaves it out, and its scope says what it needs when the deployment serves it.
func scopedRoutesNotMounted() []string {
	table := (&server{deps: Deps{TestHooksEnabled: true}}).routes()
	mounted := make(map[string]bool, len(table))
	for _, route := range table {
		mounted[route.Method+" "+route.Pattern] = true
	}
	var unmounted []string
	for name := range routeScopes {
		if !mounted[name] {
			unmounted = append(unmounted, name)
		}
	}
	sort.Strings(unmounted)
	return unmounted
}

// routeRow is a row as the table writes it: the five columns every route has, written as a
// positional literal, which is why the write class is not among them (writeClasses).
type routeRow struct {
	Method      string
	Pattern     string
	Auth        routeAuth
	Description string
	Handler     http.HandlerFunc
}

// apiRoute is one mounted route: the table's row, the class routes() stamps onto it from
// writeClasses and the OAuth scopes it stamps from routeScopes. Register mounts the table and
// GET /api/v1 lists it, so the two can never disagree about what the server serves. Every
// mutating row carries a Scope, and a GET row carries none (project_scope.go); OAuthScopes is
// empty on every row any authenticated caller may reach; the row's own five columns are read
// through it by promotion.
type apiRoute struct {
	routeRow
	Scope       writeScope
	OAuthScopes routeScopeSet
}

// routeIndexEntry is the public shape of a route in GET /api/v1. Scopes is left out of a route
// that needs none, so the index describes each route exactly as the table declares it.
type routeIndexEntry struct {
	Method      string    `json:"method"`
	Path        string    `json:"path"`
	Auth        routeAuth `json:"auth"`
	Scopes      []string  `json:"scopes,omitempty"`
	Description string    `json:"description"`
}

// routeIndexDocs names the agent-facing guide for the routes below.
const routeIndexDocs = "skills/dispatch/SKILL.md"

// routes lists every native-workspace route. Add a row here, never a mux.HandleFunc line. A
// mutating row's write class comes from writeClasses, which Register's project-scope check
// reads; a row that names no class there is one that check cannot decide, and Register refuses
// to mount it (undecidableWrites).
func (s *server) routes() []apiRoute {
	rows := []routeRow{
		{http.MethodGet, "/api/v1", authPublic, "This index: every route with its method, auth, and purpose.", s.getRouteIndex},
		{http.MethodGet, "/api/v1/schema/blocks", authPublic, "The typed document block schema as JSON.", s.getBlockSchema},
		{http.MethodGet, "/api/v1/limits", authAny, "The bounds a write is held to: how long one block of a body may be and how many blocks a body may hold, how many options an ask may have and how long its question and options are together, how long a body an event carries whole may be, and the bytes a request, a document and a handoff body may hold.", s.getLimits},
		{http.MethodGet, "/api/v1/projects", authAny, "List projects.", s.listProjects},
		{http.MethodPost, "/api/v1/projects", authAny, fmt.Sprintf("Create a project {key, name}: any signed-in person or agent session; a bearer names its session in actor. A key that is not a project key, an empty name, a name holding a control character or a bidi embedding, override or isolate, or a name with no visible character (a letter, number, punctuation or symbol other than the blank fillers U+115F, U+1160, U+3164, U+FFA0 and U+2800, or a code point Go's Unicode tables leave unassigned, such as an emoji newer than them, other than a noncharacter (U+FDD0-U+FDEF and each code point ending in FFFE or FFFF) or a reserved default-ignorable (U+2065, U+FFF0-U+FFF8, U+E0000-U+E0FFF); combining marks, format characters, private-use characters and whitespace don't count) is 400 INVALID_PROJECT; a name over %d characters is 400 CAP_EXCEEDED; a key that already exists is 409 PROJECT_EXISTS.", contracts.ProjectNameMax), s.createProject},
		{http.MethodGet, "/api/v1/projects/{key}/artifacts", authAny, "List a project's documents.", s.listProjectArtifacts},
		{http.MethodPost, "/api/v1/projects/{key}/artifacts", authAny, "Upload a project document or file (JSON or multipart); a file's kind is read from its bytes, and a new artifact's name held to its bound, as an issue upload's are.", s.uploadProjectArtifact},
		{http.MethodGet, "/api/v1/projects/{key}/related", authAny, fmt.Sprintf("Run the related-item check an issue create runs, over ?title= and ?text= (checked as the title, a newline and the text; either alone is enough), without writing anything: no issue, ask, suggestion or related_checks record. ?exclude_issue= leaves that issue of the project and everything it holds out of the results and out of the word statistics (400 EXCLUDE_ISSUE_INPUT when it is not an issue of the project); ?keywords_only=true skips meaning search. It waits at most 5 seconds and answers the create's advice.suggestions shape: outcome found, none or not_made (a blank text is 200 not_made, the reason in missing), search, search_note, search_query, related and decision. Title and text over %d characters together are 400 CAP_EXCEEDED; POST takes a longer text. An unknown project is 404 PROJECT_NOT_FOUND.", contracts.SearchQueryMax), s.getRelated},
		{http.MethodPost, "/api/v1/projects/{key}/related", authAny, fmt.Sprintf("The GET form of this route with its fields in a JSON body {title, text, exclude_issue, keywords_only}: the related-item check over a title and a text as long as an issue's spec (the body's 1 MiB bound; a title over %d characters is 400 CAP_EXCEEDED), writing nothing and waiting at most 5 seconds; answers the same suggestions shape.", contracts.IssueTitleMax), s.postRelated},
		{http.MethodGet, "/api/v1/settings/repo-projects", authAny, "List GitHub repository to project mappings, each marked whether it is its project's default and what repository it was renamed to, null for one that was never renamed.", s.listRepoProjects},
		{http.MethodPut, "/api/v1/settings/repo-projects/{owner}/{repo}", authAny, `Map a GitHub repository to a project {project, default?, renamed_to?}; default true makes it the project's one default. renamed_to is the owner/name this repository is called today, which every comparison of a repository canonicalises through: absent or null leaves the stored one alone, "" clears it, and a pair naming this repository, one naming a repository that is itself renamed, or one on a repository another mapping is already renamed to is 400 INVALID_REPO_PROJECT.`, s.putRepoProject},
		{http.MethodDelete, "/api/v1/settings/repo-projects/{owner}/{repo}", authAny, "Remove a repository mapping no issue names as its repo.", s.deleteRepoProject},
		{http.MethodGet, "/api/v1/github/app-installation/{owner}/{repo}", authAny, "Whether Dispatch's GitHub App, which GitHub sends the repository's webhook events through, is installed on the repository: 200 {installed}, false when GitHub answers 404; 503 GITHUB_APP_UNCONFIGURED when Dispatch holds no App key, 502 GITHUB_UNREADABLE naming the repository and GitHub's error on any other failure.", s.getAppInstallation},
		{http.MethodGet, "/api/v1/settings/architecture-sources", authAny, "List every project's architecture sources, by project and then in the order each project added them.", s.listArchitectureSources},
		{http.MethodGet, "/api/v1/projects/{key}/architecture-sources", authAny, "A project's architecture sources, one per repository, in the order they were added; [] when it has none. The project's component model is every source's components together.", s.listProjectArchitectureSources},
		{http.MethodGet, "/api/v1/projects/{key}/architecture-source", authAny, "Kept for dispatch clients older than this release, logged at warn: the project's first architecture source, or null when it has none. GET /api/v1/projects/{key}/architecture-sources replaces it.", s.getArchitectureSource},
		{http.MethodPut, "/api/v1/projects/{key}/architecture-sources/{owner}/{repo}", authAny, "Add a repository as an architecture source of a project, or change the branch of the source it is {branch}, after proving the GitHub App can read it and that branch is the name of a branch it has; a missing branch, a commit SHA, a relative ref (main~1, HEAD) or a refs/ path is 409 SOURCE_ACCESS naming the value and the repository, as a credential problem is, and nothing is written. A re-save resets the source's sync bookkeeping and keeps its components.", s.putArchitectureSource},
		{http.MethodDelete, "/api/v1/projects/{key}/architecture-sources/{owner}/{repo}", authAny, "Remove one architecture source of a project and the components it projected; every other source's components stay.", s.deleteArchitectureSource},
		{http.MethodPost, "/api/v1/projects/{key}/architecture-sources/{owner}/{repo}/sync", authAny, "Import one architecture source's model now; 200 carries its updated row (last_error set when the import failed), 409 SOURCE_ACCESS a credential or branch problem.", s.syncArchitectureSource},
		{http.MethodPost, "/api/v1/projects/{key}/architecture-sources/sync", authAny, "Import every architecture source of a project now, each whatever another's import did; 200 carries every updated row in the order the sources were added, each with its own last_error (an access failure included).", s.syncArchitectureSources},
		{http.MethodPost, "/api/v1/projects/{key}/architecture-source/sync", authAny, "Kept for dispatch clients older than this release, logged at warn: import every architecture source of a project now and answer one row, the first in id order whose last_error is set or else the first; 404 SOURCE_NOT_FOUND without a source. POST /api/v1/projects/{key}/architecture-sources/sync replaces it.", s.syncSingularArchitectureSource},
		{http.MethodGet, "/api/v1/projects/{key}/architecture", authAny, "A project's component tree with the work attached: its sources' sync bookkeeping, each component with its source repository, per-component counts and issue rows, plus unassigned, not-architectural, and retired-link issues; 404 SOURCE_NOT_FOUND without a source.", s.getArchitectureTree},
		{http.MethodGet, "/api/v1/settings/project-default-assignees", authAny, "List every project that names a default assignee, by project key: the last step of a create's assignee chain. Needs the person scope, as the default names a person: a credential the deployment configured, the shared token or a pod's, holds none.", s.listProjectDefaultAssignees},
		{http.MethodPut, "/api/v1/projects/{key}/default-assignee", authAny, "Set a project's default assignee {assignee} (the email of a person who has signed in, or null to clear it): who an issue created with no assignee, by nobody, under no parent or under a parent with nobody on it, goes to. Needs the person scope, as the default names a person: a credential the deployment configured, the shared token or a pod's, holds none.", s.putProjectDefaultAssignee},
		{http.MethodGet, "/api/v1/settings/delivery", authAny, "The delivery timeline's one configuration record (deploy repository, workflow paths, production job names, optional production apply step, population authors, excluded repositories, and the Pipeline page's optional staging end-to-end, staging deploy and PR-checks roll-up job names), or null when it has not been set.", s.getDeliverySettings},
		{http.MethodPut, "/api/v1/settings/delivery", authAny, "Set the delivery timeline's configuration after proving the GitHub App can read the deploy repository; 409 DELIVERY_SETTINGS_ACCESS a credential or repository problem.", s.putDeliverySettings},
		{http.MethodGet, "/api/v1/settings/legion", authAny, "Every project's Legion setting, by project key: the daemon Dispatch reads that project's `GET /legion/v1/state` from once a minute, with the actor that first set it and when it was last written; [] when no project names one.", s.listLegionSettings},
		{http.MethodPut, "/api/v1/settings/legion/{project}", authAny, "Name the Legion daemon a project's state is read from {state_url}: an absolute http(s) URL with a host and no path, else 400 LEGION_SETTING_INPUT. The setting belongs to the deployment, so a project-scoped credential is refused it.", s.putLegionSetting},
		{http.MethodDelete, "/api/v1/settings/legion/{project}", authAny, "Stop reading a project's Legion daemon; what was already read stays. 404 NOT_FOUND when the project names none.", s.deleteLegionSetting},
		{http.MethodPut, "/api/v1/legion/{project}/settings", authAny, "Relay an operator's legion.yaml change {changes: {<dotted path>: <value>}} to the project's Legion daemon, with the caller's login as the change's author, and answer the daemon's status and per-key result (applied at-once, rebuilt or next-launch, or refused with a reason) unchanged; 404 LEGION_NOT_CONFIGURED when the project names no daemon, 503 LEGION_DAEMON_UNAVAILABLE when it could not be reached.", s.putLegionProjectSettings},
		{http.MethodGet, "/api/v1/legion/metrics", authAny, "The Delivery page's Legion section for ?project= (the first project naming a daemon when none is given), [from, to) (default the last 7 days) and ?scope=all|roots|children: over the issues that closed in the window with Legion phase work, the work time to merge less parked and queued time, the phase hours of each of the seven worker bars split into waiting on a person, in a turn, CI wait, waiting on children and idle in that order, the review rounds, send-back share and post-review hours, and the issues Legion sessions filed per week by class against the issues finished per week; 400 INVALID_QUERY for a malformed window or scope, 404 NOT_FOUND for a project that does not exist.", s.getLegionMetrics},
		{http.MethodGet, "/api/v1/legion/board", authAny, "The Legion view on the Issues page for ?project= (the first project naming a daemon when none is given), from what the sampler recorded of that project's daemon: ten columns (queue, design, planning, implementing, testing, reviewing, accepting, merging, awaiting_person, held), each card naming its issue's key, title, priority, phase and when that phase began, the slot it holds, the roles in a turn now, its pull request's CI verdict, the issues a blocker link holds it on, what it waits on from a person, its tree root and the children of its tree that have not started; and a header with the slots in use, the slot cap, the queue's length and the last read of the daemon with its failure. 404 LEGION_NOT_CONFIGURED when the project names no daemon.", s.getLegionBoard},
		{http.MethodGet, "/api/v1/delivery/timeline", authAny, "The delivery timeline within [from, to) (default the last 7 days), the search q (title or PR id) and the given facets (repeatable repo, parent_agent, session, issue, priority (P0-P3), component (a parent includes its children), author, rework, deployed; __no_issue__, __no_priority__, __no_component__ and __no_session__ select by cause): merges with each PR's issue title, priority and effective components, successful production deploys with every window PR they shipped first, pipeline failures with each run's production job and failing step, and merged work waiting to deploy, each PR's deployed_status (deployed, waiting, not_tracked) computed from stored facts; facet_counts (each facet counted with every other facet applied and its own ignored), color_counts (with none applied), the components and issue titles the window names, data freshness, and measures (what GET /api/v1/delivery/measures answers for the same window, search and facets); 404 DELIVERY_NOT_CONFIGURED without a delivery_settings row.", s.getDeliveryTimeline},
		{http.MethodGet, "/api/v1/delivery/runs/{id}", authAny, "One deploy-repository run with every job it ran, by start, each with its result and timings; 400 RUN_ID_INPUT for an id that is not a positive integer, 404 NOT_FOUND for no such run, 404 DELIVERY_NOT_CONFIGURED without a delivery_settings row.", s.getDeliveryRun},
		{http.MethodGet, "/api/v1/delivery/measures", authAny, "The four delivery measures and Sami's KPI targets for [from, to) (default the last 7 days) and the same facets the timeline takes: deploys a day, lead time (merge, first commit and open to production; open to merge), change failure rate per PR and per deploy (flags_source says whether stored flags fed it), time to restore, rework share, deploy-run success, a per-UTC-day series, the open P0 issues with no owner, and whether each target is met; 404 DELIVERY_NOT_CONFIGURED without a delivery_settings row.", s.getDeliveryMeasures},
		{http.MethodGet, "/api/v1/delivery/pipeline", authAny, "The Pipeline page over the UTC days its window covers: ?at= (default now) is the seven days ending on its own, and ?from= and ?to= are the days that window covers, bounded by the same 84 days as the other delivery routes. By the day each run started: the PR-checks workflow's run wall time and success, its five slowest jobs (50 timed results to rank) and the jobs that failed in the 24 hours before the window's end, which for a whole-day window is its last day; the duration and result of the staging end-to-end, staging deploy and production deploy jobs the settings name in push runs on main; and merge-to-production and PR-open-to-merge per merge day against their targets. Not filtered by the timeline's facets; 400 INVALID_QUERY for a malformed at or window, for a window over the cap, and for an at beside from or to, 404 DELIVERY_NOT_CONFIGURED without a delivery_settings row.", s.getDeliveryPipeline},
		{http.MethodPost, "/api/v1/delivery/session-groups", authAny, "Create a Delivery session group {name, sessions}: the group becomes the Parent agent of every pull request whose first session it holds; 409 SESSION_IN_GROUP for a session another group holds, 409 SESSION_GROUP_NAME_TAKEN for a name another group has, 409 SESSION_GROUP_NAME_IS_SESSION_TITLE for a name that is the title of a session outside the group. The timeline lists every group (session_groups).", s.createDeliverySessionGroup},
		{http.MethodPatch, "/api/v1/delivery/session-groups/{id}", authAny, "Rename a Delivery session group, add sessions to it and remove sessions from it {name?, add?, remove?}, with the create route's refusals; 400 SESSION_GROUP_INPUT for an edit that would leave it empty.", s.updateDeliverySessionGroup},
		{http.MethodDelete, "/api/v1/delivery/session-groups/{id}", authAny, "Delete a Delivery session group; its sessions show as themselves again.", s.deleteDeliverySessionGroup},
		{http.MethodGet, "/api/v1/me/agent-tokens", authAny, "List the caller's personal agent tokens, each with the scopes it holds.", s.listAgentTokens},
		{http.MethodPost, "/api/v1/me/agent-tokens", authAny, "Mint a personal agent token {name, scopes?}: scopes is the set the new token holds, defaulting to the scopes the caller's own credential holds. A scope this deployment does not know, or approve without person, is 400 TOKEN_SCOPES_INPUT; a scope the caller does not hold itself is 403 OAUTH_SCOPE_REQUIRED. The answer is the one time the token's plaintext is shown.", s.createAgentToken},
		{http.MethodPatch, "/api/v1/me/agent-tokens/{id}", authAny, "Set the scopes of one of the caller's personal agent tokens {scopes}: the whole set it is to hold, with the mint's refusals, answering the updated token. A token of another person's, or none, is 404 NOT_FOUND.", s.updateAgentTokenScopes},
		{http.MethodDelete, "/api/v1/me/agent-tokens/{id}", authAny, "Revoke a personal agent token.", s.revokeAgentToken},
		{http.MethodGet, "/api/v1/users", authAny, "Everyone who has signed in, by lowercase email: the assignee picker's options.", s.listUsers},
		{http.MethodGet, "/api/v1/whoami", authAny, "Who the server takes the caller for: {kind: user, login, scopes} (login is the person's lowercase email) or {kind: agent, owner, service, session, project, scopes} (owner is a personal token's owner or a broker-issued token's person by lowercase email, null for the shared token and for a broker-issued token that names a service; service is a verified service-account token's Kubernetes subject, or the fleet a broker-issued token naming no person belongs to, null for every other credential; session is the session the caller's credential proved, null for a credential that proves none, which every one but an access token the secrets broker issued is; project is the one project key that credential lets the caller write, null where it names none). scopes is the OAuth scopes the credential holds, an empty array for one that holds none: person acts as the person on every route the dashboard reaches, and approve settles what a person settles.", s.whoami},
		{http.MethodGet, "/api/v1/dashboard/builds", authAny, "Which dashboard builds are served: {serving, live}, serving the entry chunk (/assets/index-*.js) of the build this process serves (empty when none), live the distinct entry chunks of the builds every live server process serves, sorted. A tab offers its reload notice only when live holds exactly one entry and it is not the tab's own.", s.getDashboardBuilds},
		{http.MethodGet, "/api/v1/issues", authAny, fmt.Sprintf("List issues; filters project, status, parent, label, priority (repeatable: 0-3, or none for unset), open, updated_since, route_status (live, no_holder or unknown; open issues only); ?pinned=true needs the person scope, since the pins are one person's. Each issue carries progress: {tasks, children}, its spec's task-list items and its direct children as {done, total}, each null when it has none. Pages with limit (1-%d) and offset (0 or more; alone it pages %d): a paged answer is {issues, total, limit, offset}, total counting every issue the filters match; without either it is every matching issue as an array. cursor is 400 INVALID_QUERY.", contracts.MaxIssuePageLimit, contracts.DefaultIssuePageLimit), s.listIssues},
		{http.MethodPost, "/api/v1/issues", authAny, fmt.Sprintf("Create an issue (native, or from a GitHub owner/repo#n ref); assignee defaults to the creating person, the personal token's owner, the parent's assignee, or the project's default assignee when that still names a person who has signed in. A title over %d characters is 400 CAP_EXCEEDED. After the write commits, a related-item check searches the project by meaning and by its rarest words and waits at most 5 seconds; the response's advice.suggestions says whether it found related items (outcome found, none or not_made with the reason in missing), how it searched (search, search_note), the three items most like the new issue, one per owner, with each ask item's answer state, any answered ask that settles it, words to search with by hand (search_query), and under force the title overlaps forced past (title_matches).", contracts.IssueTitleMax), s.createIssue},
		{http.MethodGet, "/api/v1/issues/resolve", authAny, "Resolve ?ref=<KEY | owner/repo#n> to an issue key.", s.resolveIssue},
		{http.MethodGet, "/api/v1/issues/{key}", authAny, "Read an issue with its open asks, primary document, children, and progress: {tasks, children}, its spec's task-list items (`- [ ]` / `- [x]`, nested lists included) and its direct children as {done, total}, each null when it has none.", s.getIssue},
		{http.MethodGet, "/api/v1/issues/{key}/summary", authAny, "What a reference to an issue shows, read without the full issue: {key, project, title, status, documents}, documents every artifact of the issue in the order the issue read lists them, each {id, slug, name, kind, latest_version, primary}, primary whether the document is the issue's spec; 404 NOT_FOUND for no such issue.", s.getIssueSummary},
		{http.MethodPatch, "/api/v1/issues/{key}", authAny, fmt.Sprintf("Update title (over %d characters is 400 CAP_EXCEEDED), status, priority, labels (or labels_add, which adds to the labels the issue holds when the write commits rather than replacing them; not with labels), route, parent, rank, assignee (the email of a person who has signed in, or null; anyone may reassign), or components ({mode: inherit|explicit|none, ids, reason}; allowed on a closed issue). Closing the issue (status done) withdraws every open ask it holds, each recorded resolved with resolution kind withdrawn naming the closer; reopening it opens exactly those asks again. expected_sequence makes the write conditional: 409 ISSUE_CHANGED when the issue's last event is no longer that sequence.", contracts.IssueTitleMax), s.patchIssue},
		{http.MethodPost, "/api/v1/issues/{key}/claim", authAny, "Claim the issue for the calling session (or human): records who is working it. The status does not move. The answer is the issue with legion_managed_status beside it: true when the issue carries the legion label in any case, or sits under one that does, which means the Legion daemon owns the status and a status another writer sets is undone. 409 ISSUE_CLAIMED names a live holder (or a human one, whose claim has no session to end); a caller whose credential holds the person scope may pass {force: true} to take it anyway, and one that does not is 403 OAUTH_SCOPE_REQUIRED. 409 CLAIM_CONTENDED means the holder changed twice while the request ran, so nothing was applied and nobody's liveness was checked: read the issue and retry.", s.claimIssue},
		{http.MethodDelete, "/api/v1/issues/{key}/claim", authAny, "Release the issue's claim: its holder, a caller whose credential holds the person scope, or anyone when the holding session is no longer live. The status does not move. The answer is the issue with legion_managed_status beside it: true when the issue carries the legion label in any case, or sits under one that does, which means the Legion daemon owns the status and a status another writer sets is undone. 409 ISSUE_CLAIMED names the holder this caller may not release (there is no force on this route); 409 CLAIM_CONTENDED means the holder changed twice while the request ran, so nothing was applied and nobody's liveness was checked.", s.releaseIssueClaim},
		{http.MethodGet, "/api/v1/issues/{key}/events", authAny, "Page an issue's event log (after, before, ids, order, limit).", s.listIssueEvents},
		{http.MethodGet, "/api/v1/issues/{key}/references", authAny, "The eight-hop closure of documents the issue's content references: {members, truncated}, each member a document in the closure with its depth and the node it was reached via, under an ETag (If-None-Match answers 304). ?versions=latest leaves each member's artifact.versions holding only its latest version, under its own ETag; absent, empty or all is the full answer; another value, or versions given more than once, is 400 INVALID_QUERY.", s.getIssueReferences},
		{http.MethodGet, "/api/v1/references", authAny, "Edges of one node in the reference graph: ?to=<dispatch ref> backlinks or ?from= links; ?kind=, ?since=<events.id> (mentions only, read up to the event log's safe horizon: every mention committed before the read, unless an open transaction holds the horizon back past 1 s, when the read answers the mentions at or below it and a later read the rest). A caller with no cursor starts from since=0, which lists every mention committed before the read that carries a source_seq, and after that reads again from the highest source_seq an answer showed it. A mention written with no event (a rebuild, a backfill, an older build) carries no source_seq until a later write of its source stamps it with that write's event, and until then only a read without since lists it. A signed-in page may start from GET /auth/whoami's event_head instead, which takes a cookie or a trusted header and not a bearer, so a bearer caller starts from 0. Never take a since from max(events.id) read straight from the database: event ids are not commit order, and an id taken that way can stand above an edge that commits later. An empty answer is not proof that nothing is new, so the caller reads again with the id it holds. A ?to= reference whose item id or block id is written short (8 characters or more, never a whole uuid) names the one item of the owner the reference names that carries it: the issue of dispatch://<KEY>/ask/<prefix>, the document of a #b-<prefix>; the node answered carries the whole id, a prefix two of them carry is 409 ID_AMBIGUOUS listing both, and one nothing carries is the 404 the reference as written always gave. A whole id is read whatever owner the reference names, as it always was.", s.getReferences},
		{http.MethodGet, "/api/v1/references/questions", authAny, "The question map of one reference source's document: ?kind= one of artifact, ask, comment or message and ?id= that source's whole uuid or a unique prefix of 8 characters or more. Answers {document, questions}: document the uuid of the document the source's text belongs to - its own for a document version, the document an ask or a comment is written on or anchored to, its issue's spec for an issue message - and questions each `:::ask{#id}` block that document's latest version declares, by block id, with the whole id of the ask settlement made for it and that ask's `dispatch://` address, which is what a `#id` in the source's text names and the address its edge is stored under (empty for an ask on an agent conversation's document, which no address names). A block no ask is settled for yet is absent, and a document declaring none answers []. A source whose text belongs to no document - a direct message, an id nothing holds - is 404 NOT_FOUND; a kind that is none of the four is 400 INVALID_KIND; a missing id is 400 INVALID_QUERY; an id prefix two sources carry is 409 ID_AMBIGUOUS.", s.getReferenceQuestions},
		{http.MethodGet, "/api/v1/block-links", authAny, "Links from blocks of one document to blocks of another: ?from=<document ref>&to=<document ref>; both block nodes, the edge kind, and when and in which version the source wrote each, newest first.", s.getBlockLinks},
		{http.MethodGet, "/api/v1/issues/{key}/subscribers", authAny, "Sessions subscribed to an issue's topics.", s.listIssueSubscribers},
		{http.MethodDelete, "/api/v1/issues/{key}/subscribers/{session_id}", authAny, "Unsubscribe a session from an issue.", s.unsubscribeIssueSession},
		{http.MethodGet, "/api/v1/issues/{key}/artifacts", authAny, "List an issue's documents and files.", s.listArtifacts},
		{http.MethodPost, "/api/v1/issues/{key}/artifacts", authAny, fmt.Sprintf("Upload an issue document or file (JSON or multipart). A text/markdown upload is a document; any other is an image only when its bytes are a PNG, JPEG, GIF or WebP, stored under the type its bytes are whatever was declared, and otherwise a file under its declared type. A name no artifact has yet is 400 INVALID_ARTIFACT when it, or the reference key <owner>/<slug> it gives the new artifact, is over %d bytes; an upload under an existing artifact's name is never refused for its length.", maxNewArtifactKeyBytes), s.uploadArtifact},
		{http.MethodGet, "/api/v1/issues/{key}/results", authAny, "The checks of the issue's Verification document in document order, each with its attributes (when, surface, run, negative_control), the version its words last changed in and their digest, and every result recorded against it from any issue, newest first: {issue, verification_artifact_id, checks}. A check's results come newest first by created_at, the time the recording transaction began, with id descending breaking a tie, and a client reading one result per check reads the first; two results recorded in transactions that overlapped can come back in either order. Each result carries currency: current (its commit is the head of an open pull request attributed to the check's issue or the final head of a merged one, or, for an after_merge or default_branch check, a commit on the default branch of that issue's repository, and the check's words have not changed since), older, or unknown (the check's issue resolves to no repository, or Dispatch's GitHub App cannot read it). An issue with no Verification document answers no checks.", s.listCheckResults},
		{http.MethodPost, "/api/v1/issues/{key}/results", authAny, "Record one result of one check: {check, role, verdict, commit, surface?, run?, observed?, negative_control?, failed?, skipped?, excluded?, screenshots?}. {key} is the issue the result is recorded on; check names a check block of its Verification document or of one of its ancestors', by block id or, when a block id is not unique in that set, by its canonical reference (409 CHECK_AMBIGUOUS); any other id is 400 UNKNOWN_CHECK. role is at most 32 characters of a-z and -, verdict is pass or fail, commit is 40 lowercase hex, and the counts default to 0; a pass counting any failed, skipped or excluded case is 400 RESULT_CONTRADICTS. A commit the repository the check's issue resolves to does not hold is 400 RESULT_COMMIT_UNKNOWN, naming the commit and that owner/repo; a repository Dispatch cannot read, or none at all, stores the result as before. A screenshot names an image document of the recording issue or of the check's issue. The result is appended as result.recorded on the check's issue. A closed issue, recording or the check's, takes a result only for a check whose when is after_merge or default_branch; every other result there is 409 ISSUE_CLOSED. The answer is {result, line}, line being the pull-request body line the result implies, which carries no link to Dispatch.", s.recordCheckResult},
		{http.MethodGet, "/api/v1/issues/{key}/coverage", authAny, "What each requirement of the issue's spec lacks: {issue, spec_artifact_id, has_plan, requirements, gaps}. A requirement is a `requirement` block of the spec; requirements lists them in document order, each with its reference, its words, and how many `check` and `plan_task` blocks of any document of the issue or of any descendant issue, at any status, name it in serves. gaps is one line per gap - `<block id> has no check`, and `<block id> has no plan task` only once the issue has a plan document - or the single line `the spec marks no requirements` for a spec that marks none. Nothing is ever refused for a gap. This is not GET /api/v1/search/coverage, which reports how much of what meaning search ranks carries a vector.", s.getIssueCoverage},
		{http.MethodPost, "/api/v1/issues/{key}/messages", authAny, "Post an issue message; target and delivery address a session or role. A reply names the message it answers in in_reply_to, and one block of it in in_reply_to_block.", s.createMessage},
		{http.MethodGet, "/api/v1/issues/{key}/messages", authAny, "List an issue's message threads: every root message, oldest first, each with its deliveries and its replies.", s.listIssueMessages},
		{http.MethodGet, "/api/v1/issues/{key}/messages/{id}", authAny, "Read one message with its deliveries and replies.", s.getMessage},
		{http.MethodPost, "/api/v1/issues/{key}/handoffs/{phase}", authBearer, fmt.Sprintf("Record a session's handoff for one phase of an issue {actor, body}: the session names itself in actor (400 ACTOR_KIND otherwise; a cookie caller is 403 HANDOFF_FORBIDDEN), phase is a lowercase slug (%s, 400 HANDOFF_INPUT otherwise), and body is one JSON object as a string (400 HANDOFF_INPUT otherwise) of at most %d bytes (413 CAP_EXCEEDED), stored exactly as sent. The record is the phase's next version, from 1; a closed issue is 409 ISSUE_CLOSED. Answers 201 {id, issue_key, phase, version, author, created_at} and appends handoff.recorded {handoff_id, phase, version}, which never carries the body.", contracts.HandoffPhasePattern, contracts.DispatchHandoffBodyMax), s.createHandoff},
		{http.MethodGet, "/api/v1/issues/{key}/handoffs", authAny, "List an issue's handoffs, newest first, each {id, issue_key, phase, version, author, created_at} without its body; a closed issue's read too.", s.listHandoffs},
		{http.MethodGet, "/api/v1/issues/{key}/handoffs/{phase}/latest", authAny, "The newest handoff of one phase of an issue with its body, the exact text written; 404 HANDOFF_NOT_FOUND when the phase has none. A closed issue's read too.", s.getLatestHandoff},
		{http.MethodGet, "/api/v1/handoffs/{id}", authAny, "One handoff by id with its body, the exact text written; 404 HANDOFF_NOT_FOUND when there is none, 400 HANDOFF_ID_INPUT for an id that is not a uuid.", s.getHandoff},
		{http.MethodGet, "/api/v1/messages/{id}", authAny, "Read the conversation a message belongs to by any message id in it, issue-less or not: the thread root with its deliveries and every reply, oldest first. An issue thread follows the issue's rule. A direct (issue-less) thread is read whole by a credential holding the person scope, since the conversation is that person's own; every other credential names its session in ?session= and reads only one whose root targets that session or that it replied in (403 THREAD_FORBIDDEN), a guard against reading another session's conversation by mistake, not an authorization boundary. Direct-conversation text also reaches every authenticated caller through that caller's events view on GET /ws/live; nothing in Dispatch restricts it by session.", s.getMessageThread},
		{http.MethodGet, "/api/v1/messages/{id}/blocks", authAny, "A message's current blocks in body order: {item, indexed, blocks: [{id, type, text, ref, from, to, embedding}]}, item the message's graph node, from and to UTF-16 code units into its stored body, ref absent for a direct message's block (it has no address), embedding where meaning search stands on the block (embedded, pending, or none while no window covers it). Read under GET /api/v1/messages/{id}'s rule: a credential without the person scope reading a direct message names its session in ?session= (400 SESSION_REQUIRED, 403 THREAD_FORBIDDEN). While the message is owed a block index, indexed is false and blocks empty; the read writes nothing.", s.getMessageBlocks},
		{http.MethodPost, "/api/v1/messages/{id}/deliveries", authAny, "Retry delivering a targeted message.", s.createDelivery},
		{http.MethodPost, "/api/v1/messages/{id}/deliveries/{attempt}/accept", authBearer, "The attempt's session records that it took the message as its user's own turn; the session names itself in actor (403 ACCEPT_FORBIDDEN for another's attempt). One compare-and-set: only a direct message to that session, on no issue, neither a broadcast's copy nor a reply in a broadcast's thread, in a thread whose root targets it (409 ACCEPT_NOT_DIRECT), that a person wrote (409 ACCEPT_NOT_WRITTEN_BY_PERSON), sent as a Send or an Aside (409 ACCEPT_NOT_ASIDE_OR_STEER); only the message's latest attempt (409 ACCEPT_SUPERSEDED), when no attempt of it was accepted before (409 ACCEPT_ALREADY_ACCEPTED), which a person requested (409 ACCEPT_NOT_REQUESTED_BY_PERSON), that person being the message's author (409 ACCEPT_NOT_REQUESTED_BY_AUTHOR), which did not fail (409 ACCEPT_FAILED; a pending or sent attempt is taken), within the last minute (409 ACCEPT_STALE). Answers the attempt with accepted_at and accepted_as and the message's stored body; leaves the attempt's state to the send, and appends message.accepted.", s.acceptDelivery},
		{http.MethodPost, "/api/v1/messages/{id}/reply", authBearer, "The targeted session's reply to a delivery; the session names itself in actor. Once the attempt is answered, ?follow_up=true posts other text as the session's follow-up, threaded under its first reply; with follow_up=false or absent, or with text the session already posted there, nothing is posted and the stored message comes back marked duplicate. Any other follow_up is 400 MESSAGE_INPUT.", s.replyMessage},
		{http.MethodGet, "/api/v1/inbox", authAny, "Open asks waiting on the caller, grouped by whose turn it is; ?project= and ?assignee=me|unassigned|<email> filter (unassigned includes document asks; an email nobody has signed in with is 400 ASSIGNEE_NOT_ALLOWED).", s.listInbox},
		{http.MethodGet, "/api/v1/me/answers", authAny, "The caller's own answers and replies on asks across issues and project documents, newest first: each row is an answer (the ask.answered event's answer, and whether it is still the ask's current one) or a reply, with the ask's question, state, owner and deep link; pages with limit (1-200, default 50) and offset; {rows, total, limit, offset}.", s.listMyAnswers},
		{http.MethodGet, "/api/v1/me/replies", authAny, "The caller's Inbox threads, of the kinds ?kind= names: reply (the default) lists the threads they wrote in - an ask, a comment thread, an issue message thread - that somebody else has written in, less an open ask the Inbox already lists on an open issue assigned to them; agent lists their one-off messages to an agent session (a BTW, a broadcast's copy, or a Send or Aside the session did not take as a turn of its live conversation) holding a reply from that session; reply,agent lists both in one order. ?state= is unread (the default), which lists the rows somebody else has written in since they read them, newest reply first; inbox, every row in their Inbox, read or not, dismissed ones aside, newest reply first; or dismissed, what they have dismissed out of it, newest dismissal first, 200 rows at a time from ?cursor= (the next_cursor of the page before it; limit and offset are 400 INVALID_QUERY there, and cursor is 400 INVALID_QUERY under any other state). Each row names the thread, its owner, its issue's priority, its project, the thread's subject, its root's deep link (an agent row's is the session's live view), the newest reply's author, first line and time, the caller's read mark, whether it is unread and how many writes in it they have not read, the root's document anchor when it has one, its dismissal mark when it has been dismissed, and, on an agent row, the session and the ids of its unread replies. ?issue= or ?artifact= narrows to one owner (both is 400 INVALID_QUERY), and an agent row, which has neither, is then in no answer. An absent limit answers every row, as the Inbox reads them; a named limit is 1-200 (limit=0 is 400 INVALID_QUERY) from ?offset=. {rows, total (both kinds, and under state=dismissed the page the read answers rather than the whole pile, which nothing counts), limit (0 when none was named), offset, as_of, next_cursor (a dismissed page that filled)}: as_of is the database time of the one read-only repeatable-read snapshot every row was read in, which is what a mark-all passes back as its read mark.", s.listMyReplies},
		{http.MethodPut, "/api/v1/me/threads/{id}/read", authAny, "Move the caller's read mark on one thread forward to {read_through} (RFC3339; stored as no later than now; more than a minute ahead of the server is 400 INVALID_STATE). It only moves forward; a thread the caller never wrote in writes nothing; 204 either way. A non-uuid id is 400 THREAD_ID_INPUT.", s.putThreadRead},
		{http.MethodPost, "/api/v1/me/inbox/read", authAny, "Mark every unread Inbox thread read in one write: {read_through} (RFC3339, required; stored as no later than now; missing, unparsable or more than a minute ahead of the server is 400 INVALID_STATE), with the Inbox's own narrowing - kind (reply, agent; both by default), priority (P0, P1, P2, P3 and none), urgency (low, med, high, blocking and none), project (a project key and none), and issue or artifact - each absent list meaning every value. It marks exactly the threads GET /api/v1/me/replies?state=unread answers under that narrowing whose newest reply is at or before read_through: a thread written in since stays unread, and one already read past the mark keeps its later mark. Marking read takes no thread out of the Inbox; POST /api/v1/me/inbox/dismiss does. An Agent row has no issue, document, project, priority or urgency, so any of those narrowings leaves it out; a Reply row has no urgency either, so an urgency list without none marks nothing. An agent reply is marked read by its id, so no session's own read mark moves and the live conversation keeps what it had. issue with artifact, an unknown kind, priority or urgency value, and an artifact that is not a document id are 400 INVALID_QUERY. Answers {marked}, the number of threads marked; a request that moves nothing answers 0 and appends no event.", s.markInboxRead},
		{http.MethodPost, "/api/v1/me/inbox/dismiss", authAny, "Take threads out of the caller's Inbox, or put them back: {threads} (the row ids reply:<thread id> and agent:<root message id>) with {seen_replies}, or the Inbox's own narrowing - kind, priority, urgency, project, and issue or artifact, as POST /api/v1/me/inbox/read reads them - with {dismiss_through} (RFC3339, required with the narrowing; stored as no later than now; missing, unparsable or more than a minute ahead of the server is 400 INVALID_STATE), never both (400 INVALID_QUERY). seen_replies is what the page drew in each row threads names, keyed by that same row id, and a dismissal naming threads without it is 400 INVALID_QUERY: a reply: row's entry is {version}, that row's version as GET /api/v1/me/replies answered it, and an agent: row's is {newest_reply, unread_replies}, that row's reply.id and the ids it drew as unread. The mark is written only while the row still holds what the page drew, so a dismissal buries no write nobody has read. A restore takes no seen_replies, and neither does the narrowing, where the server draws the rows itself (each 400 INVALID_QUERY). With the narrowing it changes exactly the threads GET /api/v1/me/replies answers under it whose newest reply, or whose dismissal, is at or before dismiss_through. {restore: true} clears the marks instead of writing them, so the threads are in the Inbox again. Dismissing marks a thread read as well, so a dismissed thread never counts as unread, and a later write by somebody else clears the mark and brings the thread back unread, while the caller's own reply leaves it dismissed. An Ask row's id, a row id of neither shape and an id that is not a uuid are 400 INVALID_QUERY. Answers {dismissed}, the number of threads it changed; a request that changes nothing answers 0 and appends no event. It takes the lock order and appends the events POST /api/v1/me/inbox/read does: one user_thread_state.updated per project and one user_agent_state.updated per session it touched.", s.dismissInbox},
		{http.MethodGet, "/api/v1/search", authAny, fmt.Sprintf("Full-text search ?q= across issues, documents, asks, comments, and messages, within ?project= when given: each kind ranked on its own and merged by reciprocal rank fusion, one page of ?limit= (1-50, default 20) from ?offset=, with the total match count and how many the pages can reach (each kind's best %d); a q over %d characters is 400 CAP_EXCEEDED, and a project that is not a project key is 400 INVALID_PROJECT.", contracts.SearchKindDepth, contracts.SearchQueryMax), s.search},
		{http.MethodGet, "/api/v1/search/coverage", authAny, "Meaning search's coverage {identity, place, model, total, kinds}: the configured embedder's identity, its place (bedrock, azure, or dev for a local harness) and model, all three null with none configured; then, in total and for each kind (issue, document_block, comment_block, message_block, ask_part), the searchable items, how many carry a current vector from that identity, how many lack one, how many failed for good, and how many seconds the oldest lacking item has waited (null when none lacks).", s.searchCoverage},
		{http.MethodGet, "/api/v1/related-checks", authAny, "Count the related-item checks recorded when an issue, an ask or a new decision block was written, within [from, to) (RFC3339, each optional) and of ?project= and ?source_kind= (issue, ask or decision_block; any other is 400 INVALID_QUERY) when given: {project, source_kind, from, to} echo the filters (null when absent), total, by_source_kind, by_outcome (found, none, not_made), by_search (meaning_and_keywords, keywords_only, not_made), by_reason (each reason a check was not made or searched by keywords only), completed (found or none), skipped_for_time (reason timeout), and keyword_ms and total_ms each {count, p50, p95, max, over_300ms}: the checks holding that time, its discrete percentiles and largest value (null with no check), and how many took over 300 ms.", s.getRelatedChecks},
		{http.MethodPost, "/api/v1/going/requests", authAny, fmt.Sprintf("Ask a question of what Dispatch holds {words, project}: records the question and answers 201 with its run - {id, author, project, words, created_at, ends_at}, whose ends_at is the %s the run has to answer in - and answers it behind that, each frame of the answer stored for GET /api/v1/going/requests/{id}/stream to carry. words is required (400 GOING_INPUT when it is blank) and at most %d characters (400 CAP_EXCEEDED); project is the project key the question is asked of, empty asking of every project (400 INVALID_PROJECT when it is not a key), and a credential scoped to one project asks only of that project. The run belongs to the credential that asked it, which the stream compares its reader with, so a credential that names nobody - the deployment's shared token - is refused 403 %s and records nothing: no reader of that run could ever be served it. With no model configured the run is recorded all the same and its stream carries the error frame %s, so a reader learns that from the stream, where every other failure of a run is reported, and not from a status of this route's own.", contracts.GoingRunWindow, contracts.GoingWordsMax, codeGoingAskerNamesNobody, contracts.GoingErrorNoModelConfigured), s.createGoingRequest},
		{http.MethodGet, "/api/v1/going/requests/{id}/stream", authAny, fmt.Sprintf("One run's answer as server-sent events: each frame is `id:` its sequence, `event:` its name (%s, %s, %s, %s, %s, %s, %s) and `data:` the JSON that frame carries, flushed as it is written, and the stream ends on %s or %s. Last-Event-ID resumes after that sequence, so a reader that drops rereads nothing. Any pod serves it, since the frames are rows and not the memory of the process that took the POST: a run past its ends_at that stored no ending is ended by the reader itself, with the error %s, which is how a stream ends when the pod that took the POST is gone. A run the record does not hold, an id that is not a uuid, and a run that is another caller's are one answer, 404 %s, so an id tells a caller nothing it did not already know.", contracts.GoingEventText, contracts.GoingEventItem, contracts.GoingEventCut, contracts.GoingEventChanged, contracts.GoingEventNone, contracts.GoingEventEnd, contracts.GoingEventError, contracts.GoingEventEnd, contracts.GoingEventError, contracts.GoingErrorAnswerDidNotArrive, codeGoingRequestNotFound), s.streamGoingAnswer},
		{http.MethodGet, "/api/v1/going/capability", authAny, "Whether this deployment has a text model configured: {model_configured} and nothing else, so no endpoint, deployment name or credential can appear in it. Any caller Dispatch admits reads it, a pod's own agent token included.", s.getGoingCapability},
		{http.MethodGet, "/api/v1/agents", authAny, "Live sessions with roles, capabilities, open asks, and last activity, from Dispatch's copy of the Envoy listener's session list, at most 5 s old.", s.listAgents},
		{http.MethodGet, "/api/v1/agents/{session_id}/answers", authAny, "What people wrote that Dispatch delivered to one session, newest first by at, then by id: each ask's current answer (replaced when it changed an earlier answer), each comment at its current body (edited_at when edited; a resolved comment stays) and each message, once however it arrived. It reads the ask.answered, comment.created, message.created and message.answered events a person wrote whose published_destinations hold the session's own topic, the session's sent message deliveries (targeted and broadcast messages) and its sent comment deliveries (mentions); it never lists an agent's or the system's words, an ask's resolution or hand-back, or a delivery to a role: route's topic. Each item names its relation to the session (mentioned_you over replied_on_your_ask over replied_in_a_comment_thread over commented_on_a_document over commented_on_an_issue for a comment; replied_to_your_message over sent_you_a_message for a message; answered_your_ask for an answer), the ask and its whole question on an answer and on a reply in an ask's thread, its owner, dispatch:// ref and href (null owner and ref, and /agents/<session_id>/live, for a direct message). ?since= (RFC3339, default 72 hours ago; older than 7 days or in the future is 400 INVALID_QUERY) starts the window; ?max_chars= (1-100000, else 400 INVALID_QUERY) stops the list after the item whose text takes the running sum of text lengths (UTF-16 units) past it, while total counts every item in the window; {session_id, since, total, items}. A session_id that is not a session id is 400 INVALID_SESSION_ID, as on the agent artifact routes. Not an authorization boundary: direct-message text already reaches every authenticated caller through GET /api/v1/events, and a person may read any session's list.", s.listSessionAnswers},
		{http.MethodGet, "/api/v1/agents/{session_id}/messages", authAny, "A session's targeted messages, newest first.", s.listAgentMessages},
		{http.MethodPost, "/api/v1/agents/{session_id}/messages", authAny, "Send an issue-less targeted message to a session: {body, delivery, in_reply_to?}, answered once its own delivery attempt has settled, on past a client that gives up. Under an Idempotency-Key header (as every POST and PATCH takes one), a repeat with the same request answers 201 with the message the key made and its deliveries as they stand, Idempotency-Replayed: ?1, first sending the one attempt it still owes (none yet, or one stranded past its claim) and nothing else; a listener it cannot reach is recorded on the attempt, so a repeat is answered while the listener is down. An attempt a stopped process left owed is resumed by the server's delivery sweep whether anyone asks again or not.", s.createAgentMessage},
		{http.MethodPost, "/api/v1/agents/{session_id}/artifacts", authAny, "Upload a file or image to an agent's conversation (multipart, as an issue upload, its kind read from its bytes): ref_key agent/<session_id>/<slug>, addressed dispatch://agent/<session_id>/artifact/<slug>. It holds files and images only: a markdown document (JSON content or a text/markdown file) is 400 ARTIFACT_INPUT, since a document belongs to an issue or a project. No event is appended. A caller whose credential proves a session of its own uploads only into that session: any other session_id is 403 SESSION_SCOPE. A session_id that is empty or holds /, ?, #, whitespace, a control character or a character that ends a reference in text (<>\"'`[]|) is 400 INVALID_SESSION_ID on every agent artifact route.", s.uploadAgentArtifact},
		{http.MethodGet, "/api/v1/agents/{session_id}/artifacts/{slug}", authAny, "Read an agent's conversation's file or image and its versions by slug.", s.getArtifact},
		{http.MethodGet, "/api/v1/agents/{session_id}/artifacts/{slug}/versions/{number}", authAny, "Download one version of an agent's conversation's file or image, as the issue form serves it (Cache-Control: private, max-age=31536000, immutable).", s.getArtifactVersion},
		{http.MethodPost, "/api/v1/broadcasts", authAny, "Send one message to many sessions: {body, delivery, session_ids}, one targeted message per recipient under a shared broadcast, answered once it is committed while each recipient's delivery runs behind the answer. A selected session that is not live or does not advertise the mode is excluded and named in the response, never switched to another mode. Under an Idempotency-Key header (as every POST and PATCH takes one), a repeat with the same request answers 201 at once with the broadcast the key made, every recipient's attempts as they stand and the first answer's exclusions, Idempotency-Replayed: ?1, and puts every recipient it still owes on its way behind the answer. A recipient a stopped process left owed is resumed by the server's delivery sweep whether anyone asks again or not.", s.createBroadcast},
		{http.MethodGet, "/api/v1/broadcasts", authAny, "Recent broadcasts, newest first, with their recipient and reply counts.", s.listBroadcasts},
		{http.MethodGet, "/api/v1/broadcasts/{id}", authAny, "One broadcast with every recipient's delivery attempts and replies, recipients in the order the send named them; a broadcast from before that order was stored falls back to created_at, id.", s.getBroadcast},
		{http.MethodPost, "/api/v1/issues/{key}/asks", authAny, "Open a question ask on an issue (options optional). After the write commits, a related-item check searches the project for the question and waits at most 5 seconds; the response's advice.suggestions says whether it found related items (outcome, with the reason in missing when not made), how it searched, the three items most like it with each ask item's answer state, and any already-answered ask that may settle the same question, with who answered and when.", s.createAsk},
		{http.MethodGet, "/api/v1/issues/{key}/asks", authAny, "List an issue's asks, each with its newest reply; ?state= filters, and ?thread=1 adds each ask's thread (replies, rewordings, answers, followers, claimant, route, route_holder).", s.listIssueAsks},
		{http.MethodGet, "/api/v1/issues/{key}/decisions", authAny, "The issue's decision records: {document, open_questions, decisions}. document is the dispatch:// address of its decisions.md, null (with no decisions) when it has none. open_questions are the issue's open asks whose block is in its spec, oldest first. Each decision is one `##` section of decisions.md in document order - a `###` section under `## Replaced` for a replaced one - with its heading block and markdown, the ask ids its blocks mention (kind person when it has any, agent otherwise), whether a paragraph of it starts `Unsure:`, the version its text last changed in, the Keep a person recorded, and one entry per link from its blocks to a spec block, each stale when the decision changed after it first linked that block and the block has not changed since.", s.getIssueDecisions},
		{http.MethodGet, "/api/v1/asks/open", authAny, "Open asks in one scope, with counts: ?author_session= for one session's own, or ?project= for every open ask on a project's issues and documents; exactly one is required.", s.listOpenAsks},
		{http.MethodGet, "/api/v1/asks/{id}", authAny, "Read one ask with its replies, edit history, answers and followers, plus claimant (the session holding its issue's claim) and route (its issue's route), whom a person's answer and replies also reach, and route_holder (the live session that route reaches now: the session itself or the role's holder); each is null when absent, route_holder also when nobody holds the route or the Envoy listener did not answer, and all are null on a project document's ask.", s.getAsk},
		{http.MethodGet, "/api/v1/asks/{id}/followers", authAny, "List the sessions an ask's answer and replies reach.", s.listAskFollowers},
		{http.MethodPut, "/api/v1/asks/{id}/followers/{session_id}", authAny, "Follow an ask (a credential holding the person scope for any session; every other for its own session alone).", s.followAsk},
		{http.MethodDelete, "/api/v1/asks/{id}/followers/{session_id}", authAny, "Unfollow an ask (a credential holding the person scope for any session; every other for its own session alone).", s.unfollowAsk},
		{http.MethodPatch, "/api/v1/asks/{id}", authAny, "Reword an open ask (its author, or a credential holding the person scope); a block ask's `:::ask` block is rewritten with it.", s.editAsk},
		{http.MethodPost, "/api/v1/asks/{id}/answer", authAny, "Answer an ask with selected options and/or text.", s.answerAsk},
		{http.MethodPost, "/api/v1/asks/{id}/resolve", authAny, "Retract or resolve an ask without answering it (kind retracted or resolved; withdrawn is written only by an issue's close, and an ask on a closed issue is 409 ISSUE_CLOSED); a block ask's block records it too.", s.resolveAsk},
		{http.MethodGet, "/api/v1/issues/{key}/comments", authAny, "List an issue's comments; ?artifact= scopes to a document.", s.listComments},
		{http.MethodPost, "/api/v1/issues/{key}/comments", authAny, "Comment on an issue, a document quote, or an ask. A reply names the comment it answers in reply_to, and one block of it in reply_to_block; with ask_id in place of reply_to, reply_to_block names one part of that ask.", s.createComment},
		{http.MethodPost, "/api/v1/comments/{id}/resolve", authAny, "Resolve a comment thread.", s.resolveComment},
		{http.MethodPost, "/api/v1/comments/{id}/reopen", authAny, "Reopen a resolved comment thread.", s.reopenComment},
		{http.MethodGet, "/api/v1/comments/{id}", authAny, "Read one comment with its thread.", s.getComment},
		{http.MethodGet, "/api/v1/comments/{id}/blocks", authAny, "A comment's current blocks in body order: {item, revision, indexed, blocks: [{id, type, text, ref, from, to, changed_revision, changed_at, embedding}]}, item the comment's graph node, revision its indexed revision, from and to UTF-16 code units into its stored body, changed_revision and changed_at the revision its block's text last changed in, embedding where meaning search stands on the block (embedded, pending, or none while no window covers it). While the comment is owed a block index, or was indexed from another body than it stores, indexed is false and blocks empty; the read writes nothing. An id written short - 8 characters or more of the hex digits and hyphens an id starts with - names the one comment that carries it, is 409 ID_AMBIGUOUS where two or more do, and is 404 NOT_FOUND where none does; any other non-uuid id is 400 COMMENT_ID_INPUT.", s.getCommentBlocks},
		{http.MethodPost, "/api/v1/comments/{id}/deliveries", authAny, "Retry a comment mention delivery; target is required when it has multiple mentions.", s.createCommentDelivery},
		{http.MethodPost, "/api/v1/comments/{id}/reply", authBearer, "The mentioned session's reply to a delivery; the session names itself in actor.", s.replyComment},
		{http.MethodPost, "/api/v1/comments/{id}/accept", authAny, "Accept a suggestion, applying its replacement.", s.acceptComment},
		{http.MethodPatch, "/api/v1/comments/{id}", authAny, "Edit a comment's body. The dashboard editor also sends blocks ([{id, from}], from in UTF-16 code units into the new body) and base_revision, the revision GET /api/v1/comments/{id}/blocks gave, so each block keeps its id; without them the ids follow the text.", s.editComment},
		{http.MethodPost, "/api/v1/comments/{id}/reject", authAny, "Reject a suggestion.", s.rejectComment},
		{http.MethodGet, "/api/v1/artifacts/{id}/asks", authAny, "List a document's asks.", s.listArtifactAsks},
		{http.MethodPost, "/api/v1/artifacts/{id}/asks", authAny, "Open an ask on a document. After the write commits, a related-item check searches the document's project for the question and waits at most 5 seconds; the response's advice.suggestions says whether it found related items (outcome, with the reason in missing when not made), how it searched, the three items most like it with each ask item's answer state, and any already-answered ask that may settle the same question, with who answered and when.", s.createArtifactAsk},
		{http.MethodGet, "/api/v1/artifacts/{id}/comments", authAny, "List a document's comments.", s.listArtifactComments},
		{http.MethodPost, "/api/v1/artifacts/{id}/comments", authAny, "Comment on a document or one of its quotes.", s.createArtifactComment},
		{http.MethodGet, "/api/v1/artifacts/{id}/events", authAny, "Page a document's event log.", s.listArtifactEvents},
		{http.MethodGet, "/api/v1/artifacts/{id}/references", authAny, "Cross-references to and from a document.", s.getArtifactReferences},
		{http.MethodGet, "/api/v1/artifacts/{id}/subscribers", authAny, "Sessions subscribed to a document's topics.", s.listArtifactSubscribers},
		{http.MethodDelete, "/api/v1/artifacts/{id}/subscribers/{session_id}", authAny, "Unsubscribe a session from a document.", s.unsubscribeArtifactSession},
		{http.MethodGet, "/api/v1/artifacts/{id}", authAny, "Read a document's metadata and current version.", s.getArtifact},
		{http.MethodPost, "/api/v1/artifacts/{id}/rebuild", authAny, "Rebuild a document history ygo cannot load from its latest saved markdown; a live document is 409 DOCUMENT_LIVE and one that loads is 409 DOCUMENT_LOADS.", s.rebuildArtifact},
		{http.MethodGet, "/api/v1/artifacts/{id}/reviews", authAny, "List a document's approval reviews.", s.listArtifactReviews},
		{http.MethodPost, "/api/v1/artifacts/{id}/reviews", authAny, "Approve or request changes on a document's latest settled version; answers the open approval ask when present, preserving its thread, and reviews every document that ask named.", s.createArtifactReview},
		{http.MethodPost, "/api/v1/artifacts/{id}/plan-reviews", authAny, "Record a plan review's approval of a plan version: {version}, on an issue's plan.md alone (409 NOT_PLAN_DOCUMENT otherwise). A plan is never sent to a person for approval, so POST /api/v1/artifacts/{id}/approval-requests refuses it with 409 PLAN_APPROVED_BY_REVIEW; a later plan version makes this approval stale, as a spec's goes stale.", s.createArtifactPlanReview},
		{http.MethodPost, "/api/v1/artifacts/{id}/approval-requests", authAny, "Open an approval ask for a document's latest version, with an optional summary, and with include_verification for the issue's Verification document beside it (409 NO_VERIFICATION_DOCUMENT when it has none), each at its own current version; the answer reviews every document the ask names. include_verification is for the issue's spec alone: on any other document, the Verification document itself included, it is 409 NOT_SPEC_DOCUMENT. A document a human approved before carries that approval on the ask (approval.approved: version, at, by). An open ask follows later versions of any document it names and waits on its agent until this route hands the same row back to a human. Every answer carries coverage, one line per requirement of the issue's spec with no check, or the single line `the spec marks no requirements` for a spec that marks none (GET /api/v1/issues/{key}/coverage), and none at all for a spec every requirement of which has a check; it is empty for a document that is not a spec, never names a missing plan task, and a gap refuses nothing. A coverage read that fails answers coverage_unread true and records the request all the same.", s.requestArtifactApproval},
		{http.MethodGet, "/api/v1/artifacts/{id}/blocks", authAny, "A document's blocks with markdown ranges, tokens, reference counts and each block's embedding, where meaning search stands on it (embedded, pending, or none while no window covers it); a typed block carries attributes, its type's attributes in declaration order as its opening line writes them ({name, value, kind}, the value unescaped and unquoted and the kind the one the block schema declares it, so a reader follows a block[] value as the pointers it holds and reads every other kind as text), and a table block lists its rows and cells by the indexes delete_row and delete_column take.", s.getArtifactBlocks},
		{http.MethodGet, "/api/v1/artifacts/{id}/blocks/{block_id}", authAny, "Where one block stands in a document: its path from the top-level block down (type, id, child index), and for a table block, row or cell the table's id, the row index (0 is the header), the cell's column index, the text of the header cell drawn above it and the row's cells; 404 TARGET_NOT_FOUND for an id the live document does not hold.", s.getArtifactBlockPath},
		{http.MethodPost, "/api/v1/artifacts/{id}/blocks/{block_id}/keep", authAny, "Record that the caller agrees with the decision that block opens, as the text they read it at: {text_sha256}, the decision's own text_sha256 from GET /api/v1/issues/{key}/decisions. A decision whose text changed since is 409 DECISION_CHANGED with nothing written, and a body without the sum is 400 INVALID_KEEP; a repeat of the sum as it stands moves the row. The block must be the heading block of a decision of an issue's decisions.md outside `## Replaced`: any other block is 404 TARGET_NOT_FOUND. Answers the keep and appends decision.kept, which wakes nobody.", s.keepDecision},
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
		{http.MethodGet, "/api/v1/me/state", authAny, "The caller's per-issue read state.", s.getUserState},
		{http.MethodPut, "/api/v1/me/issues/{key}/state", authAny, "Update the caller's read state for an issue.", s.putUserState},
		{http.MethodGet, "/api/v1/me/agents/state", authAny, "The caller's per-agent conversation state: cleared_before, read_through, and unread_replies (the session's replies to the caller's direct messages newer than both and not read by id).", s.getUserAgentState},
		{http.MethodPut, "/api/v1/me/agents/{session_id}/state", authAny, "Clear an agent's conversation for the caller (cleared_before hides exchanges at or before it), mark it read (read_through only moves forward), and/or mark some of its replies read by id (read_replies, the session's own messages); answers the session's state.", s.putUserAgentState},
		{http.MethodGet, "/api/v1/me/documents/{id}/read", authAny, "The caller's last read version of a document {artifact_id, version, read_at}; version and read_at are null when they have read none.", s.getDocumentRead},
		{http.MethodPut, "/api/v1/me/documents/{id}/read", authAny, "Record the caller's last read version of a document {version}: the Spec tab marks each block that changed since it. Answers the stored read; a version that is missing or negative is 400 INVALID_STATE.", s.putDocumentRead},
		{http.MethodPut, "/api/v1/me/asks/{id}/snooze", authAny, "Snooze an inbox row for the caller until snoozed_until.", s.putAskSnooze},
		{http.MethodDelete, "/api/v1/me/asks/{id}/snooze", authAny, "Un-snooze an inbox row for the caller.", s.deleteAskSnooze},
		{http.MethodGet, "/api/v1/credential-requests", authAny, "List credential requests the caller may decide (?approver=me only): those naming the caller, a shared secret's and a service's machine login, which anyone signed in decides, each secret request carrying approve_url, the secrets broker's page where it is decided; null without a configured secrets broker.", s.listCredentialPending},
		{http.MethodPost, "/api/v1/credential-requests/{id}/approve", authAny, "Approve a machine login as the caller with its typed code; the broker decides whether the caller is its approver, and refuses a secret request with 403 DECIDE_ON_BROKER_PAGE naming its page on the secrets broker.", s.approveCredentialRecord},
		{http.MethodPost, "/api/v1/credential-requests/{id}/deny", authAny, "Deny a machine login as the caller with its typed code; the broker decides whether the caller is its approver, and refuses a secret request with 403 DECIDE_ON_BROKER_PAGE naming its page on the secrets broker.", s.denyCredentialRecord},
		{http.MethodPost, "/api/v1/credential-requests/machine-lookup", authAny, "Resolve a pending machine login by its typed confirmation code, returning its facts.", s.lookupMachineCredential},
		{http.MethodGet, "/api/v1/credential-grants", authAny, "List credential grants the caller may revoke (?approver=me only).", s.listCredentialGrants},
		{http.MethodPost, "/api/v1/credential-grants/{id}/revoke", authAny, "Revoke a credential grant as the caller, its approver or its enrollment's operator.", s.revokeCredentialGrant},
		{http.MethodGet, "/api/v1/machine-logins", authAny, "List the machine logins the caller may revoke that can still reach a secret: the secrets broker's launcher credentials for every service's login, whoever approved it, and for the caller's own machines, unexpired or expired with a session still running, each naming who approved it.", s.listMachineLogins},
		{http.MethodPost, "/api/v1/machine-logins/{id}/revoke", authAny, "Revoke a machine login, expired or not, ending every session it enrolled, a service's pods included: anyone signed in revokes a service's login, and only the person who approved a person's machine login revokes that one.", s.revokeMachineLogin},
		{http.MethodGet, "/api/v1/signins", authAny, "List the caller's own sign-ins on their machines (`agent-secrets login`), one per box key they have not revoked, each naming the machine's account, the key's thumbprint, when the person signed in there and when that machine last used it. Nobody lists another person's: Dispatch names the person from its own session.", s.listSignIns},
		{http.MethodPost, "/api/v1/signins/{id}/revoke", authAny, "End one of the caller's own sign-ins: that machine trades no token for them again and every access token already minted under it ends, while their sign-ins on other machines keep working. A sign-in of anyone else is an unknown id.", s.revokeSignIn},
		{http.MethodGet, "/api/v1/credentials/capabilities", authAny, "What the credential pages may offer against this deployment's secrets broker, relayed from the broker's own RFC 8414 metadata rather than from any Dispatch setting: machine_code_page says whether the broker still serves the device grant, so whether a machine ever prints a code for a person to type.", s.getCredentialCapabilities},
	}
	// The live-socket disconnect, live-socket rotate, quiesce and publish hooks (the 1st, 2nd, 3rd
	// and 5th below) act on every process serving the database, answering once each has
	// (test_hooks_fanout.go); outside-schema (the 4th) writes the stored history, which every
	// process reads, and the room, room-counter, persistence and live-write hooks and the
	// slow-write arm (the last) each act on the process whose call they land on.
	if s.deps.TestHooksEnabled {
		rows = append(rows,
			routeRow{http.MethodPost, "/api/v1/live/_test/disconnect", authAny, "Test hook: close every open live socket on every process with 1012, as a restart does.", s.disconnectLiveSockets},
			routeRow{http.MethodPost, "/api/v1/live/_test/rotate", authAny, "Test hook: close every open live socket on every process with 4001 (max age), as its age does.", s.rotateLiveSockets},
			routeRow{http.MethodPost, "/api/v1/artifacts/_test/quiesce", authAny, "Test hook: close every live document on every process and finish the settlements in flight.", s.quiesceDocuments},
			routeRow{http.MethodPost, "/api/v1/artifacts/{id}/_test/outside-schema", authAny, "Test hook: write a crafted tree outside the Proof schema.", s.injectArtifactSchemaFailure},
			routeRow{http.MethodPost, "/api/v1/agents/{session_id}/stream/_test/publish", authAny, "Test hook: publish one frame to a session's conversation viewers on every process.", s.publishAgentStreamFrame},
			routeRow{http.MethodGet, "/api/v1/artifacts/_test/rooms", authAny, "Test hook: list the documents this process holds a room of.", s.listResidentRooms},
			routeRow{http.MethodGet, "/api/v1/artifacts/{id}/_test/room-counters", authAny, "Test hook: count this process's room failures, lock-waiting compactions and room closes of a document, time its settlement resumptions of it, and read the awareness client entries its room holds now.", s.getRoomCounters},
			routeRow{http.MethodPost, "/api/v1/artifacts/{id}/_test/hold-persistence", authAny, "Test hook: hold this process's appends of a document's browser updates for a duration.", s.holdRoomPersistence},
			routeRow{http.MethodPut, "/api/v1/artifacts/{id}/_test/live-write-pauses/{point}", authAny, "Test hook: park the document's next live write at a point (after-first-append, after-commit).", s.armLiveWritePause},
			routeRow{http.MethodGet, "/api/v1/artifacts/{id}/_test/live-write-pauses/{point}", authAny, "Test hook: wait until a live write parks at the armed pause.", s.awaitLiveWriteParked},
			routeRow{http.MethodDelete, "/api/v1/artifacts/{id}/_test/live-write-pauses/{point}", authAny, "Test hook: release the live write parked at the pause and disarm it.", s.releaseLiveWritePause},
			routeRow{http.MethodPost, "/api/v1/writes/_test/slow", authAny, "Test hook: {method, path, delay_ms} delays every keyed run of that method and path by delay_ms after its key is claimed, on the handler's own context; 0 disarms.", s.armSlowWrite},
		)
	}
	routes := make([]apiRoute, len(rows))
	for index, row := range rows {
		name := row.Method + " " + row.Pattern
		routes[index] = apiRoute{row, writeClasses[name], routeScopes[name]}
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
			Scopes:      route.OAuthScopes,
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
