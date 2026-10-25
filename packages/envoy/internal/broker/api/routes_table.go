package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/broker/approval"
	"github.com/sjawhar/envoy/internal/broker/enroll"
)

type routeAuth int

const (
	authNone routeAuth = iota
	authLauncher
	authProof
	authRenewProof
	authAccount
	authSignIn
)

// caller is who server.authenticate proved a request came from. Which field is set follows from
// the route's authentication, and only the adapter for that authentication reads it.
type caller struct {
	launcher   enroll.Credential
	enrollment string
	// account is the name BROKER_API_ACCOUNTS gives the service whose ServiceAccount token
	// authenticated the request, so a handler that acts can log which caller acted.
	account string
	// signIn is the broker sign-in session a request to the approval page carries, the zero
	// approval.Session when it carries none that is live.
	signIn approval.Session
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
// token: Dispatch's server, which lists what waits on a person, relays a person's machine-login
// decisions and revokes, and links the broker's own page for a secret request; and whatever
// resource server reads a token back through introspection. The token is verified against the
// broker's Kubernetes issuer and its account must be one BROKER_API_ACCOUNTS lists, and the
// handler is given that entry's name — the caller's identity, which it logs when it acts.
func accountAuth(h func(*server, http.ResponseWriter, *http.Request, string)) routeHandler {
	return routeHandler{auth: authAccount, serve: func(s *server, w http.ResponseWriter, r *http.Request, who caller) {
		h(s, w, r, who.account)
	}}
}

// signInAuth is a route of the broker's own approval page, for a person in a browser signed in to
// the broker itself (OpenID Connect with the broker's own client, BROKER_SIGNIN_*). The handler is
// given the session the request's __Host-broker-session cookie names, or the zero session when it
// names none that is live, since what that means is the route's to answer: the page starts a
// sign-in, a decision is refused. While sign-in is off every such route answers 503 naming the
// settings that turn it on. Mounted only when the broker builds its approval page.
func signInAuth(h func(*approval.Page, http.ResponseWriter, *http.Request, approval.Session)) routeHandler {
	return routeHandler{auth: authSignIn, serve: func(s *server, w http.ResponseWriter, r *http.Request, who caller) {
		h(s.deps.Approval, w, r, who.signIn)
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
		// List the pending requests and machine logins the named person decides, each secret
		// request with the broker's own page where that person decides it.
		{http.MethodGet, "/v1/pending", accountAuth((*server).listPending)},
		// Approve a pending machine login as the named person, who must type its code: the broker
		// takes the person's name on the caller's word. A secret request is decided only on the
		// broker's own page by a person signed in to the broker, so this refuses one, writing
		// nothing, with 403 DECIDE_ON_BROKER_PAGE naming that page.
		{http.MethodPost, "/v1/credential-requests/{record}/approve", accountAuth((*server).approveRecord)},
		// Deny a pending machine login as the named person, who must type its code, on the caller's
		// word as with approve. A secret request is refused, writing nothing, with 403
		// DECIDE_ON_BROKER_PAGE naming the page where a person signed in to the broker denies it.
		{http.MethodPost, "/v1/credential-requests/{record}/deny", accountAuth((*server).denyRecord)},
		// Find a pending machine login by the confirmation code its machine shows.
		{http.MethodPost, "/v1/machine-logins/lookup", accountAuth((*server).lookupMachineLogin)},
		// List the machine logins the named person may revoke, every service's, such as the Legion
		// daemon's, whoever approved it, and the person's own machines', that are not revoked and
		// are unexpired or expired with a session still running, each naming who approved it. The
		// person is the one the caller names.
		{http.MethodGet, "/v1/launcher-credentials", accountAuth((*server).listLauncherCredentials)},
		// End a machine login, expired or not: a service's (its token login, or one a person approved
		// before token logins) as anyone signed in, a person's machine's as the person who approved
		// it. Its launcher proofs stop authenticating and every session it enrolled, pods included,
		// ends with its grants and pending requests. The revoking person is the login the request
		// names, which the broker still takes on the caller's word: a revoke only takes power away.
		{http.MethodPost, "/v1/launcher-credentials/{id}/revoke-by-approver", accountAuth((*server).revokeLauncherCredential)},
		// List the live grants of the named person's sessions, automatic or approved, and those the
		// person approved. The person is the one the caller names.
		{http.MethodGet, "/v1/grants", accountAuth((*server).listGrantsForApprover)},
		// End a grant as its approver or as its enrollment's operator. The operator's revoke also
		// withholds the secrets the grant's request got automatically from that session, even when
		// the grant had already ended: its other grants that got them automatically end too, and it
		// asks before it gets them again. The revoking person is the login the request names, which
		// the broker still takes on the caller's word: a revoke only takes power away.
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
		// A person's approval page for one credential-request record, an HTML page on the broker's
		// own origin: the record's secret names, the enrollment the broker verified asked, the
		// session the request claims when it differs, the reason, the lifetime, the times, the
		// approver and the state, then every other pending secret request the person may decide,
		// with Approve and Deny for each the person may decide and one "Approve all N shown". A
		// machine login's record links Dispatch's machine-login page instead. With no live session,
		// or with ?switch=1, it redirects to the sign-in provider, with the record's approver as
		// login_hint, and returns here.
		{http.MethodGet, "/approve/{record}", signInAuth((*approval.Page).ServeRecord)},
		// Decide records an approval page printed, as the signed-in person: a form from the
		// broker's own origin only, carrying the page's token, which binds it to the session and to
		// exactly the records the page printed with a button, for an hour. Each record is decided
		// on its own, and the answer is a 303 back to the page, which says what became of each.
		{http.MethodPost, "/approve", signInAuth((*approval.Page).ServeDecide)},
		// Finish a sign-in to the approval page, the provider's redirect back with a code, checked
		// against the sign-in this browser started (its __Host-broker-signin cookie, used once):
		// state, PKCE, the ID token and the Workspace domain. It makes a new session, ending the one
		// the browser held, and answers a 303 to the record's page the sign-in started from.
		{http.MethodGet, "/signin/callback", signInAuth((*approval.Page).ServeCallback)},
		// Sign out of the approval page, from the broker's own origin only: the session ends and
		// its cookie is cleared, and the answer links back to the record.
		{http.MethodPost, "/signin/logout", signInAuth((*approval.Page).ServeLogout)},
		// The approval page's one stylesheet, embedded in the broker; served while sign-in is off
		// too, since the page saying so links it.
		{http.MethodGet, "/approve/assets/approve.css", public((*server).approvalStylesheet)},
		// Report whether the broker can reach its database. From SIGTERM onward it answers 503
		// SHUTTING_DOWN instead (Deps.Draining) while the process goes on serving every other route,
		// which is what takes a stopping pod out of its Service's endpoints before it stops
		// accepting connections. So this is a readiness probe and must never be used as a liveness
		// probe: a liveness probe on it would have the kubelet kill the container in the middle of
		// the drain, dropping the requests the pod was draining.
		{http.MethodGet, "/healthz", public((*server).healthz)},
	}
}
