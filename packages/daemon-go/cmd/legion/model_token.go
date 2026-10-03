package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/sjawhar/legion/daemon/internal/modeltoken"
)

// modelTokenTimeout bounds one sign-in: Oh My Pi waits on the apiKey command before a model call.
const modelTokenTimeout = 30 * time.Second

// runModelToken is `legion model-token`, the worker pod's model apiKey command
// (internal/modeltoken). Every setting is a flag the operator's models.yml passes, so this public
// repository names no Cognito pool, client or user. It prints the access token alone, or exits 1
// with the reason and prints nothing a caller could mistake for a token. A token that signed in
// but could not be cached is printed, and the cache failure goes to stderr.
func runModelToken(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := newFlags("model-token", stderr)
	var cfg modeltoken.Config
	flags.StringVar(&cfg.Region, "region", "", "the Cognito user pool's region")
	flags.StringVar(&cfg.ClientID, "client-id", "", "the Cognito app client the machine user signs in on")
	flags.StringVar(&cfg.Username, "username", "", "the Cognito machine user")
	flags.StringVar(&cfg.ServiceAccountTokenFile, "service-account-token-file", "", "the pod's projected service-account token, the custom challenge's answer")
	flags.StringVar(&cfg.CacheFile, "cache-file", "", "where the access token is cached, on the pod's memory-backed state volume")
	flags.StringVar(&cfg.Endpoint, "endpoint", "", "a Cognito endpoint in place of the region's own")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() > 0 {
		fmt.Fprintf(stderr, "legion model-token: unexpected argument %q\n", flags.Arg(0))
		return 2
	}
	signingIn, cancel := context.WithTimeout(ctx, modelTokenTimeout)
	defer cancel()
	token, err := modeltoken.Token(signingIn, cfg)
	var cacheErr *modeltoken.CacheError
	switch {
	case errors.As(err, &cacheErr):
		fmt.Fprintf(stderr, "legion model-token: %v\n", err)
	case err != nil:
		fmt.Fprintf(stderr, "legion model-token: %v\n", err)
		return 1
	}
	fmt.Fprintln(stdout, token)
	return 0
}
