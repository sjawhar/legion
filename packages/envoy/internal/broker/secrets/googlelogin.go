package secrets

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

const googleTokenURL = "https://oauth2.googleapis.com/token"

// GoogleLogin exchanges the refresh token in a google-login secret for a Google access token.
// TokenURL and HTTP let tests replace Google's token endpoint and its transport.
type GoogleLogin struct {
	TokenURL string
	HTTP     *http.Client
}

type googleLoginSecret struct {
	Type         string `json:"type"`
	ClientID     string `json:"client_id"`
	ClientSecret string `json:"client_secret"`
	RefreshToken string `json:"refresh_token"`
}

type googleTokenResponse struct {
	AccessToken string `json:"access_token"`
}

type googleTokenError struct {
	Code    string `json:"error"`
	Subtype string `json:"error_subtype"`
}

// googleReauthSubtypes are the invalid_grant subtypes Google answers for a login that must sign in
// again to prove itself, the two google-auth-library-python's reauth.py names. A refusal names
// one of them, or "unknown" for any other value: the subtype is Google's own text, and an answer
// that echoed the refresh token there must not reach the session or the log.
var googleReauthSubtypes = map[string]bool{"invalid_rapt": true, "rapt_required": true}

// googleSubtype is subtype as a refusal names it: itself when googleReauthSubtypes holds it,
// "unknown" otherwise.
func googleSubtype(subtype string) string {
	if googleReauthSubtypes[subtype] {
		return subtype
	}
	return "unknown"
}

// Mint returns the bare Google access token. It never puts a stored credential or returned token
// in an error, and every error wraps one of ErrMintStoredValue, ErrMintRefused or ErrMintUpstream.
func (m GoogleLogin) Mint(ctx context.Context, req MintRequest) (string, error) {
	const what = "refresh Google login"
	var secret googleLoginSecret
	// encoding/json's syntax errors quote the offending byte, so neither decode wraps its error:
	// the stored credential and Google's answer both hold secrets.
	if json.Unmarshal([]byte(req.Value), &secret) != nil {
		return "", fmt.Errorf("%w: decode Google login secret: not a JSON object of its fields", ErrMintStoredValue)
	}
	if secret.Type != "authorized_user" || secret.ClientID == "" || secret.ClientSecret == "" || secret.RefreshToken == "" {
		return "", fmt.Errorf("%w: Google login secret must be an authorized_user credential", ErrMintStoredValue)
	}
	endpoint := m.TokenURL
	if endpoint == "" {
		endpoint = googleTokenURL
	}
	form := url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {secret.RefreshToken},
		"client_id":     {secret.ClientID},
		"client_secret": {secret.ClientSecret},
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return "", fmt.Errorf("%w: create Google token request: %w", ErrMintUpstream, err)
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	payload, err := mintCall(m.HTTP, request, what, "Google", http.StatusOK)
	if status := (*mintStatusError)(nil); errors.As(err, &status) {
		var tokenError googleTokenError
		if json.Unmarshal(payload, &tokenError) == nil && tokenError.Code == "invalid_grant" {
			if tokenError.Subtype != "" {
				return "", fmt.Errorf("%w: Google login needs a new sign-in: invalid_grant (%s)", ErrMintRefused, googleSubtype(tokenError.Subtype))
			}
			return "", fmt.Errorf("%w: Google login needs a new sign-in: invalid_grant", ErrMintRefused)
		}
	}
	if err != nil {
		return "", err
	}
	var token googleTokenResponse
	if json.Unmarshal(payload, &token) != nil {
		return "", fmt.Errorf("%w: decode Google token response: not a JSON object of its fields", ErrMintUpstream)
	}
	if token.AccessToken == "" {
		return "", fmt.Errorf("%w: Google returned no access token", ErrMintUpstream)
	}
	return token.AccessToken, nil
}
