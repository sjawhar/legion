// Command broker is the AGENTC-833 secrets broker: it enrolls agent sessions and pods, decides
// their secret requests by policy or through Dispatch asks, and releases granted values.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
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
	"github.com/sjawhar/envoy/internal/broker/dispatch"
	"github.com/sjawhar/envoy/internal/broker/enroll"
	"github.com/sjawhar/envoy/internal/broker/launcher"
	"github.com/sjawhar/envoy/internal/broker/proof"
	"github.com/sjawhar/envoy/internal/broker/requests"
	"github.com/sjawhar/envoy/internal/broker/rules"
	"github.com/sjawhar/envoy/internal/broker/secrets"
	"github.com/sjawhar/envoy/internal/broker/store"
	"github.com/sjawhar/envoy/internal/broker/wake"
	"github.com/sjawhar/envoy/internal/oidc"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	cfg, err := config.Load(os.Getenv)
	fatal(err)
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
	current, err := rules.NewCurrent(ctx, loader, time.Duration(cfg.RulesReloadSeconds)*time.Second, func(e error) { slog.Error("rules reload refused; previous rules kept", "error", e) })
	fatal(err)
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
	dc := dispatch.New(cfg.DispatchURL, cfg.DispatchToken, &http.Client{Timeout: 30 * time.Second})
	enr := &enroll.Service{Store: st, Lease: time.Duration(cfg.LeaseSeconds) * time.Second, Pod: pod}
	ls := &launcher.Service{Store: st, Dispatch: dc, Enroll: enr, Project: cfg.DispatchProject}
	machine := &requests.Machine{Store: st, Rules: current, Dispatch: dc, Secrets: reader, MaxGrant: time.Duration(cfg.MaxGrantSeconds) * time.Second,
		PendingTTL: 12 * time.Hour, StandingIssue: ls.Standing, IssueAssignee: dc.IssueAssignee}
	var waker func(context.Context, string, string, string)
	if cfg.EnvoyURL != "" {
		w := wake.Envoy{URL: cfg.EnvoyURL, Token: cfg.EnvoyToken, HTTP: &http.Client{Timeout: 10 * time.Second}}
		waker = func(ctx context.Context, enrollmentID, requestID, state string) {
			sid, _ := machine.SessionID(ctx, requestID) // requests.session_id, else the enrollment's
			if sid == "" {
				sid, _ = enr.SessionID(ctx, enrollmentID)
			}
			if sid != "" {
				w.Notify(ctx, sid, requestID, state)
			}
		}
	}
	poller := &requests.Poller{Machine: machine, Dispatch: dc, Interval: time.Duration(cfg.AskPollSeconds) * time.Second, Wake: waker, Launcher: ls}
	go poller.Run(ctx)
	mux := http.NewServeMux()
	api.Register(mux, api.Deps{PublicURL: cfg.PublicURL, Enroll: enr, Machine: machine, Dispatch: dc, Launcher: ls,
		Proof:              &proof.Verifier{Skew: time.Duration(cfg.ProofSkewSeconds) * time.Second, Lookup: enr.Lookup, Replay: enr.Replay},
		TrustedProxyHeader: cfg.TrustedProxyHeader})
	srv := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           withRequestDeadline(mux, requestDeadline),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      requestDeadline + 15*time.Second,
		IdleTimeout:       2 * time.Minute,
	}
	go func() {
		slog.Info("broker listening", "addr", cfg.ListenAddr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("broker: listen", "error", err)
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
		fmt.Fprintln(os.Stderr, "broker:", err)
		os.Exit(2)
	}
}
