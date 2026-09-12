package main

import (
	"crypto/subtle"
	"fmt"
	"net"
	"net/http"
	"strings"
)

func apiAuth(token string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if token == "" || isAPIAuthExemptPath(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}

		if subtle.ConstantTimeCompare(
			[]byte(r.Header.Get("Authorization")),
			[]byte("Bearer "+token),
		) == 1 {
			next.ServeHTTP(w, r)
			return
		}

		writeJSONError(w, http.StatusUnauthorized, "unauthorized")
	})
}

func isAPIAuthExemptPath(path string) bool {
	return path == "/healthz" || path == "/metrics" || strings.HasPrefix(path, "/webhook/")
}

func validateListenerAPIAuth(listenHost, token, allowUnauthenticated string) error {
	if token != "" || allowUnauthenticated == "1" || isLoopbackHost(listenHost) {
		return nil
	}
	return fmt.Errorf("ENVOY_API_TOKEN is required when ENVOY_LISTEN_HOST=%q is not loopback; set ENVOY_API_ALLOW_UNAUTHENTICATED=1 only during the Fargate transition", listenHost)
}

func isLoopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
