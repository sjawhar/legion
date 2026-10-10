package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/ghconfig"
)

// controllerGitHubRefreshInterval is how often startControllerGitHubRefresh re-fetches the
// controller's GitHub credential. The daemon's appLease holds the review App's lease and re-mints
// it as the lease nears its expiry, so most ticks render the hosts.yml the controller's directory
// already holds and write nothing; a minute keeps the time its gh runs on a token the daemon has
// already replaced short against an installation token's hour, as tmux's and sandbox's
// gitHubRefreshInterval do for the roles the daemon launches itself. A var, not a const, so a test
// can shorten it.
var controllerGitHubRefreshInterval = time.Minute

// controllerCredentialFailure is a non-2xx answer to POST
// /legion/v1/controller/github-credential whose body parsed as an api.Failure: its code lets the
// caller tell a daemon with no GitHub App to act as (GITHUB_TOKEN_SOURCE_UNAVAILABLE,
// GITHUB_OWNER_UNCONFIGURED) apart from a capability a later start replaced
// (INVALID_CONTROLLER_CAPABILITY) or a daemon that launches its own controller
// (CONTROLLER_LAUNCHED), without parsing the sentence itself.
type controllerCredentialFailure struct {
	code    string
	message string
}

func (f *controllerCredentialFailure) Error() string { return f.message }

// controllerCredentialSuperseded says whether err is a controllerCredentialFailure that ends the
// refresh loop rather than retries it: the capability a later `legion controller start` replaced
// (INVALID_CONTROLLER_CAPABILITY), or a daemon that now launches its own controller
// (CONTROLLER_LAUNCHED). Either means this process is no longer the daemon's controller, so its gh
// files are left to last until their token's expiry — last start wins.
func controllerCredentialSuperseded(err error) bool {
	var failure *controllerCredentialFailure
	return errors.As(err, &failure) && (failure.code == "INVALID_CONTROLLER_CAPABILITY" || failure.code == "CONTROLLER_LAUNCHED")
}

// fetchControllerCredential is `legion controller start`'s refresh call, POST
// /legion/v1/controller/github-credential: the controller capability secret proves which
// controller is asking, and the daemon answers the review App's token rendered as the two files
// gh reads (ghconfig.Rendered), its expiry parsed from the API's RFC 3339 nanosecond timestamp
// (time.RFC3339Nano, the format api.ControllerCredentialResponse's ExpiresAt is written in). A
// failed request names the daemon URL, as fetchControllerSecret's does, and never tries another
// address. A refusal is a *controllerCredentialFailure carrying the daemon's code when the body
// parses as one (api.Failure), quoting its sentence through refusal otherwise.
func fetchControllerCredential(ctx context.Context, daemonURL, secret string) (ghconfig.Rendered, error) {
	const route = "/legion/v1/controller/github-credential"
	status, body, err := operator{base: daemonURL}.do(ctx, http.MethodPost, route, api.ControllerCredentialRequest{Secret: secret})
	if err != nil {
		return ghconfig.Rendered{}, fmt.Errorf("could not reach the Legion daemon at %s: %v; is the port-forward running? (never falls back to another address)", daemonURL, err)
	}
	if status/100 != 2 {
		var failure api.Failure
		if json.Unmarshal(body, &failure) == nil && failure.Code != "" {
			return ghconfig.Rendered{}, &controllerCredentialFailure{
				code:    failure.Code,
				message: fmt.Sprintf("%s%s: %s", daemonURL, route, failure.Error),
			}
		}
		return ghconfig.Rendered{}, fmt.Errorf("%s%s: %s", daemonURL, route, refusal(status, body))
	}
	var answer api.ControllerCredentialResponse
	if err := json.Unmarshal(body, &answer); err != nil {
		return ghconfig.Rendered{}, fmt.Errorf("%s%s answered with a body that is not JSON", daemonURL, route)
	}
	expiresAt, err := time.Parse(time.RFC3339Nano, answer.ExpiresAt)
	if err != nil {
		return ghconfig.Rendered{}, fmt.Errorf("%s%s answered an expiry that is not RFC 3339: %w", daemonURL, route, err)
	}
	return ghconfig.Rendered{Hosts: answer.Hosts, Config: answer.Config, App: answer.App, ExpiresAt: expiresAt}, nil
}

// controllerStartGitHubCredential is the first fetch controllerStart makes after it writes the
// secret file: success writes the controller's gh files under ghDir and starts the refresh loop,
// logging to stateDir/github-credential.log; a daemon with no GitHub App for this project
// (GITHUB_TOKEN_SOURCE_UNAVAILABLE or GITHUB_OWNER_UNCONFIGURED) is not a refusal, since nothing
// is wrong — it just has nothing to act as — so an empty 0700 gh directory is enough and no loop
// runs; any other failure is this function's error, which controllerStart treats as every other
// post-mint failure (exit 1, Oh My Pi never started, the secret file left as it is). It answers a
// function that stops the loop — a no-op when none started — for controllerStart to defer.
func controllerStartGitHubCredential(ctx context.Context, daemonURL, secret, ghDir, stateDir string, stderr io.Writer) (func(), error) {
	noop := func() {}
	rendered, err := fetchControllerCredential(ctx, daemonURL, secret)
	if err != nil {
		var failure *controllerCredentialFailure
		if !errors.As(err, &failure) || (failure.code != "GITHUB_TOKEN_SOURCE_UNAVAILABLE" && failure.code != "GITHUB_OWNER_UNCONFIGURED") {
			return noop, err
		}
		if err := os.MkdirAll(ghDir, 0o700); err != nil {
			return noop, fmt.Errorf("create %s: %w", ghDir, err)
		}
		if err := os.Chmod(ghDir, 0o700); err != nil {
			return noop, fmt.Errorf("chmod %s: %w", ghDir, err)
		}
		fmt.Fprintln(stderr, "[legion] the daemon has no GitHub App to act as; the controller's gh acts as nobody")
		return noop, nil
	}
	if _, err := ghconfig.Write(ghDir, rendered); err != nil {
		return noop, fmt.Errorf("write the controller's gh files: %w", err)
	}
	fmt.Fprintf(stderr, "[legion] the controller's gh acts as the %s App from %s; this process refreshes the token before it expires (%s)\n",
		rendered.App, ghDir, rendered.ExpiresAt.Format(time.RFC3339))
	stop, err := startControllerGitHubRefresh(daemonURL, secret, ghDir, filepath.Join(stateDir, "github-credential.log"))
	if err != nil {
		return noop, err
	}
	return stop, nil
}

// startControllerGitHubRefresh starts the loop that keeps the controller's gh directory fresh
// while Oh My Pi runs: a goroutine that re-fetches the controller's GitHub credential every
// controllerGitHubRefreshInterval and rewrites dir (ghconfig.Write), which rewrites hosts.yml only
// when it differs. The loop writes nothing to stderr or stdout once Oh My Pi owns the terminal: it
// appends one slog text line per change, per failed fetch or write, and per stop to logPath,
// opened append-only 0600 before Oh My Pi starts, and never logs the token or the hosts text. A
// failed fetch or write keeps the last files and tries again at the next tick; a superseded
// capability (controllerCredentialSuperseded) ends the loop, since the file then lasts until its
// token's expiry. Its lifetime is Oh My Pi's, not the command's signal context: a Ctrl-C in the
// terminal reaches both processes, and Oh My Pi, which handles it as its own, keeps running, so a
// loop bound to the signal would leave the controller on a token no one refreshes. It answers a
// function that cancels the loop and waits for it to stop, which controllerStart runs once
// Oh My Pi has exited, so the process never exits while a write is in flight.
func startControllerGitHubRefresh(daemonURL, secret, dir, logPath string) (stop func(), err error) {
	logFile, err := os.OpenFile(logPath, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", logPath, err)
	}
	log := slog.New(slog.NewTextHandler(logFile, nil))
	loopCtx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer logFile.Close()
		tick := time.NewTicker(controllerGitHubRefreshInterval)
		defer tick.Stop()
		for {
			select {
			case <-loopCtx.Done():
				return
			case <-tick.C:
				if !refreshControllerGitHubCredential(loopCtx, daemonURL, secret, dir, log) {
					return
				}
			}
		}
	}()
	return func() {
		cancel()
		<-done
	}, nil
}

// refreshControllerGitHubCredential is one tick of startControllerGitHubRefresh's loop. It
// answers false when the loop must stop (a superseded capability), true otherwise.
func refreshControllerGitHubCredential(ctx context.Context, daemonURL, secret, dir string, log *slog.Logger) bool {
	rendered, err := fetchControllerCredential(ctx, daemonURL, secret)
	if err != nil {
		if controllerCredentialSuperseded(err) {
			log.Info("controller: github credential refresh stopped", "reason", err.Error())
			return false
		}
		log.Warn("controller: github credential refresh failed", "error", err.Error())
		return true
	}
	changed, err := ghconfig.Write(dir, rendered)
	if err != nil {
		log.Warn("controller: github credential refresh failed", "error", err.Error())
		return true
	}
	if changed {
		log.Info("controller: github credential refreshed", "app", rendered.App, "expiresAt", rendered.ExpiresAt)
	}
	return true
}
