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
	authRenewProof
	authAccount
)

// caller is who server.authenticate proved a request came from. Which field is set follows from
// the route's authentication, and only the adapter for that authentication reads it.
type caller struct {
	launcher   enroll.Credential
	enrollment string
	// account is the name BROKER_API_ACCOUNTS gives the service whose ServiceAccount token
	// authenticated the request, so a handler that acts can log which caller acted.
	account string
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

// renewAuth is session authentication for the one route that gives an ended enrollment a distinct
// refusal. A skewed/malformed proof remains PROOF_INVALID; only proof.ErrNotLive is
// ENROLLMENT_NOT_LIVE so the pod renewer can stop rather than retry an enrollment that cannot
// recover.
func renewAuth(h func(*server, http.ResponseWriter, *http.Request, string)) routeHandler {
	return routeHandler{auth: authRenewProof, serve: func(s *server, w http.ResponseWriter, r *http.Request, who caller) {
		h(s, w, r, who.enrollment)
	}}
}

// accountAuth is a route for a service calling the broker with its own Kubernetes ServiceAccount
// token: Dispatch's server, which relays a person's decisions, and whatever resource server reads
// a token back through introspection. The token is verified against the broker's Kubernetes
// issuer and its account must be one BROKER_API_ACCOUNTS lists, and the handler is given that
// entry's name — the caller's identity, which it logs when it acts.
func accountAuth(h func(*server, http.ResponseWriter, *http.Request, string)) routeHandler {
	return routeHandler{auth: authAccount, serve: func(s *server, w http.ResponseWriter, r *http.Request, who caller) {
		h(s, w, r, who.account)
	}}
}

type apiRoute struct {
	Method  string
	Pattern string
	Handler routeHandler
}

// routes is the one list of the broker's routes; a new route is a new row here, never a bare
// mux.HandleFunc. The comment above each row says what the route does; the broker's generated
// HTTP reference (cmd/broker-refgen, published at
// https://sjawhar.github.io/legion/broker/reference/api/) prints it and refuses a row without one.
func routes() []apiRoute {
	return []apiRoute{
		// Enroll a session's or pod's signing key under the caller's machine credential.
		{http.MethodPost, "/v1/enrollments", launcherAuth((*server).createEnrollment)},
		// Revoke an enrollment made under the caller's machine credential's operator, or a pod
		// enrollment the caller's own machine credential made.
		{http.MethodDelete, "/v1/enrollments/{id}", launcherAuth((*server).deleteEnrollment)},
		// Log a service in with its own Kubernetes ServiceAccount token, which no person approves:
		// answers its launcher credential's id and expiry.
		{http.MethodPost, "/v1/launcher-credentials/token", public((*server).tokenLogin)},
		// Start a person's machine login as an OAuth device authorization (RFC 8628): answers the
		// device code the machine polls with and the code the person types. Rate-limited per source
		// address and per person its login_hint names; mounted only when the broker runs its OAuth
		// provider.
		{http.MethodPost, "/v1/oauth/device_authorization", public((*server).deviceAuthorization)},
		// Exchange an approved device code, or a refresh token, for a DPoP-bound token pair naming the
		// machine login's launcher credential; mounted only when the broker runs its OAuth provider.
		{http.MethodPost, "/v1/oauth/token", public((*server).oauthToken)},
		// Read one of the broker's access tokens back (RFC 7662): whether it is live and, when it
		// is, the session, the person and the project it carries, and the key it is bound to.
		// Mounted only when the broker runs its OAuth provider.
		{http.MethodPost, "/v1/oauth/introspect", accountAuth((*server).introspect)},
		// Extend the calling session's lease.
		{http.MethodPost, "/v1/enrollments/{id}/renew", renewAuth((*server).renewEnrollment)},
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
		{http.MethodGet, "/v1/pending", accountAuth((*server).listPending)},
		// Read a credential-request record: what was asked, by whom, why, and its decision.
		{http.MethodGet, "/v1/credential-requests/{record}", accountAuth((*server).readRecord)},
		// Open an approval of a pending record that the named person signs in for themselves:
		// answers the broker's own page to send them to, and decides nothing. A machine login
		// also needs its code. The caller is a named service identity, its own ServiceAccount
		// token, which says only which service is asking: the record is decided when the person
		// the body names has signed in to the broker's approval issuer and the broker has
		// compared that sign-in's email with the record's own approver rule. On a broker whose
		// BROKER_APPROVAL_* group is unset, 503 APPROVAL_SIGN_IN_OFF.
		{http.MethodPost, "/v1/credential-requests/{record}/approval", accountAuth((*server).openApproval)},
		// Approve a pending record as the named person, on the calling service's word; a machine
		// login also needs its code. Refused 403 APPROVAL_NEEDS_PERSON on a broker with an
		// approval issuer, where an approval carries the approver's own sign-in instead (the
		// approval route above). It serves the rollout's first half alone: a deployment whose
		// BROKER_APPROVAL_* group is not set yet.
		{http.MethodPost, "/v1/credential-requests/{record}/approve", accountAuth((*server).approveRecord)},
		// Deny a pending record as the named person; a machine login also needs its code. A
		// denial takes access away and gives none, so it stays on the calling service's word
		// whatever the approval group says.
		{http.MethodPost, "/v1/credential-requests/{record}/deny", accountAuth((*server).denyRecord)},
		// Show what an approval covers, from the broker's own record, with the button that starts
		// the person's sign-in: an HTML page a person's browser reaches by the link the approval
		// route answered, with no credential of any kind. Mounted only on a broker with an
		// approval issuer.
		{http.MethodGet, "/approve/{id}", public((*server).approvalPage)},
		// Start that sign-in: sets the browser's own binding cookie and redirects to the issuer's
		// authorization endpoint, asking for a recent authentication of the approver's account
		// and nothing to keep.
		{http.MethodPost, "/approve/{id}/start", public((*server).startApprovalSignIn)},
		// Take the issuer's answer: verifies the ID token against the issuer's keys, this
		// client, the cookie's nonce, a verified email in the deployment's domain and an
		// authentication no older than five minutes, requires that email to be the approval's own
		// approver, spends the approval once and applies the decision as that person.
		{http.MethodGet, "/approve/callback", public((*server).approvalCallback)},
		// Find a pending machine login by the confirmation code its machine shows.
		{http.MethodPost, "/v1/machine-logins/lookup", accountAuth((*server).lookupMachineLogin)},
		// List the machine logins the named person may revoke, every service's, such as the Legion
		// daemon's, whoever approved it, and the person's own machines', that are not revoked and
		// are unexpired or expired with a session still running, each naming who approved it.
		{http.MethodGet, "/v1/launcher-credentials", accountAuth((*server).listLauncherCredentials)},
		// End a machine login, expired or not: a service's (its token login, or one a person approved
		// before token logins) as anyone signed in, a person's machine's as the person who approved
		// it. Its launcher proofs stop authenticating and every session it enrolled, pods included,
		// ends with its grants and pending requests. The revoking person is the login the request
		// names, taken on the caller's word until dispatch://INFRA-314.
		{http.MethodPost, "/v1/launcher-credentials/{id}/revoke-by-approver", accountAuth((*server).revokeLauncherCredential)},
		// List the live grants of the named person's sessions, automatic or approved, and those the
		// person approved.
		{http.MethodGet, "/v1/grants", accountAuth((*server).listGrantsForApprover)},
		// End a grant as its approver or as its enrollment's operator. The operator's revoke also
		// withholds the secrets the grant's request got automatically from that session, even when
		// the grant had already ended: its other grants that got them automatically end too, and it
		// asks before it gets them again. The revoking person is the login the request names, taken
		// on the caller's word until dispatch://INFRA-314.
		{http.MethodPost, "/v1/grants/{id}/revoke-by-approver", accountAuth((*server).revokeByApprover)},
		// List the logins of the calling credential's operator's own machines, as Dispatch's
		// machine-logins page lists them, without the service logins that page also lists.
		{http.MethodGet, "/v1/operator/machines", launcherAuth((*server).listOperatorMachines)},
		// End one of the operator's own machines' logins: no proof signed with it authenticates again
		// and every session it enrolled ends. A service's login is not found here, like an unknown
		// id. Revoking the calling machine's own login ends this machine's access.
		{http.MethodPost, "/v1/operator/machines/{id}/revoke", launcherAuth((*server).revokeOperatorMachine)},
		// Retire the calling machine login after its machine logged in again: it enrolls nothing
		// more and cannot refresh, while the sessions it enrolled keep running and renewing with their
		// own keys. {id} must be the calling credential.
		{http.MethodPost, "/v1/operator/machines/{id}/retire", launcherAuth((*server).retireOperatorMachine)},
		// List the live grants of the operator's sessions and those the operator approved, exactly as
		// Dispatch's Live grants page lists them.
		{http.MethodGet, "/v1/operator/grants", launcherAuth((*server).listOperatorGrants)},
		// End a grant as the operator: the revocation Dispatch's page offers, which also withholds an
		// automatic grant's secrets from its session.
		{http.MethodPost, "/v1/operator/grants/{id}/revoke", launcherAuth((*server).revokeOperatorGrant)},
		// The namespace prefix, AWS account and region, and the agent-secrets key the broker serves:
		// what the CLI needs so it and the broker never disagree about which secret a name means.
		{http.MethodGet, "/v1/settings", public((*server).readSettings)},
		// Reread one secret from Secrets Manager now, as whoever just wrote it may ask: the change is
		// served, or gone, within seconds instead of at the next five-minute reload. Open to any caller,
		// and rate-limited both per source address and over every caller at once, since each reread holds
		// the policy's writer lock while it reads; a reread only moves the broker toward AWS's own truth.
		{http.MethodPost, "/v1/secrets/{name}/reread", public((*server).rereadSecret)},
		// Report whether the broker can reach its database. From SIGTERM onward it answers 503
		// SHUTTING_DOWN instead (Deps.Draining) while the process goes on serving every other route,
		// which is what takes a stopping pod out of its Service's endpoints before it stops
		// accepting connections. So this is a readiness probe and must never be used as a liveness
		// probe: a liveness probe on it would have the kubelet kill the container in the middle of
		// the drain, dropping the requests the pod was draining.
		{http.MethodGet, "/healthz", public((*server).healthz)},
	}
}
