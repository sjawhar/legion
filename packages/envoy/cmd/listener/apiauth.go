package main

import (
	"crypto/subtle"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"

	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/internal/oidc"
)

const bearerPrefix = "Bearer "

// apiAuth authenticates /v1 with the shared ENVOY_API_TOKEN bearer and, when a
// verifier is configured, with a projected service-account token issued to its
// issuer and audience. Exempt paths pass in every configuration; /v1 is open
// only when neither kind of credential is configured, so an empty shared token
// beside a configured verifier requires a token rather than opening the tree. A
// bearer the verifier rejects is answered 401 and never falls back to another
// authentication path; the failure class goes to the log and never to the
// caller, since naming it would tell an unauthenticated client which credential
// the listener is configured for.
func apiAuth(token string, verifier *oidc.Verifier, logger *logging.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isAPIAuthExemptPath(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		if token == "" && verifier == nil {
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
