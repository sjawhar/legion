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

// public is a route anyone may call.
func public(h func(*server, http.ResponseWriter, *http.Request)) routeHandler {
	return routeHandler{auth: authNone, serve: func(s *server, w http.ResponseWriter, r *http.Request, _ caller) { h(s, w, r) }}
}

// launcherAuth is a route for a launcher's proof (payload carries "lid", not "eid").
func launcherAuth(h func(*server, http.ResponseWriter, *http.Request, enroll.Credential)) routeHandler {
	return routeHandler{auth: authLauncher, serve: func(s *server, w http.ResponseWriter, r *http.Request, who caller) {
		h(s, w, r, who.launcher)
	}}
}

// sessionAuth is a route for an enrolled session's proof; the handler gets its enrollment id.
func sessionAuth(h func(*server, http.ResponseWriter, *http.Request, string)) routeHandler {
	return routeHandler{auth: authProof, serve: func(s *server, w http.ResponseWriter, r *http.Request, who caller) {
		h(s, w, r, who.enrollment)
	}}
}

// uiAuth is a route for Dispatch's server, authenticated with the shared UI bearer token
// (constant-time compare against Deps.UIToken). The bearer vouches for the approver login in each
// decision and revoke body: Dispatch's server sets it from the login its own session resolved,
// never from anything the browser sent, so the UI token is an approval credential and only
// Dispatch holds it.
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
// route does; the broker's generated HTTP reference (scripts/docs/broker/refgen) prints it and
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
		// Request secrets: the rules grant or deny them at once, or a person must approve them.
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
		// List the live grants the named person approved or operates.
		{http.MethodGet, "/v1/grants", uiAuth((*server).listGrantsForApprover)},
		// End a grant as its approver or as its enrollment's operator.
		{http.MethodPost, "/v1/grants/{id}/revoke-by-approver", uiAuth((*server).revokeByApprover)},
		// Report whether the broker can reach its database.
		{http.MethodGet, "/healthz", public((*server).healthz)},
	}
}
