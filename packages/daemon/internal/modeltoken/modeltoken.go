// Package modeltoken signs in to Cognito as the operator's machine user through the custom
// authentication challenge, answering it with a worker pod's projected service-account token, and
// returns the access token `legion model-token` hands Oh My Pi as the pod's model apiKey. Oh My Pi
// keeps the command's output for the life of its process and runs it again only after a 401, which
// a gateway that refuses a spent token answers only with one never used, so Token signs in on every
// call and keeps nothing. The refresh and ID tokens Cognito also returns are never kept or used.
//
// The service-account token goes only where the operator's flags send it. Oh My Pi runs the command
// in an environment that carries the workspace's .env, so the sign-in takes no proxy and no TLS root
// from the environment and follows no redirect. Whatever answers is untrusted: a response is read
// only up to maxResponseBytes, the access token must be one line of printable characters, and an
// error names Cognito's error code, never the endpoint's own text.
package modeltoken

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"slices"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cognitoidentityprovider"
	"github.com/aws/aws-sdk-go-v2/service/cognitoidentityprovider/types"
	"github.com/aws/smithy-go"
	smithyhttp "github.com/aws/smithy-go/transport/http"
	"github.com/sjawhar/legion/daemon/internal/config"
)

// maxResponseBytes caps one Cognito response: both answers fit in a few KiB, so a longer one is a
// broken or hostile endpoint, refused before it fills the pod's memory.
const maxResponseBytes = 64 << 10

// Config is one sign-in's settings, each a flag of the operator's apiKey command, so this public
// repository names no pool, client or user.
type Config struct {
	Region                  string
	ClientID                string
	Username                string
	ServiceAccountTokenFile string
	// Endpoint replaces the Cognito endpoint the SDK derives from Region: a test's fake Cognito, or
	// an operator's override.
	Endpoint string
}

// Token signs in and returns the access token, or the reason it could not and no token. The command
// calls it once in a fresh process, so it clears SSL_CERT_FILE and SSL_CERT_DIR before that process
// first loads the system TLS roots.
func Token(ctx context.Context, cfg Config) (string, error) {
	answer, err := config.ReadSecretPointer("--service-account-token-file", cfg.ServiceAccountTokenFile)
	if err != nil {
		return "", err
	}
	client, err := httpClient()
	if err != nil {
		return "", err
	}
	return signIn(ctx, cfg, client, answer)
}

// httpClient is the sign-in's only route to Cognito: dialled directly, following no redirect, and
// reading no more than maxResponseBytes. SSL_CERT_FILE and SSL_CERT_DIR are cleared before the
// default transport first loads the system certificate store.
func httpClient() (*http.Client, error) {
	for _, variable := range []string{"SSL_CERT_FILE", "SSL_CERT_DIR"} {
		if err := os.Unsetenv(variable); err != nil {
			return nil, err
		}
	}
	return &http.Client{
		Transport: cappedTransport{&http.Transport{
			Proxy: nil, // never HTTPS_PROXY or HTTP_PROXY
		}},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}, nil
}

// cappedTransport hands back each response with its body capped at maxResponseBytes.
type cappedTransport struct{ http.RoundTripper }

func (c cappedTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	response, err := c.RoundTripper.RoundTrip(request)
	if err == nil {
		response.Body = struct {
			io.Reader
			io.Closer
		}{
			Reader: io.LimitReader(response.Body, maxResponseBytes),
			Closer: response.Body,
		}
	}
	return response, err
}

// signIn is Cognito's custom authentication: InitiateAuth names the user, and the custom challenge is
// answered with the pod's service-account token, which Cognito's verify trigger checks.
func signIn(ctx context.Context, cfg Config, client *http.Client, answer string) (string, error) {
	awsConfig := aws.Config{Region: cfg.Region, HTTPClient: client}
	if cfg.Endpoint != "" {
		awsConfig.BaseEndpoint = aws.String(cfg.Endpoint)
	}
	cognito := cognitoidentityprovider.NewFromConfig(awsConfig)
	started, err := cognito.InitiateAuth(ctx, &cognitoidentityprovider.InitiateAuthInput{
		AuthFlow:       types.AuthFlowTypeCustomAuth,
		ClientId:       aws.String(cfg.ClientID),
		AuthParameters: map[string]string{"USERNAME": cfg.Username},
	})
	if err != nil {
		return "", cognitoError("InitiateAuth", err)
	}
	if started.ChallengeName != types.ChallengeNameTypeCustomChallenge {
		return "", fmt.Errorf("Cognito InitiateAuth answered %s, not CUSTOM_CHALLENGE", challenge(started.ChallengeName))
	}
	answered, err := cognito.RespondToAuthChallenge(ctx, &cognitoidentityprovider.RespondToAuthChallengeInput{
		ChallengeName:      types.ChallengeNameTypeCustomChallenge,
		ClientId:           aws.String(cfg.ClientID),
		Session:            started.Session,
		ChallengeResponses: map[string]string{"USERNAME": cfg.Username, "ANSWER": answer},
	})
	if err != nil {
		return "", cognitoError("RespondToAuthChallenge", err)
	}
	token := ""
	if result := answered.AuthenticationResult; result != nil {
		token = aws.ToString(result.AccessToken)
	}
	if token == "" || strings.ContainsFunc(token, func(r rune) bool { return r <= ' ' || r > '~' }) {
		return "", errors.New("Cognito answered the custom challenge without an access token of one line of printable characters")
	}
	return token, nil
}

// challenge names the challenge InitiateAuth answered, when Cognito defines one by that name.
func challenge(name types.ChallengeNameType) string {
	switch {
	case name == "":
		return "no challenge"
	case slices.Contains(name.Values(), name):
		return "challenge " + string(name)
	default:
		return "a challenge Cognito does not define"
	}
}

// cognitoError reports a failed Cognito call in words this package chooses: the error code of a
// refusal when it has the shape of one, the HTTP status of an answer that is not Cognito's, or a
// dial failure to the operator's configured endpoint. The endpoint's message, request id, body and
// certificate never appear, since a hostile endpoint could echo the service-account token into any
// of them.
func cognitoError(operation string, err error) error {
	var refusal smithy.APIError
	var certificate *tls.CertificateVerificationError
	var response *smithyhttp.ResponseError
	var dial *net.OpError
	switch {
	case errors.As(err, &refusal):
		if code := refusal.ErrorCode(); isErrorCode(code) {
			return fmt.Errorf("Cognito %s refused: %s", operation, code)
		}
		return fmt.Errorf("Cognito %s refused with an error code that is not one", operation)
	case errors.As(err, &certificate):
		return fmt.Errorf("Cognito %s: the endpoint's TLS certificate is not trusted", operation)
	case errors.As(err, &response) && response.Response != nil && response.Response.Response != nil && response.HTTPStatusCode() != 0:
		return fmt.Errorf("Cognito %s answered HTTP %d, which is not a Cognito answer", operation, response.HTTPStatusCode())
	case errors.Is(err, context.DeadlineExceeded):
		return fmt.Errorf("Cognito %s: no answer before the deadline", operation)
	case errors.Is(err, context.Canceled):
		return fmt.Errorf("Cognito %s: sign-in was canceled", operation)
	case errors.As(err, &dial) && dial.Op == "dial":
		return fmt.Errorf("Cognito %s: %w", operation, dial)
	default:
		// The endpoint may have answered with bytes net/http could not parse, so use fixed wording.
		return fmt.Errorf("Cognito %s: the endpoint's answer is not an HTTP response", operation)
	}
}

// isErrorCode is whether code has the shape of a Cognito error code, such as
// NotAuthorizedException: letters and digits, at most 64.
func isErrorCode(code string) bool {
	return code != "" && len(code) <= 64 && !strings.ContainsFunc(code, func(r rune) bool {
		return !('a' <= r && r <= 'z' || 'A' <= r && r <= 'Z' || '0' <= r && r <= '9')
	})
}
