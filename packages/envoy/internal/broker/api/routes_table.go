package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/broker/enroll"
)

type routeAuth int

const (
	authNone routeAuth = iota
	authLauncher
	authProof
	authUI
)

// caller is who server.authenticate proved a request came from. Which field is set follows from
// the route's authentication, and only the adapter for that authentication reads it. UI routes
// carry no caller identity of their own — the shared bearer authenticates Dispatch's server, and
// every UI handler takes its subject (an approver's login, a record id) from the path or body
// Dispatch sends.
type caller struct {
	launcher   enroll.Credential
	enrollment string
}

// routeHandler is a handler together with the authentication its signature needs. Only the
// adapters below build one, and each fixes both halves: a handler taking a launcher credential
// can only ever run behind launcher authentication, so no route row can pair a handler with the
// wrong kind of caller.
type routeHandler struct {
	auth  routeAuth
	serve func(s *server, w http.ResponseWriter, r *http.Request, who caller)
}

// public is a route anyone may call, with no credential.
func public(h func(*server, http.ResponseWriter, *http.Request)) routeHandler {
	return routeHandler{auth: authNone, serve: func(s *server, w http.ResponseWriter, r *http.Request, _ caller) { h(s, w, r) }}
}

// launcherAuth is a route for a machine's launcher (agent-secrets-helper, or the Legion daemon),
// which signs each call's Proof header with the key its machine login's credential is bound to.
func launcherAuth(h func(*server, http.ResponseWriter, *http.Request, enroll.Credential)) routeHandler {
	return routeHandler{auth: authLauncher, serve: func(s *server, w http.ResponseWriter, r *http.Request, who caller) {
		h(s, w, r, who.launcher)
	}}
}

// sessionAuth is a route for an enrolled agent session, which signs each call's Proof header with
// its own key, and which acts only on its own enrollment, requests and grants.
func sessionAuth(h func(*server, http.ResponseWriter, *http.Request, string)) routeHandler {
	return routeHandler{auth: authProof, serve: func(s *server, w http.ResponseWriter, r *http.Request, who caller) {
		h(s, w, r, who.enrollment)
	}}
}

// uiAuth is a route for Dispatch's server, which sends the broker's UI token as a bearer token and
// names the person acting (the approver) in the body or query. The broker trusts that name, so the
// UI token is an approval credential: only Dispatch's server holds it.
func uiAuth(h func(*server, http.ResponseWriter, *http.Request)) routeHandler {
	return routeHandler{auth: authUI, serve: func(s *server, w http.ResponseWriter, r *http.Request, _ caller) { h(s, w, r) }}
}

type apiRoute struct {
	Method  string
	Pattern string
	Handler routeHandler
}

// routes is the one list of the broker's routes; a new route is a new row here, never a bare
// mux.HandleFunc. The contract for every row is the shared broker contract
// (dispatch://AGENTC-393/artifact/plan-overview-md). The comment above each row says what the
// route does; the broker's generated HTTP reference (cmd/broker-refgen) prints it and
// refuses a row without one.
func routes() []apiRoute {
	return []apiRoute{
		// Enroll a box's, host session's or pod's signing key under the caller's machine credential.
		{http.MethodPost, "/v1/enrollments", launcherAuth((*server).createEnrollment)},
		// Revoke an enrollment made under the caller's machine credential's operator.
		{http.MethodDelete, "/v1/enrollments/{id}", launcherAuth((*server).deleteEnrollment)},
		// Start a machine login: answers the confirmation code a person types and a pending id.
		{http.MethodPost, "/v1/launcher-credentials", public((*server).machineLogin)},
		// Poll a machine login by its pending id; once approved, answers its credential's id.
		{http.MethodGet, "/v1/launcher-credentials/{pending}", public((*server).readMachineLogin)},
		// Extend the calling session's lease.
		{http.MethodPost, "/v1/enrollments/{id}/renew", sessionAuth((*server).renewEnrollment)},
		// Describe the calling session's enrollment and its live grants.
		{http.MethodGet, "/v1/enrollments/self", sessionAuth((*server).readSelf)},
		// Request secrets: the policy grants or denies them at once, or a person must approve them.
		{http.MethodPost, "/v1/requests", sessionAuth((*server).createRequest)},
		// Read one of the calling session's requests.
		{http.MethodGet, "/v1/requests/{id}", sessionAuth((*server).readRequest)},
		// Cancel one of the calling session's pending requests.
		{http.MethodPost, "/v1/requests/{id}/cancel", sessionAuth((*server).cancelRequest)},
		// Release a live grant's values to the session that holds it.
		{http.MethodPost, "/v1/grants/{id}/values", sessionAuth((*server).grantValues)},
		// End a grant the calling session holds.
		{http.MethodPost, "/v1/grants/{id}/revoke", sessionAuth((*server).revokeGrant)},
		// List the pending requests and machine logins the named person decides.
		{http.MethodGet, "/v1/pending", uiAuth((*server).listPending)},
		// Read a credential-request record: what was asked, by whom, why, and its decision.
		{http.MethodGet, "/v1/credential-requests/{record}", uiAuth((*server).readRecord)},
		// Approve a pending record as the named person; a machine login also needs its code.
		{http.MethodPost, "/v1/credential-requests/{record}/approve", uiAuth((*server).approveRecord)},
		// Deny a pending record as the named person; a machine login also needs its code.
		{http.MethodPost, "/v1/credential-requests/{record}/deny", uiAuth((*server).denyRecord)},
		// Find a pending machine login by the confirmation code its machine shows.
		{http.MethodPost, "/v1/machine-logins/lookup", uiAuth((*server).lookupMachineLogin)},
		// List the machine logins the named person approved, their own machines' and any
		// service's, such as the Legion daemon's, that are not revoked and are unexpired or expired
		// with a session still running.
		{http.MethodGet, "/v1/launcher-credentials", uiAuth((*server).listLauncherCredentials)},
		// End a machine login, expired or not, as the person who approved it: its launcher proofs
		// stop authenticating and every session it enrolled, pods included, ends with its grants and
		// pending requests.
		{http.MethodPost, "/v1/launcher-credentials/{id}/revoke-by-approver", uiAuth((*server).revokeLauncherCredential)},
		// List the live grants of the named person's sessions, automatic or approved, and those the
		// person approved.
		{http.MethodGet, "/v1/grants", uiAuth((*server).listGrantsForApprover)},
		// End a grant as its approver or as its enrollment's operator. The operator's revoke also
		// withholds the secrets the grant's request got automatically from that session: its other
		// grants that got them automatically end too, and it asks before it gets them again.
		{http.MethodPost, "/v1/grants/{id}/revoke-by-approver", uiAuth((*server).revokeByApprover)},
		// Report whether the broker can reach its database.
		{http.MethodGet, "/healthz", public((*server).healthz)},
	}
}
