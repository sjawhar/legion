// Command broker is the secrets broker: it enrolls agent sessions and pods, decides
// their secret requests by policy or an approver's Dispatch login over a signed credential-request
// record, and releases granted values. Its HTTP API is documented at
// https://sjawhar.github.io/legion/broker/reference/api/. The broker holds no Dispatch credential —
// Dispatch's server calls the broker's UI routes with the deciding human's login, and the broker
// never opens a Dispatch ask.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"slices"
	"syscall"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/kms"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"

	"github.com/sjawhar/envoy/internal/broker/api"
	"github.com/sjawhar/envoy/internal/broker/config"
	"github.com/sjawhar/envoy/internal/broker/enroll"
	"github.com/sjawhar/envoy/internal/broker/machine"
	"github.com/sjawhar/envoy/internal/broker/policy"
	"github.com/sjawhar/envoy/internal/broker/proof"
	"github.com/sjawhar/envoy/internal/broker/requests"
	"github.com/sjawhar/envoy/internal/broker/secrets"
	"github.com/sjawhar/envoy/internal/broker/store"
	"github.com/sjawhar/envoy/internal/broker/wake"
	"github.com/sjawhar/envoy/internal/oidc"
)

// machineLoginPendingTTL bounds how long a typed-code machine login waits for a human to decide
// it before the Sweeper expires it. Not a BROKER_* config knob (the shared broker contract's
// Configuration deltas name none for it): 15 minutes comfortably covers the "look at the
// terminal, type the code" UX the confirmation-code flow is built around.
const machineLoginPendingTTL = 15 * time.Minute

// agentSecretPendingTTL bounds how long an agent_secret request waits for approval before the
// Sweeper expires it. Unchanged from the pre-v9 poller's own hardcoded value.
const agentSecretPendingTTL = 12 * time.Hour

// policyRefresh is how often the broker rereads the namespace: every agent secret's owner and tier
// tags, its key, and whether it has a current value.
const policyRefresh = 5 * time.Minute

func main() {
	// The broker takes no flags; parsing refuses any flag given rather than ignoring it.
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg, err := config.Load(os.Getenv)
	fatal(err)
	// BROKER_FAKE_SECRETS_FILE: for local development only, a JSON file standing in for Secrets
	// Manager, {"secrets": [{"name", "kms_key_id", "tags", "value", "deleted_at"}]}: the broker lists
	// the agent secrets and reads their values from it instead of from AWS, reading the file again
	// on each of those calls, so an edit to it is a write as Secrets Manager would see one. A secret
	// with no "value" is one created without a value, which the broker refuses as no-current-value;
	// one with a "deleted_at" (RFC 3339) is scheduled for deletion, which the broker no longer lists.
	fakeSecrets := os.Getenv("BROKER_FAKE_SECRETS_FILE")
	fatal(refusePortZeroPublicURLInProduction(cfg.PublicURL, fakeSecrets))
	// An IAM-form BROKER_DATABASE_URL signs every connection in with an RDS IAM token, minted
	// from the AWS config, so that config is loaded before the database is opened, and the
	// Secrets Manager and KMS clients below reuse it.
	var awsCfg *aws.Config
	loadAWS := func() aws.Config {
		if awsCfg == nil {
			loaded, err := awsconfig.LoadDefaultConfig(ctx)
			fatal(err)
			awsCfg = &loaded
		}
		return *awsCfg
	}
	var storeOpts []store.Option
	if cfg.DatabaseIAM {
		mint, err := store.RDSAuthTokens(loadAWS())
		fatal(err)
		storeOpts = append(storeOpts, store.WithTokenMinter(mint))
	}
	st, err := store.Open(ctx, cfg.DatabaseURL, storeOpts...)
	fatal(err)
	fatal(st.Migrate(ctx))
	loader := policy.Loader{Prefix: cfg.SecretsPrefix, KeyARN: cfg.SecretsKMSKeyARN, Services: slices.Sorted(maps.Keys(cfg.ServiceAccounts))}
	var reader secrets.Reader
	if fakeSecrets != "" {
		local, err := secrets.LocalFromFile(fakeSecrets)
		fatal(err)
		loader.Secrets, loader.Describer, loader.Aliases, reader = local, local, local, secrets.AWS{Client: local}
	} else {
		sm := secretsmanager.NewFromConfig(loadAWS())
		loader.Secrets, loader.Describer, loader.Aliases, reader = sm, sm, kms.NewFromConfig(loadAWS()), secrets.AWS{Client: sm}
	}
	var pod enroll.PodVerifier
	// Discovery is bounded: an issuer that accepts the connection and never answers refuses the
	// boot instead of hanging it with no health endpoint serving.
	verifier, err := oidc.Discover(ctx, cfg.K8sOIDCIssuer, cfg.K8sOIDCAudience, oidc.DiscoveryTimeout)
	fatal(err)
	if verifier != nil {
		pod = enroll.K8sPodVerifier{Verifier: verifier}
	}

	current, err := policy.NewCurrent(ctx, loader, policyRefresh)
	fatal(err)

	// Bind now, synchronously, right after every guard that can still refuse to
	// boot has already run (config, the port-0 public-URL guard, migrations, the first policy
	// load) — the only way any caller, dev-broker.sh included, can learn which process holds an
	// address is the log line below, printed only once this exact Listen call has already
	// succeeded. With a shared fixed
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
	// gate) already refused this above, right after config.Load — before st.Migrate, any AWS
	// call, and this Listen call — so reaching here means it's safe to apply.
	if u, urlErr := url.Parse(cfg.PublicURL); urlErr == nil && u.Port() == "0" {
		cfg.PublicURL = "http://" + listener.Addr().String()
	}
	slog.Info("broker listening", "addr", listener.Addr().String())

	enr := &enroll.Service{Store: st, Lease: time.Duration(cfg.LeaseSeconds) * time.Second, Pod: pod}
	enr.Chain = enroll.NewChainVerifier(st, cfg.PublicURL, time.Duration(cfg.ProofSkewSeconds)*time.Second)
	reqMachine := newRequestMachine(cfg, st, current, reader, enr.Replay)
	mach := &machine.Service{
		Store: st, Enroll: enr, Policy: current,
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
		Enrollments: enr, Machine: reqMachine, MachineLogins: mach,
		Interval: time.Duration(cfg.SweepSeconds) * time.Second, Wake: waker,
	}
	go sweeper.Run(ctx)

	mux := http.NewServeMux()
	api.Register(mux, api.Deps{PublicURL: cfg.PublicURL, UIToken: cfg.UIToken,
		Enroll: enr, Machine: reqMachine, MachineLogin: mach,
		Proof:  &proof.Verifier{Skew: time.Duration(cfg.ProofSkewSeconds) * time.Second, Lookup: enr.Lookup, LookupLauncher: enr.AuthenticateLauncher, Replay: enr.Replay},
		Policy: current, SecretsPrefix: cfg.SecretsPrefix, SecretsKMSKeyARN: cfg.SecretsKMSKeyARN,
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
// unless BROKER_FAKE_SECRETS_FILE makes this a local run: port 0 is never dialable, so it can only
// be dev-broker.sh's own "derive my public URL from whatever address I actually bind" convention
// (BROKER_PUBLIC_URL mirrors BROKER_LISTEN_ADDR=127.0.0.1:0). A stray literal ":0" reaching a real
// deployment must fail loudly at boot, never silently reinterpret the broker's own public
// identity as its internal bind address. Extracted from main so a test can drive it directly
// instead of through fatal, which calls os.Exit.
func refusePortZeroPublicURLInProduction(publicURL, fakeSecretsFile string) error {
	u, err := url.Parse(publicURL)
	if err != nil || u.Port() != "0" || fakeSecretsFile != "" {
		return nil
	}
	return fmt.Errorf("BROKER_PUBLIC_URL %q: port 0 is never dialable in production (BROKER_FAKE_SECRETS_FILE is unset); only a local run may use it as dev-broker.sh's derive-from-bind convention", publicURL)
}

// newRequestMachine builds the requests.Machine the broker decides every secret request with, from
// its configuration: the grant lifetime, the audience and skew every request object is checked
// against, and the service accounts that prove a pod is a registered service's.
func newRequestMachine(cfg config.Config, st *store.Store, current *policy.Current, reader secrets.Reader, replay func(context.Context, string, time.Time) (bool, error)) *requests.Machine {
	m := &requests.Machine{
		Store: st, Policy: current, Secrets: reader,
		MaxGrant: time.Duration(cfg.MaxGrantSeconds) * time.Second, PendingTTL: agentSecretPendingTTL,
		Audience: cfg.PublicURL, Skew: time.Duration(cfg.ProofSkewSeconds) * time.Second, Replay: replay,
		ServiceAccounts: cfg.ServiceAccounts,
	}
	m.Chain = requests.NewChainVerifier(st, cfg.PublicURL, time.Duration(cfg.ProofSkewSeconds)*time.Second)
	return m
}
