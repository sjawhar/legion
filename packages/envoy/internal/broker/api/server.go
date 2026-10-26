// Package api is the broker's HTTP surface: routes_table.go lists every route with the
// authentication it needs, and this file authenticates callers and writes responses.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/sjawhar/envoy/internal/broker/approval"
	"github.com/sjawhar/envoy/internal/broker/enroll"
	"github.com/sjawhar/envoy/internal/broker/machine"
	"github.com/sjawhar/envoy/internal/broker/oauth"
	"github.com/sjawhar/envoy/internal/broker/policy"
	"github.com/sjawhar/envoy/internal/broker/proof"
	"github.com/sjawhar/envoy/internal/broker/ratelimit"
	"github.com/sjawhar/envoy/internal/broker/record"
	"github.com/sjawhar/envoy/internal/broker/requests"
	"github.com/sjawhar/envoy/internal/oidc"
)

type Deps struct {
	PublicURL string
	// APIAccounts is BROKER_API_ACCOUNTS: the ServiceAccount subject of each listed caller of the
	// routes accountAuth authenticates, answering the caller's name. A subject it does not name
	// is refused, so an account taken off it stops calling as soon as the broker reads it again.
	APIAccounts map[string]string
	// AccountTokens verifies those callers' projected ServiceAccount tokens, against the same
	// Kubernetes issuer and audience a pod enrolls with (cmd/broker passes the one verifier it
	// builds). Nil is a broker that trusts no Kubernetes issuer, where every such route refuses
	// every caller.
	AccountTokens enroll.PodVerifier
	Enroll        *enroll.Service
	Machine       *requests.Machine
	MachineLogin  *machine.Service
	Proof         *proof.Verifier
	// Approval is how an approval carries the approving person's own sign-in: the broker's own
	// OAuth client at BROKER_APPROVAL_ISSUER, and the single-use challenges a person spends on
	// the broker's own pages (handlers_approval.go). With it, POST
	// /v1/credential-requests/{record}/approve is refused 403 APPROVAL_NEEDS_PERSON, whoever
	// calls it, and an approval goes through those pages instead.
	//
	// Nil is a broker whose BROKER_APPROVAL_* group is unset: the pages are not mounted, POST
	// .../approval is refused 503 APPROVAL_SIGN_IN_OFF, and approve goes on taking the approver
	// on the calling service's word. That is the rollout's first half, and cmd/broker warns at
	// boot for as long as it lasts.
	Approval *ApprovalSignIn
	// Policy is the live agent-secret policy, which POST /v1/secrets/{name}/reread rereads one
	// name of (policy.Current.RefreshOne).
	Policy *policy.Current
	// NotifyReread tells every other pod serving this database to reread name, the fan-out
	// internal/broker/reread's Notifier sends over Postgres LISTEN/NOTIFY; nil means no fan-out,
	// which is a single-pod broker and every test that wires none. POST
	// /v1/secrets/{name}/reread is its one call site (rereadSecret), which explains there both
	// when it is called and why it is called from the route rather than from RefreshOne.
	//
	// It reports nothing, because the handler has nothing to do with a failure: the local reread
	// has already succeeded and its answer stands, so whether a failed fan-out is logged, and at
	// what level, belongs to the function cmd/broker supplies.
	NotifyReread func(ctx context.Context, name string)
	// SecretsPrefix is BROKER_SECRETS_PREFIX, the namespace GET /v1/settings answers.
	SecretsPrefix string
	// SecretsStore is BROKER_SECRETS_STORE, the store GET /v1/settings names: secrets.StoreAWS or
	// secrets.StoreGCP.
	SecretsStore string
	// SecretsKMSKey is the agent-secrets key GET /v1/settings answers: the AWS store's key ARN
	// (BROKER_SECRETS_KMS_KEY_ARN), with its region and account, or the GCP store's Cloud KMS key
	// name (BROKER_SECRETS_KMS_KEY_NAME).
	SecretsKMSKey string
	// LauncherLimits bounds POST /v1/oauth/device_authorization and POST
	// /v1/launcher-credentials/token; nil means DefaultLauncherLimits.
	LauncherLimits *LauncherLimits
	// RereadLimit bounds POST /v1/secrets/{name}/reread per source address; nil means
	// RereadLimitFor of the shared bucket this process mounted - RereadOverallLimit, or
	// DefaultRereadOverallLimit when that is nil too - so that an address's own rate is ordered
	// against the bucket this process actually has rather than against a constant derived from a
	// bucket it may never mount.
	RereadLimit *ratelimit.Limit
	// RereadOverallLimit bounds that route across every caller at once; nil means
	// DefaultRereadOverallLimit.
	RereadOverallLimit *ratelimit.Limit
	// TokenLimit bounds POST /v1/oauth/token per source address; nil means DefaultTokenLimit.
	TokenLimit *ratelimit.Limit
	// AccountLimit bounds every route accountAuth authenticates per source address, in one bucket
	// the whole class shares and spent before the caller's token is verified; nil means
	// DefaultAccountLimit.
	AccountLimit *ratelimit.Limit
	// ApprovalLimit bounds the three approval pages a person's browser reaches per source
	// address, in one bucket they share; nil means DefaultApprovalLimit.
	ApprovalLimit *ratelimit.Limit
	// TrustedProxyHeader names a request header (e.g. "X-Forwarded-For") the rate limiters'
	// per-address buckets trust for the real client address; empty means keying on r.RemoteAddr,
	// correct only when the broker is reached directly rather than through a reverse proxy or load
	// balancer. See limits.go's clientAddress.
	TrustedProxyHeader string
	// OAuth is the machine login's OAuth provider and the token exchange. POST
	// /v1/oauth/device_authorization, POST /v1/oauth/token and POST /v1/oauth/introspect are
	// mounted only when it is set, with DeviceLogin and both TokenHooks, which it then requires
	// (Register panics without them).
	OAuth *oauth.Provider
	// DeviceLogin opens a device login's record from the device authorization request's form and
	// the user code the provider minted, in the provider's transaction (oauthstore.TxFrom on ctx);
	// an error refuses the request (deviceLoginRefusal maps it to the broker's code).
	DeviceLogin func(ctx context.Context, form url.Values, userCode string) error
	// TokenHooks are the broker's steps around each token the provider issues.
	TokenHooks oauth.TokenHooks
	// Draining reports whether this process has begun stopping, which GET /healthz answers 503
	// SHUTTING_DOWN for while every other route goes on being served; nil means it never does.
	//
	// This is how a pod stops being sent traffic before it stops accepting it. Kubernetes removes
	// a terminating pod from its Service's EndpointSlice concurrently with the kubelet beginning
	// termination, not before it, so a pod that simply closed its listener at SIGTERM would be
	// refusing connections that were still being routed to it. Failing the health check is what
	// makes a readiness probe, and anything else reading readiness, stop choosing this pod, which
	// is why the drain has to show up here rather than in the listener. nil is every caller that
	// never stops in the first place - brokertest and the in-process rigs - for which the broker
	// is never draining.
	Draining func() bool
}

type server struct {
	deps            Deps
	launcherLimiter *launcherLimiter
	// rereadLimiter is RereadLimit applied per source address (clientAddress), and rereadOverall
	// RereadOverallLimit over one bucket every caller shares; refuseReread takes from both or
	// neither.
	rereadLimiter *ratelimit.Keyed
	rereadOverall *ratelimit.Bucket
	// tokenLimiter is TokenLimit applied per source address (clientAddress), over its route's own
	// bucket, accountLimiter AccountLimit, over the bucket every accountAuth route shares, and
	// approvalLimiter ApprovalLimit, over the bucket the approval pages share.
	tokenLimiter    *ratelimit.Keyed
	accountLimiter  *ratelimit.Keyed
	approvalLimiter *ratelimit.Keyed
}

func Register(mux *http.ServeMux, deps Deps) {
	if deps.OAuth != nil && (deps.MachineLogin == nil || deps.DeviceLogin == nil || deps.TokenHooks.Before == nil || deps.TokenHooks.After == nil) {
		panic("api: Deps.OAuth needs MachineLogin, DeviceLogin and both TokenHooks")
	}
	limits := DefaultLauncherLimits
	if deps.LauncherLimits != nil {
		limits = *deps.LauncherLimits
	}
	rereadOverall := DefaultRereadOverallLimit
	if deps.RereadOverallLimit != nil {
		rereadOverall = *deps.RereadOverallLimit
	}
	// Derived from the shared bucket resolved just above, and only when the caller named no
	// bucket of its own: a caller that passes one - the rows that drive the per-address bucket
	// on its own - keeps what it passed, exactly as every other optional dep here is defaulted
	// rather than overridden.
	reread := RereadLimitFor(rereadOverall)
	if deps.RereadLimit != nil {
		reread = *deps.RereadLimit
	}
	token := DefaultTokenLimit
	if deps.TokenLimit != nil {
		token = *deps.TokenLimit
	}
	account := DefaultAccountLimit
	if deps.AccountLimit != nil {
		account = *deps.AccountLimit
	}
	approvalLimit := DefaultApprovalLimit
	if deps.ApprovalLimit != nil {
		approvalLimit = *deps.ApprovalLimit
	}
	s := &server{
		deps:            deps,
		launcherLimiter: newLauncherLimiter(limits, deps.TrustedProxyHeader),
		rereadLimiter:   ratelimit.NewKeyed(reread),
		rereadOverall:   ratelimit.NewBucket(rereadOverall),
		tokenLimiter:    ratelimit.NewKeyed(token),
		accountLimiter:  ratelimit.NewKeyed(account),
		approvalLimiter: ratelimit.NewKeyed(approvalLimit),
	}
	for _, route := range routes() {
		if deps.OAuth == nil && strings.HasPrefix(route.Pattern, oauthRoutes) {
			continue
		}
		// A broker with no approval issuer has no page to serve: the account route stays, and
		// says so (APPROVAL_SIGN_IN_OFF), so a caller learns what is unset rather than meeting a
		// bare 404 from the mux.
		if deps.Approval == nil && strings.HasPrefix(route.Pattern, approvalRoutes) {
			continue
		}
		mux.HandleFunc(route.Method+" "+route.Pattern, func(w http.ResponseWriter, r *http.Request) {
			who, ok := s.authenticate(w, r, route.Handler.auth)
			if !ok {
				return
			}
			route.Handler.serve(s, w, r, who)
		})
	}
}

// oauthRoutes prefixes the routes the OAuth provider serves, which Register mounts only with one.
const oauthRoutes = "/v1/oauth/"

// ApprovalSignIn is what the broker needs to take an approval with the approving person's own
// sign-in: the challenges a person spends, and the broker's own confidential OAuth client at the
// issuer that signs them in. cmd/broker builds one from the BROKER_APPROVAL_* group.
//
// The client is the whole point of holding it here. It is the broker's, not the relaying
// service's: the ID token it gets back is issued for this client alone, by the issuer, to the
// browser the person is signed in with, so no service that relays Dispatch's pages — and no
// holder of that service's own credentials, its Google client secret or the refresh tokens it
// keeps — can mint one.
type ApprovalSignIn struct {
	// Challenges stores the single-use approvals: one row per record a person was sent to the
	// broker's own page for.
	Challenges *approval.Service
	// Flow is that OAuth client, for the issuer, the client id and the Workspace domain
	// BROKER_APPROVAL_ISSUER, BROKER_APPROVAL_CLIENT_ID and BROKER_APPROVAL_DOMAIN name
	// (oidc.NewConfirmFlow). The deployment registers BROKER_PUBLIC_URL + /approve/callback as
	// its one redirect URI.
	Flow *oidc.CodeFlow
	// Domain is BROKER_APPROVAL_DOMAIN, the Workspace domain Flow holds every sign-in to, which
	// the page names so a person knows which account to pick.
	Domain string
}

// authenticate proves who r comes from, as auth requires, or writes the refusal. A bad or missing
// credential is a 401; a store that cannot answer is a 503 naming it, never mistaken for a bad
// credential. A route a ServiceAccount token authenticates first spends its source address's
// bucket (DefaultAccountLimit), a refusal being 429 RATE_LIMITED with Retry-After, so a flood of
// forged tokens past that bucket costs no verification and no fetch of the issuer's keys.
func (s *server) authenticate(w http.ResponseWriter, r *http.Request, auth routeAuth) (caller, bool) {
	switch auth {
	case authNone:
		return caller{}, true
	case authAccount:
		if s.refuseAccountAddress(w, r) {
			return caller{}, false
		}
		return s.accountCaller(w, r)
	case authLauncher:
		subject, err := s.proofSubject(r)
		switch {
		case errors.Is(err, proof.ErrInvalid), errors.Is(err, proof.ErrNotLive):
			writeError(w, http.StatusUnauthorized, "LAUNCHER_INVALID", "the launcher credential is not valid")
			return caller{}, false
		case err != nil:
			writeUnavailable(w, "DATABASE_UNAVAILABLE", "verify launcher proof", err)
			return caller{}, false
		}
		if subject.LauncherID == "" {
			writeError(w, http.StatusUnauthorized, "LAUNCHER_INVALID", "this route needs a launcher proof, not a session proof")
			return caller{}, false
		}
		cred, err := s.deps.Enroll.Credential(r.Context(), subject.LauncherID)
		if err != nil {
			writeUnavailable(w, "DATABASE_UNAVAILABLE", "read launcher credential", err)
			return caller{}, false
		}
		return caller{launcher: cred}, true
	case authProof:
		return s.sessionCaller(w, r, "PROOF_INVALID", "proof invalid: not live")
	case authRenewProof:
		return s.sessionCaller(w, r, "ENROLLMENT_NOT_LIVE", "enrollment is not live")
	}
	writeInternal(w, "authenticate", fmt.Errorf("route has unknown authentication %d", auth))
	return caller{}, false
}

// sessionCaller authenticates a session proof for authenticate, answering a proof whose
// enrollment is no longer live with notLiveCode and notLiveMessage: PROOF_INVALID on every
// session route but renew, which alone answers ENROLLMENT_NOT_LIVE so a pod renewer can stop.
// Every other bad proof is PROOF_INVALID on both.
func (s *server) sessionCaller(w http.ResponseWriter, r *http.Request, notLiveCode, notLiveMessage string) (caller, bool) {
	subject, err := s.proofSubject(r)
	switch {
	case errors.Is(err, proof.ErrNotLive):
		writeError(w, http.StatusUnauthorized, notLiveCode, notLiveMessage)
		return caller{}, false
	case errors.Is(err, proof.ErrInvalid):
		writeError(w, http.StatusUnauthorized, "PROOF_INVALID", err.Error())
		return caller{}, false
	case err != nil:
		writeUnavailable(w, "DATABASE_UNAVAILABLE", "verify proof", err)
		return caller{}, false
	}
	if subject.EnrollmentID == "" {
		writeError(w, http.StatusUnauthorized, "PROOF_INVALID", "this route needs a session proof, not a launcher proof")
		return caller{}, false
	}
	return caller{enrollment: subject.EnrollmentID}, true
}

// accountCaller authenticates a listed caller's own projected ServiceAccount token for
// authenticate, the credential accountAuth's routes take. It is the check a service's token login
// makes (machine.Service.VerifyServiceToken), against the same verifier and with the same
// taxonomy: a broker that trusts no Kubernetes issuer, a token that has expired, one that does
// not verify, one that lives longer than machine.MaxTokenLoginLifetime from its issue (so a
// copied token is spendable for at most an hour), and an account BROKER_API_ACCOUNTS does not
// name. Only the last is a 403: the credential is sound and it is the broker's list that refuses.
// No refusal repeats the token.
func (s *server) accountCaller(w http.ResponseWriter, r *http.Request) (caller, bool) {
	if s.deps.AccountTokens == nil {
		writeError(w, http.StatusUnauthorized, "ACCOUNT_TOKENS_OFF", "this broker trusts no Kubernetes token issuer, so no caller authenticates with a ServiceAccount token")
		return caller{}, false
	}
	claims, err := s.deps.AccountTokens.VerifyAccount(r.Context(), bearer(r))
	switch {
	case errors.Is(err, oidc.ErrExpired):
		writeError(w, http.StatusUnauthorized, "ACCOUNT_TOKEN_EXPIRED", "the ServiceAccount token has expired")
		return caller{}, false
	case err != nil:
		writeError(w, http.StatusUnauthorized, "ACCOUNT_TOKEN_INVALID", "the ServiceAccount token does not verify")
		return caller{}, false
	case claims.IssuedAt.IsZero():
		writeError(w, http.StatusUnauthorized, "ACCOUNT_TOKEN_TOO_LONG_LIVED", "the ServiceAccount token names no issue time (iat)")
		return caller{}, false
	case claims.Expiry.Sub(claims.IssuedAt) > machine.MaxTokenLoginLifetime+s.deps.Proof.Skew:
		writeError(w, http.StatusUnauthorized, "ACCOUNT_TOKEN_TOO_LONG_LIVED", "the ServiceAccount token lives longer than an hour from its issue")
		return caller{}, false
	}
	name, listed := s.deps.APIAccounts[claims.Subject]
	if !listed {
		writeError(w, http.StatusForbidden, "ACCOUNT_NOT_LISTED", "this ServiceAccount is not in BROKER_API_ACCOUNTS")
		return caller{}, false
	}
	return caller{account: name}, true
}

// proofSubject verifies r's Proof header and returns who it authenticates, unwrapped: authLauncher
// and authProof each translate a failure into their own vocabulary (LAUNCHER_INVALID vs
// PROOF_INVALID), so this reports
// only the raw error.
func (s *server) proofSubject(r *http.Request) (proof.Subject, error) {
	return s.deps.Proof.Verify(r.Context(), r.Header.Get("Proof"), r.Method, s.deps.PublicURL+r.URL.Path, time.Now())
}

func bearer(r *http.Request) string {
	return strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
}

// maxJSONBodyBytes caps every JSON request body the broker reads, as Dispatch's own decodeJSON does.
const maxJSONBodyBytes = 1 << 20

// readJSON decodes r's body as exactly one JSON value into v: at most maxJSONBodyBytes (413
// REQUEST_TOO_LARGE), no field v does not declare and nothing after the value (400 with
// invalidCode). It writes the refusal itself and reports whether decoding succeeded.
func readJSON(w http.ResponseWriter, r *http.Request, v any, invalidCode string) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxJSONBodyBytes)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	err := decoder.Decode(v)
	if err == nil && decoder.More() {
		err = errors.New("body must contain one JSON value")
	}
	var tooLarge *http.MaxBytesError
	switch {
	case err == nil:
		return true
	case errors.As(err, &tooLarge):
		writeError(w, http.StatusRequestEntityTooLarge, "REQUEST_TOO_LARGE", fmt.Sprintf("request body exceeds %d bytes", maxJSONBodyBytes))
	default:
		writeError(w, http.StatusBadRequest, invalidCode, "body must be one valid JSON object: "+err.Error())
	}
	return false
}

// pathUUID reads the {name} path segment as a UUID, or refuses it with 400 code: an id that is not
// a UUID names nothing, and must never reach a uuid column as a Postgres type error.
func pathUUID(w http.ResponseWriter, r *http.Request, name, code, what string) (string, bool) {
	id := r.PathValue(name)
	if _, err := uuid.Parse(id); err != nil {
		writeError(w, http.StatusBadRequest, code, what+" ids are UUIDs")
		return "", false
	}
	return id, true
}

// recordIDPattern matches a credential-request record id: content-addressed lowercase-hex
// SHA-256, never a UUID (record.Body.ID).
var recordIDPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// pathRecordID reads the {name} path segment as a credential-request record id, or refuses it
// with 400 code: record ids are lowercase-hex sha256 hashes, so pathUUID's check would wrongly
// reject every valid one.
func pathRecordID(w http.ResponseWriter, r *http.Request, name, code string) (string, bool) {
	id := r.PathValue(name)
	if !recordIDPattern.MatchString(id) {
		writeError(w, http.StatusBadRequest, code, "record ids are lowercase-hex sha256 hashes")
		return "", false
	}
	return id, true
}

// requireApprover refuses an approver login that canonicalizes to nothing with 400
// APPROVER_REQUIRED: every UI route names the human it acts for, whether a decision's or a
// revoke's approver field or a list's ?approver= query.
func requireApprover(w http.ResponseWriter, login string) bool {
	if record.CanonicalLogin(login) == "" {
		writeError(w, http.StatusBadRequest, "APPROVER_REQUIRED", "approver is required")
		return false
	}
	return true
}

func writeError(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{"code": code, "error": msg})
}

// writeInternal logs a failed operation with its error and answers 500 INTERNAL naming the
// operation. Errors on these paths carry no secret values; the log line is the only place the
// cause of a 500 is recorded.
func writeInternal(w http.ResponseWriter, op string, err error) {
	slog.Error("broker: "+op+" failed", "error", err)
	writeError(w, http.StatusInternalServerError, "INTERNAL", op+" failed")
}

// writeUnavailable logs a dependency that could not answer and answers 503 with code naming it.
func writeUnavailable(w http.ResponseWriter, code, op string, err error) {
	slog.Error("broker: "+op+" failed", "error", err)
	writeError(w, http.StatusServiceUnavailable, code, op+" failed: a dependency the broker needs is unavailable")
}

// writeJSON answers status with v, which is one of this package's named response types: the
// broker's generated HTTP reference (cmd/broker-refgen) documents each route's answer
// from that type's fields and refuses a value it cannot name.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

// stateResponse is what a denial, a cancel or a revoke answers.
type stateResponse struct {
	// The state the call left the request, record or grant in: "denied", "cancelled" or "revoked".
	State string `json:"state"`
}

// healthResponse is GET /healthz's answer while the broker is not draining and can reach
// Postgres. Neither refusal has a type of its own: both are the ordinary broker error object
// writeError writes, so this stays the route's one named response type.
type healthResponse struct {
	// "ok".
	Status string `json:"status"`
}

func (s *server) healthz(w http.ResponseWriter, r *http.Request) {
	// A draining pod fails its health check before anything else, the Postgres ping included: the
	// answer has to flip the moment the process begins stopping, whatever the database is doing,
	// because this is what takes the pod out of its Service's endpoints while it goes on serving
	// the requests already routed to it. The refusal is an ordinary broker error object rather
	// than a healthResponse with some other status, so a caller reads the same {code, error}
	// shape every other refusal has.
	if s.deps.Draining != nil && s.deps.Draining() {
		writeError(w, http.StatusServiceUnavailable, "SHUTTING_DOWN", "the broker is shutting down and is no longer taking new traffic")
		return
	}
	if err := s.deps.Enroll.Store.Pool.Ping(r.Context()); err != nil {
		writeUnavailable(w, "DATABASE_UNAVAILABLE", "ping Postgres", err)
		return
	}
	writeJSON(w, http.StatusOK, healthResponse{Status: "ok"})
}

// strPtr is nil for "" and &s otherwise, for an optional wire field that is null rather than "".
func strPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
