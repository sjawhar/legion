// Package identity resolves the person attached to a Dispatch request, named by lowercase email.
package identity

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/sjawhar/envoy/internal/dispatch/auth"
)

// ErrNoIdentity means the request did not prove a person: no cookie or header, a cookie whose
// session has ended, or a person whose membership could not be confirmed.
var ErrNoIdentity = errors.New("no identity")

// Identity resolves the person a request comes from, by lowercase email.
type Identity interface {
	Login(r *http.Request) (email string, err error)
}

// CookieIdentity resolves a signed Dispatch session cookie.
type CookieIdentity struct {
	SigningKey string
	Sessions   auth.SessionStore
	// Membership confirms, at least hourly, that the person a cookie names may still use Dispatch,
	// and ends their session when the sign-in pool no longer says so. It is nil only on the local
	// dev sign-in server, whose people never signed in through the pool.
	Membership *Membership
}

// Login returns the email of a valid session cookie whose generation is current and whose person
// is still a member, or ErrNoIdentity.
func (i CookieIdentity) Login(r *http.Request) (string, error) {
	session, ok := auth.SessionFromRequest(r, i.SigningKey)
	if !ok {
		return "", ErrNoIdentity
	}
	if i.Sessions == nil {
		return "", ErrNoIdentity
	}
	generation, found, err := i.Sessions.CurrentSessionGeneration(r.Context(), session.Login)
	if err != nil {
		return "", err
	}
	if !found || generation != session.Generation {
		return "", ErrNoIdentity
	}
	if i.Membership != nil {
		if err := i.Membership.Confirm(r.Context(), session.Login); err != nil {
			return "", err
		}
	}
	return session.Login, nil
}

// HeaderIdentity resolves the person a trusted proxy names in Header, by email. Every person it
// resolves is recorded as having signed in.
type HeaderIdentity struct {
	Header string
	People auth.PeopleStore
}

// Login returns the header's person, lowercased, or ErrNoIdentity when the header is absent.
func (i HeaderIdentity) Login(r *http.Request) (string, error) {
	email := strings.ToLower(strings.TrimSpace(r.Header.Get(i.Header)))
	if email == "" {
		return "", ErrNoIdentity
	}
	if err := i.People.Record(r.Context(), email); err != nil {
		return "", err
	}
	return email, nil
}

// WriteError renders an Identity error as Dispatch's JSON error response.
func WriteError(w http.ResponseWriter, err error) {
	status := http.StatusInternalServerError
	body := map[string]string{"error": "identity resolution failed", "code": "IDENTITY_ERROR"}
	if errors.Is(err, ErrNoIdentity) {
		status = http.StatusUnauthorized
		body = map[string]string{"error": "unauthorized", "code": "NO_IDENTITY"}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
