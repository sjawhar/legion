// Package modeltoken is `legion model-token`: the command a worker pod's Oh My Pi runs as its model
// apiKey. It signs in to Cognito as the operator's machine user through the custom authentication
// challenge, answering it with the pod's projected service-account token, and prints the access
// token. The access token is cached on the pod's memory-backed state volume until shortly before it
// expires; the refresh token Cognito also returns is never stored or used.
package modeltoken

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cognitoidentityprovider"
	"github.com/aws/aws-sdk-go-v2/service/cognitoidentityprovider/types"
)

// renewBefore is how long before its expiry a cached access token is replaced: long enough that a
// model call started with it does not outlive it.
const renewBefore = 5 * time.Minute

// Config is the command's settings, every one the operator's (flags in its models.yml), so the
// public repository names no pool, client or user.
type Config struct {
	Region                  string
	ClientID                string
	Username                string
	ServiceAccountTokenFile string
	CacheFile               string
	// Endpoint replaces the Cognito endpoint the SDK derives from Region; tests only.
	Endpoint string
	// Now is the clock the cache is judged by; time.Now when nil.
	Now func() time.Time
}

type cached struct {
	AccessToken string    `json:"accessToken"`
	ExpiresAt   time.Time `json:"expiresAt"`
}

// CacheError is a fresh access token that could not be cached: the token is valid and returned
// with it, and the caller reports the error. The image probe's pod has no state volume, so its one
// sign-in has nowhere to cache; a worker pod's every sign-in does.
type CacheError struct{ Err error }

func (e *CacheError) Error() string { return "cache the access token: " + e.Err.Error() }
func (e *CacheError) Unwrap() error { return e.Err }

// Token returns an access token: the cached one while it has more than renewBefore left, otherwise
// a fresh sign-in's, which then replaces the cache atomically (0600). A refused or failed sign-in
// returns its reason and no token, and leaves the cache as it was. A token that could not be cached
// is returned with a *CacheError.
func Token(ctx context.Context, cfg Config) (string, error) {
	if err := cfg.check(); err != nil {
		return "", err
	}
	now := time.Now
	if cfg.Now != nil {
		now = cfg.Now
	}
	if token, ok := readCache(cfg.CacheFile, now()); ok {
		return token, nil
	}
	answer, err := os.ReadFile(cfg.ServiceAccountTokenFile)
	if err != nil {
		return "", fmt.Errorf("read the service-account token: %w", err)
	}
	signedInAt := now()
	token, expiresIn, err := signIn(ctx, cfg, strings.TrimSpace(string(answer)))
	if err != nil {
		return "", err
	}
	if err := writeCache(cfg.CacheFile, cached{AccessToken: token, ExpiresAt: signedInAt.Add(expiresIn)}); err != nil {
		return token, &CacheError{Err: err}
	}
	return token, nil
}

func (cfg Config) check() error {
	for _, setting := range []struct{ name, value string }{
		{"region", cfg.Region}, {"client id", cfg.ClientID}, {"username", cfg.Username},
		{"service-account token file", cfg.ServiceAccountTokenFile}, {"cache file", cfg.CacheFile},
	} {
		if setting.value == "" {
			return fmt.Errorf("model-token: no %s", setting.name)
		}
	}
	return nil
}

// readCache is the cached token when the cache holds one with more than renewBefore left. Anything
// else, a missing or unreadable cache included, means a fresh sign-in.
func readCache(path string, now time.Time) (string, bool) {
	body, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	var entry cached
	if json.Unmarshal(body, &entry) != nil || entry.AccessToken == "" || !now.Add(renewBefore).Before(entry.ExpiresAt) {
		return "", false
	}
	return entry.AccessToken, true
}

// signIn is Cognito's custom authentication: InitiateAuth names the user, and the custom challenge is
// answered with the pod's service-account token, which Cognito's verify trigger checks.
func signIn(ctx context.Context, cfg Config, answer string) (string, time.Duration, error) {
	awsConfig := aws.Config{Region: cfg.Region}
	if cfg.Endpoint != "" {
		awsConfig.BaseEndpoint = aws.String(cfg.Endpoint)
	}
	client := cognitoidentityprovider.NewFromConfig(awsConfig)
	started, err := client.InitiateAuth(ctx, &cognitoidentityprovider.InitiateAuthInput{
		AuthFlow:       types.AuthFlowTypeCustomAuth,
		ClientId:       aws.String(cfg.ClientID),
		AuthParameters: map[string]string{"USERNAME": cfg.Username},
	})
	if err != nil {
		return "", 0, fmt.Errorf("model-token: Cognito InitiateAuth: %w", err)
	}
	if started.ChallengeName != types.ChallengeNameTypeCustomChallenge {
		return "", 0, fmt.Errorf("model-token: Cognito InitiateAuth answered challenge %q, want CUSTOM_CHALLENGE", started.ChallengeName)
	}
	answered, err := client.RespondToAuthChallenge(ctx, &cognitoidentityprovider.RespondToAuthChallengeInput{
		ChallengeName:      types.ChallengeNameTypeCustomChallenge,
		ClientId:           aws.String(cfg.ClientID),
		Session:            started.Session,
		ChallengeResponses: map[string]string{"USERNAME": cfg.Username, "ANSWER": answer},
	})
	if err != nil {
		return "", 0, fmt.Errorf("model-token: Cognito RespondToAuthChallenge: %w", err)
	}
	result := answered.AuthenticationResult
	if result == nil || aws.ToString(result.AccessToken) == "" || result.ExpiresIn <= 0 {
		return "", 0, errors.New("model-token: Cognito answered the custom challenge without an access token")
	}
	return aws.ToString(result.AccessToken), time.Duration(result.ExpiresIn) * time.Second, nil
}

// writeCache replaces path with entry, owner-only, by renaming a complete file beside it into place.
func writeCache(path string, entry cached) error {
	body, err := json.Marshal(entry)
	if err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	defer os.Remove(temporary.Name())
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return err
	}
	if _, err := temporary.Write(body); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporary.Name(), path)
}
