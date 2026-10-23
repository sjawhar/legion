package secrets

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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

// Mint returns the bare Google access token. It never puts a stored credential or returned token
// in an error.
func (m GoogleLogin) Mint(ctx context.Context, value string) (string, error) {
	var secret googleLoginSecret
	// encoding/json's syntax errors quote the offending byte, so neither decode wraps its error:
	// the stored credential and Google's answer both hold secrets.
	if json.Unmarshal([]byte(value), &secret) != nil {
		return "", errors.New("decode Google login secret: not a JSON object of its fields")
	}
	if secret.Type != "authorized_user" || secret.ClientID == "" || secret.ClientSecret == "" || secret.RefreshToken == "" {
		return "", errors.New("Google login secret must be an authorized_user credential")
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
		return "", fmt.Errorf("create Google token request: %w", err)
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	client := m.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	response, err := client.Do(request)
	if err != nil {
		return "", fmt.Errorf("refresh Google login: %w", err)
	}
	payload, readErr := io.ReadAll(response.Body)
	response.Body.Close()
	if readErr != nil {
		return "", fmt.Errorf("read Google token response: %w", readErr)
	}
	if response.StatusCode != http.StatusOK {
		var tokenError googleTokenError
		if json.Unmarshal(payload, &tokenError) == nil && tokenError.Code == "invalid_grant" {
			if tokenError.Subtype != "" {
				return "", fmt.Errorf("Google login needs a new sign-in: invalid_grant (%s)", tokenError.Subtype)
			}
			return "", errors.New("Google login needs a new sign-in: invalid_grant")
		}
		return "", fmt.Errorf("refresh Google login: Google returned %s", response.Status)
	}
	var token googleTokenResponse
	if json.Unmarshal(payload, &token) != nil {
		return "", errors.New("decode Google token response: not a JSON object of its fields")
	}
	if token.AccessToken == "" {
		return "", errors.New("Google returned no access token")
	}
	return token.AccessToken, nil
}
