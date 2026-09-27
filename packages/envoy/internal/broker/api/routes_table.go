package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/broker/enroll"
	"github.com/sjawhar/envoy/internal/broker/requests"
)

type routeAuth int

const (
	authNone routeAuth = iota
	authLauncher
	authProof
	authHumanOrProof
)

// caller is who server.authenticate proved a request came from. Which field is set follows from
// the route's authentication, and only the adapter for that authentication reads it.
type caller struct {
	launcher   enroll.Credential
	enrollment string
	human      string
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

// launcherAuth is a route for a launcher's bearer credential.
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

// humanOrSessionAuth is a route for either an enrolled session's proof or a Dispatch-authenticated
// human; the handler gets whichever it was as a requests.Revoker.
func humanOrSessionAuth(h func(*server, http.ResponseWriter, *http.Request, requests.Revoker)) routeHandler {
	return routeHandler{auth: authHumanOrProof, serve: func(s *server, w http.ResponseWriter, r *http.Request, who caller) {
		h(s, w, r, requests.Revoker{EnrollmentID: who.enrollment, Login: who.human})
	}}
}

type apiRoute struct {
	Method  string
	Pattern string
	Handler routeHandler
}

// routes is the one list of the broker's routes; a new route is a new row here, never a bare
// mux.HandleFunc. The contract for every row is the AGENTC-393 overview document.
func routes() []apiRoute {
	return []apiRoute{
		{http.MethodPost, "/v1/enrollments", launcherAuth((*server).createEnrollment)},
		{http.MethodDelete, "/v1/enrollments/{id}", launcherAuth((*server).deleteEnrollment)},
		{http.MethodPost, "/v1/launcher-credentials", public((*server).requestLauncherCredential)},
		{http.MethodGet, "/v1/launcher-credentials/{pending}", public((*server).readLauncherCredential)},
		{http.MethodPost, "/v1/enrollments/{id}/renew", sessionAuth((*server).renewEnrollment)},
		{http.MethodGet, "/v1/enrollments/self", sessionAuth((*server).readSelf)},
		{http.MethodPost, "/v1/requests", sessionAuth((*server).createRequest)},
		{http.MethodGet, "/v1/requests/{id}", sessionAuth((*server).readRequest)},
		{http.MethodPost, "/v1/requests/{id}/cancel", sessionAuth((*server).cancelRequest)},
		{http.MethodPost, "/v1/grants/{id}/values", sessionAuth((*server).grantValues)},
		{http.MethodPost, "/v1/grants/{id}/revoke", humanOrSessionAuth((*server).revokeGrant)},
		{http.MethodGet, "/healthz", public((*server).healthz)},
	}
}
