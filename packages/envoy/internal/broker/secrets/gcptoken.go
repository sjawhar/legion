package secrets

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	iamcredentials "google.golang.org/api/iamcredentials/v1"
	"google.golang.org/api/option"
)

// cloudPlatformScope is the one scope a minted token carries: everything the service account's
// own IAM grants allow.
const cloudPlatformScope = "https://www.googleapis.com/auth/cloud-platform"

// NewGCPTokenMinter opens the IAM Credentials API under Application Default Credentials (on GKE,
// Workload Identity for the broker's ServiceAccount) and answers the function that mints a
// minted secret's value: an access token for a service account, lasting lifetime, which the
// broker's identity may mint only where it holds roles/iam.serviceAccountTokenCreator on that
// account. The client's logger is off, as NewGCPClient's is, since at debug it prints response
// bodies, a token among them.
func NewGCPTokenMinter(ctx context.Context, opts ...option.ClientOption) (func(context.Context, string, time.Duration) (string, error), error) {
	svc, err := iamcredentials.NewService(ctx, append([]option.ClientOption{option.WithLogger(slog.New(slog.DiscardHandler))}, opts...)...)
	if err != nil {
		return nil, fmt.Errorf("open IAM Credentials: %w", err)
	}
	return func(ctx context.Context, serviceAccount string, lifetime time.Duration) (string, error) {
		seconds := int(lifetime.Seconds())
		if seconds < 1 {
			return "", fmt.Errorf("mint a token for %s: no lifetime left", serviceAccount)
		}
		resp, err := svc.Projects.ServiceAccounts.GenerateAccessToken("projects/-/serviceAccounts/"+serviceAccount,
			&iamcredentials.GenerateAccessTokenRequest{Scope: []string{cloudPlatformScope}, Lifetime: fmt.Sprintf("%ds", seconds)}).
			Context(ctx).Do()
		if err != nil {
			return "", fmt.Errorf("mint a token for %s: %w", serviceAccount, err)
		}
		return resp.AccessToken, nil
	}, nil
}
