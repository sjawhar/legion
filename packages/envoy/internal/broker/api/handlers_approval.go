// handlers_approval.go: POST /v1/credential-requests/{record}/approval, and the three pages a
// person's own browser reaches — GET /approve/{id}, POST /approve/{id}/start and
// GET /approve/callback. Together they are how an approval carries the approving person's own
// sign-in rather than the word of the service that relays Dispatch's pages.
//
// A listed caller opens an approval for a record and the login it believes is deciding, which
// proves nothing: the answer is a link, and the record is untouched. The person opens that link
// on the broker itself, reads what is being approved from the broker's own record, and signs in at
// the issuer BROKER_APPROVAL_ISSUER names, for an OAuth client only the broker holds the secret
// of, so the proof comes from the issuer to the broker and no service in between can mint it. The
// callback applies the decision only for the person the challenge names, once.
//
// What this does not cover, deliberately: a denial and a revoke still travel on the calling
// service's word, since either can only take access away.
package api

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/broker/approval"
	"github.com/sjawhar/envoy/internal/broker/machine"
	"github.com/sjawhar/envoy/internal/broker/record"
	"github.com/sjawhar/envoy/internal/broker/requests"
	"github.com/sjawhar/envoy/internal/oidc"
)

const (
	// approvalRoutes prefixes the pages a person's browser reaches, which Register mounts only on
	// a broker with an approval issuer (Deps.Approval).
	approvalRoutes = "/approve/"
	// ApprovalCallbackPath is where the issuer sends the browser back. A deployment registers
	// BROKER_PUBLIC_URL + this as the OAuth client's one redirect URI, which cmd/broker logs at
	// boot.
	ApprovalCallbackPath = "/approve/callback"
	// approvalCookie is the cookie the start page sets: the browser's half of the binding the
	// sign-in's nonce is derived from (approval.Nonce).
	approvalCookie = "approval_binding"
)

// ApprovalMaxAge is how recently the person must have authenticated to the issuer for their
// sign-in to approve anything: the max_age the start page asks for, and the bound the callback
// holds the ID token's own auth_time to. Five minutes means a browser that merely holds a live
// session at the issuer does not approve on its own — the issuer asks the person again — while
// nobody has to sign in twice inside one approval.
const ApprovalMaxAge = 5 * time.Minute

// approvalBody is POST /v1/credential-requests/{record}/approval's request body:
// {"approver", "code"?}, the same two fields a decision carries. The approver is who the broker
// will require the sign-in to be, so a caller naming anyone else opens an approval that person
// cannot spend; code is a machine login's typed confirmation code, which the broker checks against
// the record now and keeps only as a hash.
type approvalBody struct {
	// The login of the person who is to approve, whose own sign-in the broker then requires.
	Approver string `json:"approver"`
	// A machine login only: its confirmation code, as the person typed it; ignored for a secret
	// request.
	Code *string `json:"code"`
}

// approvalResponse is what opening an approval answers: where to send the person, and when the
// link stops working.
type approvalResponse struct {
	// The broker's own approval page for this record, on BROKER_PUBLIC_URL. The person opens it
	// themselves; it is not a route a service calls.
	URL string `json:"url"`
	// When the approval expires, after which the page says so and Dispatch opens another.
	ExpiresAt time.Time `json:"expires_at"`
}

// openApproval opens a single-use approval for the record and answers the page's URL. It refuses
// everything the approval itself would refuse — an unknown or decided record, a login the record's
// rule or the current policy does not admit, a machine login's wrong or missing code, a service's
// login, which no person approves — so a refusal reaches the person on the page they clicked in
// rather than at the end of a sign-in. It decides nothing: the record changes only when the person
// has signed in (approvalCallback).
func (s *server) openApproval(w http.ResponseWriter, r *http.Request, callerName string) {
	recordID, ok := pathRecordID(w, r, "record", "RECORD_ID_INPUT")
	if !ok {
		return
	}
	var body approvalBody
	if !readJSON(w, r, &body, "INVALID_DECISION") {
		return
	}
	if !requireApprover(w, body.Approver) {
		return
	}
	if s.deps.Approval == nil {
		writeError(w, http.StatusServiceUnavailable, "APPROVAL_SIGN_IN_OFF",
			"this broker takes no person's sign-in: BROKER_APPROVAL_ISSUER and its group are unset")
		return
	}
	detail, err := s.deps.Machine.ReadRecord(r.Context(), recordID)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		writeError(w, http.StatusNotFound, "NOT_FOUND", "no such credential request")
		return
	case err != nil:
		writeInternal(w, "read credential request", err)
		return
	}
	login := record.CanonicalLogin(body.Approver)
	if !record.MayDecide(detail.Kind, detail.Approver, detail.Service, login) {
		writeError(w, http.StatusForbidden, "NOT_APPROVER", "this credential request names another approver")
		return
	}
	if detail.State != "pending" {
		writeError(w, http.StatusConflict, "RECORD_TERMINAL", "this credential request is already decided")
		return
	}
	var codeHash []byte
	if detail.Kind == record.KindLauncherCredential {
		if detail.Service != "" {
			writeError(w, http.StatusBadRequest, "SERVICE_LOGS_IN_WITH_TOKEN",
				"a service logs in with its own token, so no person approves its login")
			return
		}
		if body.Code == nil || *body.Code == "" {
			writeError(w, http.StatusBadRequest, "CODE_REQUIRED", "code is required to decide a machine login")
			return
		}
		codeHash = machine.CodeHash(*body.Code)
		switch _, err := s.deps.MachineLogin.CodeFor(r.Context(), recordID, codeHash); {
		case errors.Is(err, machine.ErrCodeMismatch):
			writeError(w, http.StatusForbidden, "CODE_MISMATCH", machine.ErrCodeMismatch.Error())
			return
		case errors.Is(err, machine.ErrNotFound):
			writeError(w, http.StatusNotFound, "NOT_FOUND", "no such credential request")
			return
		case err != nil:
			writeInternal(w, "read machine login code", err)
			return
		}
	} else if err := s.deps.Machine.ApprovalAdmitted(r.Context(), recordID, login); err != nil {
		switch {
		case errors.Is(err, record.ErrNotApprover):
			writeError(w, http.StatusForbidden, "NOT_APPROVER", err.Error())
		case errors.Is(err, pgx.ErrNoRows):
			writeError(w, http.StatusNotFound, "NOT_FOUND", "no such credential request")
		case errors.Is(err, requests.ErrGrantChainInvalid):
			writeError(w, http.StatusForbidden, "GRANT_CHAIN_INVALID", err.Error())
		default:
			writeInternal(w, "check the approval against the current policy", err)
		}
		return
	}
	challenge, err := s.deps.Approval.Challenges.Open(r.Context(), recordID, detail.Kind, login, codeHash)
	if err != nil {
		writeInternal(w, "open an approval", err)
		return
	}
	// The challenge id is a capability: whoever holds it can start the sign-in, though only the
	// named person can finish one. So the log names the record and the person and never the id.
	slog.Info("approval opened", "caller", callerName, "approver", login, "record", recordID, "kind", detail.Kind)
	writeJSON(w, http.StatusOK, approvalResponse{
		URL:       s.deps.PublicURL + approvalRoutes + challenge.ID,
		ExpiresAt: challenge.ExpiresAt,
	})
}

// approvalPage shows what an approval covers, from the broker's own record, and the one button
// that starts the sign-in. It is a page rather than JSON because its reader is a person's browser.
// An id no challenge has is 404, one already spent or expired 410, and a record decided since the
// approval was opened 409 — each its own page saying what happened.
func (s *server) approvalPage(w http.ResponseWriter, r *http.Request) {
	if s.refuseApprovalAddress(w, r) {
		return
	}
	challenge, ok := s.challengeOf(w, r, r.PathValue("id"))
	if !ok {
		return
	}
	detail, err := s.deps.Machine.ReadRecord(r.Context(), challenge.RecordID)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		approval.Write(w, http.StatusNotFound, approval.Page{
			Title:   "Nothing to approve",
			Message: "The credential request this approval was for is gone.",
		})
		return
	case err != nil:
		s.writeApprovalInternal(w, "read credential request", err)
		return
	}
	if detail.State != "pending" {
		approval.Write(w, http.StatusConflict, approval.Page{
			Title:   "Already decided",
			Message: "This credential request has already been decided, so there is nothing to approve.",
		})
		return
	}
	approval.Write(w, http.StatusOK, approval.Page{
		Title:   "Approve a credential request",
		Message: approvalPrompt(s.deps.Approval.Domain),
		Facts:   approvalFacts(detail, challenge),
		Form:    approvalRoutes + challenge.ID + "/start",
		Button:  "Sign in to approve",
	})
}

// approvalPrompt is the page's one paragraph: what the button does, and which account it has to
// be done with.
func approvalPrompt(domain string) string {
	account := "your own account"
	if domain != "" {
		account = "your own " + domain + " account"
	}
	return "Signing in approves this request as the person named below. The broker checks the sign-in with " +
		"the identity provider itself, so it only counts from " + account + "."
}

// approvalFacts is what is being approved, as the broker's own record states it, and nothing more:
// this page can show no detail the record does not hold.
func approvalFacts(detail requests.RecordDetail, challenge approval.Challenge) []approval.Fact {
	facts := []approval.Fact{{Label: "Approver", Value: challenge.Approver}}
	if detail.Kind == record.KindLauncherCredential {
		facts = append(facts, approval.Fact{Label: "Machine login", Value: strings.Join(detail.Identifiers, ", ")})
	} else {
		facts = append(facts, approval.Fact{Label: "Secrets", Value: strings.Join(detail.Identifiers, ", ")})
	}
	if detail.Session.Request != "" || detail.Session.Enrollment != "" {
		session := detail.Session.Request
		if session == "" {
			session = detail.Session.Enrollment
		}
		facts = append(facts, approval.Fact{Label: "Requesting session", Value: session})
	}
	if detail.Enrollment != nil {
		facts = append(facts, approval.Fact{
			Label: "Requested by",
			Value: fmt.Sprintf("%s %s", detail.Enrollment.Kind, detail.Enrollment.RuntimeID),
		})
		if detail.Enrollment.Operator != "" {
			facts = append(facts, approval.Fact{Label: "Session's operator", Value: detail.Enrollment.Operator})
		}
	}
	if detail.Reason != "" {
		facts = append(facts, approval.Fact{Label: "Stated reason", Value: detail.Reason})
	}
	if detail.LifetimeSeconds != nil {
		facts = append(facts, approval.Fact{
			Label: "Grant lasts",
			Value: (time.Duration(*detail.LifetimeSeconds) * time.Second).String(),
		})
	}
	facts = append(facts,
		approval.Fact{Label: "Requested at", Value: detail.RequestedAt.UTC().Format(time.RFC3339)},
		approval.Fact{Label: "Approval expires", Value: challenge.ExpiresAt.UTC().Format(time.RFC3339)},
	)
	return facts
}

// startApprovalSignIn sets the browser's own binding cookie and sends it to the issuer. The cookie
// never leaves the browser and the issuer never sees it: the sign-in's nonce is the hash of it and
// the challenge id (approval.Nonce), so the ID token that comes back proves the browser that
// finishes the sign-in is the one that started it, and the broker keeps nothing per sign-in to
// check that with.
func (s *server) startApprovalSignIn(w http.ResponseWriter, r *http.Request) {
	if s.refuseApprovalAddress(w, r) {
		return
	}
	challenge, ok := s.challengeOf(w, r, r.PathValue("id"))
	if !ok {
		return
	}
	binding, err := approval.NewBinding()
	if err != nil {
		s.writeApprovalInternal(w, "start an approval sign-in", err)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:  approvalCookie,
		Value: binding,
		// The three pages' own prefix: the cookie reaches the callback and nothing else the
		// broker serves.
		Path:     approvalRoutes,
		HttpOnly: true,
		Secure:   s.approvalCookieSecure(),
		SameSite: http.SameSiteLaxMode,
		// As long as the approval itself, and never zero, which would make it a session cookie.
		MaxAge: max(int(time.Until(challenge.ExpiresAt).Seconds()), 1),
	})
	target := s.deps.Approval.Flow.ConfirmURL(
		s.deps.PublicURL+ApprovalCallbackPath, challenge.ID, approval.Nonce(challenge.ID, binding),
		challenge.Approver, ApprovalMaxAge)
	http.Redirect(w, r, target, http.StatusSeeOther)
}

// approvalCookieSecure is whether the binding cookie is Secure: on every deployment, whose
// BROKER_PUBLIC_URL is https, and not on a local http broker, where a Secure cookie would never be
// sent back and the flow could not be run at all.
func (s *server) approvalCookieSecure() bool {
	return strings.HasPrefix(s.deps.PublicURL, "https://")
}

// approvalCallback is where the issuer sends the person back, and the only place a decision is
// applied as a person. It verifies the ID token against the issuer's own keys and this client
// (oidc.CodeFlow.Confirm: audience, signature, expiry, the nonce the cookie binds, a verified
// email, the Workspace domain and an authentication no older than ApprovalMaxAge), requires that
// email to be the challenge's approver, spends the challenge, and only then applies the decision —
// the same ApplyDecision every other approval goes through, under that person's login, writing
// the same event, grant and audit rows. Any failure renders a page and applies nothing.
func (s *server) approvalCallback(w http.ResponseWriter, r *http.Request) {
	if s.refuseApprovalAddress(w, r) {
		return
	}
	query := r.URL.Query()
	if refusal := query.Get("error"); refusal != "" {
		// The issuer's own refusal (the person cancelled, or it would not sign them in). Its code
		// is the issuer's vocabulary, so the log keeps it and the page does not.
		slog.Info("approval sign-in refused by the issuer", "error", refusal)
		approval.Write(w, http.StatusForbidden, approval.Page{
			Title:   "Not approved",
			Message: "The identity provider did not sign you in, so nothing was approved. Start again from Dispatch.",
		})
		return
	}
	id, code := query.Get("state"), query.Get("code")
	if id == "" || code == "" {
		approval.Write(w, http.StatusBadRequest, approval.Page{
			Title:   "Not approved",
			Message: "This sign-in came back without what the broker needs to read it, so nothing was approved. Start again from Dispatch.",
		})
		return
	}
	challenge, ok := s.challengeOf(w, r, id)
	if !ok {
		return
	}
	binding, err := r.Cookie(approvalCookie)
	if err != nil || binding.Value == "" {
		approval.Write(w, http.StatusForbidden, approval.Page{
			Title:   "Not approved",
			Message: "This browser does not hold the approval it started, so nothing was approved. Open the approval link again in the browser you are signed in with.",
		})
		return
	}
	now := time.Now()
	claims, err := s.deps.Approval.Flow.Confirm(r.Context(), s.deps.PublicURL+ApprovalCallbackPath,
		code, approval.Nonce(id, binding.Value), ApprovalMaxAge, now)
	if err != nil {
		s.refuseApprovalSignIn(w, challenge.RecordID, err)
		return
	}
	if record.CanonicalLogin(claims.Email) != challenge.Approver {
		slog.Warn("approval sign-in was another person's", "record", challenge.RecordID, "approver", challenge.Approver, "issuer", claims.Issuer, "subject", claims.Subject)
		approval.Write(w, http.StatusForbidden, approval.Page{
			Title:   "Not approved",
			Message: "The account you signed in with is not the one this approval is for, so nothing was approved.",
			Facts:   []approval.Fact{{Label: "Approver", Value: challenge.Approver}},
		})
		return
	}
	spent, err := s.deps.Approval.Challenges.Spend(r.Context(), id, claims.Issuer, claims.Subject, now)
	if err != nil {
		if !writeChallengeRefusal(w, err) {
			s.writeApprovalInternal(w, "spend an approval", err)
		}
		return
	}
	s.applyApproval(w, r, spent, claims)
}

// applyApproval decides the spent challenge's record as its approver: a machine login through
// machine.Service.ApplyDecision with the record's own code, read back through the hash the
// challenge kept, and a request for secrets through requests.Machine.ApplyDecision. The challenge
// is already spent, so a refusal here ends this approval: the person starts again from Dispatch,
// which opens another.
func (s *server) applyApproval(w http.ResponseWriter, r *http.Request, spent approval.Challenge, claims oidc.Claims) {
	var decided approval.Page
	var err error
	if spent.Kind == record.KindLauncherCredential {
		var code, credentialID string
		if code, err = s.deps.MachineLogin.CodeFor(r.Context(), spent.RecordID, spent.CodeHash); err == nil {
			_, credentialID, err = s.deps.MachineLogin.ApplyDecision(r.Context(), spent.RecordID, true, spent.Approver, code)
		}
		decided = approval.Page{
			Title:   "Approved",
			Message: "The machine login is approved. The machine has its credential; you can close this page.",
			Facts:   []approval.Fact{{Label: "Machine credential", Value: credentialID}},
		}
	} else {
		var decision requests.Decision
		decision, err = s.deps.Machine.ApplyDecision(r.Context(), spent.RecordID, true, spent.Approver)
		decided = approval.Page{
			Title:   "Approved",
			Message: "The secrets are granted to the session that asked for them; you can close this page.",
			Facts:   []approval.Fact{{Label: "Grant", Value: decision.GrantID}},
		}
	}
	if err != nil {
		s.refuseApprovalDecision(w, spent.RecordID, err)
		return
	}
	slog.Info("approval decided with the approver's own sign-in", "record", spent.RecordID, "kind", spent.Kind,
		"approver", spent.Approver, "issuer", claims.Issuer, "subject", claims.Subject)
	approval.Write(w, http.StatusOK, decided)
}

// challengeOf reads the challenge id names for a page. It answers only one that can still be
// spent, and otherwise writes the page saying why it cannot: unknown, used, expired, or a
// database that could not answer.
func (s *server) challengeOf(w http.ResponseWriter, r *http.Request, id string) (approval.Challenge, bool) {
	challenge, err := s.deps.Approval.Challenges.Read(r.Context(), id)
	switch {
	case err != nil:
	case !challenge.UsedAt.IsZero():
		err = approval.ErrUsed
	case !challenge.ExpiresAt.After(time.Now()):
		err = approval.ErrExpired
	}
	if err == nil {
		return challenge, true
	}
	if !writeChallengeRefusal(w, err) {
		s.writeApprovalInternal(w, "read an approval", err)
	}
	return approval.Challenge{}, false
}

// writeChallengeRefusal writes the page for an approval that cannot be spent — unknown, already
// used, or expired — and reports whether it wrote one. Every other error is the caller's: only it
// knows what the broker was doing when the database would not answer.
func writeChallengeRefusal(w http.ResponseWriter, err error) bool {
	switch {
	case errors.Is(err, approval.ErrNotFound):
		approval.Write(w, http.StatusNotFound, approval.Page{
			Title:   "Nothing to approve",
			Message: "This approval link is not one this broker knows. Open the credential request in Dispatch and approve it again.",
		})
	case errors.Is(err, approval.ErrUsed):
		approval.Write(w, http.StatusGone, approval.Page{
			Title:   "Already used",
			Message: "This approval has already been used. Each approval is good once; open the credential request in Dispatch to see what it decided.",
		})
	case errors.Is(err, approval.ErrExpired):
		approval.Write(w, http.StatusGone, approval.Page{
			Title:   "Expired",
			Message: "This approval has expired. Open the credential request in Dispatch and approve it again.",
		})
	default:
		return false
	}
	return true
}

// refuseApprovalSignIn writes the page for a sign-in the broker would not take, naming the class
// and never anything of the token. The log line carries the broker's own reason.
func (s *server) refuseApprovalSignIn(w http.ResponseWriter, recordID string, err error) {
	slog.Warn("approval sign-in refused", "record", recordID, "error", err)
	message := "The identity provider's answer did not verify, so nothing was approved. Start again from Dispatch."
	switch {
	case errors.Is(err, oidc.ErrNonce):
		message = "This sign-in does not belong to the approval this browser started, so nothing was approved. Open the approval link again."
	case errors.Is(err, oidc.ErrEmailUnverified):
		message = "The account you signed in with has no verified email address, so nothing was approved."
	case errors.Is(err, oidc.ErrHostedDomain):
		message = "The account you signed in with is not in this deployment's domain, so nothing was approved."
	case errors.Is(err, oidc.ErrStaleSignIn):
		message = "You signed in too long ago for this approval. Open the approval link again and sign in when asked."
	}
	approval.Write(w, http.StatusForbidden, approval.Page{Title: "Not approved", Message: message})
}

// refuseApprovalDecision writes the page for a decision the broker would not apply after a sound
// sign-in: the record was decided or expired meanwhile, or the policy moved under it.
func (s *server) refuseApprovalDecision(w http.ResponseWriter, recordID string, err error) {
	switch {
	case errors.Is(err, machine.ErrAlreadyDecided), errors.Is(err, machine.ErrLoginExpired),
		errors.Is(err, requests.ErrTerminal), errors.Is(err, requests.ErrExpired):
		approval.Write(w, http.StatusConflict, approval.Page{
			Title:   "Nothing to approve",
			Message: "This credential request was decided or expired before your sign-in came back, so nothing was approved.",
		})
	case errors.Is(err, record.ErrNotApprover), errors.Is(err, machine.ErrCodeMismatch),
		errors.Is(err, machine.ErrServiceLogsInWithToken), errors.Is(err, requests.ErrGrantChainInvalid),
		errors.Is(err, machine.ErrKeyHoldsLiveCredential):
		slog.Warn("approval refused at the decision", "record", recordID, "error", err)
		approval.Write(w, http.StatusForbidden, approval.Page{
			Title:   "Not approved",
			Message: "The broker will not approve this credential request any more: " + err.Error(),
		})
	default:
		s.writeApprovalInternal(w, "apply an approval", err)
	}
}

// writeApprovalInternal is writeInternal for a person's browser: the cause is logged, and the page
// says only that it failed.
func (s *server) writeApprovalInternal(w http.ResponseWriter, op string, err error) {
	slog.Error("broker: "+op+" failed", "error", err)
	approval.Write(w, http.StatusInternalServerError, approval.Page{
		Title:   "Something went wrong",
		Message: "The broker could not " + op + ". Nothing was approved; try the link again in a moment.",
	})
}

// refuseApprovalAddress writes the page saying so, with the same Retry-After every other bucket
// sends, and reports true when r's source address has no request left in the bucket the three
// approval pages share (DefaultApprovalLimit). It answers a page rather than the broker's JSON
// refusal because its reader is a person's browser.
func (s *server) refuseApprovalAddress(w http.ResponseWriter, r *http.Request) bool {
	if s.approvalLimiter.AllowAt(clientAddress(r, s.deps.TrustedProxyHeader), time.Now()) {
		return false
	}
	w.Header().Set("Retry-After", strconv.Itoa(retryAfterSeconds(s.approvalLimiter.Every())))
	approval.Write(w, http.StatusTooManyRequests, approval.Page{
		Title:   "Too many requests",
		Message: "This address has asked the broker for too many approval pages. Wait a moment and open the link again.",
	})
	return true
}
