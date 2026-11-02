package api

import (
	"maps"
	"net/http"
	"slices"

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
	authService
	authSignIn
)

// caller is who server.authenticate proved a request came from. Which field is set follows from
// the route's authentication, and only the adapter for that authentication reads it.
type caller struct {
	launcher   enroll.Credential
	enrollment string
	// account is the caller BROKER_API_ACCOUNTS gives the service whose ServiceAccount token
	// authenticated the request: its name, which a handler that acts logs, and the powers that
	// name carries, which authenticate checks against the route's before the handler runs.
	account Account
	// service is the service BROKER_SERVICE_LOGINS says the ServiceAccount token that
	// authenticated the request logs in as, which a serviceAuth handler logs when it acts.
	service string
	// signIn is the broker sign-in session a request to the approval page carries, the zero
	// approval.Session when it carries none that is live.
	signIn approval.Session
}

// power is one authority over the broker a caller of an accountAuth route may hold: one bit, so
// a set of them is one value and an Account stays comparable. Every such route declares the one
// it needs and every caller name answers the ones it has, so a caller listed for one power never
// reaches a route that needs another.
type power uint

const (
	// noPower is the empty set, which no caller holds and no route may declare: a row that names
	// no power reaches nothing (may refuses it), and TestEveryAccountRouteDeclaresItsPower fails
	// on one.
	noPower power = 0
	// readsTokensBack is reading one of the broker's own access tokens back (RFC 7662): whether
	// it is live and what it carries. A resource server needs this and nothing else.
	readsTokensBack power = 1 << iota
	// relaysDecisions is reading what a person has to decide and relaying the decisions
	// themselves — approvals, denials and both revocations by approver — and ending one of the
	// broker's own access tokens (RFC 7009), which is that same authority over a credential
	// someone holds. It is an approval credential over every person's secrets, so Dispatch's
	// server alone holds it.
	relaysDecisions
)

// Account is a caller of the accountAuth routes: the name BROKER_API_ACCOUNTS gives its
// ServiceAccount, and the powers that name carries. Only AccountFor builds one with powers, so a
// wiring site cannot invent a caller's authority, and the zero Account reaches no such route.
type Account struct {
	// Name is what the caller calls as, the identity a handler logs when it acts.
	Name   string
	powers power
}

// may reports whether this caller holds p. noPower is held by nobody, so a route that declares
// none is refused every caller rather than open to all of them.
func (a Account) may(p power) bool { return p != noPower && a.powers&p == p }

// accountPowers is the broker's whole table of caller names: every name BROKER_API_ACCOUNTS may
// give a ServiceAccount, and what that name may do. dispatch is Dispatch's server, which relays a
// person's decisions and reads tokens back; listener is the Envoy listener, which reads a token
// back and relays no decision. A new caller is a deliberate row here, since a name is an
// authority and not a label: a name this table does not know refuses the broker's boot.
var accountPowers = map[string]power{
	"dispatch": readsTokensBack | relaysDecisions,
	"listener": readsTokensBack,
}

// AccountFor answers the account a caller name carries, and whether the broker knows the name at
// all. config.Load calls it for each BROKER_API_ACCOUNTS entry, so an unknown name is refused at
// boot rather than silently becoming a caller that reaches nothing.
func AccountFor(name string) (Account, bool) {
	powers, known := accountPowers[name]
	if !known {
		return Account{}, false
	}
	return Account{Name: name, powers: powers}, true
}

// KnownCallers is every caller name the broker knows, sorted, so a refusal can name them all.
func KnownCallers() []string {
	return slices.Sorted(maps.Keys(accountPowers))
}

// routeHandler is a handler together with the authentication its signature needs, and, for a
// route a listed caller's ServiceAccount token authenticates, the power its caller must hold.
// Only the adapters below build one, and each fixes both halves: a handler taking a launcher
// credential can only ever run behind launcher authentication, so no route row can pair a
// handler with the wrong kind of caller.
type routeHandler struct {
	auth routeAuth
	// power is what a caller must hold to reach this route, noPower on every route no
	// ServiceAccount token authenticates.
	power power
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
//
// Each such route declares the power its caller must hold, and the caller's name answers the
// powers it has (accountPowers), which authenticate checks before the handler runs: being listed
// reaches the routes of that caller's own powers and no others.
func accountAuth(needs power, h func(*server, http.ResponseWriter, *http.Request, string)) routeHandler {
	return routeHandler{auth: authAccount, power: needs, serve: func(s *server, w http.ResponseWriter, r *http.Request, who caller) {
		h(s, w, r, who.account.Name)
	}}
}

// accountAuthAfterRecordKind is a route whose handler authenticates its own caller, because what
// it may answer is settled before any credential is read: the two decide routes read the record's
// kind first and refuse a secret request 403 DECIDE_ON_BROKER_PAGE naming the broker's own page,
// to every caller, token or none. Register checks nothing for such a row; the handler spends the
// account class's per-address bucket and then authenticates a machine login's decision exactly as
// accountAuth does (decideRecord), so one is still decided only on a listed service's
// ServiceAccount token.
//
// The row declares the power that caller must hold all the same, and the handler is given it:
// authenticate cannot check it here, since it is the handler that reads the caller, so the check
// is decideRecord's, against the same accountPowers and with the same 403 ACCOUNT_LACKS_POWER as
// every other row of that authority.
func accountAuthAfterRecordKind(needs power, h func(*server, http.ResponseWriter, *http.Request, power)) routeHandler {
	return routeHandler{auth: authNone, power: needs, serve: func(s *server, w http.ResponseWriter, r *http.Request, _ caller) {
		h(s, w, r, needs)
	}}
}

// serviceAuth is a route for a service calling the broker with its own Kubernetes ServiceAccount
// token, the account BROKER_SERVICE_LOGINS lets log in as that service — a deployment's Legion
// daemon, which registers the pods it starts. The token is verified and mapped to its service by
// the Kubernetes half of the check a token login makes (machine.Service.VerifyKubernetesServiceToken):
// a Google VM's identity token logs a VM in as its fleet's service, but a VM is no daemon and
// registers no pod, so it authenticates no route of this class. The handler is given the service's
// name.
//
// It is a class of its own rather than a power of accountAuth's because the two lists are two
// authorities: BROKER_API_ACCOUNTS names the callers that relay a person's decisions and read
// tokens back, and a daemon that registers its own pods is not one of them. A deployment already
// lists its per-project daemons in BROKER_SERVICE_LOGINS for their token logins, so these routes
// need no new setting and no new account.
func serviceAuth(h func(*server, http.ResponseWriter, *http.Request, string)) routeHandler {
	return routeHandler{auth: authService, serve: func(s *server, w http.ResponseWriter, r *http.Request, who caller) {
		h(s, w, r, who.service)
	}}
}

// signInAuth is a route of the broker's own approval page, for a person in a browser signed in to
// the broker itself (OpenID Connect with the broker's own client, BROKER_SIGNIN_*). The handler is
// given the session the request's __Host-broker-session cookie names, or the zero session when it
// names none that is live, since what that means is the route's to answer: the page starts a
// sign-in, a decision answers a page linking the record it names. Every route of the class spends
// its source address's bucket first, the one the account class spends (authenticate), so no
// caller drives a record read, a token request to the sign-in provider or a session write
// unmetered. Mounted only when the broker builds its approval page, which every broker does.
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
		// Log a service in with its own token, a Kubernetes ServiceAccount's or a Google VM's
		// identity token, which no person approves: answers its launcher credential's id and expiry.
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
		{http.MethodPost, "/v1/oauth/introspect", accountAuth(readsTokensBack, (*server).introspect)},
		// End one of the broker's own access tokens (RFC 7009), an exchanged pod's or workflow
		// run's included, which nothing else can end before its own expiry: the form names the
		// token, the client it was issued to, and optionally which kind to look for first. It
		// needs relaysDecisions rather than readsTokensBack because ending one of the broker's
		// tokens is the same authority class as ending a machine login or a grant, both already
		// behind that power; a caller listed only to read a token back must not be able to end
		// one. Mounted only when the broker runs its OAuth provider.
		{http.MethodPost, "/v1/oauth/revoke", accountAuth(relaysDecisions, (*server).revokeOAuthToken)},
		// Register that one pod may exchange under one session name: the row the token exchange
		// reads before it mints a token for a pod of an account that fixes no session, written by
		// the deployment's own daemon as it starts that pod. Mounted only when the broker runs its
		// OAuth provider.
		{http.MethodPost, "/v1/oauth/exchange/pod-sessions", serviceAuth((*server).registerPodSession)},
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
		{http.MethodGet, "/v1/pending", accountAuth(relaysDecisions, (*server).listPending)},
		// Read a credential-request record: what was asked, by whom, why, and its decision. A
		// secret request's record also carries the broker's own page where it is decided, as the
		// pending list's row does.
		{http.MethodGet, "/v1/credential-requests/{record}", accountAuth(relaysDecisions, (*server).readRecord)},
		// Approve a pending machine login as the named person, who must type its code: the broker
		// takes the person's name on the caller's word, from a listed service's ServiceAccount
		// token, which the handler verifies itself — and whose name must carry relaysDecisions,
		// the power this row declares — after it has read the record's kind. It reads the kind
		// first because a secret request is refused before any caller is looked at: it is decided
		// only on the broker's own page by a person signed in to the broker, so this answers 403
		// DECIDE_ON_BROKER_PAGE naming that page to every caller, token or none, writing nothing.
		{http.MethodPost, "/v1/credential-requests/{record}/approve", accountAuthAfterRecordKind(relaysDecisions, (*server).approveRecord)},
		// Deny a pending machine login as the named person, who must type its code, on the
		// caller's word and behind the same authentication and the same power as approve. A
		// secret request is refused the same way, writing nothing, with 403
		// DECIDE_ON_BROKER_PAGE naming the page where a person signed in to the broker denies it.
		{http.MethodPost, "/v1/credential-requests/{record}/deny", accountAuthAfterRecordKind(relaysDecisions, (*server).denyRecord)},
		// Find a pending machine login by the confirmation code its machine shows.
		{http.MethodPost, "/v1/machine-logins/lookup", accountAuth(relaysDecisions, (*server).lookupMachineLogin)},
		// List the machine logins the named person may revoke, every service's, such as the Legion
		// daemon's, whoever approved it, and the person's own machines', that are not revoked and
		// are unexpired or expired with a session still running, each naming who approved it. The
		// person is the one the caller names.
		{http.MethodGet, "/v1/launcher-credentials", accountAuth(relaysDecisions, (*server).listLauncherCredentials)},
		// End a machine login, expired or not: a service's (its token login, or one a person approved
		// before token logins) as anyone signed in, a person's machine's as the person who approved
		// it. Its launcher proofs stop authenticating and every session it enrolled, pods included,
		// ends with its grants and pending requests. The revoking person is the login the request
		// names, which the broker still takes on the caller's word: a revoke only takes power away.
		{http.MethodPost, "/v1/launcher-credentials/{id}/revoke-by-approver", accountAuth(relaysDecisions, (*server).revokeLauncherCredential)},
		// List the live grants of the named person's sessions, automatic or approved, and those the
		// person approved. The person is the one the caller names.
		{http.MethodGet, "/v1/grants", accountAuth(relaysDecisions, (*server).listGrantsForApprover)},
		// End a grant as its approver or as its enrollment's operator. The operator's revoke also
		// withholds the secrets the grant's request got automatically from that session, even when
		// the grant had already ended: its other grants that got them automatically end too, and it
		// asks before it gets them again. The revoking person is the login the request names, which
		// the broker still takes on the caller's word: a revoke only takes power away.
		{http.MethodPost, "/v1/grants/{id}/revoke-by-approver", accountAuth(relaysDecisions, (*server).revokeByApprover)},
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
		// session the request claims when it differs, the reason or "No reason given", the
		// lifetime, the times, the approver and the state, one sentence saying what Approve gives
		// and for how long and one saying what Deny and the expiry do, then every other pending
		// secret request the person may decide, with Approve and Deny for each the person may
		// decide and one "Approve all N shown". It checks the id's shape, then the session, then
		// the record: a malformed id is a 400, a request with no live session, or with ?switch=1,
		// redirects to the sign-in provider and returns here, and only a signed-in person's
		// unknown record id is a 404. A machine login's record is printed with the note that it
		// is decided by typing its code on Dispatch's machine-login page, and offers no button.
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
		// Report the broker's sign-in: whether its sign-in provider accepted the broker's own
		// client at the last hourly check and when, when a person last signed in, when a
		// sign-in last failed and the coded reason it failed with, and when a decision was last
		// made on the approval page. It needs no credential because a production check fetches
		// it from a pod where no person is signed in, and because it names no login, no secret
		// and no provider's message: every word it answers is one of two closed sets this
		// broker chose (internal/broker/store's signin_status). It spends its source address's
		// bucket in its own handler, as the page's routes spend it in authenticate, so this read
		// of the broker's own row is metered too.
		{http.MethodGet, "/signin/status", public((*server).signInStatus)},
		// The approval page's one stylesheet, embedded in the broker; it needs no credential and
		// spends no rate-limit bucket, since every page it dresses is read before anyone has
		// signed in and the bytes are the binary's.
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
