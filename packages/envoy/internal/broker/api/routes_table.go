// packages/envoy/internal/broker/api/routes_table.go
package api

import "net/http"

type routeAuth int

const (
	authNone routeAuth = iota
	authLauncher
	authProof
	authHumanOrProof
)

type apiRoute struct {
	Method  string
	Pattern string
	Auth    routeAuth
	Handler func(*server, http.ResponseWriter, *http.Request)
}

// routes is the one list of the broker's routes; a new route is a new row here, never a bare
// mux.HandleFunc. The contract for every row is the AGENTC-393 overview document.
func routes() []apiRoute {
	return []apiRoute{
		{http.MethodPost, "/v1/enrollments", authLauncher, (*server).createEnrollment},
		{http.MethodDelete, "/v1/enrollments/{id}", authLauncher, (*server).deleteEnrollment},
		{http.MethodPost, "/v1/launcher-credentials", authNone, (*server).requestLauncherCredential},
		{http.MethodGet, "/v1/launcher-credentials/{pending}", authNone, (*server).readLauncherCredential},
		{http.MethodPost, "/v1/enrollments/{id}/renew", authProof, (*server).renewEnrollment},
		{http.MethodGet, "/v1/enrollments/self", authProof, (*server).readSelf},
		{http.MethodPost, "/v1/requests", authProof, (*server).createRequest},
		{http.MethodGet, "/v1/requests/{id}", authProof, (*server).readRequest},
		{http.MethodPost, "/v1/requests/{id}/cancel", authProof, (*server).cancelRequest},
		{http.MethodPost, "/v1/grants/{id}/values", authProof, (*server).grantValues},
		{http.MethodPost, "/v1/grants/{id}/revoke", authHumanOrProof, (*server).revokeGrant},
		{http.MethodGet, "/healthz", authNone, (*server).healthz},
	}
}
