// Command broker is the AGENTC-833 secrets broker: it enrolls agent sessions and pods, decides
// their secret requests by policy or an approver's Dispatch login over a signed credential-request
// record, and releases granted values. AGENTC-393 v9: the broker holds no Dispatch credential —
// Dispatch's server calls the broker's UI routes with the deciding human's login, and the broker
// never opens a Dispatch ask.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"

	"github.com/sjawhar/envoy/internal/broker/api"
	"github.com/sjawhar/envoy/internal/broker/config"
	"github.com/sjawhar/envoy/internal/broker/enroll"
	"github.com/sjawhar/envoy/internal/broker/machine"
	"github.com/sjawhar/envoy/internal/broker/proof"
	"github.com/sjawhar/envoy/internal/broker/requests"
	"github.com/sjawhar/envoy/internal/broker/rules"
	"github.com/sjawhar/envoy/internal/broker/secrets"
	"github.com/sjawhar/envoy/internal/broker/store"
	"github.com/sjawhar/envoy/internal/broker/wake"
	"github.com/sjawhar/envoy/internal/oidc"
)

// machineLoginPendingTTL bounds how long a typed-code machine login waits for a human to decide
// it before the Sweeper expires it. Not a BROKER_* config knob (AGENTC-393 v9's Configuration
// deltas name none for it): 15 minutes comfortably covers the "look at the terminal, type the
// code" UX the confirmation-code flow is built around.
const machineLoginPendingTTL = 15 * time.Minute

// agentSecretPendingTTL bounds how long an agent_secret request waits for approval before the
// Sweeper expires it. Unchanged from the pre-v9 poller's own hardcoded value.
const agentSecretPendingTTL = 12 * time.Hour

func main() {
	// The broker takes no flags; parsing refuses any flag given rather than ignoring it.
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg, err := config.Load(os.Getenv)
	fatal(err)
	fatal(refusePortZeroPublicURLInProduction(cfg.PublicURL, cfg.RulesS3URI))
	st, err := store.Open(ctx, cfg.DatabaseURL)
	fatal(err)
	fatal(st.Migrate(ctx))
	var loader rules.Loader = rules.FileLoader{Path: cfg.RulesFile}
	var awsCfg aws.Config
	if cfg.RulesS3URI != "" {
		awsCfg, err = awsconfig.LoadDefaultConfig(ctx)
		fatal(err)
		bucket, key, err := rules.ParseS3URI(cfg.RulesS3URI)
		fatal(err)
		loader = rules.S3Loader{Client: s3.NewFromConfig(awsCfg), Bucket: bucket, Key: key}
	}
	var reader secrets.Reader = secrets.Fake{}
	if cfg.RulesS3URI != "" {
		reader = secrets.AWS{Client: secretsmanager.NewFromConfig(awsCfg)}
	} else if path := os.Getenv("BROKER_FAKE_SECRETS_FILE"); path != "" { // local development only
		reader, err = secrets.FakeFromFile(path)
		fatal(err)
	} else {
		fatal(errors.New("BROKER_RULES_FILE needs BROKER_FAKE_SECRETS_FILE for local runs; production uses BROKER_RULES_S3_URI and Secrets Manager"))
	}
	var pod enroll.PodVerifier
	// Discovery is bounded: an issuer that accepts the connection and never answers refuses the
	// boot instead of hanging it with no health endpoint serving.
	verifier, err := oidc.Discover(ctx, cfg.K8sOIDCIssuer, cfg.K8sOIDCAudience, oidc.DiscoveryTimeout)
	fatal(err)
	if verifier != nil {
		pod = enroll.K8sPodVerifier{Verifier: verifier}
	}

	current, err := rules.NewCurrent(ctx, loader, time.Duration(cfg.RulesReloadSeconds)*time.Second,
		func(e error) { slog.Error("rules reload refused; previous rules kept", "error", e) })
	fatal(err)

	// AGENTC-833: bind now, synchronously, right after every guard that can still refuse to
	// boot has already run (config, the port-0 public-URL guard, migrations, the first rules
	// load) — the only way any caller, dev-broker.sh included, can learn which process holds an
	// address is the log line
	// below, printed only once this exact Listen call has already succeeded. With a shared fixed
	// dev port, a losing instance's own readiness curl could see a different, already-running
	// instance's healthz answer and report "ready" pointing at the wrong broker; splitting
	// Listen from Serve and logging only after a real bind closes that regardless of how the
	// address is chosen, and dev-broker.sh's BROKER_LISTEN_ADDR=127.0.0.1:0 makes a same-port
	// collision between two dev instances impossible in the first place — the kernel never hands
	// out an address a live listener already holds.
	listener, err := net.Listen("tcp", cfg.ListenAddr)
	fatal(err)
	// A configured BROKER_PUBLIC_URL with port 0 is never itself dialable, so it can only be
	// dev-broker.sh's own "derive my public URL from whatever address I actually bind"
	// convention (its BROKER_PUBLIC_URL mirrors BROKER_LISTEN_ADDR=127.0.0.1:0): resolve it from
	// the real bound address before anything checks a request's audience against it. Every
	// audience-consuming construct below (enr.Chain included) is built after this point, so none
	// ever sees the stale placeholder. refusePortZeroPublicURLInProduction (the dev-vs-production
	// gate) already refused this above, right after config.Load — before st.Migrate, any S3 read,
	// and this Listen call — so reaching here means it's safe to apply.
	if u, urlErr := url.Parse(cfg.PublicURL); urlErr == nil && u.Port() == "0" {
		cfg.PublicURL = "http://" + listener.Addr().String()
	}
	slog.Info("broker listening", "addr", listener.Addr().String())

	enr := &enroll.Service{Store: st, Lease: time.Duration(cfg.LeaseSeconds) * time.Second, Pod: pod}
	enr.Chain = enroll.NewChainVerifier(st, cfg.PublicURL, time.Duration(cfg.ProofSkewSeconds)*time.Second)
	reqMachine := &requests.Machine{
		Store: st, Rules: current, Secrets: reader,
		MaxGrant: time.Duration(cfg.MaxGrantSeconds) * time.Second, PendingTTL: agentSecretPendingTTL,
		Audience: cfg.PublicURL, Skew: time.Duration(cfg.ProofSkewSeconds) * time.Second, Replay: enr.Replay,
	}
	reqMachine.Chain = requests.NewChainVerifier(st, cfg.PublicURL, time.Duration(cfg.ProofSkewSeconds)*time.Second)
	mach := &machine.Service{
		Store: st, Enroll: enr, Rules: current,
		Audience: cfg.PublicURL, Skew: time.Duration(cfg.ProofSkewSeconds) * time.Second,
		PendingTTL: machineLoginPendingTTL, CredentialLifetime: time.Duration(cfg.LauncherCredentialSeconds) * time.Second,
		Replay: enr.Replay,
	}

	var waker func(context.Context, string, string, string)
	if cfg.EnvoyURL != "" {
		w := wake.Envoy{URL: cfg.EnvoyURL, Token: cfg.EnvoyToken, HTTP: &http.Client{Timeout: 10 * time.Second}}
		waker = func(ctx context.Context, enrollmentID, requestID, state string) {
			sid, _ := reqMachine.SessionID(ctx, requestID) // requests.session_id, else the enrollment's
			if sid == "" {
				sid, _ = enr.SessionID(ctx, enrollmentID)
			}
			if sid != "" {
				w.Notify(ctx, sid, requestID, state)
			}
		}
	}
	sweeper := &requests.Sweeper{
		Machine: reqMachine, MachineLogins: mach,
		Interval: time.Duration(cfg.SweepSeconds) * time.Second, Wake: waker,
	}
	go sweeper.Run(ctx)

	mux := http.NewServeMux()
	api.Register(mux, api.Deps{PublicURL: cfg.PublicURL, UIToken: cfg.UIToken,
		Enroll: enr, Machine: reqMachine, MachineLogin: mach,
		Proof:              &proof.Verifier{Skew: time.Duration(cfg.ProofSkewSeconds) * time.Second, Lookup: enr.Lookup, LookupLauncher: enr.AuthenticateLauncher, Replay: enr.Replay},
		TrustedProxyHeader: cfg.TrustedProxyHeader})
	srv := &http.Server{
		Handler:           withRequestDeadline(mux, requestDeadline),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      requestDeadline + 15*time.Second,
		IdleTimeout:       2 * time.Minute,
	}
	go func() {
		if err := srv.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("broker: serve", "error", err)
			stop()
		}
	}()

	<-ctx.Done()
	slog.Info("broker shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		slog.Warn("broker: shutdown", "error", err)
	}
}

// requestDeadline bounds every request's own context, so no handler outlives its caller by more
// than a Dispatch call's worth of time; WriteTimeout sits past it so a handler that honours its
// context always gets to write its answer.
const requestDeadline = 45 * time.Second

// shutdownTimeout is how long SIGTERM waits for in-flight requests before the process exits.
const shutdownTimeout = 10 * time.Second

func withRequestDeadline(next http.Handler, deadline time.Duration) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), deadline)
		defer cancel()
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func fatal(err error) {
	if err != nil {
		slog.Error("broker: fatal", "error", err)
		os.Exit(1)
	}
}

// refusePortZeroPublicURLInProduction refuses a BROKER_PUBLIC_URL whose port is literally "0"
// whenever BROKER_RULES_S3_URI names a production rules source: port 0 is never dialable, so it
// can only be dev-broker.sh's own
// "derive my public URL from whatever address I actually bind" convention (BROKER_PUBLIC_URL
// mirrors BROKER_LISTEN_ADDR=127.0.0.1:0). A stray literal ":0" reaching a real deployment must
// fail loudly at boot, never silently reinterpret the broker's own public identity as its
// internal bind address. Extracted from main so a test can drive it directly instead of through
// fatal, which calls os.Exit.
func refusePortZeroPublicURLInProduction(publicURL, rulesS3URI string) error {
	u, err := url.Parse(publicURL)
	if err != nil || u.Port() != "0" || rulesS3URI == "" {
		return nil
	}
	return fmt.Errorf("BROKER_PUBLIC_URL %q: port 0 is never dialable in production (BROKER_RULES_S3_URI is set); only a local run may use it as dev-broker.sh's derive-from-bind convention", publicURL)
}
