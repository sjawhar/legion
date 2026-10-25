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
	"strconv"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/kms"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	"github.com/google/uuid"

	"github.com/sjawhar/envoy/internal/broker/api"
	"github.com/sjawhar/envoy/internal/broker/config"
	"github.com/sjawhar/envoy/internal/broker/enroll"
	"github.com/sjawhar/envoy/internal/broker/machine"
	"github.com/sjawhar/envoy/internal/broker/oauth"
	"github.com/sjawhar/envoy/internal/broker/oauthstore"
	"github.com/sjawhar/envoy/internal/broker/policy"
	"github.com/sjawhar/envoy/internal/broker/proof"
	"github.com/sjawhar/envoy/internal/broker/ratelimit"
	"github.com/sjawhar/envoy/internal/broker/requests"
	"github.com/sjawhar/envoy/internal/broker/reread"
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

// rereadListenWait bounds how long the bind waits for the reread listener's first successful
// LISTEN. What it waits on is pglisten.Listener's OnConnected, which runs only once the LISTEN
// statement itself has returned, so while this waits the pod either becomes one the fan-out can
// reach or the bound runs out: a pod whose dial or whose LISTEN failed waits the whole of it,
// logs one warning and serves anyway, which is the other half of what the bound is for - it is
// what keeps a database that takes the connection and then says nothing from holding the boot.
//
// Five seconds is insurance, not a figure a boot spends: in twenty measured boots the LISTEN was
// in place before the wait was reached at all. What a boot that did reach it loses is the
// notifications sent in that window, which costs that one pod its own five-minute reload
// (policyRefresh) and nothing else.
const rereadListenWait = 5 * time.Second

// oauthSecretTimeout bounds oauthstore.Store.Secret, the one boot call that runs after the bind:
// an insert ... on conflict do nothing and a select on a one-row table. It needs a bound of its
// own because it runs on serving, which no signal cancels - a pod the load balancer may already
// be sending requests to has to drain rather than die mid-request - and a hang there is a pod
// holding a bound listening socket with no srv.Serve behind it until the kubelet's SIGKILL at
// the end of the grace period. Ten seconds is far above any contention two statements against a
// one-row table ever meet, and far below the 60-second shutdownTimeout the grace period already
// has to cover, so it can only fire on a database that has stopped answering.
const oauthSecretTimeout = 10 * time.Second

func main() {
	// The broker takes no flags; parsing refuses any flag given rather than ignoring it.
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	// again carries this process's signals, registered here before any boot work rather than
	// inside the drain where it used to be, and held for the life of the process: so "stop it
	// now" works throughout. signal.NotifyContext's own relay goroutine returns as soon as it has
	// cancelled ctx, while its channel stays registered, so without this registration every
	// signal after the first was swallowed instead of taking SIGTERM's default disposition -
	// kill -TERM twice did nothing at all until the boot had finished. Room for two, because
	// "stop it now" is two signals in quick succession and signal.Notify drops what a full
	// channel cannot take.
	again := make(chan os.Signal, 2)
	signal.Notify(again, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(again)
	// serveStarted is closed once the socket is bound and srv.Serve is about to take it. It is
	// the one piece of shared state that decides which of again's two readers a second signal
	// belongs to, so that they cannot both fire: before it closes there is no server and nothing
	// to drain, and the goroutine below ends the process; after it, the drain's own select reads
	// that signal and ends the drain early instead.
	serveStarted := make(chan struct{})
	go func() {
		// The first signal is the one signal.NotifyContext has already turned into ctx's
		// cancellation; this channel is sent it too. Taking it off here is what leaves the next
		// signal to mean "now" for exactly one reader: this goroutine is parked on the receive
		// before any signal can arrive, so that first delivery is handed straight to it and
		// never sits in the buffer for the drain's select - which the same cancellation is what
		// unblocks.
		<-again
		select {
		case <-serveStarted:
			// Serving: a second signal is the drain's to read, and ending the drain rather than
			// the process is right once there are in-flight requests for srv.Shutdown to finish.
		case <-again:
			// A second signal with nothing serving yet. Some of the boot runs on a context no
			// signal cancels, and all of it runs with no health endpoint answering, so an
			// operator who has asked twice is owed an exit rather than a process that runs on to
			// bind a socket and drain a server that never served. The lines logged above this
			// one say how far the boot had got; the status is non-zero because this is a boot
			// that did not finish, not a clean stop.
			slog.Error("broker: a second signal arrived while the broker was still starting up, so it is stopping without a drain: it has served nothing")
			os.Exit(1)
		}
	}()

	cfg, err := config.Load(os.Getenv)
	fatal(err)
	// BROKER_FAKE_SECRETS_FILE: for local development only, a JSON file standing in for the secret
	// store, in Secrets Manager's shape whatever BROKER_SECRETS_STORE says,
	// {"secrets": [{"name", "kms_key_id", "tags", "value", "deleted_at"}]}: the broker lists
	// the agent secrets and reads their values from it instead of from the store, reading the file again
	// on each of those calls, so an edit to it is a write as Secrets Manager would see one. A secret
	// with no "value" is one created without a value, which the broker refuses as no-current-value;
	// one with a "deleted_at" (RFC 3339) is scheduled for deletion, which the broker no longer lists.
	// Its "kms_key_id" names the agent-secrets key as the selected store configures it:
	// BROKER_SECRETS_KMS_KEY_ARN, or BROKER_SECRETS_KMS_KEY_NAME under gcp-secret-manager.
	fakeSecrets := os.Getenv("BROKER_FAKE_SECRETS_FILE")
	fatal(refusePortZeroPublicURLInProduction(cfg.PublicURL, fakeSecrets))
	// The whole stop budget, logged as soon as the configuration naming its first half is loaded,
	// because a deployment's terminationGracePeriodSeconds has to cover it and nothing in this
	// repository can check that it does: the guard this repository would otherwise write — a test
	// in the owning module reading the dependent's rendered manifest — cannot cross repositories,
	// since the Deployment and its grace period live in the deployment's own repository. This line
	// and the operator documentation are that guard's replacement, so total is the figure an
	// operator transcribes: the grace period must cover it in addition to any preStop pause the
	// deployment adds, because the pause and this drain are both paid out of the one grace period.
	drain := time.Duration(cfg.DrainSeconds) * time.Second
	slog.Info("broker stop budget", "drain", wholeSeconds(drain), "shutdown", wholeSeconds(shutdownTimeout),
		"total", wholeSeconds(drain+shutdownTimeout))
	// How the broker signs in to its database, logged before anything reaches for AWS or the
	// database, so an operator reads it from the log rather than the task's environment: rds-iam
	// when each connection signs in with an RDS IAM token the broker mints, and password when it
	// signs in as BROKER_DATABASE_URL and libpq's defaults say (a password in the URL, PGPASSWORD or
	// a passfile, or none for a server that trusts the client).
	signIn := "password"
	if cfg.DatabaseIAM {
		signIn = "rds-iam"
	}
	slog.Info("database sign-in", "method", signIn)
	// The AWS config, loaded once when anything needs it: an IAM-form BROKER_DATABASE_URL, whose
	// connections sign in with RDS IAM tokens minted from it (so it is loaded before the database
	// is opened), and the Secrets Manager and KMS clients of the AWS store, unless the fake
	// secrets file stands in. The GCP store needs none.
	var awsCfg aws.Config
	if cfg.DatabaseIAM || (fakeSecrets == "" && cfg.SecretsStore == secrets.StoreAWS) {
		awsCfg, err = awsconfig.LoadDefaultConfig(ctx)
		fatal(err)
	}
	var storeOpts []store.Option
	if cfg.DatabaseIAM {
		mint, err := rdsAuthTokens(awsCfg)
		fatal(err)
		storeOpts = append(storeOpts, store.WithTokenMinter(mint))
	}
	st, err := store.Open(ctx, cfg.DatabaseURL, storeOpts...)
	fatal(err)
	fatal(st.Migrate(ctx))
	// The secret store, logged with how it signs in so an operator reads it from the log: the
	// fake file (an AWS-shaped store whatever BROKER_SECRETS_STORE says, since the file holds
	// Secrets Manager's fields), Secrets Manager under the AWS SDK's credential chain, or Secret
	// Manager under Application Default Credentials.
	var secretStore secrets.Store
	var credentials string
	switch {
	case fakeSecrets != "":
		local, err := secrets.LocalFromFile(fakeSecrets)
		fatal(err)
		// The file's kms_key_id names the agent-secrets key as the selected store configures it.
		// Under gcp-secret-manager this is still the AWS store over the file, holding the Cloud KMS
		// key name where it would hold an ARN: names follow Secrets Manager's rules and a source
		// is a local ARN, which only a local run sees.
		secretStore, credentials = secrets.AWS{Client: local, KMS: local, KeyARN: cfg.SecretsKMSKey}, "fake secrets file"
	case cfg.SecretsStore == secrets.StoreGCP:
		client, err := secrets.NewGCPClient(ctx)
		fatal(err)
		defer client.Close()
		secretStore, credentials = secrets.NewGCP(client, cfg.SecretsKMSKey), "application-default"
	default:
		sm := secretsmanager.NewFromConfig(awsCfg)
		secretStore, credentials = secrets.AWS{Client: sm, KMS: kms.NewFromConfig(awsCfg), KeyARN: cfg.SecretsKMSKey}, "aws-sdk"
	}
	slog.Info("secret store", "store", cfg.SecretsStore, "credentials", credentials)
	loader := policy.Loader{Store: secretStore, Prefix: cfg.SecretsPrefix, Services: slices.Sorted(maps.Keys(cfg.ServiceAccounts))}
	var pod enroll.PodVerifier
	// Discovery is bounded: an issuer that accepts the connection and never answers refuses the
	// boot instead of hanging it with no health endpoint serving.
	verifier, err := oidc.Discover(ctx, cfg.K8sOIDCIssuer, cfg.K8sOIDCAudience, oidc.DiscoveryTimeout)
	fatal(err)
	if verifier != nil {
		// One verifier for both: a pod's enrollment token and a service's login token are the same
		// cluster's projected tokens, for the same audience.
		pod = enroll.K8sPodVerifier{Verifier: verifier}
	}

	// The context the work that has to outlive the drain runs on: the five-minute policy reload
	// below and the reread listener under it. ctx is signal.NotifyContext's, which the first
	// SIGTERM cancels, and a pod goes on being sent new requests for the whole of its drain —
	// that is what the drain is for — so for that window it is still one of the pods a reread has
	// to reach, and a retag that narrows who may have a secret has to be in force there too.
	// context.WithoutCancel keeps every value ctx carries and drops only its cancellation;
	// endServing, called once srv.Shutdown has returned and no request is left to answer, is what
	// ends these loops instead. Everything that legitimately stops at the signal — the request
	// sweeper below — stays on ctx.
	serving, endServing := context.WithCancel(context.WithoutCancel(ctx))
	defer endServing()

	// The policy and its five-minute reload, on serving rather than on ctx so the backstop the
	// fan-out leans on goes on reloading while the pod drains: stopped at the signal it would
	// leave the one pod the load balancer is still choosing with neither the fan-out nor the
	// reload to narrow the tags it serves. One thing about boot changes with it: the first load
	// no longer ends early on a signal that arrives while it is running, so a pod signalled
	// mid-boot finishes that load before it drains. NewCurrent's own contract is untouched — its
	// loop logs no failed-load line once its context has ended, which is now the end of the
	// shutdown rather than the signal, so the line a deployment alarms on still counts only loads
	// that failed while the broker was meant to be serving.
	current, err := policy.NewCurrent(serving, loader, policyRefresh)
	fatal(err)
	// A signal that arrived while that first load ran leaves nothing to drain. The load runs on
	// serving and so does not end early, which is deliberate, but a process signalled before it
	// has bound anything has no caller to answer and no in-flight request to finish: returning
	// here is the whole of such a process's shutdown. Binding a socket and then running a drain
	// and a shutdown budget for a server that never served would hold the pod open for over a
	// minute and serve nobody.
	if ctx.Err() != nil {
		slog.Info("broker: signalled while starting up, before it had bound anything", "stage", "the first policy load")
		return
	}

	// This process's own sender id, minted once at boot. Postgres delivers a notification to every
	// session listening on the channel, this pod's own included, so without an id stamped on what
	// it sends a pod would reread every secret it just reread on behalf of a caller. Both halves of
	// the fan-out take it: the notifier stamps it, and the listener drops what carries it.
	senderID := uuid.NewString()
	// firstListen carries the reread listener's first successful LISTEN to the bind below, which
	// waits for it: a pod the load balancer can reach is a pod a retag has to reach, so a process
	// that bound before its own LISTEN was in place would spend those milliseconds serving
	// traffic while a peer's reread of a secret it holds goes to a channel nobody on this pod is
	// on yet - the one hole the fan-out exists to close, and one nothing but the five-minute
	// reload would close again. OnConnected is what makes the wait mean that rather than nearly
	// that: pglisten runs it after the LISTEN statement has succeeded, where a hook on the dial
	// would fire with the LISTEN still in flight. The token is sent without blocking and the
	// channel is never drained again, so every redial behind the boot costs the listener nothing.
	//
	// The receiving half of the reread fan-out, started after the first policy load (it rereads
	// into that policy) and before net.Listen, so a pod that cannot hold a LISTEN connection still
	// serves: Run redials forever on its own and never refuses the boot. What such a pod loses
	// while it has no connection is the notifications sent in that window — it goes on serving the
	// tags it last read and catches up at its own five-minute reload (policyRefresh), which is the
	// fan-out's documented backstop. It runs on serving, not on the signal's own context: a
	// draining pod is still being sent requests, so it is still a pod a peer's reread has to
	// reach, and a listener stopped at the signal would leave that pod serving the tags it read
	// before the retag for the whole of its drain. The shutdown ends it instead.
	firstListen := make(chan struct{}, 1)
	go (&reread.Listener{
		Sender: senderID,
		Dial:   st.Listener,
		OnConnected: func() {
			select {
			case firstListen <- struct{}{}:
			default:
			}
		},
		Refresh: func(ctx context.Context, name string) error {
			_, err := current.RefreshOne(ctx, name)
			return err
		},
	}).Run(serving)
	// The wait that pairs with it, and the whole of what it promises: either this pod is listening
	// on the channel before it takes its first request, or rereadListenWait has run out. A pod
	// whose dial or whose LISTEN failed waits the bound out and then serves anyway, the listener
	// redialing behind it, so nothing here can refuse a boot; the bound is what keeps a Postgres
	// that takes the connection and never answers from holding one.
	select {
	case <-firstListen:
	case <-time.After(rereadListenWait):
		slog.Warn("broker: binding before the reread listener has had an answer", "waited", rereadListenWait)
	}

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

	enr := &enroll.Service{Store: st, Lease: time.Duration(cfg.LeaseSeconds) * time.Second, Pod: pod, ServiceLogins: cfg.ServiceLogins}
	enr.Chain = enroll.NewChainVerifier(st, cfg.PublicURL, time.Duration(cfg.ProofSkewSeconds)*time.Second)
	reqMachine := newRequestMachine(cfg, st, current, secretStore, enr.Replay)
	credentialLifetime := time.Duration(cfg.LauncherCredentialSeconds) * time.Second
	oauthStore := oauthstore.New(st.Pool)
	oauthStore.Reused = enroll.RevokeReusedCredential
	// On serving, not ctx, for the same reason the policy's own loads are: this runs after the
	// bind above, so a SIGTERM arriving here finds a pod the load balancer may already be sending
	// requests to, and such a pod has to drain rather than die with "context canceled" where its
	// callers see a reset connection. The signal is still read, by <-ctx.Done() below, which the
	// moment this boot finishes begins the drain it asked for. What no signal cancels still has
	// to be bounded, though, because nothing is serving the socket this pod has already bound:
	// oauthSecretTimeout is that bound, and its own comment says what it is sized against.
	secretCtx, cancelSecret := context.WithTimeout(serving, oauthSecretTimeout)
	oauthSecret, err := oauthStore.Secret(secretCtx)
	cancelSecret()
	fatal(err)
	// verification_uri is Dispatch's machine-login page, where the person types the code.
	provider, err := oauth.New(oauth.Config{
		Secret: oauthSecret, DeviceLifetime: machineLoginPendingTTL, RefreshLifetime: credentialLifetime,
		VerificationURL: cfg.MachineLoginURL, ProofSkew: time.Duration(cfg.ProofSkewSeconds) * time.Second,
	}, oauthStore)
	fatal(err)
	mach := &machine.Service{
		Store: st, Enroll: enr, Policy: current,
		Audience: cfg.PublicURL, Skew: time.Duration(cfg.ProofSkewSeconds) * time.Second,
		PendingTTL: machineLoginPendingTTL, CredentialLifetime: credentialLifetime,
		OAuth: provider, Replay: enr.Replay,
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
		Enrollments: enr, Machine: reqMachine, MachineLogins: mach, OAuth: oauthStore,
		Interval: time.Duration(cfg.SweepSeconds) * time.Second, Wake: waker,
	}
	go sweeper.Run(ctx)

	mux := http.NewServeMux()
	// Under the GCP store a reread spends Secret Manager's shared metadata quota, so its overall
	// cap is the GCP one.
	var rereadOverall *ratelimit.Limit
	if cfg.SecretsStore == secrets.StoreGCP {
		rereadOverall = &api.GCPRereadOverallLimit
	}
	// draining is set once, by the signal handler below, and read by every handler goroutine
	// GET /healthz runs on, which is why it is atomic rather than a plain bool.
	var draining atomic.Bool
	// The sending half of the reread fan-out. reread.Notifier returns the error rather than
	// logging it, because what a failed fan-out means is this program's decision, and the decision
	// is one WARN and nothing else: the reread this pod was asked for has already happened and the
	// answer it gives its caller stands, and the peers that did not hear reread this secret at
	// their own next five-minute reload (policyRefresh) anyway. Neither the caller's 200 nor the
	// process is worth failing over a notification Postgres would not take.
	notify := reread.Notifier(st.Pool, senderID)
	api.Register(mux, api.Deps{PublicURL: cfg.PublicURL, UIToken: cfg.UIToken, RereadOverallLimit: rereadOverall,
		Enroll: enr, Machine: reqMachine, MachineLogin: mach,
		OAuth: provider, DeviceLogin: mach.LoginDevice, TokenHooks: mach.TokenHooks(),
		Proof:  &proof.Verifier{Skew: time.Duration(cfg.ProofSkewSeconds) * time.Second, Lookup: enr.Lookup, LookupLauncher: enr.AuthenticateLauncher, Replay: enr.Replay},
		Policy: current, SecretsStore: cfg.SecretsStore, SecretsPrefix: cfg.SecretsPrefix,
		SecretsKMSKey:      cfg.SecretsKMSKey,
		TrustedProxyHeader: cfg.TrustedProxyHeader,
		Draining:           draining.Load,
		NotifyReread: func(ctx context.Context, name string) {
			if err := notify(ctx, name); err != nil {
				slog.Warn("broker: the other pods were not told to reread a secret", "secret", name, "error", err)
			}
		}})
	srv := &http.Server{
		Handler:           withRequestDeadline(mux, requestDeadline),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      writeTimeout,
		IdleTimeout:       2 * time.Minute,
	}
	// From here a second signal belongs to the drain below rather than to the watchdog at the top
	// of main: the socket is bound and srv.Serve is about to take it, so there are requests to
	// finish and a drain whose job is finishing them.
	close(serveStarted)
	go func() {
		if err := srv.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("broker: serve", "error", err)
			stop()
		}
	}()

	<-ctx.Done()
	// The drain comes first and the listener stays open through it, rather than the reverse: a pod
	// leaves its Service's EndpointSlice concurrently with the kubelet beginning termination, not
	// before it, so for a while after SIGTERM the kubelet is still routing new connections here.
	// Failing readiness now — GET /healthz answers 503 SHUTTING_DOWN from this point, while every
	// other route is served exactly as before — and closing the listener only once the drain has
	// run out is what keeps those connections from being refused by a socket that is already gone.
	slog.Info("broker draining", "drain", wholeSeconds(drain))
	draining.Store(true)
	if drain > 0 {
		// A second signal ends the drain at once: an operator, or a kubelet that has decided this
		// pod has to be gone now, must not have to wait out a drain whose whole purpose has
		// already been served. ctx is no help here — signal.NotifyContext cancelled it with the
		// first signal — so the second one is read from again, the channel registered at the top
		// of main, whose watchdog goroutine has stood aside now that serveStarted is closed. A
		// zero drain takes neither this select nor a timer, which would be a wait on something
		// that never fires.
		timer := time.NewTimer(drain)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-again:
			slog.Info("broker: a second signal ended the drain early")
		}
	}
	slog.Info("broker shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		slog.Warn("broker: shutdown", "error", err)
	}
	// The last request has been answered, so the work kept alive through the drain has nothing
	// left to serve: this is where the reread listener and the five-minute policy reload stop.
	// The order is a dependency — the listener holds its LISTEN connection through the store
	// (st.Listener) and redials it forever, so it is ended before the store is closed rather than
	// left redialing a pool that is going away. Closing the store, which nothing outside a test
	// did until this shutdown path became what ends those loops, is what releases that connection
	// and the two pools behind it, each of whose health checks is a goroutine of its own.
	endServing()
	st.Close()
}

// requestDeadline bounds every request's own context, so no handler outlives its caller by more
// than a Dispatch call's worth of time; writeTimeout sits past it so a handler that honours its
// context always gets to write its answer.
const requestDeadline = 45 * time.Second

// writeTimeout is the http.Server's own WriteTimeout: the request deadline plus the fifteen
// seconds a handler that honours its context needs to write the answer it already has.
const writeTimeout = requestDeadline + 15*time.Second

// shutdownTimeout is how long the broker waits for its in-flight requests once the drain has run
// out, and it is the write timeout rather than a figure of its own because that is the longest
// exchange the server itself allows: a request admitted in the last instant of the drain still
// gets its whole 45-second handler deadline, and WriteTimeout bounds the exchange around it at
// 60 seconds, so 60 seconds covers every request the server ever let in. It is only safe to be
// this long because the deployment's terminationGracePeriodSeconds has to cover it — past that
// the kubelet SIGKILLs the process whatever it believes it is still allowed, and the in-flight
// requests this budget exists to finish are dropped anyway. The stop-budget line main logs at
// boot is how a deployment reads the number it has to cover.
const shutdownTimeout = writeTimeout

// wholeSeconds renders a duration as a whole number of seconds ("65s") rather than as
// time.Duration's own "1m5s", because the stop budget's reader is a deployment setting
// terminationGracePeriodSeconds, which is counted in seconds and nothing else. Every duration it
// is given is a whole number of them.
func wholeSeconds(d time.Duration) string { return strconv.Itoa(int(d/time.Second)) + "s" }

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
// its configuration: the audience and skew every request object is checked against, and the
// service accounts that prove a pod is a registered service's. A grant lasts the lifetime its
// request signed, or until its session ends.
func newRequestMachine(cfg config.Config, st *store.Store, current *policy.Current, secretStore secrets.Store, replay func(context.Context, string, time.Time) (bool, error)) *requests.Machine {
	m := &requests.Machine{
		Store: st, Policy: current, Secrets: secretStore, PendingTTL: agentSecretPendingTTL,
		Audience: cfg.PublicURL, Skew: time.Duration(cfg.ProofSkewSeconds) * time.Second, Replay: replay,
		ServiceAccounts: cfg.ServiceAccounts,
		Minters:         kindMinters(secrets.GitHubApp{}, secrets.GoogleLogin{}),
	}
	m.Chain = requests.NewChainVerifier(st, cfg.PublicURL, time.Duration(cfg.ProofSkewSeconds)*time.Second)
	return m
}

// kindMinters is the broker's minter for each kind: the GitHub App minter for github-app secrets
// and the Google login minter for google-login ones. Its arguments are the production minters in
// newRequestMachine; a test passes ones aimed at its own endpoints, so wiring a kind to the wrong
// minter fails it.
func kindMinters(githubApp secrets.GitHubApp, googleLogin secrets.GoogleLogin) map[string]requests.Minter {
	return map[string]requests.Minter{
		policy.KindGitHubApp:   githubApp.Mint,
		policy.KindGoogleLogin: googleLogin.Mint,
	}
}
