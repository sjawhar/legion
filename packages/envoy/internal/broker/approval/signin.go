package approval

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"golang.org/x/oauth2"

	"github.com/sjawhar/envoy/internal/broker/record"
	"github.com/sjawhar/envoy/internal/broker/requests"
	"github.com/sjawhar/envoy/internal/oidc"
)

// pendingSignIn is a sign-in in progress, held in the browser's signInCookie between the redirect
// to the provider and the callback: the state the callback compares, the nonce the ID token must
// carry, the PKCE verifier, and the page the sign-in returns to.
type pendingSignIn struct {
	State    string `json:"state"`
	Nonce    string `json:"nonce"`
	Verifier string `json:"verifier"`
	Next     string `json:"next"`
}

// approvePrefix is the path every page of a record starts with, and the only place a sign-in may
// return to.
const approvePrefix = "/approve/"

// validNext reports whether next is a record's page, /approve/<record id>: a sign-in returns
// nowhere else, so its callback is no open redirect.
func validNext(next string) bool {
	id, ok := strings.CutPrefix(next, approvePrefix)
	return ok && record.ValidID(id)
}

// beginSignIn sends the browser to the provider to sign in and come back to detail's page,
// holding the sign-in in its signInCookie. login_hint names the record's approver when a person is
// (none for a shared secret's), so a person already signed in to the provider is asked nothing;
// selectAccount asks the provider for its account chooser, for a person switching accounts. The
// session the browser may hold now is left alone: the callback ends it when it makes the new one.
func (p *Page) beginSignIn(w http.ResponseWriter, r *http.Request, detail requests.RecordDetail, selectAccount bool) {
	flow, err := p.codeFlow(r)
	if err != nil {
		p.writeDiscoveryFailed(w, err)
		return
	}
	pending := pendingSignIn{State: randomValue(), Nonce: randomValue(), Verifier: oauth2.GenerateVerifier(), Next: approvePrefix + detail.RecordID}
	raw, err := json.Marshal(pending)
	if err != nil {
		writeUnavailable(w, "start the sign-in", err)
		return
	}
	setCookie(w, signInCookie, base64.RawURLEncoding.EncodeToString(raw), signInMaxAge)
	hint := ""
	if detail.Approver != record.AnyoneApprover {
		hint = detail.Approver
	}
	redirect(w, http.StatusFound, flow.OnlineAuthURL(p.redirectURL, oidc.OnlineSignIn{
		State: pending.State, Nonce: pending.Nonce, Verifier: pending.Verifier, LoginHint: hint, SelectAccount: selectAccount,
	}))
}

// readPending is the sign-in in progress r's signInCookie holds, when it holds one.
func readPending(r *http.Request) (pendingSignIn, bool) {
	c, err := r.Cookie(signInCookie)
	if err != nil {
		return pendingSignIn{}, false
	}
	raw, err := base64.RawURLEncoding.DecodeString(c.Value)
	if err != nil {
		return pendingSignIn{}, false
	}
	var pending pendingSignIn
	if err := json.Unmarshal(raw, &pending); err != nil || pending.State == "" {
		return pendingSignIn{}, false
	}
	return pending, true
}

// ServeCallback is GET /signin/callback, where the provider returns the browser with a code. The
// sign-in in progress is used once: its cookie is cleared whatever happens next. The callback
// refuses, creating no session, a browser with no sign-in in progress, a state that is not the
// one the sign-in sent (compared in constant time), a sign-in returning anywhere but a record's
// page, a code the provider will not exchange with the sign-in's PKCE verifier, an ID token that
// does not verify (issuer, audience, signature, expiry, nonce), and a person outside the Workspace
// domain or with an unverified email (oidc.WorkspaceLogin). Otherwise it mints a fresh random
// session secret into the session cookie, stores only its SHA-256 with the session's expiry, ends
// the session the browser held before (no fixation, no lingering session), and answers 303 to the
// sign-in's page.
func (p *Page) ServeCallback(w http.ResponseWriter, r *http.Request, _ Session) {
	pending, ok := readPending(r)
	clearCookie(w, signInCookie)
	if !ok {
		writeMessage(w, http.StatusBadRequest, say("No sign-in in progress",
			"This browser has no sign-in to the secrets broker in progress, or it took more than ten minutes. Open the request again to sign in."))
		return
	}
	query := r.URL.Query()
	if subtle.ConstantTimeCompare([]byte(query.Get("state")), []byte(pending.State)) != 1 {
		slog.Warn("approval sign-in refused: the state is not the sign-in's")
		writeMessage(w, http.StatusBadRequest, say("Sign-in refused",
			"This sign-in is not the one this browser started, so it was refused. Open the request again to sign in."))
		return
	}
	if !validNext(pending.Next) {
		slog.Warn("approval sign-in refused: it returns somewhere other than a request's page")
		writeMessage(w, http.StatusBadRequest, say("Sign-in refused", "A sign-in returns only to a request's page."))
		return
	}
	if providerError := query.Get("error"); providerError != "" {
		slog.Warn("approval sign-in refused by the provider", "error", providerError)
		writeMessage(w, http.StatusForbidden, say("Not signed in",
			"The sign-in provider did not sign you in. Open the request again to try once more."))
		return
	}
	flow, err := p.codeFlow(r)
	if err != nil {
		p.writeDiscoveryFailed(w, err)
		return
	}
	claims, err := flow.OnlineExchange(r.Context(), p.redirectURL, query.Get("code"), oidc.OnlineSignIn{
		State: pending.State, Nonce: pending.Nonce, Verifier: pending.Verifier,
	})
	if err != nil {
		slog.Warn("approval sign-in refused: the code exchange or the ID token failed", "error", err)
		writeMessage(w, http.StatusForbidden, say("Sign-in refused",
			"The sign-in provider's answer did not verify, so you are not signed in. Open the request again to sign in."))
		return
	}
	login, err := oidc.WorkspaceLogin(claims, p.cfg.SignIn.Domain)
	if err != nil {
		slog.Warn("approval sign-in refused", "login", login, "error", err)
		writeMessage(w, http.StatusForbidden, say("Sign-in refused", workspaceRefusal(err, p.cfg.SignIn.Domain)))
		return
	}
	secret := randomBytes()
	if old, ok := sessionSecret(r); ok {
		if err := p.cfg.Store.DeleteBrowserSession(r.Context(), sha256.Sum256(old)); err != nil {
			writeUnavailable(w, "end the previous sign-in", err)
			return
		}
	}
	lifetime := p.cfg.SignIn.SessionLifetime
	if err := p.cfg.Store.CreateBrowserSession(r.Context(), sha256.Sum256(secret), login, time.Now().Add(lifetime)); err != nil {
		writeUnavailable(w, "keep the sign-in", err)
		return
	}
	setCookie(w, sessionCookie, base64.RawURLEncoding.EncodeToString(secret), lifetime)
	slog.Info("approval sign-in", "login", login)
	redirect(w, http.StatusSeeOther, pending.Next)
}

// workspaceRefusal is what the page tells a person oidc.WorkspaceLogin refused.
func workspaceRefusal(err error, domain string) string {
	switch {
	case errors.Is(err, oidc.ErrNotInDomain):
		return "Only an account of the " + domain + " Workspace may sign in to the secrets broker."
	case errors.Is(err, oidc.ErrEmailNotVerified):
		return "Your account's email address is not verified, so you may not sign in to the secrets broker."
	}
	return "The sign-in provider named no account the secrets broker can sign in."
}

// ServeLogout is POST /signin/logout, from the broker's own origin only: it ends the session the
// browser holds (its row is deleted, so a copy of the cookie signs nothing in) and clears the
// cookie, answering a page saying so with a link back to the request the form names. It sends the
// browser nowhere: the provider's own sign-in is the person's.
func (p *Page) ServeLogout(w http.ResponseWriter, r *http.Request, sess Session) {
	if !p.sameOrigin(r) {
		refuseOrigin(w)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxFormBytes)
	if err := r.ParseForm(); err != nil {
		writeMessage(w, http.StatusBadRequest, say("Not signed out", "The form could not be read."))
		return
	}
	if secret, ok := sessionSecret(r); ok {
		if err := p.cfg.Store.DeleteBrowserSession(r.Context(), sha256.Sum256(secret)); err != nil {
			writeUnavailable(w, "end the sign-in", err)
			return
		}
	}
	clearCookie(w, sessionCookie)
	if sess.live() {
		slog.Info("approval sign-out", "login", sess.Login)
	}
	m := say("Signed out", "You are signed out of the secrets broker.")
	if next := r.PostForm.Get("next"); validNext(next) {
		m.Link = &link{Href: next, Text: "Back to the request"}
	}
	writeMessage(w, http.StatusOK, m)
}
