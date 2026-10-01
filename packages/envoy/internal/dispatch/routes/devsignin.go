package routes

// Dev sign-in: GET /auth/_dev/signin issues an allowlisted login the session cookie a GitHub sign-in
// issues, with no GitHub exchange, so a local instance can be driven signed-in through the
// production cookie identity. cmd/dispatch turns it on with DISPATCH_DEV_SIGNIN=1 behind its boot
// fence, and BuildAppContext mounts it only for a loopback origin. Everything that can mint a
// cookie without GitHub is in this file.

import (
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"

	"github.com/sjawhar/envoy/internal/dispatch/identity"
)

// DevSignInOrigin returns the host:port of serverURL, the dashboard origin, when it names this
// machine, and refuses it otherwise: a deployment's origin is its public one, so the dev sign-in
// route cannot take effect there. The host must be "localhost" or a loopback IP literal.
func DevSignInOrigin(serverURL string) (string, error) {
	parsed, err := url.Parse(serverURL)
	if err != nil || parsed.Host == "" || !LoopbackName(parsed.Hostname()) {
		return "", fmt.Errorf("DISPATCH_DEV_SIGNIN=1 is for a loopback origin only: the dashboard origin %q (DISPATCH_SERVER_URL, or dispatch.serverUrl in envoy.json) must name 127.0.0.1, [::1] or localhost", serverURL)
	}
	return parsed.Host, nil
}

// LoopbackName reports whether host is "localhost", in any case, or a loopback IP literal.
func LoopbackName(host string) bool {
	return strings.EqualFold(host, "localhost") || LoopbackIP(host)
}

// LoopbackIP reports whether host is an IP literal for a loopback address, with no zone and not an
// IPv4-mapped IPv6 address. Nothing is resolved: a name that points at this machine does not count.
// host carries no brackets; net.SplitHostPort and url.URL.Hostname remove them.
func LoopbackIP(host string) bool {
	addr, err := netip.ParseAddr(host)
	return err == nil && addr.Zone() == "" && !addr.Is4In6() && addr.IsLoopback()
}

// forwardingHeaders are the headers a proxy adds to a request it relays. The dev sign-in route
// refuses a request carrying any of them, whatever its value: the peer it sees is then the proxy,
// not the browser.
var forwardingHeaders = []string{"Forwarded", "X-Forwarded-For", "X-Forwarded-Host", "X-Forwarded-Proto", "X-Real-IP", "Via"}

// authDevSignIn is the local workflow's sign-in: authCallback's allowlist check and issueSession,
// with GitHub's code exchange left out. The allowlist is checked as the callback checks it and the
// spelling requested is minted, so a test can sign in as GitHub spells a login. Only a loopback
// peer that no proxy forwarded is served, and every mint is logged at WARN.
func (r *router) authDevSignIn(w http.ResponseWriter, req *http.Request) {
	host, _, err := net.SplitHostPort(req.RemoteAddr)
	if err != nil || !LoopbackIP(host) {
		writeCodeError(w, http.StatusForbidden, fmt.Sprintf("dev sign-in serves loopback peers only, not %s", req.RemoteAddr), "DEV_SIGNIN_FORBIDDEN")
		return
	}
	for _, name := range forwardingHeaders {
		if _, present := req.Header[http.CanonicalHeaderKey(name)]; present {
			writeCodeError(w, http.StatusForbidden, "dev sign-in refuses a forwarded request: "+name+" is set", "DEV_SIGNIN_FORBIDDEN")
			return
		}
	}
	login := strings.TrimSpace(req.URL.Query().Get("login"))
	if login == "" {
		writeCodeError(w, http.StatusBadRequest, "login query parameter required", "DEV_SIGNIN_INPUT")
		return
	}
	if _, allowed := r.ctx.AllowedLogins[strings.ToLower(login)]; !allowed {
		identity.WriteError(w, identity.ErrLoginNotAllowed)
		return
	}
	if !r.issueSession(w, req, login) {
		return
	}
	slog.Warn("dispatch: dev sign-in minted a session cookie", "login", login, "remote_addr", req.RemoteAddr)
	w.Header().Set("Cache-Control", "no-store")
	http.Redirect(w, req, sanitizeNext(req.URL.Query().Get("next")), http.StatusFound)
}

// requireHost refuses every request whose Host is not host, the dashboard origin's host:port,
// compared without regard to case: a browser that reached this server through another name (a
// DNS-rebinding page, a tunnel) is told so instead of being answered as the dashboard.
func requireHost(host string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if !strings.EqualFold(req.Host, host) {
			writeCodeError(w, http.StatusMisdirectedRequest, fmt.Sprintf("request Host %q is not the dashboard origin %q", req.Host, host), "HOST_MISMATCH")
			return
		}
		next.ServeHTTP(w, req)
	})
}
