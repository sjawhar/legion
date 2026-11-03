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

// controllerDispatchBroker proves the Dispatch credential `dispatch_token_broker` names — this
// machine's own identity, traded at the secrets broker for one of its access tokens — and refuses
// the start where that trade does not work.
//
// The controller's own session holds the credential and this process holds none: the Envoy client
// makes a key of its own, trades this machine's identity at AGENT_SECRETS_URL inside the running
// session and presents the minted token as `Authorization: DPoP <token>` with a proof signed per
// request (`brokerCredential`, packages/envoy-client/src/broker-credential.ts), renewing it
// there. Nobody copies a person's Dispatch token onto a machine, which is the whole of why this
// grant exists. A configuration that names `dispatch_token_file` instead takes neither this trade
// nor this route: the controller reads that bearer, as it always has.
//
// Nothing of that credential is written to a file and no DISPATCH_TOKEN_FILE points at one.
// Such a token is bound to the key its holder proves: Dispatch refuses it presented as a bearer
// (RFC 9449 Section 7.2, `brokertoken.Presented`), so a pointer to it is a pointer the session
// cannot spend. controllerEnvironment hands the session DISPATCH_TOKEN_BROKER=1 instead, under
// which its client reads no bearer from any source — not DISPATCH_TOKEN_FILE, not DISPATCH_TOKEN,
// not the token the operator's envoy.json holds — and turns Dispatch off with an error when it
// can trade none; controllerStart also drops an inherited DISPATCH_TOKEN and DISPATCH_TOKEN_FILE.
//
// What this start does do is trade once itself, before the daemon mints a capability: a machine
// that proves no identity, a deployment that names no broker, or a broker that refuses this
// machine's identity — no entry listing it, an entry that fixes no session while nothing names
// one — refuses the start, in the broker's own words where they are the broker's, rather than
// leaving a controller to find at its first write that it can write nothing. That trade's token
// is read by nobody, is never written down, and the key it is bound to lives and dies with this
// call; the session's own credential is its own trade.
func controllerDispatchBroker(ctx context.Context, stderr io.Writer) error {
	identity, err := brokerclient.MachineIdentity(os.Getenv)
	if err != nil {
		return err
	}
	if identity == nil {
		return fmt.Errorf("dispatch_token_broker is set, so the controller's Dispatch credential is this machine's own identity, and %s names none",
			brokerclient.IdentityVar)
	}
	broker := os.Getenv(brokerclient.BrokerURLVar)
	if broker == "" {
		return fmt.Errorf("%s names the identity this machine proves, so %s must name the secrets broker it proves it to",
			brokerclient.IdentityVar, brokerclient.BrokerURLVar)
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return fmt.Errorf("make the key the controller's trade is proved with: %w", err)
	}
	client, err := brokerclient.New(brokerclient.Config{
		BrokerURL: broker, Key: key, Subject: identity, Session: os.Getenv(brokerclient.SessionVar),
		HTTP: brokerclient.HTTPClient(),
	})
	if err != nil {
		return err
	}
	timed, cancel := context.WithTimeout(ctx, controllerDispatchTradeWait)
	defer cancel()
	token, err := client.Token(timed)
	if err != nil {
		return fmt.Errorf("the controller's Dispatch credential cannot be minted at %s: %w", broker, err)
	}
	fmt.Fprintf(stderr, "[legion] the controller's Dispatch credential is this machine's own: %s trades %s for a token bound to the session's key, which the session mints and renews itself (this check's own trade expires %s); no token is written down and no bearer is handed to Oh My Pi\n",
		broker, identity.Describe(), token.Expiry.UTC().Format(time.RFC3339))
	return nil
}
