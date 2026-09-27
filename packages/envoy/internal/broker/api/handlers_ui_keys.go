// handlers_ui_keys.go: approver key routes — listing an approver's keys and driving the WebAuthn
// registration and endorsement ceremonies Dispatch's key pages open. Part of the UI routes
// (uiAuth) Dispatch's server relays to on behalf of the browser.
package api

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"time"

	"github.com/sjawhar/envoy/internal/broker/approvers"
	"github.com/sjawhar/envoy/internal/broker/record"
)

// uiOriginHost is BROKER_UI_ORIGIN's host: the WebAuthn rpId every ceremony this file opens
// embeds, and the audience registration/assertion verification checks against on the other end
// (approvers.Verifier.Origin, wired from the same BROKER_UI_ORIGIN in cmd/broker/main.go).
func (s *server) uiOriginHost() string {
	u, err := url.Parse(s.deps.UIOrigin)
	if err != nil || u.Hostname() == "" {
		return s.deps.UIOrigin
	}
	return u.Hostname()
}

type keyInfoResp struct {
	CredentialID string     `json:"credential_id"`
	AAGUID       string     `json:"aaguid"`
	RegisteredAt time.Time  `json:"registered_at"`
	LastUsedAt   *time.Time `json:"last_used_at"`
	State        string     `json:"state"`
	EndorsedBy   *string    `json:"endorsed_by"`
	Seeded       bool       `json:"seeded"`
}

func (s *server) listKeys(w http.ResponseWriter, r *http.Request) {
	login := r.PathValue("login")
	keys, err := s.deps.Approvers.Keys(r.Context(), login)
	if err != nil {
		writeInternal(w, "list approver keys", err)
		return
	}
	out := make([]keyInfoResp, len(keys))
	for i, k := range keys {
		out[i] = keyInfoResp{
			CredentialID: k.CredentialID, AAGUID: k.AAGUID.String(), RegisteredAt: k.RegisteredAt,
			LastUsedAt: k.LastUsedAt, State: k.State, EndorsedBy: strPtr(k.EndorsedBy), Seeded: k.Seeded,
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"keys": out})
}

// webauthn* types are the exact JSON shapes navigator.credentials.create()/get() expect
// (PublicKeyCredentialCreationOptions / PublicKeyCredentialRequestOptions, WebAuthn L3 JSON
// serialization: byte strings are base64url).
type webauthnRP struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type webauthnUser struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	DisplayName string `json:"displayName"`
}

type webauthnPubKeyCredParam struct {
	Type string `json:"type"`
	Alg  int    `json:"alg"`
}

type webauthnAuthenticatorSelection struct {
	UserVerification string `json:"userVerification"`
	ResidentKey      string `json:"residentKey"`
}

type registrationOptions struct {
	RP                     webauthnRP                     `json:"rp"`
	User                   webauthnUser                   `json:"user"`
	Challenge              string                         `json:"challenge"`
	PubKeyCredParams       []webauthnPubKeyCredParam      `json:"pubKeyCredParams"`
	Timeout                int                            `json:"timeout"`
	Attestation            string                         `json:"attestation"`
	AuthenticatorSelection webauthnAuthenticatorSelection `json:"authenticatorSelection"`
}

// webauthnCeremonyTimeoutMS is how long the browser's create()/get() call itself waits for the
// authenticator, independent of the 10-minute server-side ceremony row expiry.
const webauthnCeremonyTimeoutMS = 60000

func (s *server) registrationCreationOptions(login string, challenge []byte) registrationOptions {
	login = record.CanonicalLogin(login)
	sum := sha256.Sum256([]byte(login))
	return registrationOptions{
		RP:               webauthnRP{ID: s.uiOriginHost(), Name: "Trajectory Labs Secrets"},
		User:             webauthnUser{ID: base64.RawURLEncoding.EncodeToString(sum[:]), Name: login, DisplayName: login},
		Challenge:        base64.RawURLEncoding.EncodeToString(challenge),
		PubKeyCredParams: []webauthnPubKeyCredParam{{Type: "public-key", Alg: -7}},
		Timeout:          webauthnCeremonyTimeoutMS,
		Attestation:      "direct",
		AuthenticatorSelection: webauthnAuthenticatorSelection{
			UserVerification: "required", ResidentKey: "discouraged",
		},
	}
}

type webauthnAllowCredential struct {
	Type string `json:"type"`
	ID   string `json:"id"`
}

type assertionOptions struct {
	Challenge        string                    `json:"challenge"`
	RPID             string                    `json:"rpId"`
	AllowCredentials []webauthnAllowCredential `json:"allowCredentials"`
	UserVerification string                    `json:"userVerification"`
	Timeout          int                       `json:"timeout"`
}

func (s *server) assertionCreationOptions(challenge []byte, credentialID string) assertionOptions {
	return assertionOptions{
		Challenge:        base64.RawURLEncoding.EncodeToString(challenge),
		RPID:             s.uiOriginHost(),
		AllowCredentials: []webauthnAllowCredential{{Type: "public-key", ID: credentialID}},
		UserVerification: "required",
		Timeout:          webauthnCeremonyTimeoutMS,
	}
}

func (s *server) beginRegister(w http.ResponseWriter, r *http.Request) {
	login := r.PathValue("login")
	ceremony, err := s.deps.Approvers.BeginRegister(r.Context(), login)
	if err != nil {
		writeInternal(w, "begin key registration", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ceremony_id": ceremony.ID,
		"publicKey":   s.registrationCreationOptions(login, ceremony.Challenge),
	})
}

type finishCeremonyBody struct {
	CeremonyID string          `json:"ceremony_id"`
	Response   json.RawMessage `json:"response"`
}

func (s *server) finishRegister(w http.ResponseWriter, r *http.Request) {
	login := r.PathValue("login")
	var body finishCeremonyBody
	if !readJSON(w, r, &body, "INVALID_FINISH") {
		return
	}
	yamlText, err := s.deps.Approvers.FinishRegister(r.Context(), login, body.CeremonyID, body.Response)
	switch {
	case errors.Is(err, approvers.ErrCeremonyNotFound):
		writeError(w, http.StatusNotFound, "CEREMONY_NOT_FOUND", err.Error())
		return
	case errors.Is(err, approvers.ErrCeremonyExpired):
		writeError(w, http.StatusBadRequest, "CEREMONY_EXPIRED", err.Error())
		return
	case errors.Is(err, approvers.ErrRegistrationInvalid):
		writeError(w, http.StatusBadRequest, "REGISTRATION_INVALID", err.Error())
		return
	case err != nil:
		writeInternal(w, "finish key registration", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"yaml": yamlText})
}

type beginEndorseBody struct {
	CredentialID string `json:"credential_id"`
	KeyHash      string `json:"key_hash"`
}

func (s *server) beginEndorse(w http.ResponseWriter, r *http.Request) {
	login := r.PathValue("login")
	var body beginEndorseBody
	if !readJSON(w, r, &body, "INVALID_ENDORSE") {
		return
	}
	if body.CredentialID == "" || body.KeyHash == "" {
		writeError(w, http.StatusBadRequest, "INVALID_ENDORSE", "credential_id and key_hash are required")
		return
	}
	ceremony, err := s.deps.Approvers.BeginEndorse(r.Context(), login, body.CredentialID, body.KeyHash)
	if err != nil {
		writeInternal(w, "begin key endorsement", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ceremony_id": ceremony.ID,
		"publicKey":   s.assertionCreationOptions(ceremony.Challenge, body.CredentialID),
	})
}

func (s *server) finishEndorse(w http.ResponseWriter, r *http.Request) {
	login := r.PathValue("login")
	var body finishCeremonyBody
	if !readJSON(w, r, &body, "INVALID_FINISH") {
		return
	}
	yamlText, err := s.deps.Approvers.FinishEndorse(r.Context(), login, body.CeremonyID, body.Response)
	switch {
	case errors.Is(err, approvers.ErrCeremonyNotFound):
		writeError(w, http.StatusNotFound, "CEREMONY_NOT_FOUND", err.Error())
		return
	case errors.Is(err, approvers.ErrCeremonyExpired):
		writeError(w, http.StatusBadRequest, "CEREMONY_EXPIRED", err.Error())
		return
	case errors.Is(err, approvers.ErrEndorserNotFound):
		writeError(w, http.StatusBadRequest, "ENDORSER_NOT_FOUND", err.Error())
		return
	case isAssertionError(err):
		writeError(w, http.StatusForbidden, "ASSERTION_INVALID", err.Error())
		return
	case err != nil:
		writeInternal(w, "finish key endorsement", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"yaml": yamlText})
}
