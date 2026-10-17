package main

import (
	"crypto/subtle"
	"fmt"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"slices"
	"strings"

	"github.com/sjawhar/envoy/internal/config"
	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/internal/oidc"
)

const bearerPrefix = "Bearer "

// apiAuth requires JSON bodies for /v1, then authenticates it with the shared ENVOY_API_TOKEN
// bearer and, when a verifier is configured, with a projected service-account token issued to its
// issuer and audience. Exempt paths pass in every configuration; /v1 is open only when neither
// kind of credential is configured, so an empty shared token beside a configured verifier requires
// a token rather than opening the tree. An open /v1 serves only a request whose Host names this
// listener (servesHost: a loopback name or address, or one of allowedHosts), so a page that
// rebinds its own name to an address reaching the listener cannot use it; a listener that requires
// a bearer checks no Host, since such a page holds no bearer. A bearer the verifier rejects is
// answered 401 and never falls back to another authentication path; the failure class goes to the
// log and never to the caller, since naming it would tell an unauthenticated client which
// credential the listener is configured for.
func apiAuth(token string, verifier *oidc.Verifier, allowedHosts []string, logger *logging.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isAPIAuthExemptPath(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		open := token == "" && verifier == nil
		if open && !servesHost(r.Host, allowedHosts) {
			logger.Warn("listener: request for a host it does not serve",
				slog.String("host", r.Host),
				slog.String("path", r.URL.Path))
			writeJSONError(w, http.StatusMisdirectedRequest,
				fmt.Sprintf("request Host %q is not a host this listener serves; an unauthenticated listener serves loopback and the names in ENVOY_ALLOWED_HOSTS", r.Host))
			return
		}
		if r.ContentLength != 0 {
			contentType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
			if err != nil || contentType != "application/json" {
				writeJSONError(w, http.StatusUnsupportedMediaType, "Content-Type must be application/json")
				return
			}
		}
		if open {
			next.ServeHTTP(w, r)
			return
		}

		authorization := r.Header.Get("Authorization")
		if token != "" && subtle.ConstantTimeCompare(
			[]byte(authorization),
			[]byte(bearerPrefix+token),
		) == 1 {
			next.ServeHTTP(w, r)
			return
		}

		if verifier != nil {
			if raw, ok := strings.CutPrefix(authorization, bearerPrefix); ok && oidc.LooksLikeJWT(raw) {
				_, err := verifier.Verify(r.Context(), raw)
				if err == nil {
					next.ServeHTTP(w, r)
					return
				}
				logger.Warn("listener: service-account token rejected",
					slog.String("reason", oidc.Reason(err)),
					slog.String("path", r.URL.Path))
			}
		}

		writeJSONError(w, http.StatusUnauthorized, "unauthorized")
	})
}

// servesHost reports whether a request's Host names this listener: its name, port stripped,
// brackets off an IPv6 address, case folded and one trailing dot dropped, is a loopback name or
// address or one of allowedHosts, which config.Load normalizes the same way. An empty Host (an
// HTTP/1.0 request that sent none) names nothing.
func servesHost(host string, allowedHosts []string) bool {
	name := config.HostName(host)
	if name == "" {
		return false
	}
	return isLoopbackHost(name) || slices.Contains(allowedHosts, name)
}

func isAPIAuthExemptPath(path string) bool {
	return path == "/healthz" || path == "/metrics" || strings.HasPrefix(path, "/webhook/")
}

// resolveListenerOIDCConfig binds the listener's two variable names to the
// shared both-or-neither rule. They are configured both or neither: one alone
// is a refusal naming the missing one.
func resolveListenerOIDCConfig(getenv func(string) string) (issuer, audience string, err error) {
	return oidc.ConfigFromEnv(getenv, "ENVOY_OIDC_ISSUER", "ENVOY_OIDC_AUDIENCE")
}

// describeListenerAPIAuth names the credentials /v1 accepts, for the boot log.
func describeListenerAPIAuth(token, issuer string) string {
	switch {
	case token != "" && issuer != "":
		return "shared token and oidc " + issuer
	case token != "":
		return "shared token"
	case issuer != "":
		return "oidc " + issuer
	}
	return "disabled"
}

// describeListenerAPIHosts names the Hosts /v1 is served to, for the boot log: any Host when a
// bearer is required, loopback and ENVOY_ALLOWED_HOSTS otherwise. A list set beside a bearer is
// said to be unused rather than left to look as if it applied.
func describeListenerAPIHosts(token string, verifier *oidc.Verifier, allowedHosts []string) string {
	if token != "" || verifier != nil {
		if len(allowedHosts) > 0 {
			return "any (a bearer is required, so ENVOY_ALLOWED_HOSTS is unused)"
		}
		return "any (a bearer is required)"
	}
	if len(allowedHosts) == 0 {
		return "loopback"
	}
	return "loopback, " + strings.Join(allowedHosts, ", ")
}

func validateListenerAPIAuth(listenHost, token, allowUnauthenticated string, verifier *oidc.Verifier) error {
	if token != "" || verifier != nil || allowUnauthenticated == "1" || isLoopbackHost(listenHost) {
		return nil
	}
	return fmt.Errorf("ENVOY_API_TOKEN or the ENVOY_OIDC_ISSUER/ENVOY_OIDC_AUDIENCE pair is required when ENVOY_LISTEN_HOST=%q is not loopback; set ENVOY_API_ALLOW_UNAUTHENTICATED=1 only during the Fargate transition", listenHost)
}

func isLoopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
