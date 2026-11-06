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

	"github.com/google/uuid"
	"github.com/sjawhar/envoy/brokerclient"
)

// controllerDispatchTradeWait bounds the one trade this start makes to prove the controller can
// hold a Dispatch credential at all. A controller that cannot write to Dispatch cannot do its
// work, so a broker that is down refuses the start rather than letting Oh My Pi come up with no
// credential.
const controllerDispatchTradeWait = 60 * time.Second

// controllerCheckSession is the session name this start's own trade writes under: a name of its
// own, new on every start, as the controller session's own id is. It is never a name a session
// writes under, so the claim it takes at the broker takes nothing from the controller, and it is
// new each time so that two machines running this start against one broker never collide on it
// (a session name belongs to one principal).
func controllerCheckSession() string { return "legion-controller-start-" + uuid.NewString() }

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
// machine's identity — no entry listing it, an entry that fixes a session, an entry whose pods
// write under registered names — refuses the start, in the broker's own words where they are
// the broker's, rather than leaving a controller to find at its first write that it can write
// nothing. That trade's token is read by nobody, is never written down, and the key it is bound
// to lives and dies with this call; the session's own credential is its own trade.
//
// It trades under a session name of this start's own (controllerCheckSession) and never under
// AGENT_SECRETS_SESSION, because that is what the session does: Oh My Pi mints the controller's
// session id when the session starts, so nothing here can name it, and the client trades under
// the session it acts as. A name of this start's own is refused by
// exactly the entries that refuse the session's — one that fixes a session answers
// ErrSessionMismatch to both, and one whose pods write under registered names answers both that
// no registration names them — and taken by the one entry that takes the session's, which fixes
// none and registers nothing. Trading under the operator's variable instead would pass for an
// entry that fixes that very name and launch a controller every Dispatch write of which is
// refused.
//
// The trade declines the launcher credential this machine's identity holds
// (brokerclient.Config.NoLauncherCredential): this check enrols no session and reads no secret,
// and its key dies with the call, so a credential minted for it would be a launcher_credentials
// row and a token_login audit row on the operator's machine-logins list for every start, each
// keyed by a thumbprint nothing holds afterwards. The controller's own session trades for
// itself, and that trade asks for what it needs.
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
	// A name of this start's own, new each time: nothing here can name the controller session's.
	client, err := brokerclient.New(brokerclient.Config{
		BrokerURL: broker, Key: key, Subject: identity, Session: controllerCheckSession(),
		NoLauncherCredential: true, HTTP: brokerclient.HTTPClient(),
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
	fmt.Fprintf(stderr, "[legion] the controller's Dispatch credential is this machine's own: %s trades %s under a session name of its own for a token bound to the session's key, which the session mints and renews itself under its own session id (this check's own trade expires %s); no token is written down and no bearer is handed to Oh My Pi\n",
		broker, identity.Describe(), token.Expiry.UTC().Format(time.RFC3339))
	return nil
}
