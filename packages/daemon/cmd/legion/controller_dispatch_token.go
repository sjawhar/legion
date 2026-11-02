package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/sjawhar/envoy/brokerclient"
)

// controllerDispatchTradeWait bounds the one trade this start makes to prove the controller can
// hold a Dispatch credential at all. A controller that cannot write to Dispatch cannot do its
// work, so a broker that is down refuses the start rather than letting Oh My Pi come up with no
// credential.
const controllerDispatchTradeWait = 60 * time.Second

// controllerDispatchBroker says whether the controller's Dispatch credential is the secrets
// broker's — this machine's own identity traded for one of the broker's access tokens — rather
// than a bearer the configuration points at, and refuses the start where that trade does not
// work.
//
// With AGENT_SECRETS_MACHINE_IDENTITY unset it answers false and does nothing: the controller
// reads the `dispatch_token_file` its configuration names, as it always has. With it set, the
// controller's own session holds the credential and this process holds none: the Envoy client
// makes a key of its own, trades this machine's identity at AGENT_SECRETS_URL inside the running
// session and presents the minted token as `Authorization: DPoP <token>` with a proof signed per
// request (`brokerCredential`, packages/envoy-client/src/broker-credential.ts), renewing it
// there. Nobody copies a person's Dispatch token onto a machine, which is the whole of why this
// grant exists.
//
// Nothing of that credential is written to a file and no DISPATCH_TOKEN_FILE points at one.
// Such a token is bound to the key its holder proves: Dispatch refuses it presented as a bearer
// (RFC 9449 Section 7.2, `brokertoken.Presented`), and the client resolves every bearer —
// DISPATCH_TOKEN_FILE, then DISPATCH_TOKEN, then the token an envoy.json holds — before it
// consults the broker at all. A pointer to such a token is therefore a pointer the session
// cannot spend, and handing it one shuts the broker route off; controllerEnvironment hands it
// none, and controllerStart drops an inherited one.
//
// What this start does do is trade once itself, before the daemon mints a capability: a broker
// that refuses this machine's identity — no entry listing it, an entry that fixes no session
// while nothing names one — refuses the start, in the broker's own words, rather than leaving a
// controller to find at its first write that it can write nothing. That trade's token is read by
// nobody, is never written down, and the key it is bound to lives and dies with this call; the
// session's own credential is its own trade.
func controllerDispatchBroker(ctx context.Context, stderr io.Writer) (bool, error) {
	identity, err := brokerclient.MachineIdentity(os.Getenv)
	if err != nil || identity == nil {
		return false, err
	}
	broker := os.Getenv(brokerclient.BrokerURLVar)
	if broker == "" {
		return false, fmt.Errorf("%s names the identity this machine proves, so %s must name the secrets broker it proves it to",
			brokerclient.IdentityVar, brokerclient.BrokerURLVar)
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return false, fmt.Errorf("make the key the controller's trade is proved with: %w", err)
	}
	client, err := brokerclient.New(brokerclient.Config{
		BrokerURL: broker, Key: key, Subject: identity, Session: os.Getenv(brokerclient.SessionVar),
		HTTP: brokerclient.HTTPClient(),
	})
	if err != nil {
		return false, err
	}
	timed, cancel := context.WithTimeout(ctx, controllerDispatchTradeWait)
	defer cancel()
	token, err := client.Token(timed)
	if err != nil {
		return false, fmt.Errorf("the controller's Dispatch credential cannot be minted at %s: %w", broker, err)
	}
	fmt.Fprintf(stderr, "[legion] the controller's Dispatch credential is this machine's own: %s trades %s for a token bound to the session's key, which the session mints and renews itself (this check's own trade expires %s); no token is written down and no bearer is handed to Oh My Pi\n",
		broker, identity.Describe(), token.Expiry.UTC().Format(time.RFC3339))
	return true, nil
}
