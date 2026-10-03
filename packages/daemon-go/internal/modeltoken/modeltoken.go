// Package modeltoken is `legion model-token`: the command a worker pod's Oh My Pi runs as its model
// apiKey. It signs in to Cognito as the operator's machine user through the custom authentication
// challenge, answering it with the pod's projected service-account token, and prints the access
// token. It signs in on every run and keeps nothing: Oh My Pi keeps the command's output for the
// life of its process and runs it again only after a 401, which a gateway that refuses a spent token
// answers only with one never used. The refresh and ID tokens Cognito also returns are never kept or
// used.
package modeltoken

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cognitoidentityprovider"
	"github.com/aws/aws-sdk-go-v2/service/cognitoidentityprovider/types"
)

// Config is the command's settings, every one the operator's (flags in its models.yml), so the
// public repository names no pool, client or user.
type Config struct {
	Region                  string
	ClientID                string
	Username                string
	ServiceAccountTokenFile string
	// Endpoint replaces the Cognito endpoint the SDK derives from Region; tests only.
	Endpoint string
}

// Token signs in and returns the access token. A refused or failed sign-in returns its reason and
// no token.
func Token(ctx context.Context, cfg Config) (string, error) {
	if err := cfg.check(); err != nil {
		return "", err
	}
	answer, err := os.ReadFile(cfg.ServiceAccountTokenFile)
	if err != nil {
		return "", fmt.Errorf("read the service-account token: %w", err)
	}
	return signIn(ctx, cfg, strings.TrimSpace(string(answer)))
}

func (cfg Config) check() error {
	for _, setting := range []struct{ name, value string }{
		{"region", cfg.Region}, {"client id", cfg.ClientID}, {"username", cfg.Username},
		{"service-account token file", cfg.ServiceAccountTokenFile},
	} {
		if setting.value == "" {
			return fmt.Errorf("no %s", setting.name)
		}
	}
	return nil
}

// signIn is Cognito's custom authentication: InitiateAuth names the user, and the custom challenge is
// answered with the pod's service-account token, which Cognito's verify trigger checks.
func signIn(ctx context.Context, cfg Config, answer string) (string, error) {
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
		return "", fmt.Errorf("Cognito InitiateAuth: %w", err)
	}
	if started.ChallengeName != types.ChallengeNameTypeCustomChallenge {
		return "", fmt.Errorf("Cognito InitiateAuth answered challenge %q, want CUSTOM_CHALLENGE", started.ChallengeName)
	}
	answered, err := client.RespondToAuthChallenge(ctx, &cognitoidentityprovider.RespondToAuthChallengeInput{
		ChallengeName:      types.ChallengeNameTypeCustomChallenge,
		ClientId:           aws.String(cfg.ClientID),
		Session:            started.Session,
		ChallengeResponses: map[string]string{"USERNAME": cfg.Username, "ANSWER": answer},
	})
	if err != nil {
		return "", fmt.Errorf("Cognito RespondToAuthChallenge: %w", err)
	}
	if answered.AuthenticationResult == nil || aws.ToString(answered.AuthenticationResult.AccessToken) == "" {
		return "", errors.New("Cognito answered the custom challenge without an access token")
	}
	return aws.ToString(answered.AuthenticationResult.AccessToken), nil
}
