// Package approval is the secrets broker's own approval page: a person signs in to the broker with
// OpenID Connect (authorization code with PKCE S256, nonce and state, the broker's own client), the
// broker keeps that sign-in as a session of its own in Postgres, and the page prints a
// credential-request record's facts from the broker's own record, with Approve and Deny posting
// back to the broker's own origin, bound to the session and to the exact records the page printed.
//
// Nothing here trusts SameSite: the broker and Dispatch are sibling hosts under one public suffix
// (<tailnet>.ts.net), the same site and different origins. Both cookies are __Host- cookies, which
// a sibling host cannot set, and every POST must come from the broker's exact origin. Every answer
// is html/template output with no script, under a policy that frames nothing and loads nothing but
// the page's own stylesheet.
package approval

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"embed"
	"encoding/base64"
	"errors"
	"fmt"
	"html/template"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/broker/config"
	"github.com/sjawhar/envoy/internal/broker/requests"
	"github.com/sjawhar/envoy/internal/broker/store"
	"github.com/sjawhar/envoy/internal/oidc"
)

const (
	// sessionCookie holds a sign-in's session secret, of which Postgres keeps only the SHA-256.
	sessionCookie = "__Host-broker-session"
	// signInCookie holds a sign-in in progress (pendingSignIn) between the redirect to the
	// provider and its callback, so no row is written for a visitor who never signs in and any
	// broker replica can finish any sign-in.
	signInCookie = "__Host-broker-signin"
	// signInMaxAge is how long a sign-in in progress may take at the provider.
	signInMaxAge = 10 * time.Minute
	// secretBytes is the length of a session secret, and of a sign-in's state and nonce.
	secretBytes = 32
	// discoveryTimeout bounds the read of the provider's discovery document at a first sign-in.
	discoveryTimeout = 10 * time.Second
	// maxFormBytes bounds a POST's form body: a decision names at most maxRecords ids twice.
	maxFormBytes = 64 << 10
)

//go:embed templates/*.html assets/approve.css
var files embed.FS

var (
	pages      = template.Must(template.ParseFS(files, "templates/*.html"))
	stylesheet = mustRead("assets/approve.css")
)

func mustRead(name string) []byte {
	b, err := files.ReadFile(name)
	if err != nil {
		panic(err)
	}
	return b
}

// Config is what the page is built from.
type Config struct {
	// SignIn is the broker's own OpenID Connect client (BROKER_SIGNIN_*). Nil is sign-in off:
	// every route of the page but its stylesheet answers 503 naming the settings, and no person
	// can approve a secret request.
	SignIn *config.SignIn
	// PublicURL is BROKER_PUBLIC_URL as the broker resolved it: the one origin every POST must
	// come from, and the base of the sign-in's redirect URI, <PublicURL>/signin/callback.
	PublicURL string
	// MachineLoginURL is BROKER_MACHINE_LOGIN_URL, Dispatch's page where a person types a
	// machine login's code; a machine login's record links it and offers no button.
	MachineLoginURL string
	// Store holds the sessions.
	Store *store.Store
	// Machine reads the records and decides them (ReadRecord, PendingForApprover, ApplyDecision).
	Machine *requests.Machine
}

// Page is the approval page and its sign-in, whose methods are the broker's routes for them.
type Page struct {
	cfg Config
	// origin is PublicURL's origin as a browser serializes it in an Origin header.
	origin string
	// redirectURL is the sign-in's redirect URI.
	redirectURL string

	// flow is the provider's code flow, discovered at the first sign-in that needs it rather than
	// at boot, so a provider outage never stops the broker serving machines; a failed discovery
	// is not kept, and the next sign-in tries again.
	mu   sync.Mutex
	flow *oidc.CodeFlow
}

// New builds the page from cfg, refusing a public URL that is not an absolute http(s) URL.
func New(cfg Config) (*Page, error) {
	u, err := url.Parse(cfg.PublicURL)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return nil, fmt.Errorf("approval: the public URL %q is not an absolute http(s) URL", cfg.PublicURL)
	}
	if cfg.Store == nil || cfg.Machine == nil {
		return nil, errors.New("approval: the page needs the store and the request machine")
	}
	return &Page{cfg: cfg, origin: originOf(u), redirectURL: strings.TrimRight(cfg.PublicURL, "/") + "/signin/callback"}, nil
}

// originOf is u's origin as a browser serializes it: lowercase scheme and host, and the port only
// when it is not the scheme's default.
func originOf(u *url.URL) string {
	scheme, host, port := strings.ToLower(u.Scheme), strings.ToLower(u.Hostname()), u.Port()
	if port == "" || scheme == "https" && port == "443" || scheme == "http" && port == "80" {
		if strings.Contains(host, ":") {
			host = "[" + host + "]"
		}
		return scheme + "://" + host
	}
	return scheme + "://" + net.JoinHostPort(host, port)
}

// Session is the live broker sign-in a request's session cookie names; the zero Session is none.
type Session struct {
	// Login is the signed-in person's canonical login, the lowercased email of their account.
	Login string
	// secret is the raw session secret from the cookie, the key of the page's token.
	secret []byte
}

func (s Session) live() bool { return s.Login != "" }

// Authenticate answers the live session r's cookie names, or the zero Session when it names none
// (no cookie, a malformed one, an expired or logged-out session), since what that means is each
// route's to answer: the page starts a sign-in, a decision is refused. It writes the answer
// itself, and reports false, while sign-in is off (503 naming the settings) and when Postgres
// cannot answer (503).
func (p *Page) Authenticate(w http.ResponseWriter, r *http.Request) (Session, bool) {
	if p.cfg.SignIn == nil {
		writeMessage(w, http.StatusServiceUnavailable, say("Approval sign-in is off",
			"This secrets broker has no sign-in configured, so no person can approve a secret request here.",
			"Its operator turns sign-in on by setting "+strings.Join(config.SignInSettings, ", ")+"."))
		return Session{}, false
	}
	secret, ok := sessionSecret(r)
	if !ok {
		return Session{}, true
	}
	login, err := p.cfg.Store.BrowserSession(r.Context(), sha256.Sum256(secret), time.Now())
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return Session{}, true
	case err != nil:
		writeUnavailable(w, "read the sign-in session", err)
		return Session{}, false
	}
	return Session{Login: login, secret: secret}, true
}

// sessionSecret is the session secret r's session cookie carries, when it carries one of the
// right shape.
func sessionSecret(r *http.Request) ([]byte, bool) {
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return nil, false
	}
	secret, err := base64.RawURLEncoding.DecodeString(c.Value)
	if err != nil || len(secret) != secretBytes {
		return nil, false
	}
	return secret, true
}

// setCookie sets one of the page's cookies. Both are host-only (__Host-, no Domain), Secure,
// HttpOnly, Path=/ and SameSite=Lax whatever the public URL's scheme: a browser treats a loopback
// http origin as secure, so a local run keeps them too. SameSite=Lax only lets the session survive
// the top-level navigation from Dispatch's Inbox in a deployment where the two are different
// sites; it is not what keeps another origin out.
func setCookie(w http.ResponseWriter, name, value string, maxAge time.Duration) {
	http.SetCookie(w, &http.Cookie{
		Name: name, Value: value, Path: "/", MaxAge: int(maxAge / time.Second),
		Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode,
	})
}

// clearCookie tells the browser to drop one of the page's cookies, with the attributes it was set
// with, since a browser refuses a __Host- cookie without them.
func clearCookie(w http.ResponseWriter, name string) {
	setCookie(w, name, "", -time.Second)
}

// randomBytes is secretBytes bytes from crypto/rand.
func randomBytes() []byte {
	b := make([]byte, secretBytes)
	_, _ = rand.Read(b) // crypto/rand.Read never fails.
	return b
}

// randomValue is secretBytes random bytes, base64url without padding.
func randomValue() string {
	return base64.RawURLEncoding.EncodeToString(randomBytes())
}

// sameOrigin reports whether a POST comes from the broker's own origin: its one Origin header is
// exactly that origin or, with no Origin header, Sec-Fetch-Site says same-origin. Origin: null,
// any other origin, a sibling host's same-site, a cross-site request, and a request with neither
// header are refused, since SameSite does not tell a sibling host under the same public suffix
// from the broker. This is Dispatch's own check (internal/dispatch/routes/router.go).
func (p *Page) sameOrigin(r *http.Request) bool {
	switch origins := r.Header.Values("Origin"); len(origins) {
	case 0:
		return r.Header.Get("Sec-Fetch-Site") == "same-origin"
	case 1:
		return origins[0] == p.origin
	}
	return false
}

// refuseOrigin answers a POST sameOrigin refuses, having written nothing.
func refuseOrigin(w http.ResponseWriter) {
	writeMessage(w, http.StatusForbidden, say("Refused",
		"This request did not come from the secrets broker's own page, so nothing was changed."))
}

// contentSecurityPolicy loads nothing but the page's own stylesheet, runs no script, frames the
// page nowhere, and lets a form post only to the broker's own origin.
const contentSecurityPolicy = "default-src 'none'; style-src 'self'; img-src 'self'; form-action 'self'; frame-ancestors 'none'; base-uri 'none'"

// setHeaders sets the headers every answer of the page carries. Referrer-Policy is same-origin,
// not no-referrer: under no-referrer a browser sends Origin: null with the page's own form posts,
// which sameOrigin refuses, while same-origin keeps their real Origin and sends no referrer to any
// other origin. No answer carries a CORS header.
func setHeaders(h http.Header) {
	h.Set("Content-Security-Policy", contentSecurityPolicy)
	h.Set("X-Frame-Options", "DENY")
	h.Set("Referrer-Policy", "same-origin")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Content-Type-Options", "nosniff")
}

// redirect answers status with location and no body.
func redirect(w http.ResponseWriter, status int, location string) {
	setHeaders(w.Header())
	w.Header().Set("Location", location)
	w.WriteHeader(status)
}

// chrome is what every page's header prints: its title, and, for a signed-in person, their login,
// the switch-account link to the record the page is about, and a sign-out button returning there.
type chrome struct {
	Title string
	// Login and Record are empty when the page does not know them.
	Login, Record string
}

// link is a page's link onward.
type link struct{ Href, Text string }

// message is a page that says one thing: a refusal, an error, or that the person signed out.
type message struct {
	chrome
	Lines []string
	Link  *link
}

// say is a message page with title and lines and nothing else.
func say(title string, lines ...string) message {
	return message{chrome: chrome{Title: title}, Lines: lines}
}

func writeMessage(w http.ResponseWriter, status int, m message) {
	writePage(w, status, "message.html", m)
}

// writeUnavailable logs a dependency that could not answer and answers 503 saying so.
func writeUnavailable(w http.ResponseWriter, op string, err error) {
	slog.Error("approval page: "+op, "error", err)
	writeMessage(w, http.StatusServiceUnavailable, say("Unavailable",
		"The secrets broker could not "+op+" just now. Try again shortly."))
}

// writePage renders the named template into a buffer first, so a template that fails answers 500
// rather than half a page.
func writePage(w http.ResponseWriter, status int, name string, view any) {
	var buf bytes.Buffer
	setHeaders(w.Header())
	if err := pages.ExecuteTemplate(&buf, name, view); err != nil {
		slog.Error("approval page: render", "page", name, "error", err)
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("the approval page failed to render\n"))
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(buf.Bytes())
}

// ServeStylesheet serves the page's one stylesheet, embedded in the binary. It is served while
// sign-in is off too, since the page saying so links it.
func ServeStylesheet(w http.ResponseWriter, _ *http.Request) {
	setHeaders(w.Header())
	w.Header().Set("Content-Type", "text/css; charset=utf-8")
	_, _ = w.Write(stylesheet)
}

// codeFlow is the provider's code flow, discovered now when no sign-in has discovered it yet.
func (p *Page) codeFlow(r *http.Request) (*oidc.CodeFlow, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.flow == nil {
		s := p.cfg.SignIn
		flow, err := oidc.DiscoverCodeFlow(r.Context(), s.Issuer, s.ClientID, s.ClientSecret, s.Domain, discoveryTimeout)
		if err != nil {
			return nil, err
		}
		p.flow = flow
	}
	return p.flow, nil
}

// writeDiscoveryFailed answers a sign-in whose provider's discovery document could not be read.
func (p *Page) writeDiscoveryFailed(w http.ResponseWriter, err error) {
	slog.Error("approval sign-in: discover the provider", "issuer", p.cfg.SignIn.Issuer, "error", err)
	writeMessage(w, http.StatusServiceUnavailable, say("Sign-in unavailable",
		"The secrets broker could not reach its sign-in provider, "+p.cfg.SignIn.Issuer+". Try again shortly."))
}
