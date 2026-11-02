package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/sjawhar/envoy/brokerclient"

	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// controllerDispatchTokenWait bounds how long the start waits for the first Dispatch token the
// broker mints. A controller with no Dispatch token cannot do its work, so a broker that is down
// refuses the start rather than letting Oh My Pi come up with no credential.
const controllerDispatchTokenWait = 60 * time.Second

// controllerStartDispatchToken mints the controller's own Dispatch token, where this machine
// proves an identity of its own, and keeps it fresh while Oh My Pi runs.
//
// With AGENT_SECRETS_MACHINE_IDENTITY unset this does nothing and answers an empty path: the
// controller reads the `dispatch_token_file` its configuration names, as it always has. With it
// set, the machine's identity is traded at the secrets broker (AGENT_SECRETS_URL) for one of the
// broker's access tokens, written 0600 under the state directory as every other credential of
// the controller's is, and the environment's DISPATCH_TOKEN_FILE points at that file instead —
// so nobody copies a person's Dispatch token onto a machine, which is the whole of why this
// grant exists. The token is bound to a key this process makes, whose public half alone reaches
// the broker and which lives as long as this process does.
//
// The trade issues no refresh token, so the renewal is a fresh identity token traded again,
// which internal/brokerclient's own loop does before each token expires; every renewal rewrites
// the same file, and nothing of the controller restarts. The loop writes nothing to the terminal
// Oh My Pi owns: it appends one line per renewal and per failure to
// `<state_dir>/dispatch-token.log`, opened append-only 0600 before Oh My Pi starts, and never
// logs a token. Its lifetime is Oh My Pi's, as the GitHub refresh loop's is, and the stop it
// answers waits for it, so the process never exits while a write is in flight.
func controllerStartDispatchToken(ctx context.Context, stateDir string, stderr io.Writer) (stop func(), tokenFile string, err error) {
	noop := func() {}
	identity, err := brokerclient.MachineIdentity(os.Getenv)
	if err != nil {
		return noop, "", err
	}
	if identity == nil {
		return noop, "", nil
	}
	broker := os.Getenv(brokerclient.BrokerURLVar)
	if broker == "" {
		return noop, "", fmt.Errorf("%s names the identity this machine proves, so %s must name the secrets broker it proves it to",
			brokerclient.IdentityVar, brokerclient.BrokerURLVar)
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return noop, "", fmt.Errorf("make the key the controller's Dispatch token is bound to: %w", err)
	}
	logPath := filepath.Join(stateDir, "dispatch-token.log")
	logFile, err := os.OpenFile(logPath, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return noop, "", fmt.Errorf("open %s: %w", logPath, err)
	}
	log := slog.New(slog.NewTextHandler(logFile, nil))
	client, err := brokerclient.New(brokerclient.Config{
		BrokerURL: broker, Key: key, Subject: identity, Session: os.Getenv(brokerclient.SessionVar),
		HTTP: brokerclient.HTTPClient(), Log: log,
	})
	if err != nil {
		logFile.Close()
		return noop, "", err
	}

	loopCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	done := make(chan struct{})
	first := make(chan error, 1)
	var path string
	go func() {
		defer close(done)
		defer logFile.Close()
		err := client.Run(loopCtx, func(_ context.Context, token *brokerclient.Token) error {
			written, err := runtime.WriteDispatchTokenFile(stateDir, token.AccessToken)
			if err != nil {
				return fmt.Errorf("write the controller's Dispatch token: %w", err)
			}
			path = written
			log.Info("controller: dispatch token minted", "file", written, "expires", token.Expiry.UTC().Format(time.RFC3339))
			select {
			case first <- nil:
			default:
			}
			return nil
		})
		if err != nil {
			log.Error("controller: the Dispatch token can no longer be minted", "error", err.Error())
		}
		select {
		case first <- err:
		default:
		}
	}()
	stop = func() {
		cancel()
		<-done
	}
	select {
	case err := <-first:
		if err != nil {
			stop()
			return noop, "", err
		}
	case <-time.After(controllerDispatchTokenWait):
		stop()
		return noop, "", fmt.Errorf("the secrets broker at %s did not mint the controller's Dispatch token within %s", broker, controllerDispatchTokenWait)
	case <-ctx.Done():
		stop()
		return noop, "", ctx.Err()
	}
	fmt.Fprintf(stderr, "[legion] the controller holds a Dispatch token of this machine's own from %s, written to %s; this process renews it before it expires (log: %s)\n",
		broker, path, logPath)
	return stop, path, nil
}
