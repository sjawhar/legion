// Command broker is the secrets broker: it enrolls agent sessions and pods, decides their secret
// requests by policy or, for a request that needs a person, on its own approval page, where the
// approver signs in to the broker itself (BROKER_SIGNIN_*), over a signed credential-request
// record, and releases granted values. Its HTTP API is documented at
// https://sjawhar.github.io/legion/broker/reference/api/. The broker holds no Dispatch credential
// and never opens a Dispatch ask: Dispatch's server lists pending requests and links each to the
// broker's page, and relays a machine login's decision with its typed code.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/kms"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	"github.com/google/uuid"

	"github.com/sjawhar/envoy/internal/broker/api"
	"github.com/sjawhar/envoy/internal/broker/approval"
	"github.com/sjawhar/envoy/internal/broker/config"
	"github.com/sjawhar/envoy/internal/broker/enroll"
	"github.com/sjawhar/envoy/internal/broker/exchange"
	"github.com/sjawhar/envoy/internal/broker/machine"
	"github.com/sjawhar/envoy/internal/broker/oauth"
	"github.com/sjawhar/envoy/internal/broker/oauthstore"
	"github.com/sjawhar/envoy/internal/broker/policy"
	"github.com/sjawhar/envoy/internal/broker/proof"
	"github.com/sjawhar/envoy/internal/broker/record"
	"github.com/sjawhar/envoy/internal/broker/requests"
	"github.com/sjawhar/envoy/internal/broker/reread"
	"github.com/sjawhar/envoy/internal/broker/secrets"
	"github.com/sjawhar/envoy/internal/broker/store"
	"github.com/sjawhar/envoy/internal/broker/wake"
	"github.com/sjawhar/envoy/internal/oidc"
	"github.com/sjawhar/envoy/internal/ratelimit"
	"github.com/sjawhar/envoy/internal/redact"
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

// oauthSecretTimeout bounds oauthstore.Store.Secret, one of the boot calls that run after the
// bind: an insert ... on conflict do nothing and a select on a one-row table. It needs a bound
// of its own because it runs on serving, which no signal cancels - a pod the load balancer may
// already be sending requests to has to drain rather than die mid-request - and a hang there is
// a pod holding a bound listening socket with no srv.Serve behind it until the kubelet's SIGKILL
// at the end of the grace period. Ten seconds is far above any contention two statements against
// a one-row table ever meet, and far below the 60-second shutdownTimeout the grace period
// already has to cover, so it can only fire on a database that has stopped answering.
//
// The headroom is measured, not assumed. Two sessions running these two statements at the same
// moment against a fresh database - the contention a cold two-pod boot makes on
// oauth_provider_secret, where both pods insert the row and one of them loses - take 2.7-6.3 ms
// for the session that waits and 0.8-1.5 ms for the one that does not, over five runs: three
// orders of magnitude inside this bound. The caveat belongs with the figure. That was measured
// with psql against a one-row table of the same shape rather than through the broker's own pgx
// call on the migrated schema, so it is the order of magnitude of the contention and not the
// whole cost of the call. oauth_secret_timeout_test.go measures the call itself, and asserts a
// margin rather than a duration.
const oauthSecretTimeout = 10 * time.Second

// clientCheckInterval is how often the broker asks its sign-in provider whether that provider
// still accepts the broker's own client, after the pass it takes at boot. An hour is what the
// answer is worth: a client is deleted or its secret rotated by a person doing it deliberately,
// and the cost of hearing an hour late is an hour of a status page that is out of date, against
// one token request per pod per hour at the provider.
const clientCheckInterval = time.Hour

func main() {
	// A subcommand is read before flag.Parse, and serves nothing: it is the whole of what this
	// process does. Reading it here is what makes an older image refuse `broker end-sessions`
	// rather than fall through to serving - and so to migrating the database - when a
	// deployment asks it for a subcommand it does not have (cmd/dispatch/main.go's own
	// precedent, runSubcommand there).
	if len(os.Args) > 1 {
		os.Exit(runSubcommand(context.Background(), os.Args[1:], os.Stdout, os.Stderr))
	}
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
	// serveStarted is closed once the socket is bound and srv.Serve is about to take it, and
	// drainNow is how the watchdog below hands a second signal to the drain. The watchdog is
	// again's only reader, and it decides once, from serveStarted's state, who a second signal
	// belongs to (secondSignalFindsServing): the drain, through drainNow, or the watchdog's own
	// exit. Room for one, so that handing it over never blocks on a drain that is not reading
	// yet - there is never a second hand-over to make room for.
	serveStarted := make(chan struct{})
	drainNow := make(chan struct{}, 1)
	go func() {
		if secondSignalFindsServing(again, serveStarted) {
			// Serving: ending the drain rather than the process is right once there are
			// in-flight requests for srv.Shutdown to finish.
			drainNow <- struct{}{}
			return
		}
		// A second signal read before serveStarted had closed. Some of the boot runs on a
		// context no signal cancels, and all of it runs with no health endpoint answering, so
		// an operator who has asked twice is owed an exit rather than a process that runs on to
		// bind a socket and drain a server that never served. The lines logged above this one
		// say how far the boot had got; the status is non-zero because this is a boot that did
		// not finish, not a clean stop.
		//
		// The line says what the check established and not what the exit finds: main may close
		// serveStarted and start srv.Serve in the two statements between the two, so "had not
		// begun serving when the second signal was read" is true where "has served nothing"
		// would be a claim about an instant this goroutine never observes.
		slog.Error("broker: a second signal arrived while the broker was still starting up, so it is stopping without a drain: it had not begun serving when that signal was read")
		os.Exit(1)
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
	// Who a person signs in to the approval page as, logged once at boot. Its client secret is
	// never logged.
	slog.Info("approval sign-in on", "issuer", cfg.SignIn.Issuer, "client_id", cfg.SignIn.ClientID, "domain", cfg.SignIn.Domain)
	// The database, and with it the AWS config loaded once when anything needs it: an IAM-form
	// BROKER_DATABASE_URL, whose connections sign in with RDS IAM tokens minted from it, and the
	// Secrets Manager and KMS clients of the AWS store, unless the fake secrets file stands in.
	// The GCP store needs none.
	st, awsCfg, err := openStore(ctx, cfg, fakeSecrets == "" && cfg.SecretsStore == secrets.StoreAWS)
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
	loader := policy.Loader{Store: secretStore, Prefix: cfg.SecretsPrefix, Services: serviceNames(cfg)}
	// One key source per issuer, read by everything that needs that issuer's keys: the verifiers
	// discoverVerifiers builds and, for the cluster's issuer and Google's, the RFC 8693 exchange
	// below, which resolves a subject token's own key through the same *oidc.Keys. So the boot
	// makes one discovery read per issuer rather than two, one set is held in memory, and a
	// rotation becomes visible to both at the same moment.
	//
	// Discovery is bounded: an issuer that accepts the connection and never answers refuses the
	// boot instead of hanging it with no health endpoint serving.
	discovered, err := discoverVerifiers(ctx, cfg, issuerKeys)
	fatal(err)
	pod := discovered.Pod

	// The context the work that has to outlive the drain runs on: the five-minute policy reload
	// below and the reread listener under it. ctx is signal.NotifyContext's, which the first
	// SIGTERM cancels, and a pod goes on being sent new requests for the whole of its drain —
	// that is what the drain is for — so for that window it is still one of the pods a reread has
	// to reach, and a retag that narrows who may have a secret has to be in force there too.
	// context.WithoutCancel keeps every value ctx carries and drops only its cancellation;
	// endServing, called once srv.Shutdown has returned - having answered the last request, or
	// having spent its 60-second allowance - is what ends these loops instead. Everything that legitimately stops at the signal — the request
	// sweeper below — stays on ctx.
	serving, endServing := context.WithCancel(context.WithoutCancel(ctx))
	defer endServing()

	// The policy and its five-minute reload, on serving rather than on ctx so the backstop the
	// fan-out leans on goes on reloading while the pod drains: stopped at the signal it would
	// leave the one pod the load balancer is still choosing with neither the fan-out nor the
	// reload to narrow the tags it serves. One thing about boot changes with it: the first load
	// no longer ends early on a signal that arrives while it is running, so a pod signalled
	// mid-boot finishes that load and then returns without draining at all, there being nothing
	// bound to drain (the return below). NewCurrent's own contract is untouched — its
	// loop logs no failed-load line once its context has ended, which is now the end of the
	// shutdown rather than the signal, so the line a deployment alarms on still counts only loads
	// that failed while the broker was meant to be serving.
	//
	// Written down so nobody rediscovers it as a hang: one signal does not interrupt this call.
	// A store that never answers the first load leaves it running - it runs on serving, which no
	// signal cancels, and the ctx.Err() check below sits after it returns - so a single SIGTERM
	// is noted and nothing more until the load ends. That is safe, and it stays as it is. A pod
	// here has bound nothing, is in no EndpointSlice and serves no caller, so there is no request
	// to answer and nothing to drain by ending it sooner; what does end it is a second signal,
	// which the watchdog goroutine at the top of main reads from `again` and answers with
	// os.Exit(1) (measured: gone within 30 ms of that signal), or the kubelet's SIGKILL at the
	// end of the grace period.
	current, err := policy.NewCurrent(serving, loader, policyRefresh)
	fatal(err)
	// A signal that arrived while that first load ran leaves nothing to drain. The load runs on
	// serving and so does not end early, which is deliberate, but a process signalled before it
	// has bound anything has no caller to answer and no in-flight request to finish: returning
	// here is the whole of such a process's shutdown. Binding a socket and then running a drain
	// and a shutdown budget for a server that never served would hold the pod open for the whole
	// drain and serve nobody - the shutdown budget itself is a maximum rather than a wait, since
	// srv.Shutdown with nothing in flight returns at once, so what is saved here is the drain,
	// and what is avoided is a pod in an EndpointSlice that answers no caller.
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
	// on yet. That wait orders net.Listen against the LISTEN and nothing else. It does nothing for
	// a notification committed before the LISTEN, while the first policy load above was reading
	// the namespace - the load was taken before the subscription existed - and the catch-up load
	// after the wait is what covers that one. OnConnected is what makes the wait mean "listening"
	// rather than nearly that: pglisten runs it after the LISTEN statement has succeeded, where a
	// hook on the dial would fire with the LISTEN still in flight. The token is sent without
	// blocking and the channel is never drained again, so every redial behind the boot costs the
	// listener nothing.
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
	// on the channel before net.Listen binds the socket callers reach it on, or rereadListenWait
	// has run out. A pod whose dial or whose LISTEN failed waits the bound out and then serves
	// anyway, the listener redialing behind it, so nothing here can refuse a boot; the bound is
	// what keeps a Postgres that takes the connection and never answers from holding one.
	select {
	case <-firstListen:
		// The catch-up: one more whole-namespace load into the live policy, taken now that the
		// LISTEN is in place, so that the subscription exists before the read whose result it is
		// meant to invalidate. Postgres delivers nothing to a session for a NOTIFY committed before
		// that session's LISTEN, and the listener's onConnected deliberately catches nothing up, so
		// without this load the following is lost on this pod until its five-minute reload, with
		// nothing logged: B starts and begins its first load; A serves a reread of a narrowing
		// retag and commits its notification, which no session of B's is listening for; B's
		// LISTEN succeeds. With this load, B then reads the namespace again here and binds with
		// the retag in force. A notification committed after the LISTEN is delivered instead; the
		// RefreshOne it drives and this Refresh take the policy's one writer lock, and Refresh
		// re-describes every name a reread changed within the store's ListLag (Current's recent
		// anchors), so a reread that lands first is not undone by this load's listing. What this
		// load reads is the store's listing, as every whole-namespace load does, so a store whose
		// listing lags a write by up to its ListLag (Store.ListLag) can still show it the tags
		// from before the retag. That exposure is the store's rather than this ordering's - the
		// alternative shapes of this fix all share it, since a retag committed before any
		// subscription is only ever visible through a listing - and it is tracked, with the two
		// ways to close it and their costs, at dispatch://INFRA-375.
		//
		// It costs one extra namespace read per boot, and nothing after: the whole listing - on
		// Secrets Manager its ListSecrets pages and at most one KMS alias lookup, on Secret
		// Manager those pages and one GetSecretVersion per secret.
		//
		// A failed load does not refuse the boot. The first load succeeded, so the policy is
		// live and this is a catch-up; the line says what failing it loses, and it is named
		// apart from the "broker: <operation> failed" lines, which stand for a request answered
		// 500 or 503. It runs on serving, as the first load does and for the same reasons, so
		// what the first load's comment writes down about one signal holds here too: a store
		// that never answers holds the boot until a second signal or the kubelet's SIGKILL.
		if err := current.Refresh(serving); err != nil {
			slog.Warn("broker: serving without the boot's catch-up policy load, so a retag another pod served while this one was starting may not be in force here until its five-minute reload", "error", err)
		}
		// The same check as the one after the first load, for the same reason: a process
		// signalled before it has bound anything has nothing to drain.
		if ctx.Err() != nil {
			slog.Info("broker: signalled while starting up, before it had bound anything", "stage", "the catch-up policy load")
			return
		}
	case <-time.After(rereadListenWait):
		// No catch-up here. With no LISTEN in place there is no subscription to order a read
		// after, so a second namespace read would not close the window, only move where it
		// starts: a notification sent before the listener connects is lost on this pod whatever
		// is read now, as this warning says, and its five-minute reload is what catches it up.
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

	lease, sweep := time.Duration(cfg.LeaseSeconds)*time.Second, time.Duration(cfg.SweepSeconds)*time.Second
	enr := &enroll.Service{
		Store: st, Lease: lease, Pod: pod, ServiceLogins: cfg.ServiceLogins,
		VMServicesByAccount: cfg.VMServicesByAccount,
	}
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
	// The exchange's listed identities and the issuers whose tokens it accepts as a subject
	// token. Both halves are optional and the grant is composed either way: a broker told about
	// no issuer accepts no subject token at all, which the line below says once at boot rather
	// than leaving an operator to read it out of a refusal.
	exchangePolicy, err := exchange.NewSet(cfg.ExchangeAccounts, cfg.ExchangeWorkflows, cfg.ExchangeGoogleAccounts, cfg.SignIn.Domain)
	fatal(err)
	exchangeConfig := oauth.ExchangeConfig{Policy: exchangePolicy}
	if cfg.ExchangeK8sAudience != "" {
		// The key source the account verifier already reads: a subject token is the same
		// cluster's projected token, for an audience of its own (BROKER_EXCHANGE_K8S_AUDIENCE),
		// and BROKER_EXCHANGE_K8S_AUDIENCE needs BROKER_K8S_OIDC_ISSUER, so this is never nil.
		exchangeConfig.Kubernetes = oauth.IssuerConfig{Issuer: cfg.K8sOIDCIssuer, Audience: cfg.ExchangeK8sAudience, Keys: discovered.K8sKeys}
	}
	if cfg.GoogleOIDCAudience != "" {
		// The key set discoverVerifiers read for Google, for the same reason the cluster's is
		// shared: BROKER_GOOGLE_OIDC_AUDIENCE is what made it read one, so this is never nil.
		exchangeConfig.Google = oauth.IssuerConfig{Issuer: cfg.GoogleOIDCIssuer, Audience: cfg.GoogleOIDCAudience, Keys: discovered.GoogleKeys}
		exchangeConfig.MachineCredentials = machineCredentials(enr)
	}
	if cfg.SignInDeviceAudience != "" {
		// A person signs in through Google, which is also the issuer of the machine's actor
		// token, so BROKER_SIGNIN_DEVICE_AUDIENCE needs BROKER_GOOGLE_OIDC_AUDIENCE and the two
		// share the key set read above. What a person may be is BROKER_SIGNIN_DOMAIN's to say,
		// which the exchange's policy already holds.
		exchangeConfig.PersonAudience = cfg.SignInDeviceAudience
		exchangeConfig.PersonSignIns = personSignIns()
		slog.Info("google person sign-in", "issuer", cfg.GoogleOIDCIssuer, "audience", cfg.SignInDeviceAudience, "domain", cfg.SignIn.Domain)
	}
	if cfg.ExchangeGitHubIssuer != "" {
		// The GitHub issuer has no second reader, so it keeps a key source of its own, read here
		// rather than at the boot's start: nothing before the bind needs it.
		keys, err := issuerKeys(serving, cfg.ExchangeGitHubIssuer)
		fatal(err)
		exchangeConfig.GitHub = oauth.IssuerConfig{Issuer: cfg.ExchangeGitHubIssuer, Audience: cfg.ExchangeGitHubAudience, Keys: keys}
	}
	if cfg.ExchangeK8sAudience == "" && cfg.ExchangeGitHubIssuer == "" && cfg.GoogleOIDCAudience == "" {
		slog.Info("token exchange", "state", "unconfigured", "detail", "no subject token issuer is set, so no pod, workflow run or machine exchanges")
	} else {
		slog.Info("token exchange", "state", "on", "kubernetes", cfg.ExchangeK8sAudience != "", "github", cfg.ExchangeGitHubIssuer != "",
			"accounts", len(cfg.ExchangeAccounts), "workflows", len(cfg.ExchangeWorkflows), "machines", len(cfg.ExchangeGoogleAccounts))
	}
	// verification_uri is Dispatch's machine-login page, where the person types the code.
	provider, err := oauth.New(oauth.Config{
		Secret: oauthSecret, DeviceLifetime: machineLoginPendingTTL,
		AccessLifetime:  time.Duration(cfg.AccessTokenSeconds) * time.Second,
		RefreshLifetime: credentialLifetime, VerificationURL: cfg.MachineLoginURL,
		ProofSkew: time.Duration(cfg.ProofSkewSeconds) * time.Second,
		Exchange:  exchangeConfig,
	}, oauthStore)
	fatal(err)
	mach := &machine.Service{
		Store: st, Enroll: enr, Policy: current,
		Audience: cfg.PublicURL, Skew: time.Duration(cfg.ProofSkewSeconds) * time.Second,
		PendingTTL: machineLoginPendingTTL, CredentialLifetime: credentialLifetime,
		OAuth: provider, Replay: enr.Replay,
	}
	// The approval page signs a person in with the broker's own client and returns them to
	// BROKER_PUBLIC_URL as resolved above.
	page, err := approval.New(approval.Config{
		SignIn: cfg.SignIn, PublicURL: cfg.PublicURL, MachineLoginURL: cfg.MachineLoginURL,
		Store: st, Machine: reqMachine,
	})
	fatal(err)

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
		Interval: sweep, Wake: waker,
	}
	go sweeper.Run(ctx)
	// The client check, beside the sweeper and on the same context: at boot and every hour
	// after, one token request asking the sign-in provider whether it still accepts the
	// broker's own client, written into the row GET /signin/status answers. It never refuses a
	// boot and never stops the process - a provider that is down stops nobody signing in later,
	// since a sign-in reads the discovery document on its own path - so it is a goroutine with
	// no error to return. It stops at the signal, as the sweeper does: a draining pod is
	// answering its last requests, not reporting on a provider.
	go runClientChecks(ctx, cfg.SignIn, st)

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
	api.Register(mux, api.Deps{PublicURL: cfg.PublicURL, RereadOverallLimit: rereadOverall,
		APIAccounts: cfg.APIAccounts, AccountTokens: pod,
		Enroll: enr, Machine: reqMachine, MachineLogin: mach,
		OAuth: provider, DeviceLogin: mach.LoginDevice, TokenHooks: mach.TokenHooks(),
		Proof:  &proof.Verifier{Skew: time.Duration(cfg.ProofSkewSeconds) * time.Second, Lookup: enr.Lookup, LookupLauncher: enr.AuthenticateLauncher, Replay: enr.Replay},
		Policy: current, SecretsStore: cfg.SecretsStore, SecretsPrefix: cfg.SecretsPrefix,
		SecretsKMSKey:      cfg.SecretsKMSKey,
		TrustedProxyHeader: cfg.TrustedProxyHeader,
		Approval:           page,
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
	// From here a second signal belongs to the drain below rather than to the watchdog's exit:
	// the socket is bound and srv.Serve is about to take it, so there are requests to finish and
	// a drain whose job is finishing them. Closing serveStarted before srv.Serve starts is what
	// makes the watchdog's exit line true: a decision that finds it open is one taken before
	// srv.Serve could have answered a single request.
	close(serveStarted)
	// The serving clock starts only now, beside srv.Serve: a beat says a broker is serving, and a
	// boot that fails above (the OAuth secret, the exchange's issuers, the provider) answers
	// nothing, so a fleet crash-looping there must read as no broker serving rather than have its
	// leases that run out swept. It beats on serving, not ctx: this pod still answers renewals
	// through its drain, so it is still a broker serving, and a gap in the beats there would read
	// as an outage.
	clock := &enroll.ServingClock{Enrollments: enr, Interval: sweep, NotServingAfter: config.NotServingSweeps * sweep}
	go clock.Run(serving)
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
		// first signal — so the second one arrives on drainNow, handed over by the watchdog at the
		// top of main once it has found serveStarted closed. A zero drain takes neither this
		// select nor a timer, which would be a wait on something that never fires.
		timer := time.NewTimer(drain)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-drainNow:
			slog.Info("broker: a second signal ended the drain early")
		}
	}
	slog.Info("broker shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		slog.Warn("broker: shutdown", "error", err)
	}
	// Shutdown has returned, either having answered the last request or having spent the 60
	// seconds above, so the work kept alive through the drain has nothing left to serve: this is
	// where the reread listener and the five-minute policy reload stop.
	//
	// The order is a dependency, and it is not the one the pool makes: st.Listener hands pglisten
	// a connection hijacked out of the pool (store.go, conn.Hijack), so st.Close never touches it
	// and closing the store would not end a LISTEN. What ends it is pglisten's own deferred
	// close of that connection when its loop returns, which endServing asks for by cancelling
	// serving. endServing only asks: nothing here waits for that goroutine, and the process
	// exits with the cancellation delivered and the close in flight, which costs nothing because
	// Postgres ends a session whose connection goes with the process. The order still matters for
	// the reload and the pools: endServing stops the loop that would otherwise read through a
	// pool st.Close is about to shut, and st.Close releases the two pools behind it, each of
	// whose health checks is a goroutine of its own.
	endServing()
	st.Close()
}

// secondSignalFindsServing takes the first two signals off again and then answers whether
// serveStarted was closed: true when the second signal belongs to the drain, false when it
// belongs to the watchdog's exit. It is the watchdog's whole decision, kept out of main so a test
// drives it with no process to kill.
//
// The order is the point. It takes both signals before it looks at serveStarted, and then looks
// once without waiting. Waiting on serveStarted and the second signal in one select instead would
// be a select over two ready channels whenever a second signal reached a broker already serving,
// and Go picks among ready cases uniformly at random: some of those stops would os.Exit(1) a
// serving broker, skipping the drain and srv.Shutdown and logging that it had served nothing.
// Read this way, serveStarted's state is the only input, and it only ever moves one way, so once
// it is closed every second signal answers drain.
//
// The first signal is the one signal.NotifyContext has already turned into ctx's cancellation;
// again is sent it too, and taking it off here is what leaves the next one to mean "now". Nothing
// else reads again, so no other reader can take either signal first.
//
// What this establishes is the state at the look and not at what the caller does next: found
// open, the exit two statements later runs while main may already have closed serveStarted and
// started srv.Serve, so what is true of that exit is that the broker had not begun serving when
// the second signal was read. The ERROR line says that and no more. Nothing orders the two, and
// nothing should: this window is between a function returning and the next two statements, where
// the defect it replaces was a choice between two ready cases measured at 39 of 100 on a signal
// pair the kernel cannot merge - orders of magnitude apart, and the exit is what the operator
// asked for twice. A lock or a second check here would be a fix to a window that does not need
// one.
//
// Either answer stops the process. Found open, srv.Serve has not started at the look, since main
// closes serveStarted first, so no request was ever accepted to drop; found closed, a server
// exists or is one statement from existing, and the drain is the right owner for it - it ends at
// once on the hand-over and runs srv.Shutdown.
func secondSignalFindsServing(again <-chan os.Signal, serveStarted <-chan struct{}) bool {
	<-again
	<-again
	select {
	case <-serveStarted:
		return true
	default:
		return false
	}
}

// requestDeadline bounds every request's own context, so no handler outlives its caller by more
// than a Dispatch call's worth of time; writeTimeout sits past it so a handler that honours its
// context always gets to write its answer.
const requestDeadline = 45 * time.Second

// writeTimeout is the http.Server's own WriteTimeout: the request deadline plus the fifteen
// seconds a handler that honours its context needs to write the answer it already has. Go resets
// the write deadline once a request's headers are read, so what it bounds is the exchange after
// the headers and not the whole accepted connection, whose header read has its own
// ReadHeaderTimeout above.
const writeTimeout = requestDeadline + 15*time.Second

// shutdownTimeout is how long the broker waits for its in-flight requests once the drain has run
// out, and it is the write timeout rather than a figure of its own because that is the longest
// exchange the server itself allows a request already past its headers: one admitted in the last
// instant of the drain still gets its whole 45-second handler deadline, and WriteTimeout bounds
// the exchange around it at 60 seconds. A connection still sending headers when the drain ends
// is not what this budget is for: Shutdown closes an idle connection at once and treats one that
// has gone five seconds without its headers as idle (net/http closeIdleConns), so the longest it
// waits for a connection that never sends a request is those five seconds. It is only safe to be
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

// openStore opens the broker's database as cfg says, and answers the AWS configuration it loaded
// for it. An IAM-form BROKER_DATABASE_URL signs each new connection in with an RDS IAM token
// minted from that configuration, so it is loaded before the database is opened; awsAlso asks for
// it where the database does not, for the Secrets Manager and KMS clients of the AWS store, and
// with neither the answer is the zero aws.Config. It migrates nothing: the server migrates after
// this, and the end-sessions subcommand must not. Both of them open the store here, so the
// subcommand reaches the same database and signs in to it the same way as the server.
func openStore(ctx context.Context, cfg config.Config, awsAlso bool) (*store.Store, aws.Config, error) {
	var awsCfg aws.Config
	var err error
	if cfg.DatabaseIAM || awsAlso {
		if awsCfg, err = awsconfig.LoadDefaultConfig(ctx); err != nil {
			return nil, aws.Config{}, fmt.Errorf("load the AWS configuration: %w", err)
		}
	}
	var opts []store.Option
	if cfg.DatabaseIAM {
		mint, err := rdsAuthTokens(awsCfg)
		if err != nil {
			return nil, aws.Config{}, err
		}
		opts = append(opts, store.WithTokenMinter(mint))
	}
	st, err := store.Open(ctx, cfg.DatabaseURL, opts...)
	if err != nil {
		return nil, aws.Config{}, fmt.Errorf("open the database: %w", err)
	}
	return st, awsCfg, nil
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
	return fmt.Errorf("BROKER_PUBLIC_URL %q: port 0 is never dialable in production (BROKER_FAKE_SECRETS_FILE is unset); only a local run may use it as dev-broker.sh's derive-from-bind convention", redact.URL(publicURL))
}

// newRequestMachine builds the requests.Machine the broker decides every secret request with, from
// its configuration: the audience and skew every request object is checked against, and, for each
// service account, the registered service a pod running as it proves. A grant lasts the lifetime
// its request signed, or until its session ends.
func newRequestMachine(cfg config.Config, st *store.Store, current *policy.Current, secretStore secrets.Store, replay func(context.Context, string, time.Time) (bool, error)) *requests.Machine {
	m := &requests.Machine{
		Store: st, Policy: current, Secrets: secretStore, PendingTTL: agentSecretPendingTTL,
		Audience: cfg.PublicURL, Skew: time.Duration(cfg.ProofSkewSeconds) * time.Second, Replay: replay,
		ServicesByAccount: cfg.ServicesByAccount,
		Minters:           kindMinters(secrets.GitHubApp{}, secrets.GoogleLogin{}),
	}
	m.Chain = requests.NewChainVerifier(st, cfg.PublicURL, time.Duration(cfg.ProofSkewSeconds)*time.Second)
	return m
}

// serviceNames is every registered service, sorted and each once: BROKER_SERVICES' and
// BROKER_VM_SERVICES'. The policy loader serves a secret whose owner tag names one of them as that
// service's, and refuses any other owner that is not a person.
func serviceNames(cfg config.Config) []string {
	names := slices.AppendSeq(slices.Collect(maps.Values(cfg.ServicesByAccount)), maps.Values(cfg.VMServicesByAccount))
	slices.Sort(names)
	return slices.Compact(names)
}

// keysFunc is issuerKeys' shape, a seam for a test that must not reach a real issuer.
type keysFunc func(ctx context.Context, issuer string) (*oidc.Keys, error)

// verifiers are what one boot's discovery of the token issuers the broker trusts produced: a
// verifier per issuer and the key set behind each, since the RFC 8693 exchange resolves a subject
// token's own key itself and must read the very set its verifier reads.
type verifiers struct {
	// Pod verifies a pod's enrollment token and a service's login token, nil on a broker told
	// about no cluster.
	Pod enroll.PodVerifier
	// K8sKeys is the cluster issuer's published keys, nil where Pod is.
	K8sKeys *oidc.Keys
	// GoogleKeys is the Google issuer's published keys, nil on a broker with no Google audience,
	// which registers no Google subject token type.
	GoogleKeys *oidc.Keys
}

// discoverVerifiers reads the key sets of the token issuers the broker trusts (keysFor, issuerKeys
// in production) and builds a verifier on each: the cluster's (BROKER_K8S_OIDC_ISSUER and
// BROKER_K8S_OIDC_AUDIENCE), one verifier for both a pod's enrollment token and a service's login
// token, which are the same cluster's projected tokens for the same audience, whose key set it
// also answers for the RFC 8693 exchange; and Google's (BROKER_GOOGLE_OIDC_ISSUER), for a
// machine's identity token, only when BROKER_GOOGLE_OIDC_AUDIENCE is set, whose key set the
// exchange resolves a machine's subject token against, logging one line that names the issuer,
// the audience, the machine services and how many machine entries the exchange holds. With no
// Google audience it reads and logs nothing about Google, and answers no Google keys, so the
// broker registers no Google subject token type.
func discoverVerifiers(ctx context.Context, cfg config.Config, keysFor keysFunc) (verifiers, error) {
	var found verifiers
	if cfg.K8sOIDCIssuer != "" {
		keys, err := keysFor(ctx, cfg.K8sOIDCIssuer)
		if err != nil {
			return verifiers{}, err
		}
		found.K8sKeys = keys
		found.Pod = enroll.K8sPodVerifier{Verifier: oidc.NewWithKeys(cfg.K8sOIDCIssuer, cfg.K8sOIDCAudience, keys)}
	}
	if cfg.GoogleOIDCAudience == "" {
		return found, nil
	}
	googleKeys, err := keysFor(ctx, cfg.GoogleOIDCIssuer)
	if err != nil {
		return verifiers{}, err
	}
	services := slices.Sorted(maps.Values(cfg.VMServicesByAccount))
	slog.Info("google vm identity", "issuer", cfg.GoogleOIDCIssuer, "audience", cfg.GoogleOIDCAudience,
		"services", strings.Join(slices.Compact(services), ","), "fleets", len(cfg.ExchangeGoogleAccounts))
	found.GoogleKeys = googleKeys
	return found, nil
}

// machineCredentials is oauth.ExchangeConfig.MachineCredentials over the enrollment service: the
// launcher credential a machine's exchange mints or moves, written in the exchange's own
// transaction (enroll.Service.IssueTokenLoginTx), so the credential and the access token that
// names it land together. A key another live credential holds is oauth.ErrMachineKeyTaken, which
// the exchange answers invalid_grant.
func machineCredentials(enr *enroll.Service) oauth.MachineCredentials {
	return func(ctx context.Context, cred oauth.MachineCredential) (string, error) {
		tx, ok := oauthstore.TxFrom(ctx)
		if !ok {
			return "", errors.New("broker: a machine's launcher credential is minted in the exchange's transaction")
		}
		id, err := enr.IssueTokenLoginTx(ctx, tx, enroll.TokenLogin{
			Subject:      cred.Subject,
			Service:      cred.Service,
			Host:         cred.Host,
			Operator:     cred.Operator,
			TokenExpires: cred.Expires,
			Thumbprint:   cred.Thumbprint,
			JWK:          cred.JWK,
			Expires:      cred.Expires,
		})
		if store.IsUniqueViolation(err) {
			return "", fmt.Errorf("%w: %v", oauth.ErrMachineKeyTaken, err)
		}
		if err != nil {
			return "", err
		}
		return id.String(), nil
	}
}

// personSignIns is oauth.ExchangeConfig.PersonSignIns over the broker's store: the sign-in a
// person's trade belongs to, written in the exchange's own transaction
// (store.RecordPersonSignIn), so the sign-in and the access token minted under it land
// together. A box key that person has revoked is store.ErrSignInRevoked, which the exchange
// answers invalid_grant naming the sign-in command.
func personSignIns() oauth.PersonSignIns {
	return func(ctx context.Context, in oauth.PersonSignIn) error {
		tx, ok := oauthstore.TxFrom(ctx)
		if !ok {
			return errors.New("broker: a person's sign-in is recorded in the exchange's transaction")
		}
		err := store.RecordPersonSignIn(ctx, tx, in.Person, in.Thumbprint, in.Machine)
		if errors.Is(err, store.ErrSignInRevoked) {
			return fmt.Errorf("%w: %v", oauth.ErrSignInRevoked, err)
		}
		return err
	}
}

// issuerKeys reads issuer's published signing keys: the source a token's own key is resolved
// against, both for the verifier that answers whether a token verifies (a projected
// service-account token) and for the RFC 8693 exchange, which
// needs the key itself. It takes the same boot deadline
// discovery does: an issuer that accepts the connection and never answers refuses the boot rather
// than hanging it with no health endpoint serving. No key is fetched here; the first token read
// reads the set.
func issuerKeys(ctx context.Context, issuer string) (*oidc.Keys, error) {
	discovery, cancel := context.WithTimeout(ctx, oidc.DiscoveryTimeout)
	defer cancel()
	return oidc.NewKeys(discovery, issuer)
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

// runClientChecks checks the sign-in client at once and every clientCheckInterval after, until
// ctx ends.
func runClientChecks(ctx context.Context, signIn approval.SignIn, st *store.Store) {
	for {
		checkSignInClient(ctx, signIn, st)
		select {
		case <-ctx.Done():
			return
		case <-time.After(clientCheckInterval):
		}
	}
}

// checkSignInClient takes one pass of the client check and writes what it found, with the time,
// into the row GET /signin/status answers. A test drives this rather than waiting an hour for
// the loop above to take its second pass.
//
// Neither half can stop the broker. A provider that cannot be asked is the answer
// issuer_unreachable and a line in the log, not a boot that fails: people sign in through a
// path that reads the discovery document again on its own, so a check that fails says nothing
// about whether the next sign-in works. A write that fails loses this pass's answer and the
// next pass writes again.
func checkSignInClient(ctx context.Context, signIn approval.SignIn, st *store.Store) {
	check, err := oidc.CheckClient(ctx, signIn.Issuer, signIn.ClientID, signIn.ClientSecret)
	if err != nil {
		slog.Warn("broker: the sign-in client check", "state", check, "issuer", signIn.Issuer, "error", err)
	}
	if ctx.Err() != nil {
		// The process is stopping: this answer is the end of the context, not the provider's.
		return
	}
	if err := st.RecordClientCheck(ctx, string(check), time.Now()); err != nil {
		slog.Warn("broker: the sign-in client check was not written", "state", check, "error", err)
	}
}

// endSessionsCommand is the one argument the broker takes in place of serving.
const endSessionsCommand = "end-sessions"

// runSubcommand runs the subcommand args name and answers its exit code. It is every argument
// this binary takes: an unknown one, a missing login or a second argument is refused with 2 and
// a usage line, never a fall-through to serving.
func runSubcommand(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if args[0] != endSessionsCommand {
		fmt.Fprintf(stderr, "broker: unknown subcommand %q; the only subcommand is %s <login>, and broker with no argument serves\n", args[0], endSessionsCommand)
		return 2
	}
	if len(args) != 2 || strings.TrimSpace(args[1]) == "" {
		fmt.Fprintf(stderr, "broker: usage: broker %s <login>, one login and nothing else\n", endSessionsCommand)
		return 2
	}
	return endSessions(ctx, strings.TrimSpace(args[1]), stdout, stderr)
}

// endSessions deletes every session login is signed in to the approval page with, wherever and
// whenever it was made, and prints how many it deleted: the operator's way to end one person's
// sign-ins everywhere at once, run in a broker pod by whoever holds pods/exec in the broker's
// namespace.
//
// It reads the same configuration the server does and opens the database the same way, the RDS
// IAM token minter included (openStore), and it binds no listener. It does not migrate: an image
// older than this subcommand cannot be allowed to boot and migrate the database because a
// deployment asked it to end some sessions, so the schema is whatever the servers have already
// applied (cmd/dispatch/main.go's subcommands take the opposite view, and say why: each of them
// exists to work on a schema it brings up to date first).
func endSessions(ctx context.Context, login string, stdout, stderr io.Writer) int {
	// The form every login comparison in the module is made in (record.CanonicalLogin), and the
	// form oidc.WorkspaceLogin stores a session under: a login typed in another case ends the
	// person's sessions here as surely as the one they signed in with.
	login = record.CanonicalLogin(login)
	cfg, err := config.Load(os.Getenv)
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", endSessionsCommand, err)
		return 1
	}
	st, _, err := openStore(ctx, cfg, false)
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", endSessionsCommand, err)
		return 1
	}
	defer st.Close()
	ended, err := st.DeleteBrowserSessionsForLogin(ctx, login)
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", endSessionsCommand, err)
		return 1
	}
	sessions := "sessions"
	if ended == 1 {
		sessions = "session"
	}
	fmt.Fprintf(stdout, "%s: ended %d %s of %s\n", endSessionsCommand, ended, sessions, login)
	return 0
}
