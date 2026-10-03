package main

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/sjawhar/legion/daemon/internal/modeltoken"
)

// modelTokenTimeout bounds one sign-in, and stays under Oh My Pi's 10 s limit on an apiKey command.
const modelTokenTimeout = 8 * time.Second

// runModelToken is `legion model-token` (internal/modeltoken): it prints the access token alone on
// stdout, or exits 1 with the reason on stderr and nothing on stdout.
func runModelToken(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := newFlags("model-token", stderr)
	var cfg modeltoken.Config
	flags.StringVar(&cfg.Region, "region", "", "the Cognito user pool's region")
	flags.StringVar(&cfg.ClientID, "client-id", "", "the Cognito app client the machine user signs in on")
	flags.StringVar(&cfg.Username, "username", "", "the Cognito machine user")
	flags.StringVar(&cfg.ServiceAccountTokenFile, "service-account-token-file", "", "the pod's projected service-account token, the custom challenge's answer")
	flags.StringVar(&cfg.Endpoint, "endpoint", "", "a Cognito endpoint in place of the region's own: a test's fake Cognito, or an override")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() > 0 {
		fmt.Fprintf(stderr, "legion model-token: unexpected argument %q\n", flags.Arg(0))
		return 2
	}
	for _, name := range []string{"region", "client-id", "username", "service-account-token-file"} {
		if flags.Lookup(name).Value.String() == "" {
			fmt.Fprintf(stderr, "legion model-token: --%s is required\n", name)
			return 1
		}
	}
	signingIn, cancel := context.WithTimeout(ctx, modelTokenTimeout)
	defer cancel()
	token, err := modeltoken.Token(signingIn, cfg)
	if err != nil {
		fmt.Fprintf(stderr, "legion model-token: %v\n", err)
		return 1
	}
	fmt.Fprintln(stdout, token)
	return 0
}
