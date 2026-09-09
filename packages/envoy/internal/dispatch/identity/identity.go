// Package identity resolves the human login attached to a Dispatch request.
package identity

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/sjawhar/envoy/internal/dispatch/auth"
)

var (
	// ErrNoIdentity means the request did not prove a caller identity.
	ErrNoIdentity = errors.New("no identity")
	// ErrLoginNotAllowed means the request identity is outside the allowlist.
	ErrLoginNotAllowed = errors.New("login not allowed")
)

// Identity resolves the human login for a request.
type Identity interface {
	Login(r *http.Request) (login string, err error)
}

// CookieIdentity resolves a signed Dispatch session cookie.
type CookieIdentity struct {
	SigningKey    string
	AllowedLogins map[string]struct{}
}

// Login returns the valid session login or ErrNoIdentity.
func (i CookieIdentity) Login(r *http.Request) (string, error) {
	login := auth.SessionLogin(r, i.SigningKey)
	if login == "" {
		return "", ErrNoIdentity
	}
	if _, allowed := i.AllowedLogins[login]; !allowed {
		return "", ErrLoginNotAllowed
	}
	return login, nil
}

// HeaderIdentity resolves the trusted proxy header and enforces the login
// allowlist for every request.
type HeaderIdentity struct {
	Header        string
	AllowedLogins map[string]struct{}
}

// Login returns the supplied allowed header login or an identity sentinel.
func (i HeaderIdentity) Login(r *http.Request) (string, error) {
	login := strings.TrimSpace(r.Header.Get(i.Header))
	if login == "" {
		return "", ErrNoIdentity
	}
	if _, allowed := i.AllowedLogins[login]; !allowed {
		return "", ErrLoginNotAllowed
	}
	return login, nil
}

// WriteError renders an Identity error as Dispatch's JSON error response.
func WriteError(w http.ResponseWriter, err error) {
	status := http.StatusInternalServerError
	body := map[string]string{"error": "identity resolution failed", "code": "IDENTITY_ERROR"}
	switch {
	case errors.Is(err, ErrNoIdentity):
		status = http.StatusUnauthorized
		body = map[string]string{"error": "unauthorized", "code": "NO_IDENTITY"}
	case errors.Is(err, ErrLoginNotAllowed):
		status = http.StatusForbidden
		body = map[string]string{"error": "login not allowed", "code": "LOGIN_NOT_ALLOWED"}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
