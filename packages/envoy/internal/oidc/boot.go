package oidc

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// DiscoveryTimeout bounds an issuer's discovery read at boot. Discovery is a
// network read taken before either binary binds its port, so an issuer that
// accepts the connection and never answers has to refuse the boot rather than
// hang it: a hung boot serves no health endpoint and reports nothing.
const DiscoveryTimeout = 30 * time.Second

// ConfigFromEnv reads the both-or-neither issuer/audience pair named by
// issuerVar and audienceVar. Half a pair is a refusal naming the variable that
// is missing; neither set returns two empty strings and no error, which is
// "run no verifier". It reads getenv and returns errors and nothing else, so a
// binary can call it from wherever it validates the rest of its configuration
// — the discovery read is Discover's job, never this one's.
func ConfigFromEnv(getenv func(string) string, issuerVar, audienceVar string) (issuer, audience string, err error) {
	issuer = strings.TrimSpace(getenv(issuerVar))
	audience = strings.TrimSpace(getenv(audienceVar))
	switch {
	case issuer == "" && audience == "":
		return "", "", nil
	case issuer == "":
		return "", "", fmt.Errorf("%s is required when %s is set", issuerVar, audienceVar)
	case audience == "":
		return "", "", fmt.Errorf("%s is required when %s is set", audienceVar, issuerVar)
	}
	return issuer, audience, nil
}

// Discover is New under a deadline, and (nil, nil) for an unconfigured pair.
// The deadline bounds the discovery read only: the key set refreshes on its own
// background context, so a boot deadline never disables key rotation.
func Discover(ctx context.Context, issuer, audience string, timeout time.Duration) (*Verifier, error) {
	if issuer == "" {
		return nil, nil
	}
	discovery, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return New(discovery, issuer, audience)
}
