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
// carry no caller identity at all — the shared bearer authenticates which service is relaying
// (Dispatch's server), never who approves; every UI handler takes its subject (an approver's
// login, a record id) from the path or body instead.
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
// (constant-time compare against Deps.UIToken). It never proves who approves — the WebAuthn
// assertion each of these handlers verifies is the only authorization signal.
func uiAuth(h func(*server, http.ResponseWriter, *http.Request)) routeHandler {
	return routeHandler{auth: authUI, serve: func(s *server, w http.ResponseWriter, r *http.Request, _ caller) { h(s, w, r) }}
}

type apiRoute struct {
	Method  string
	Pattern string
	Handler routeHandler
}

// routes is the one list of the broker's routes; a new route is a new row here, never a bare
// mux.HandleFunc. The contract for every row is the AGENTC-393 overview document (contract v9).
func routes() []apiRoute {
	return []apiRoute{
		{http.MethodPost, "/v1/enrollments", launcherAuth((*server).createEnrollment)},
		{http.MethodDelete, "/v1/enrollments/{id}", launcherAuth((*server).deleteEnrollment)},
		{http.MethodPost, "/v1/launcher-credentials", public((*server).machineLogin)},
		{http.MethodGet, "/v1/launcher-credentials/{pending}", public((*server).readMachineLogin)},
		{http.MethodPost, "/v1/enrollments/{id}/renew", sessionAuth((*server).renewEnrollment)},
		{http.MethodGet, "/v1/enrollments/self", sessionAuth((*server).readSelf)},
		{http.MethodPost, "/v1/requests", sessionAuth((*server).createRequest)},
		{http.MethodGet, "/v1/requests/{id}", sessionAuth((*server).readRequest)},
		{http.MethodPost, "/v1/requests/{id}/cancel", sessionAuth((*server).cancelRequest)},
		{http.MethodPost, "/v1/grants/{id}/values", sessionAuth((*server).grantValues)},
		{http.MethodPost, "/v1/grants/{id}/revoke", sessionAuth((*server).revokeGrant)},
		{http.MethodGet, "/v1/pending", uiAuth((*server).listPending)},
		{http.MethodGet, "/v1/credential-requests/{record}", uiAuth((*server).readRecord)},
		{http.MethodPost, "/v1/credential-requests/{record}/approve", uiAuth((*server).approveRecord)},
		{http.MethodPost, "/v1/credential-requests/{record}/deny", uiAuth((*server).denyRecord)},
		{http.MethodPost, "/v1/machine-logins/lookup", uiAuth((*server).lookupMachineLogin)},
		{http.MethodGet, "/v1/approvers/{login}/keys", uiAuth((*server).listKeys)},
		{http.MethodPost, "/v1/approvers/{login}/keys/register/begin", uiAuth((*server).beginRegister)},
		{http.MethodPost, "/v1/approvers/{login}/keys/register/finish", uiAuth((*server).finishRegister)},
		{http.MethodPost, "/v1/approvers/{login}/keys/endorse/begin", uiAuth((*server).beginEndorse)},
		{http.MethodPost, "/v1/approvers/{login}/keys/endorse/finish", uiAuth((*server).finishEndorse)},
		{http.MethodGet, "/v1/grants", uiAuth((*server).listGrantsForApprover)},
		{http.MethodPost, "/v1/grants/{id}/revoke-by-approver", uiAuth((*server).revokeByApprover)},
		{http.MethodGet, "/healthz", public((*server).healthz)},
	}
}
