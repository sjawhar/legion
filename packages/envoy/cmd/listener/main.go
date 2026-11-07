package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/cistore"
	"github.com/sjawhar/envoy/internal/config"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dedupe"
	"github.com/sjawhar/envoy/internal/envconf"
	"github.com/sjawhar/envoy/internal/ghrename"
	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/internal/metrics"
	"github.com/sjawhar/envoy/internal/session"
	"github.com/sjawhar/envoy/internal/store"
	"github.com/sjawhar/envoy/internal/webhook"
	"github.com/sjawhar/envoy/listenfd"
)

// listenerDeps holds the NATS-dependent resources, all open. main builds the /v1 handlers over it
// once the stores are open. /healthz, which answers during startup too, reads it through an
// atomic.Pointer that stays nil until main has started the receive loop; the list gauges read it
// through another that openV1Routes publishes when /v1 opens (registerListGauges).
type listenerDeps struct {
	client   *bus.Client
	registry *store.Registry
	sessions *session.SessionRegistry
	// ciStore is nil on a listener that mounts no GitHub webhook route, the one route that uses it.
	ciStore *cistore.Store
	// roleLane is the role lane's own client (bus.ConnectRoleLane), nil in a test that builds the
	// handlers without it.
	roleLane *bus.Client
	// consumers manages the session consumers registrations ask for (sessionDelivery); nil in a
	// test that builds the handlers without it, which answers every such registration with core.
	// delivery, below, is the delivery handlers' own configuration, which the routes share.
	consumers *sessionDelivery
	// caches lists the stores above that are open, for the sites that treat every cache alike.
	caches []listenerCache
	// receiver is the durable's receive loop, whose role and lag /healthz reports.
	receiver *durableReceiver
	// wakes publishes the wake reports a role send with no holder and an unplaceable agent
	// message raise, off those paths and under the listener's own lifetime (wakeReporter); nil
	// in a test that builds the deps without it, which reports no wake.
	wakes      *wakeReporter
	streamName string
	streamInfo streamInfoLookup
	// delivery is the config main hands the delivery handlers (the role lane and the durable),
	// wired once, so the publish route forwards through its forwardRole, records each session it
	// confirms in its delivery ledger (so no lane forwards that key to the session again, on this
	// pod or any other of the machine), and writes each send's delivery line and delivery_status
	// count through its logger and messagesDelivered, reading as the role lane's do, beside the
	// route's own request record (request_record.go). The route never reads that ledger for its
	// own dedupe answer: the ledger records what this machine delivered, a forward this pod
	// published, and not the recipient's word that it has the message.
	delivery *listenerDeliveryHandlerConfig
	// confirmed is the publish route's own record of each (dedupe key, session) whose receipt arrived
	// through the route, which only the route writes, and which this pod alone holds (README,
	// "State the pods share"). A send skips a holder or recipient recorded
	// there, so envoy-client's retry of a send does not forward again to one the first attempt
	// confirmed, and no one is counted as confirming a message without having sent its receipt.
	confirmed *dedupe.Cache
	// sendsToUnresponsive counts the sends POST /v1/messages/send took for a recipient past the
	// live window and stored for it (sendHandler). It is how a box's operator sees that a growing
	// share of its traffic is going to sessions that have stopped answering, before any of them
	// reaches the gone horizon.
	sendsToUnresponsive *metrics.Counter
}

// kvCache is what the listener asks of each KV-backed cache it keeps.
type kvCache interface {
	Rewatch(*nats.Conn) error
	WatchErr() error
	// NeedsRead reports a copy that does not follow its bucket with no read under way to make it
	// (kvwatch.Watcher.NeedsRead), which only a Rewatch repairs.
	NeedsRead() bool
	// Disconnected says the connection the cache's watcher reads through was lost
	// (kvwatch.Watcher.Disconnected).
	Disconnected()
	Ping() error
	StopWatch()
	EndWatch(context.Context)
}

// listenerCache is one cache under the name /healthz and self-health report it by.
type listenerCache struct {
	name  string
	cache kvCache
}

// listenerCaches lists the caches the listener keeps. It is the one place that names them, so
// rewatch, self-health, /healthz and shutdown each reach every cache. ciStore is nil on a listener
// that mounts no GitHub webhook route, and then no site reaches a CI cache.
func listenerCaches(registry *store.Registry, sessions *session.SessionRegistry, ciStore *cistore.Store) []listenerCache {
	caches := []listenerCache{
		{name: "interest", cache: registry},
		{name: "session", cache: sessions},
	}
	if ciStore != nil {
		caches = append(caches, listenerCache{name: "CI", cache: ciStore})
	}
	return caches
}

// sessionMayBeLive is the reapers' test of a session: it answers false only for a session that is
// gone (session.Gone), and the session bucket itself is what says so (liveSessionEntry: a record
// the cache holds, or else a read of the session's own key). So a reaper takes nothing from a
// session that has merely stopped answering - its interests and its role claims stand for the
// whole gone horizon - nor from one another listener registered a moment ago, nor from one the
// session cache does not hold because it has not read its bucket yet or is reading it again. Its
// error is a read that did not answer, on which the reaper keeps what it would have removed for
// its next cycle.
//
// A reaper answers no caller, so it holds no request context and reads under context.Background()
// with roleHolderReadTimeout as its bound, the same bound role delivery's resolution waits.
func sessionMayBeLive(sessions *session.SessionRegistry, sessionID string) (bool, error) {
	_, _, err := liveSessionEntry(context.Background(), sessions, sessionID, roleHolderReadTimeout, notGone)
	if errors.Is(err, nats.ErrKeyNotFound) {
		return false, nil
	}
	return true, err
}

// roleHolderMayBeLive is the role reaper's liveness check (sessionMayBeLive), logging a read that
// did not answer, on which the claim is kept for the next cycle.
func roleHolderMayBeLive(logger *logging.Logger, sessions *session.SessionRegistry, sessionID string) bool {
	live, err := sessionMayBeLive(sessions, sessionID)
	if err != nil {
		logger.Warn("role claim reaper kept a claim whose holder's session it could not read", slog.String("session_id", sessionID), slog.String("error", err.Error()))
	}
	return live
}

// defaultSessionReapInterval is how often the session reaper runs when ENVOY_SESSION_REAP_INTERVAL
// names no other period. A record is deleted within one period of reaching the gone horizon, and
// the bucket's maximum age is settled once per period, so a raise made elsewhere reaches this
// listener that soon (session.ReapSessions): a minute, which is what the two-minute adoption this
// change is accepted on needs, and what the setting's documentation states.
const defaultSessionReapInterval = time.Minute

// defaultRoleClaimReapInterval is how often the role claim reaper runs when
// ENVOY_ROLE_CLAIM_REAP_INTERVAL names no other period: the five minutes the fixed pass this
// change replaces ran at, which the deployment keeps.
//
// It is not the session reaper's period, short though that is. A session's record reads gone
// when the session bucket expires its key as well as when its last registration passes the gone
// horizon, and the bucket ships with a five-minute maximum age (session.BucketMaxAge), so a
// stalled session's key is gone five minutes after its last heartbeat and the reaper then finds
// its holder gone. Judged every minute, a stalled holder would lose its role up to five times
// sooner than the fixed pass this change replaces, which is the opposite of what this change is
// for; the raise of that maximum age past the gone horizon is dispatch://INFRA-887's, and it
// cannot land first. The grace a restored claim gets is measured from the listener's own start
// against the live window (store.Registry.releaseReadExpiredRoleClaim, which the reaper's pass
// hands the claim it read), not counted in passes, so the period does not move it; what the
// period decides is how long after a holder is gone its role is free.
const defaultRoleClaimReapInterval = 5 * time.Minute

// warnIgnoredEnv writes one WARN naming an environment variable this listener no longer reads,
// for each such variable present in the environment at all - an empty value is a variable an
// operator still sets, so it is still reported. reason says what the listener stopped doing, so
// the line tells the operator why the setting configures nothing now, and every line ends by
// asking for its removal from the deployment. The listener starts either way: a deployment that
// still carries the setting must keep running until its operator catches up, which is the
// warn-and-continue line envDuration takes for a value it cannot use. The message and its fields
// are what an operator's log filter matches, so they name the variable exactly as it is written
// in the environment.
func warnIgnoredEnv(logger *logging.Logger, name string, reason string) {
	value, set := os.LookupEnv(name)
	if !set {
		return
	}
	logger.Warn(name+" is set and ignored: "+reason+"; remove it from the deployment",
		slog.String("value", value))
}

// rewatchListenerKVWatchers recreates cache watchers that are not represented
// by bus.Client subscriptions. It attempts every one so a failed rebuild of one
// cannot leave the other caches permanently stale too.
func rewatchListenerKVWatchers(conn *nats.Conn, caches []listenerCache) error {
	var errs []error
	for _, c := range caches {
		if err := c.cache.Rewatch(conn); err != nil {
			errs = append(errs, fmt.Errorf("rewatch %s cache: %w", c.name, err))
		}
	}
	return errors.Join(errs...)
}

// endListenerKVWatchers ends every cache's watchers at shutdown, all at once and within ctx, while
// the connection still carries their consumer deletes (kvwatch.Watcher.End); the bus's Drain runs
// it as a drain hook before it drains the connection.
func endListenerKVWatchers(ctx context.Context, caches []listenerCache) {
	var ended sync.WaitGroup
	for _, c := range caches {
		ended.Go(func() { c.cache.EndWatch(ctx) })
	}
	ended.Wait()
}

// hookListenerKVWatchers ties every cache's KV watcher to the client's connection. KV watchers are
// not bus subscriptions, so a disconnect tells each cache that its copy stopped following its
// bucket (nats.go keeps a watcher's subscription open through an outage, so nothing else would),
// every recovered connection rewatches them, which reads each bucket again, and the drain ends
// them before the connection's own.
func hookListenerKVWatchers(client *bus.Client, caches []listenerCache) {
	client.AddDisconnectHook(func() {
		for _, c := range caches {
			c.cache.Disconnected()
		}
	})
	client.AddReconnectHook(func(conn *nats.Conn) error {
		return rewatchListenerKVWatchers(conn, caches)
	})
	client.AddDrainHook(func(ctx context.Context) {
		endListenerKVWatchers(ctx, caches)
	})
}

// isUnrecoverableSelfHealthFailure distinguishes state that must be rebuilt
// from transient JetStream deadlines. Rebuild only while the NATS client is
// connected; a disconnected client owns its own infinite reconnect loop. A
// missing bucket is terminal too: a rebuild cannot create a bucket, so its
// failed rebuilds end in a restart, whose store Open creates it again. A cache
// whose copy needs a read (kvCache.NeedsRead) is rebuilt as a stopped watcher
// is: nothing but a Rewatch reads its bucket again, for instance after the
// reconnect hook's Rewatch failed while the old watcher survived.
func isUnrecoverableSelfHealthFailure(err error, client *bus.Client, caches []listenerCache) bool {
	if err == nil || !client.Connected() {
		return false
	}
	if errors.Is(err, nats.ErrConsumerNotFound) || errors.Is(err, errDurableUnbound) ||
		errors.Is(err, nats.ErrConnectionClosed) || errors.Is(err, nats.ErrStreamNotFound) {
		return true
	}
	for _, c := range caches {
		if c.cache.WatchErr() != nil || c.cache.NeedsRead() {
			return true
		}
	}
	return false
}

// rebuildListenerDependencies rewatches the KV caches and, when the durable probe reports the
// consumer gone or not delivering to this listener's subscription, subscribes the durable again
// (resubscribe), creating it when it is gone. The resubscribe reports success only once the server
// reports this listener bound, so the probe that follows a rebuild reads a settled consumer rather
// than the gap in which the subscription is being replaced. Only the receiving pod's probe reports
// the consumer gone or unbound; a standby's never does, so a standby's rebuild touches nothing but
// the caches.
//
// Nothing here touches relay ownership: the receive term alone elects the pod that relays its
// machine's session consumers (durableReceiver.begin and durableReceiver.end,
// sessionRelays.setOwned). A resubscribe reports success for a pod with no term as well
// (durableReceiver.resubscribe), so a rebuild that set the flag would have a standby relay beside
// the holder.
func rebuildListenerDependencies(
	ctx context.Context,
	client *bus.Client,
	caches []listenerCache,
	durableProbe func() error,
	resubscribe func(context.Context) error,
) error {
	if !client.Connected() {
		return nats.ErrConnectionClosed
	}
	err := rewatchListenerKVWatchers(client.Conn, caches)
	if durableProbe == nil {
		return err
	}
	probeErr := durableProbe()
	if !errors.Is(probeErr, nats.ErrConsumerNotFound) && !errors.Is(probeErr, errDurableUnbound) {
		return err
	}
	if durableErr := resubscribe(ctx); durableErr != nil {
		return errors.Join(err, fmt.Errorf("resubscribe the durable consumer: %w", durableErr))
	}
	return err
}

// disconnectedFailureThreshold bounds how many consecutive probe failures the self-health monitor
// tolerates while the NATS client reads as not connected (RECONNECTING or CLOSED, mid-recovery).
// nats.go retries a dropped connection every second (ReconnectWait) on its own, so a client still
// disconnected after this many intervals is not inside a brief reconnect gap: the transport itself
// has not come back (a NATS restart or deploy taking longer than expected, or a local forward that
// keeps accepting connections without actually reaching the server), and nats.go's in-place
// reconnect has no way to notice that and give up on its own. At the production interval (30s)
// this is 10 minutes of reading as disconnected. A transport that stalls the open connection
// rather than closing it reads as connected until nats.go's ping check marks it stale (its
// defaults: a ping every 2 minutes, 2 outstanding), so there the exit comes about 15 minutes
// after the stall.
const disconnectedFailureThreshold = 20

// runSelfHealthMonitor leaves transient dependency timeouts degraded while
// NATS reconnects. A terminal watcher, closed KV handle, or missing durable
// consumer is rebuilt immediately, and a rebuild that reports success is
// probed at once: a healthy probe is a recovery and resets the count, so
// separate faults that each rebuild repairs never add up. A failed probe
// there, transient or terminal, keeps the count and becomes the error the
// terminal line names. Only a terminal failure still there after threshold
// consecutive rebuilds enters the bounded shutdown path so Docker can replace
// an unrecoverable listener.
// A probe that lands in a reconnect gap is that transient case: it fails at
// once with "outbound buffer limit exceeded" while the bus is disconnected (its
// reconnect buffer is off), or at its deadline when the reconnect lands during
// the probe. Either is one WARN, then "self-health recovered" from the next -
// unless connected reads false for disconnectedFailureThreshold consecutive
// probes, in which case the bounded shutdown path runs for this too. That
// count is its own, advanced only while connected reads false and reset the
// moment it reads true again, so it never mixes with a transient failure that
// happens while the client is connected (an unclassified JetStream timeout,
// say), which keeps retrying under the unbounded path above.
func runSelfHealthMonitor(
	ctx context.Context,
	logger *logging.Logger,
	probe func() error,
	isUnrecoverable func(error) bool,
	connected func() bool,
	rebuild func() error,
	terminate func(),
	interval time.Duration,
	threshold int,
) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	failures := 0
	terminalFailures := 0
	disconnectedFailures := 0
	// readsDisconnected classifies every probe result, the one right after a rebuild included, so
	// the disconnected count is the same run of probes wherever in the loop they fall.
	readsDisconnected := func() bool { return connected != nil && !connected() }
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		err := probe()
		if err != nil {
			failures++
			disconnected := readsDisconnected()
			bound := threshold
			if disconnected {
				bound = disconnectedFailureThreshold
			}
			logger.Warn("self-health probe failed", slog.Int("consecutive", failures), slog.Int("threshold", bound), slog.String("error", err.Error()))
			if disconnected {
				disconnectedFailures++
				terminalFailures = 0
				if disconnectedFailures >= disconnectedFailureThreshold {
					logger.Error("self-health disconnected failure threshold exceeded; terminating for restart",
						slog.Int("consecutive", disconnectedFailures),
						slog.String("error", err.Error()),
					)
					if terminate != nil {
						terminate()
					}
					return
				}
				continue
			}
			disconnectedFailures = 0
			if isUnrecoverable == nil || !isUnrecoverable(err) {
				terminalFailures = 0
				continue
			}
			terminalFailures++
			if rebuild != nil {
				if rebuildErr := rebuild(); rebuildErr != nil {
					// The rebuild runs under ctx, so a shutdown that cancels ctx mid-rebuild cuts it
					// with context.Canceled. That error is the shutdown, not a failed rebuild, and an
					// ordered shutdown writes no ERROR, so the monitor stops here without logging it.
					if ctx.Err() != nil {
						return
					}
					logger.Error("self-health rebuild failed", slog.Int("consecutive", terminalFailures), slog.String("error", rebuildErr.Error()))
				} else {
					logger.Info("self-health rebuild started", slog.Int("consecutive", terminalFailures))
					// Whatever this probe reads, transient or terminal, the count keeps its
					// increment: only a healthy read is a recovery.
					if err = probe(); err != nil {
						logger.Warn("self-health probe after rebuild failed", slog.Int("consecutive", terminalFailures), slog.String("error", err.Error()))
						if readsDisconnected() {
							// The connected branch above reset the count, so this probe starts it.
							disconnectedFailures = 1
						}
					}
				}
			}
		}
		if err == nil {
			if failures > 0 {
				logger.Info("self-health recovered", slog.Int("prior_consecutive_failures", failures))
			}
			failures = 0
			terminalFailures = 0
			disconnectedFailures = 0
			continue
		}
		if terminalFailures < threshold {
			continue
		}
		logger.Error("self-health terminal failure threshold exceeded; terminating for restart",
			slog.String("error", err.Error()),
		)
		if terminate != nil {
			terminate()
		}
		return
	}
}

// errCacheNeedsRead is the self-health failure of a cache whose copy does not follow its bucket
// with no read under way (kvCache.NeedsRead).
var errCacheNeedsRead = errors.New("the cache does not follow its bucket and no read of it is under way")

// checkSelfHealth pings the KV-backed caches, reports one whose copy needs a read, and runs the
// optional durable consumer probe. Returns the first error encountered, or nil when all are
// healthy. A copy that needs a read answers its Ping, so without its own failure here the monitor
// would never rebuild it (isUnrecoverableSelfHealthFailure).
func checkSelfHealth(caches []listenerCache, durable func() error) error {
	for _, c := range caches {
		if err := c.cache.Ping(); err != nil {
			return fmt.Errorf("%s kv: %w", c.name, err)
		}
		if c.cache.NeedsRead() {
			return fmt.Errorf("%s kv: %w", c.name, errCacheNeedsRead)
		}
	}
	if durable != nil {
		if err := durable(); err != nil {
			return fmt.Errorf("durable consumer: %w", err)
		}
	}
	return nil
}

// sessionHealthFields returns observability fields about the session cache for
// /healthz. Kept separate from the handler so it stays cheap (no Keys()/List(),
// just in-memory reads) and unit-testable.
func sessionHealthFields(sessions *session.SessionRegistry) map[string]interface{} {
	watchError := ""
	if err := sessions.WatchErr(); err != nil {
		watchError = err.Error()
	}
	return map[string]interface{}{
		"session_cache_ready": sessions.CacheReady(),
		"session_cache_size":  sessions.CacheSize(),
		"session_watch_error": watchError,
		// The maximum age in use: the age the session bucket really carries, which the registry
		// reads back after it writes the deployment's own (ENVOY_SESSION_MAX_AGE) and again on
		// every pass of the session reaper. With the setting unset it is the registry's default,
		// so the field is always present once the registry is open, and an operator can see from
		// here whether a record stands long enough for the gone horizon to be reached.
		"session_max_age": sessions.TTL().String(),
	}
}

// healthzDeadline bounds one /healthz answer. A dependency that has not answered when it passes
// makes the answer 503, so a readiness probe whose own timeout is longer always gets an answer.
const healthzDeadline = 5 * time.Second

// healthAnswer is one /healthz answer: its HTTP status and its JSON body.
type healthAnswer struct {
	code int
	body map[string]interface{}
}

// healthzHandler is the listener's /healthz, the readiness answer of every pod of a machine id. It
// answers 503 "starting" until main publishes deps, which it does once NATS is connected, the
// webhook and /v1 gates are open over warm caches and the receive loop has started, and, on a
// listener configured with an OIDC issuer, until the issuer's discovery has succeeded (verifier).
// Then it answers 200 "healthy" once every dependency answers, naming this pod's "role": "active"
// while it receives the durable (it holds the lease and is subscribed, or it runs without a lease
// and receives), with the durable's lag (num_pending, num_ack_pending), or "standby" otherwise,
// naming the holder in "active_instance" when the lease record names one and, while it holds the
// lease but does not receive yet, what it waits for in "receive". A pod that is the machine's
// deliverer and does not receive yet - it holds the lease, or runs without one, or has read no
// holder at all - is 200 "degraded" rather than healthy, however well its dependencies answer:
// nothing of the durable reaches a session until it receives. It stays ready, since a readiness
// probe that takes any 200 keeps routing /v1 to it, as it does to a standby. A standby of another
// holder is ready and healthy: it serves /v1, the webhooks and the role lane, and takes over
// delivery when the lease comes free. A listener without the lease's grant says so in "lease"
// (listenerLeaseUnavailable). It answers 503 "unhealthy" when NATS is unavailable, the role
// lane's own client is not connected or holds no valid core subscription, since no role message
// is forwarded then, a cache's watcher has stopped, a bus check did not answer within
// healthzBudget, or on the active pod when its subscription is inactive, the durable consumer is
// gone, or the durable consumer is "not bound to this listener's subscription" because it was
// recreated server-side and now delivers elsewhere; and 200
// "degraded" for a transient KV failure the self-health monitor and the NATS reconnect retry, an
// active pod's durable lag that could not be read, or a KV watch rebuild stalled past
// bus.RepairGrace (below). A pod that holds the lease, or runs without one, and fails to read,
// create, correct or subscribe to the durable answers 200 "degraded" with the failure in "error"
// and how long it has failed in "receive_failing_ms", and 503 "unhealthy" once that has lasted
// the alarm bound (durableTakeoverAlarmAfter): a holder gives the lease up then and is a standby
// again. A listener that mounts no GitHub webhook route keeps no CI cache, and its answer
// carries "ci_cache": "not_applicable". The checks run under one healthzDeadline, and an answer
// they have not reached by then is 503 "unhealthy".
//
// A rewatch of the KV caches that keeps failing after a reconnect ends no watcher and clears no
// cache, so every other check still passes while the caches follow buckets they no longer watch
// (INFRA-743). The handler therefore reads the client's HookStall, how long the bus's reconnect
// hooks have been due without a run that succeeded, and reports the dependency "KV watch rebuild"
// once it reaches bus.RepairGrace. It reads it last, below every 503 check and immediately before
// either 200 answer, an active pod's and a standby's alike (okAnswer), so a stall never stands in
// for a worse fault: a durable consumer that is gone, unbound, or whose info read did not answer,
// a subscription that is not valid and a takeover that has failed past durableTakeoverAlarmAfter
// are each still 503, since a listener that receives nothing is not the case the 200 is for. That
// answer is 200 "degraded" and not 503, because the probes act on /healthz: the ECS container
// health check is curl -sf /healthz, every 30 s with 3 retries
// (infra/aws/rendering/listener.go:202-203), and GKE's readiness probe reads it too
// (infra/gcp/dispatch_kube.go, readyProbe). A 503 would unroute, or replace, a pod whose caches
// still hold what they warmed up with, and its replacement rebuilds into the same failing rewatch
// with cold caches that warm up empty, whose fanout then acknowledges general-topic messages as
// unmatched and loses them (delivery.go, fanoutDelivery); a standby taken out of service this way
// is also one pod less to take the lease over. The paging signal for this state is the bus's ERROR
// "envoy nats reconnect hooks have not succeeded" (internal/bus/recovery.go, reportHookFailure),
// which the AWS rendering counts as a log metric filter (infra/aws/rendering/metricfilters.go). A
// shorter stall is reported nowhere here: the bus runs the hooks again within seconds of a failure
// (internal/bus/recovery.go, recoverUntilHealthy), so a rebuild under the bound is being repaired,
// and a listener would otherwise read as degraded over every reconnect. The stall is one
// bus.Client's, naming no cache, so a listener that keeps no CI cache needs no branch of its own.
//
// The checks that go to the bus share one healthzBudget taken from that deadline: the flush, each
// cache's KV ping and the active pod's durable consumer info read. A ping or that read that has not
// answered when it ends (healthzPing) is 503 unhealthy naming the dependency and the context's
// deadline, so a stall in JetStream alone, which the flush's core PING does not see, fails the
// container health check inside the budget, as the 10 s JetStream wait it replaces failed it by
// running past the check's timeout. A check that answered with a failure keeps its answer.
//
// The role claim view is reported too, below every 503 check and as a 200 "degraded" naming it
// (store.Registry.RoleViewErr, read in okAnswer): until its watcher's first scan has delivered
// every claim the view answers no read, so GET /v1/sessions and GET /v1/registry/<session_id>
// answer 503 while every other check here passes, and the report reads the very state those
// routes read. Every pod of a machine serves those routes, the lease holder and each standby
// alike (openV1Routes), so the active pod's 200 and a standby's each carry it. It is read there
// so that it hides nothing: every check that can answer 503 has passed by then, so a degraded
// view never takes the place of a fault that does not repair itself. A KV ping or durable info
// read that answered with a transient failure is reported before it, as its own 200 "degraded"
// carrying only status and error (dependencyHealth), and returns before the checks after it. The
// role claim view's degraded answer instead carries the healthy body's own fields, so a reader
// loses no counter while the view fills, and it stands beside the other reports a 200 carries - a
// standby's failing takeover, an active pod's durable lag that could not be read, the KV watch
// rebuild stall, which is named after it - rather than in place of any of them. Degraded and not
// unhealthy, because that view fills itself through its watcher's own rescan and a 503 would take
// a serving listener out of a load balancer for a state that repairs itself: a readiness check
// runs curl -f or -sf against /healthz, which a 200 passes. For the same reason the view is no
// part of Ping or WatchErr, whose terminal failures the self-health monitor self-terminates the
// listener over: a restart completes no scan that keeps timing out.
func healthzHandler(deps *atomic.Pointer[listenerDeps], verifier *apiVerifier) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), healthzDeadline)
		defer cancel()
		answered := make(chan healthAnswer, 1)
		go func() { answered <- listenerHealth(ctx, deps.Load(), verifier) }()
		var answer healthAnswer
		select {
		case answer = <-answered:
		case <-ctx.Done():
			answer = healthAnswer{http.StatusServiceUnavailable, map[string]interface{}{
				"status": "unhealthy",
				"error":  "health checks did not finish within " + healthzDeadline.String(),
			}}
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(answer.code)
		_ = json.NewEncoder(w).Encode(answer.body)
	}
}

// listenerHealth runs /healthz's checks over d, nil until main publishes it, within ctx, its bus
// checks within healthzBudget of it.
func listenerHealth(ctx context.Context, d *listenerDeps, verifier *apiVerifier) healthAnswer {
	if d == nil || !verifier.ready() {
		return healthAnswer{http.StatusServiceUnavailable, map[string]interface{}{"status": "starting"}}
	}
	unhealthy := func(body map[string]interface{}) healthAnswer {
		body["status"] = "unhealthy"
		return healthAnswer{http.StatusServiceUnavailable, body}
	}
	ctx, cancel := context.WithTimeout(ctx, healthzBudget)
	defer cancel()
	if err := d.client.Conn.FlushWithContext(ctx); err != nil {
		return unhealthy(map[string]interface{}{"error": "nats unavailable"})
	}
	if d.roleLane != nil && (!d.roleLane.Connected() || !d.roleLane.CoreSubOK()) {
		return unhealthy(map[string]interface{}{"error": "role lane subscription inactive"})
	}
	for _, c := range d.caches {
		if err := c.cache.WatchErr(); err != nil {
			return dependencyHealth(c.name+" KV watcher", err, true)
		}
	}
	for _, c := range d.caches {
		if answered, err := healthzPing(ctx, c.cache); err != nil {
			return dependencyHealth(c.name+" KV", err, !answered || c.cache.WatchErr() != nil || errors.Is(err, nats.ErrConnectionClosed))
		}
	}
	response := map[string]interface{}{"status": "healthy"}
	for k, v := range sessionHealthFields(d.sessions) {
		response[k] = v
	}
	if d.ciStore == nil {
		response["ci_cache"] = "not_applicable"
	}
	report := d.receiver.report()
	if report.NoLease {
		response["lease"] = listenerLeaseUnavailable
	}
	if report.State != receiveActive {
		response["role"] = "standby"
		if report.Lease.ActiveInstance != "" {
			response["active_instance"] = report.Lease.ActiveInstance
		}
		if report.State != receiveStandby {
			response["receive"] = report.State.String()
		}
		if report.Deliverer {
			// This pod is the machine's deliverer and does not receive yet (receiveReport.Deliverer):
			// nothing of the durable reaches a session until that changes, which no answer of this
			// pod may call healthy. It stays 200, as a standby does, so a readiness probe keeps
			// sending it /v1.
			response["status"] = "degraded"
		}
		if report.Failing > 0 {
			response["error"] = "durable consumer unavailable: " + report.FailingReason
			response["receive_failing_ms"] = report.Failing.Milliseconds()
			if report.Stuck {
				return unhealthy(response)
			}
			response["status"] = "degraded"
		}
		return okAnswer(response, d)
	}
	response["role"] = "active"
	if !report.SubscriptionValid {
		return unhealthy(map[string]interface{}{"role": "active", "error": "subscription inactive"})
	}
	consumerInfo, err := d.client.JS().ConsumerInfo(bus.Stream, d.receiver.consumer, nats.Context(ctx))
	switch {
	case errors.Is(err, nats.ErrConsumerNotFound):
		return dependencyHealth("durable consumer", err, true)
	case err != nil && ctx.Err() != nil:
		// A read ctx ended did not answer, the info read's own form of healthzPing's unanswered.
		return dependencyHealth("durable consumer", err, true)
	case err != nil:
		response["status"] = "degraded"
		response["error"] = "durable consumer unavailable: " + err.Error()
	case consumerInfo.Config.DeliverSubject != report.Subject:
		// One read and no confirming second one, unlike the self-health probe's: the endpoint
		// reports the fault the moment a request finds it, and a request landing in a
		// resubscribe's own gap simply answers 503 until the next one.
		return dependencyHealth("durable consumer", errDurableUnbound, true)
	default:
		response["num_pending"] = consumerInfo.NumPending
		response["num_ack_pending"] = consumerInfo.NumAckPending
	}
	return okAnswer(response, d)
}

// healthzBudget bounds the bus checks of one /healthz answer.
const healthzBudget = 3 * time.Second

// healthzPing is cache's Ping as a /healthz request holding ctx waits for it (bus.Bounded): nats.go's
// KV Status takes no context, so a ping that has not answered when ctx ends is answered false, with
// ctx's error, the "context deadline exceeded" a ping that waited out the JetStream MaxWait failed
// with. The abandoned ping runs on to its answer or that wait; what it can still do then - record a
// recreated bucket's error on its watcher, or prune the session cache's last-seen times - is what
// any later ping would do.
func healthzPing(ctx context.Context, cache kvCache) (answered bool, err error) {
	_, answered, err = bus.Bounded(ctx, func() (struct{}, error) { return struct{}{}, cache.Ping() })
	return answered, err
}

// dependencyHealth is /healthz's answer for a dependency that failed with err: 503 "unhealthy"
// when the failure is terminal, else 200 "degraded".
func dependencyHealth(dependency string, err error, terminal bool) healthAnswer {
	statusCode := http.StatusOK
	status := "degraded"
	if terminal {
		statusCode = http.StatusServiceUnavailable
		status = "unhealthy"
	}
	return healthAnswer{statusCode, map[string]interface{}{
		"status": status,
		"error":  dependencyUnavailable(dependency, err),
	}}
}

// okAnswer is /healthz's 200 answer over response, and the last thing the endpoint decides. Only
// the 200 answers come through here, below every 503 check, so neither report it adds ever stands
// in for a worse fault.
//
// It reads the role claim view first (store.Registry.RoleViewErr): until that view answers, the
// two routes that report a session's roles - GET /v1/sessions and GET /v1/registry/<session_id> -
// answer 503 while every other check passes, and every pod of a machine serves them, so an active
// pod's answer and a standby's each carry it.
//
// Then the rewatch: a rewatch of the KV caches that keeps failing after a reconnect ends no
// watcher and clears no cache, so every other check answers while the caches follow buckets they
// no longer watch (INFRA-743); this reads the client's HookStall for exactly that. A stall that
// has reached bus.RepairGrace names the "KV watch rebuild", and a shorter one is reported
// nowhere: the bus runs the hooks again within seconds of a failure (internal/bus/recovery.go,
// recoverUntilHealthy), so a rebuild under the bound is being repaired and a listener would
// otherwise read as degraded over every reconnect.
//
// Each report makes the answer "degraded" and is carried beside, not instead of, one the pod
// already has - a standby's failing takeover, an active pod's durable lag that could not be read,
// the role claim view - in the order they are read here (carryReport): every one of them is this
// same 200 "degraded", and an operator reading the answer wants the whole of what is wrong, not
// whichever part the endpoint found last.
func okAnswer(response map[string]interface{}, d *listenerDeps) healthAnswer {
	if err := d.registry.RoleViewErr(); err != nil {
		carryReport(response, "role claim view", err)
	}
	stall, hookErr := d.client.HookStall()
	if stall < bus.RepairGrace {
		return healthAnswer{http.StatusOK, response}
	}
	rebuild := fmt.Errorf("no rebuild has succeeded for %s", stall.Round(time.Second))
	if hookErr != nil {
		// The last failure says what is broken; the stall, for how long. A run still in flight past
		// the grace has no error of its own, and a dependency report needs one, so then the stall
		// is the whole of it.
		rebuild = fmt.Errorf("%w (%w)", hookErr, rebuild)
	}
	carryReport(response, "KV watch rebuild", rebuild)
	return healthAnswer{http.StatusOK, response}
}

// carryReport adds a dependency's failure to a 200 /healthz answer: the answer is "degraded", and
// a report it already carries keeps its place, with this one named after it.
func carryReport(response map[string]interface{}, dependency string, err error) {
	reported := dependencyUnavailable(dependency, err)
	if carried, ok := response["error"].(string); ok {
		reported = carried + "; " + reported
	}
	response["status"] = "degraded"
	response["error"] = reported
}

// dependencyUnavailable is the `error` field a /healthz answer gives for a dependency, so the
// degraded answer that keeps the healthy body's fields reads exactly as dependencyHealth's does.
func dependencyUnavailable(dependency string, err error) string {
	return dependency + " unavailable: " + err.Error()
}

// webhookRoute is one configured webhook path and the handler it serves over the only
// dependencies a webhook uses: the publisher it publishes through and the CI store the GitHub
// route records checks in, which main opens only when that route is configured.
//
// The handler is built here, and not before webhookRoutes is called, because neither is open when
// main registers the paths: the routes answer 503 until they are.
type webhookRoute struct {
	path    string
	handler func(webhook.Publisher, *cistore.Store) http.Handler
}

// webhookRoutes lists the webhook routes the configuration enables. main registers the paths
// before NATS is up, so they answer 503 while it connects, and builds the handlers once NATS and,
// for the GitHub route, the CI store are open.
//
// Every route publishes through the rename publisher, which costs the Slack and the Ghost Wispr
// routes nothing: a pair names a GitHub repository, and a subject that is not
// `notifications.github.<owner>.<name>.…` names none, so those two publish exactly as they did.
func webhookRoutes(cfg *webhook.WebhookConfig) []webhookRoute {
	var routes []webhookRoute
	if github := cfg.GitHub; github != nil {
		routes = append(routes, webhookRoute{"/webhook/github", func(publisher webhook.Publisher, ciStore *cistore.Store) http.Handler {
			return webhook.GitHubHandler(github.Secret, github.MentionTrigger, github.ReviewerAppID, publisher, ciStore)
		}})
	}
	if slack := cfg.Slack; slack != nil {
		routes = append(routes, webhookRoute{"/webhook/slack", func(publisher webhook.Publisher, _ *cistore.Store) http.Handler {
			return webhook.SlackHandler(slack.Secret, publisher)
		}})
	}
	if ghostWispr := cfg.GhostWispr; ghostWispr != nil {
		routes = append(routes, webhookRoute{"/webhook/ghostwispr", func(publisher webhook.Publisher, _ *cistore.Store) http.Handler {
			return webhook.GhostWisprHandler(ghostWispr.Secret, publisher)
		}})
	}
	return routes
}

// newHTTPServer is the listener's HTTP server over handler. Every route reads its header and body
// within ReadTimeout, 10 s from the request's start, and writes its answer within WriteTimeout,
// 10 s from the end of the header. The GitHub webhook route alone moves its own connection's
// deadlines (webhook.GitHubHandler), since a large delivery's body can take longer than that to
// arrive; every other route keeps these.
func newHTTPServer(handler http.Handler) *http.Server {
	return &http.Server{
		Handler:      handler,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
		IdleTimeout:  60 * time.Second,
	}
}

// startingGate answers 503 "service starting" until open hands it the mux to serve, and logs each
// request it refuses: that line is the listener's only record of a caller it turned away while it
// started. main builds each gate's mux only once every dependency of its handlers is open, so a
// request never reaches a handler over a dependency that is not there. The two list reads, GET
// /v1/sessions and GET /v1/interests/, get the refilling answer the routes give while their copies
// cannot be served (writeListRefilling), since before /v1 opens no copy has been read: a caller
// reads one answer for "the list is not ready" whether the gate or the route gives it.
type startingGate struct {
	name   string
	logger *logging.Logger
	mux    atomic.Pointer[http.ServeMux]
}

// newStartingGate returns a closed gate whose refusals log under name.
func newStartingGate(name string, logger *logging.Logger) *startingGate {
	return &startingGate{name: name, logger: logger}
}

func (g *startingGate) open(mux *http.ServeMux) { g.mux.Store(mux) }

func (g *startingGate) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	mux := g.mux.Load()
	if mux == nil {
		g.logger.Warn("request refused while starting",
			slog.String("gate", g.name),
			slog.String("method", r.Method),
			slog.String("path", r.URL.Path),
		)
		if isListRead(r) {
			writeListRefilling(w)
			return
		}
		writeJSONError(w, http.StatusServiceUnavailable, "service starting")
		return
	}
	mux.ServeHTTP(w, r)
}

// isListRead reports whether r is one of the two list reads, GET /v1/sessions or GET
// /v1/interests/, whose closed-gate answer is the refilling one.
func isListRead(r *http.Request) bool {
	return r.Method == http.MethodGet && (r.URL.Path == "/v1/sessions" || r.URL.Path == "/v1/interests/")
}

// openWebhooks builds the webhook routes over the publisher they publish through and the CI store
// (nil without the GitHub route) and opens the webhook gate onto them. A webhook waits on nothing
// else, not even the interest and session caches /v1 waits on, and GitHub does not redeliver a
// delivery refused while they warm.
func openWebhooks(gate *startingGate, hooks []webhookRoute, publisher webhook.Publisher, ciStore *cistore.Store) {
	routes := http.NewServeMux()
	for _, hook := range hooks {
		routes.Handle(hook.path, hook.handler(publisher, ciStore))
	}
	gate.open(routes)
}

// openV1Routes builds the /v1 routes over the stores, opens the /v1 gate onto them and publishes
// the stores to served, which the list gauges read (registerListGauges). No /v1 handler reads the
// durable consumer, so /v1 serves on every pod of a machine, the lease holder and each standby
// alike, and callers that resolve the listener's name reach any of them, so a list /v1 answers
// then has its sample.
func openV1Routes(gate *startingGate, ready *listenerDeps, served *atomic.Pointer[listenerDeps], machineID string, logger *logging.Logger, listeningAt time.Time) {
	routes := http.NewServeMux()
	registerV1Routes(routes, ready, machineID, logger)
	gate.open(routes)
	served.Store(ready)
	logger.Info("envoy-listener /v1 open", slog.Int64("since_listening_ms", time.Since(listeningAt).Milliseconds()))
}

// listenerInstance names this process among the listeners that run for one machine id: the
// ENVOY_INSTANCE_ID environment variable when it is set, else the host name, which in Kubernetes is
// the pod's name.
func listenerInstance() (string, error) {
	if instance := os.Getenv("ENVOY_INSTANCE_ID"); instance != "" {
		return instance, nil
	}
	hostname, err := os.Hostname()
	if err != nil {
		return "", fmt.Errorf("instance: ENVOY_INSTANCE_ID is unset and the host name is unreadable: %w", err)
	}
	if hostname == "" {
		return "", errors.New("instance: ENVOY_INSTANCE_ID is unset and the host name is empty")
	}
	return hostname, nil
}

// startRetry is how long a start waits between attempts to reach NATS while it cannot answer: the
// connect (connectListener), each store's buckets (openStartStore) and the listener state bucket
// (openDeliveryLease). /healthz answers 503 "starting" meanwhile, and a SIGTERM or SIGINT ends the
// process at once, since the start has installed no handler for either yet.
const startRetry = 2 * time.Second

// listenerGrant names the grant that gives the listener's NATS user its KV buckets, for the line
// that reports a bucket the grant refuses.
const listenerGrant = "infra/core ListenerPermissions"

// natsConnectionError reports whether err is a failure to reach NATS, or to get an answer from it,
// that a later attempt can get past: no server answered the dial or closed it during the
// handshake, a request ran out of time or found no responder, the connection closed or is
// reconnecting, or JetStream answered that it is not available yet. Any other error, a credential
// or configuration the connect refuses before it dials among them, is not one.
func natsConnectionError(err error) bool {
	for _, transient := range []error{
		io.EOF, io.ErrUnexpectedEOF, syscall.ECONNREFUSED, syscall.ECONNRESET, context.DeadlineExceeded,
		nats.ErrNoServers, nats.ErrTimeout, nats.ErrNoResponders, nats.ErrNoStreamResponse,
		nats.ErrConnectionClosed, nats.ErrDisconnected, nats.ErrConnectionReconnecting, nats.ErrStaleConnection,
	} {
		if errors.Is(err, transient) {
			return true
		}
	}
	if netErr := net.Error(nil); errors.As(err, &netErr) {
		return true
	}
	var jsErr nats.JetStreamError
	if errors.As(err, &jsErr) {
		api := jsErr.APIError()
		return api != nil && api.Code == http.StatusServiceUnavailable
	}
	return false
}

// connectListener connects to NATS and reconciles the stream the listener owns
// (bus.ConnectOwningStream), as instance, at the deployment's replica count and byte limit and
// with the buckets it keeps (listenerStreamOptions). A NATS that cannot be reached
// (natsConnectionError) is tried again every startRetry for as long as it takes, so a listener
// started before its NATS waits for it rather than exiting. Any other failure ends the process
// after one ERROR naming it.
func connectListener(cfg config.Service, instance string, logger *logging.Logger) *bus.Client {
	for {
		options := append(listenerStreamOptions(cfg), bus.WithName("envoy-listener "+instance))
		client, err := bus.ConnectOwningStream(cfg.NATSURLs, options...)
		if err == nil {
			return client
		}
		if !natsConnectionError(err) {
			logger.Error("NATS connect failed", slog.String("error", err.Error()))
			os.Exit(1)
		}
		logger.Warn("NATS connect failed; retrying", slog.String("retry_in", startRetry.String()), slog.String("error", err.Error()))
		time.Sleep(startRetry)
	}
}

// openStartStore opens one of the listener's stores on client's connection with open, which opens
// buckets, until it succeeds. An open that fails for want of NATS (natsConnectionError) is retried
// every startRetry. An open the NATS user's grant refuses ends the process after one ERROR naming
// the bucket and the grant it lacks: nats-server drops a refused request and reports the refusal
// only as the connection's last error, so the open runs out of time, and a last error that changed
// across the open to a refusal of one of buckets' subjects is that refusal (bucketRefusal). Any
// other failure ends the process after one ERROR naming it.
func openStartStore[T any](client *bus.Client, logger *logging.Logger, what string, buckets []string, open func(*nats.Conn) (T, error)) T {
	for {
		conn := client.Conn
		before := conn.LastError()
		store, err := open(conn)
		if err == nil {
			return store
		}
		if bucket, violation := bucketRefusal(buckets, err, before, conn.LastError()); violation != nil {
			logger.Error("NATS refused KV bucket "+bucket+": the listener's NATS user lacks the grant for it",
				slog.String("bucket", bucket),
				slog.String("grant", listenerGrant),
				slog.String("error", violation.Error()),
			)
			conn.Close()
			os.Exit(1)
		}
		if !natsConnectionError(err) {
			logger.Error(what+" open failed", slog.String("buckets", strings.Join(buckets, ",")), slog.String("error", err.Error()))
			conn.Close()
			os.Exit(1)
		}
		logger.Warn(what+" open failed; retrying",
			slog.String("buckets", strings.Join(buckets, ",")),
			slog.String("retry_in", startRetry.String()),
			slog.String("error", err.Error()),
		)
		time.Sleep(startRetry)
	}
}

// bucketRefusal returns the bucket of buckets whose subjects the server refused a failed open, and
// the refusal: err when it is itself such a refusal, else after, the connection's last error once
// the open failed, when it differs from before, the last error before it. nats.go keeps one last
// error per connection, so a refusal another goroutine causes in between hides this one, and the
// open reads as a connection error that is retried and refused again.
func bucketRefusal(buckets []string, err, before, after error) (string, error) {
	for _, bucket := range buckets {
		if refusesBucket(err, bucket) {
			return bucket, err
		}
		if !sameLastError(before, after) && refusesBucket(after, bucket) {
			return bucket, after
		}
	}
	return "", nil
}

// refusesBucket reports whether err is the server's refusal of a publish to a subject of KV bucket
// bucket: a JetStream API call on its stream KV_<bucket>, or a write to one of its keys,
// $KV.<bucket>.<key>.
func refusesBucket(err error, bucket string) bool {
	if err == nil || !errors.Is(err, nats.ErrPermissionViolation) {
		return false
	}
	match := publishViolation.FindStringSubmatch(err.Error())
	if match == nil {
		return false
	}
	subject, stream := match[1], ".KV_"+bucket
	return strings.HasSuffix(subject, stream) || strings.Contains(subject, stream+".") ||
		strings.HasPrefix(subject, "$KV."+bucket+".")
}

// openDeliveryLease opens the listener state bucket and makes machineID's delivery lease over it
// for this process, instance, and returns it with the connection the bucket is on. The bucket is
// opened on a NATS connection of the lease's own (bus.Dial, as the user and through the proxy the
// environment names), which nats.go reconnects in place for the life of the process: the bus client
// replaces its connection after a close, and a handle on the replaced one would never renew the
// lease again. An open that fails for want of a connection is retried every startRetry, so a NATS
// that answers late does not end the start. An open the NATS user's grant refuses
// (errListenerStateRefused) returns no lease and no connection after one ERROR naming the bucket
// and the missing grant: the listener then delivers without a lease, as the machine's one
// deliverer receiving only while no other subscriber holds the durable, and says so in /healthz.
func openDeliveryLease(urls []string, replicas int, machineID, instance string, logger *logging.Logger) (*lease, *nats.Conn) {
	var conn *nats.Conn
	for {
		var err error
		if conn == nil {
			conn, err = bus.Dial("envoy-listener-lease "+instance, urls, os.LookupEnv)
		}
		if err == nil {
			var kv bus.KeyValue
			if kv, err = openListenerState(conn, replicas, listenerStateRequestWait); err == nil {
				return newLease(kv, machineID, instance, productionLeaseConfig(), time.Now, logger), conn
			}
		}
		if errors.Is(err, errListenerStateRefused) {
			logger.Error("listener lease unavailable: the NATS user lacks the grant for KV bucket "+bus.ListenerStateBucket+"; delivering without a lease",
				slog.String("bucket", bus.ListenerStateBucket),
				slog.String("grant", listenerGrant),
				slog.String("error", err.Error()),
			)
			conn.Close()
			return nil, nil
		}
		logger.Warn("listener state bucket open failed; retrying",
			slog.String("bucket", bus.ListenerStateBucket),
			slog.String("retry_in", startRetry.String()),
			slog.String("error", err.Error()),
		)
		time.Sleep(startRetry)
	}
}

// deliveryHandlingLimit is the longest one durable message's handling may take while it holds a
// delivery-ledger attempt: its session read, its ledger calls, and, on the role lane, the
// forward's publish and receipt wait. The delivery ledger's take-over age and the unplaceable
// deferrals' expiry (unplaceableDeferralExpiry) count on it.
const deliveryHandlingLimit = 30 * time.Second

// ledgerTakeoverMargin is how much longer than one message's handling (deliveryHandlingLimit) a
// pod waits on another pod's unresolved delivery attempt before it takes the attempt over: the
// other pod may still be settling it until then, and after it, that pod died without resolving it.
const ledgerTakeoverMargin = 5 * time.Second

// ledgerReconnectWait bounds how long the durable lane waits for the delivery ledger's connection
// to come back, to ask the ledger once more, before it NAKs the message for durableRetryDelay.
const ledgerReconnectWait = 5 * time.Second

// ledgerConnected is the delivery handler's ledgerReconnected over conn, the connection the
// listener state bucket is on: it waits, polling, until conn is connected, for at most
// ledgerReconnectWait and while ctx lasts. A listener without the bucket (conn nil) keeps its
// ledger in process and needs none.
func ledgerConnected(conn *nats.Conn) func(ctx context.Context) bool {
	if conn == nil {
		return nil
	}
	return func(ctx context.Context) bool {
		ctx, cancel := context.WithTimeout(ctx, ledgerReconnectWait)
		defer cancel()
		poll := time.NewTicker(50 * time.Millisecond)
		defer poll.Stop()
		for !conn.IsConnected() {
			select {
			case <-ctx.Done():
				return conn.IsConnected()
			case <-poll.C:
			}
		}
		return true
	}
}

// newDeliveryLedger makes the delivery ledger every lane asks before it forwards a role-lane
// message to a session, reports a refusal of one, or gives an unplaceable delivery up: the
// `delivery.` keys of the listener state bucket, shared by every pod of machineID, under the
// lease's boot, so a pod that takes the lease over neither forwards again what the pod before it
// delivered nor reports a refusal it already reported, and takes at once an attempt a former
// lease holder (lease.FormerHolder) only made. A former holder that released the lease is alive
// and its role lane is finishing the forward it had in flight, so its attempt is deferred until
// it has had ledgerFormerSettle to record, give up or report that forward, and only a holder
// taken over by force is taken over at once. Another pod's young attempt is otherwise
// deferred until it is requestLimit and ledgerTakeoverMargin old. A listener with no lease (its
// NATS user lacks the grant for the bucket) is the machine's one deliverer and keeps the ledger
// in process (dedupe.Memory), which a restart forgets.
//
// A record is keyed by the envelope's dedupe key, not its event id: fan-out normalization can
// produce envelopes with one event id and distinct dedupe keys. Each forward is recorded as
// attempted before it is sent and as delivered once the holder receipts it, so a receipt this
// pod gave up waiting for costs no second forward.
func newDeliveryLedger(deliveryLease *lease, machineID string, requestLimit time.Duration) dedupe.Ledger {
	takeoverAfter := requestLimit + ledgerTakeoverMargin
	if deliveryLease == nil {
		return dedupe.NewMemory(takeoverAfter)
	}
	return dedupe.NewKV(deliveryLease.kv, dedupe.KVConfig{
		Durable:       bus.ListenerDurable(machineID),
		Boot:          deliveryLease.boot,
		TakeoverAfter: takeoverAfter,
		GoneBoot:      deliveryLease.FormerHolder,
	})
}

// listenerStreamOptions are the options envoy-listener starts the stream with
// (bus.ConnectOwningStream): the deployment's replica count and byte limit, from cfg, and the
// buckets the listener opens at that count (bus.ListenerBuckets: store.Open's interest and role
// buckets, session.OpenSessionRegistry's and cistore.Open's, the list its NATS grant is built
// from), which the client's keeper raises back to it when a concurrent start or an operator
// lowered one (bus.WithKeptBuckets). The CI bucket is named even on a listener with no GitHub
// route, which opens none: the keeper skips a bucket that does not exist. The listener state
// bucket is not named here but added once the lease has opened it (bus.Client.KeepBucket, main),
// so a pod whose NATS user lacks the grant for it is not refused on it every period. main and the
// tests that start the listener's stream both take them from here.
//
// Each connection the client makes declares itself lost two to three seconds after the hub stops
// answering it (bus.WithSilentCutDetection). A hub whose packets stop closes nothing. On nats.go's
// default pings (one every two minutes, two outstanding) the connection reads connected for four
// to six minutes, and the reconnect that finds a hub which answers starts only once it reads lost.
func listenerStreamOptions(cfg config.Service) []bus.ConnectOption {
	return []bus.ConnectOption{
		bus.WithReplicas(cfg.NATSReplicas),
		bus.WithStreamMaxBytes(cfg.StreamMaxBytes),
		bus.WithKeptBuckets(bus.ListenerBuckets()...),
		bus.WithSilentCutDetection(),
	}
}

// registerListGauges registers the two gauges that count a list /v1 answers: envoy_active_sessions
// counts listedSessions, the listing GET /v1/sessions answers, and envoy_active_interests the
// interest copy GET /v1/interests/ answers. served is the pointer openV1Routes publishes. Before
// /v1 opens, and while a list would answer the refilling 503, a gauge writes no sample: a count of
// a copy that cannot be served is no count of the live sessions, and a missing sample is not read
// as zero, which only a complete empty copy reads.
func registerListGauges(met *metrics.Registry, served *atomic.Pointer[listenerDeps]) {
	met.NewOptionalGaugeFunc("envoy_active_sessions", "Number of active sessions", func() (int64, bool) {
		d := served.Load()
		if d == nil {
			return 0, false
		}
		count, err := listedSessionCount(d.registry, d.sessions)
		if err != nil {
			return 0, false
		}
		return int64(count), true
	})
	met.NewOptionalGaugeFunc("envoy_active_interests", "Number of active interest subscriptions", func() (int64, bool) {
		d := served.Load()
		if d == nil {
			return 0, false
		}
		interests, err := d.registry.List()
		if err != nil {
			return 0, false
		}
		return int64(len(interests)), true
	})
}

func main() {
	instance, err := listenerInstance()
	if err != nil {
		log.Fatal(err)
	}
	// Go's default logger, which carries internal/bus's lines and the webhooks' plain ones, gets no
	// instance prefix: the deployed CloudWatch text filters (infra/aws/rendering/metricfilters.go)
	// match fields by position after the date and time, and a word there would stop three alarms.
	// The bus's lifecycle lines name the instance in their client name (`envoy-listener <instance>`).

	// Phase 1: Load config (synchronous, fast). With ENVOY_LISTEN_FD, Load inspects the inherited
	// socket and takes its address as cfg's, so the API auth fence below judges the address that
	// serves; Phase 2 adopts it.
	cfg, err := config.Load(9020)
	if err != nil {
		log.Fatal(err)
	}
	logger := logging.New(cfg.MachineID, logging.WithInstance(instance))
	// The first lines of the log belong to the settings a deployment still carries and this
	// listener does not read, before it does any other work, so an operator reading from the top
	// meets them.
	warnIgnoredEnv(logger, "ENVOY_HOST_BRIDGE", "the listener no longer pushes a message to a session's serve port, so it opens no connection this would address")
	warnIgnoredEnv(logger, "ENVOY_SUBSCRIBE_ALL_TOPICS", "the listener no longer holds a live topics subscription, so there is no such subscription to turn on")
	apiToken := os.Getenv("ENVOY_API_TOKEN")
	oidcIssuer, oidcAudience, err := resolveListenerOIDCConfig(os.Getenv)
	if err != nil {
		log.Fatal(err)
	}
	// An issuer's discovery runs in the background until it succeeds (apiVerifier.discover): /v1
	// answers 503 "service starting" and /healthz "starting" meanwhile, so an issuer that does not
	// answer at start leaves the listener up and unready instead of ending it.
	apiVerifier := newAPIVerifier(oidcIssuer, oidcAudience)
	// The broker-token settings are read here, beside the other two credentials, so a
	// half-configured one refuses the start before anything is bound. Its verifier is built
	// later, once JetStream is open: the spent-proof bucket it records a proof in lives there
	// (brokerTokens.open).
	brokerConfig, err := resolveListenerBrokerConfig(os.Getenv)
	if err != nil {
		log.Fatal(err)
	}
	brokerTokenAuth := newBrokerTokens(brokerConfig)
	if err := validateListenerAPIAuth(cfg, apiToken, os.Getenv("ENVOY_API_ALLOW_UNAUTHENTICATED"), apiVerifier); err != nil {
		log.Fatal(err)
	}
	logger.Info("listener API auth: " + describeListenerAPIAuth(apiToken, oidcIssuer, brokerTokenAuth))
	logger.Info("listener API hosts: " + describeListenerAPIHosts(apiToken, apiVerifier, cfg.AllowedHosts))
	if apiVerifier != nil {
		go apiVerifier.discover(context.Background(), logger)
	}

	// Load webhook config (fast — env var reads only).
	webhookCfg, err := webhook.LoadWebhookConfig()
	if err != nil {
		log.Fatal(err)
	}
	// The renames a deployment configures belong beside the other settings lines: an operator who
	// reads from the top sees from them alone whether ENVOY_GITHUB_REPOSITORY_RENAMES took, and a
	// value that configures none logs nothing, as every other repository's publishing is unchanged.
	// The line names the subject prefix each pair's copies go out on, not the pair alone, because
	// the former name is written into the subject as the value spells it while a NATS subject is
	// compared with case: an operator who spelled it in a case GitHub never used reads here the
	// subject nothing subscribes to, rather than meeting the silence later (Renames.String).
	if github := webhookCfg.GitHub; github != nil && !github.RepositoryRenames.IsZero() {
		logger.Info("github repository renames: every envelope of the current repository is published a second time under the former name",
			slog.String("renames", github.RepositoryRenames.String()))
	}

	// Phase 2: Serve on the socket the listener inherited (ENVOY_LISTEN_FD) or bind its address,
	// in the main goroutine before any NATS work begins, so /healthz is reachable as soon as Serve
	// starts, whatever the NATS connection's latency.
	var ln net.Listener
	if cfg.ListenSocket != nil {
		ln, err = listenfd.Adopt(*cfg.ListenSocket)
	} else {
		ln, err = net.Listen("tcp", cfg.ListenAddress())
	}
	if err != nil {
		log.Fatal(err)
	}

	// Shared state: deps stays nil until the durable consumer binds (/healthz reads it), v1Deps
	// until /v1 opens (the list gauges read it).
	var deps, v1Deps atomic.Pointer[listenerDeps]

	// Metrics registry — /metrics available during startup (like /healthz).
	met := metrics.New()
	messagesReceived := met.NewCounter("envoy_messages_received_total", "Total messages received by the listener")
	messagesDelivered := met.NewCounter("envoy_messages_delivered_total", "Total message delivery attempts")
	messagesNAKed := met.NewCounter("envoy_messages_naked_total", "Total messages NAK'd for retry")
	// What a box under load does to its sessions, counted where it happens: the records the
	// session reaper took away and the messages they still had stored, the sends this listener
	// took for a session that had stopped answering, and the fan-outs it skipped for one. A box
	// whose sessions keep stalling shows as skips and unresponsive sends long before a record
	// reaches the horizon.
	sessionsReaped := met.NewCounter("envoy_sessions_reaped_total", "Session records deleted at the gone horizon with no clean stop")
	sessionStoredAfterLastSeen := met.NewCounter("envoy_session_messages_stored_after_last_seen_total", "Messages the stream held for a reaped session, stored after its last registration; not a count of unread messages, since the listener keeps no read receipt")
	sendsToUnresponsive := met.NewCounter("envoy_sends_unresponsive_total", "Sends accepted and stored for a recipient past the live window")
	fanoutUnresponsiveSkips := met.NewCounter("envoy_fanout_unresponsive_skipped_total", "Topic deliveries to a session past the live window that were not made: a push not sent, or an ad-hoc topic send's receipt not waited for; the message is stored for the session either way")
	roleKeptExpired := met.NewCounter("envoy_role_kept_expired_total", "Kept role messages a claim's drain dropped unforwarded, the stream having held them longer than the keep window")
	deliveryDuration := met.NewHistogram("envoy_delivery_duration_seconds", "Duration of message delivery attempts", metrics.DefaultBuckets)
	// The CI summary loop runs only beside the GitHub webhook route (Phase 6b2), so only a listener
	// that mounts it carries the gauge the loop sets.
	var ciLegacyHeld *metrics.Gauge
	if webhookCfg.GitHub != nil {
		ciLegacyHeld = met.NewGauge("envoy_ci_legacy_records_held", "CI records a head-gated listener left unsettled that the summary loop's last pass held back")
	}
	registerListGauges(met, &v1Deps)

	// Phase 3: Build HTTP mux.
	mux := http.NewServeMux()

	// /metrics is always reachable — NOT gated by readiness.
	mux.Handle("/metrics", met.Handler())

	// /healthz is always reachable, before NATS setup as well as after it, and answers 503
	// "starting" until the listener is ready.
	mux.HandleFunc("/healthz", healthzHandler(&deps, apiVerifier))

	// GaugeFunc for consumer pending — queries NATS at scrape time

	// The webhook and /v1 routes answer 503 "service starting" until their dependencies are open
	// (the two /v1 list reads the refilling answer, startingGate), each behind its own gate: the
	// webhooks once NATS and, for the GitHub route, the CI store are (Phase 5), /v1 once the
	// interest and session caches are too (Phase 6) and, with an OIDC issuer configured, its
	// discovery has succeeded (apiAuth answers that wait itself). Neither waits for the durable
	// consumer, which only /healthz reads. The webhook paths reach their gate bare, /v1 through
	// apiAuth.
	webhookGate, v1Gate := newStartingGate("webhook", logger), newStartingGate("v1", logger)
	hooks := webhookRoutes(webhookCfg)
	for _, hook := range hooks {
		mux.Handle(hook.path, webhookGate)
	}
	// Serve /v1/* on the listener port for local plugin registration.
	v1Handler := apiAuth(apiToken, apiVerifier, brokerTokenAuth, cfg.AllowedHosts, logger, v1Gate)
	mux.Handle("/v1", v1Handler)
	mux.Handle("/v1/", v1Handler)

	// Phase 4: Start HTTP server on the listener Phase 2 holds.
	server := newHTTPServer(mux)
	fatal := make(chan error, 1)
	go func() {
		if err := server.Serve(ln); err != http.ErrServerClosed {
			fatal <- err
		}
	}()
	listeningAt := time.Now()
	// The address ln is bound to, whether inherited or bound here, so the line names the port a
	// kernel chose (PORT=0 or a socket a harness opened on port 0).
	logger.Info("envoy-listener listening", slog.String("addr", ln.Addr().String()))

	// Phase 5: Connect to NATS, for as long as NATS takes to answer. The listener owns
	// ENVOY_NOTIFICATIONS: this start reconciles the stream (bus.ConnectOwningStream) at the
	// deployment's replica count and byte limit (listenerStreamOptions).
	client := connectListener(cfg, instance, logger)
	// The spent-proof bucket, before anything of /v1 can be served: a listener configured to take
	// broker access tokens on a bus that cannot open it refuses to start, naming the setting,
	// rather than recording spent proofs where its own replicas cannot see them.
	if err := brokerTokenAuth.open(client.Conn, cfg.NATSReplicas); err != nil {
		log.Fatal(err)
	}
	consumer := bus.ListenerDurable(cfg.MachineID)
	// The durable is checked here, before the cache warm-ups below (up to 30 s each), so a refused
	// one ends the start within the NATS connect: no pod could receive from it, and nothing serves
	// yet, so the start ends at once. A lookup that fails is left to the receive loop, which retries
	// it.
	if _, err := listenerDurable(context.Background(), client, consumer); errors.Is(err, errListenerDurableRefused) {
		logger.Error(durableRefusedLine, slog.String("error", err.Error()))
		client.Conn.Close()
		os.Exit(1)
	} else if err != nil {
		logger.Warn("durable consumer check failed; the receive loop retries it", slog.String("consumer", consumer), slog.String("error", err.Error()))
	}
	// The delivery lease makes one pod of this machine id its deliverer and every other a standby
	// (lease.go). It is created here, after the durable check so a refused start opens no bucket,
	// and started once /v1 serves and the shutdown can release it. A pod that opened the bucket
	// has the grant for it, so its keeper raises that bucket to the deployment's copies every
	// period too (bus.Client.KeepBucket): naming it at the connect would have a pod whose grant
	// lacks it, which delivers without a lease, refused on it every period instead.
	deliveryLease, leaseConn := openDeliveryLease(cfg.NATSURLs, cfg.NATSReplicas, cfg.MachineID, instance, logger)
	if deliveryLease != nil {
		client.KeepBucket(bus.ListenerStateBucket)
	}
	// CI-summary aggregation state. Its WatchAll cache warms asynchronously like the registries
	// below; the summary loop tolerates an empty cache until it fills. It opens ahead of them, as the
	// one store the webhooks need, and after the durable check, so a refused start opens no bucket.
	//
	// Only the GitHub webhook route records checks in it, and the listeners that receive GitHub
	// webhooks publish every CI settlement, so a listener without the route opens no CI store and
	// runs no summary loop. It must not scan the bucket either: the watch delivers every record as
	// it starts (62 MB in production), and over a relayed link that burst holds up the replies the
	// stores below wait on past their 10 s deadline.
	var ciStore *cistore.Store
	var renames ghrename.Renames
	if webhookCfg.GitHub != nil {
		renames = webhookCfg.GitHub.RepositoryRenames
		ciStore = openStartStore(client, logger, "CI store", []string{bus.CIStateBucket}, func(conn *nats.Conn) (*cistore.Store, error) {
			return cistore.Open(conn, logger, cistore.WithReplicas(cfg.NATSReplicas), cistore.WithTTL(7*24*time.Hour))
		})
	} else {
		logger.Info("CI store not opened: no GitHub webhook route, so this listener records and settles no checks")
	}
	// The one publisher every envelope goes out through: the webhook routes' deliveries and the CI
	// summary loop's settlements alike, so a renamed repository's subscribers of the former name
	// hear both. It is built here, over the open client, because it publishes through it, and it
	// carries the configured pairs, which the start-up line above names; a listener with no GitHub
	// route configures none and publishes exactly what the client publishes.
	publisher := ghrename.Publishing(client, renames, logger)
	openWebhooks(webhookGate, hooks, publisher, ciStore)
	logger.Info("envoy-listener webhooks open (NATS connected)")

	// The registry and its cache watcher log through the listener's own handler, so its warm-up
	// and its reaper cycles are JSON records with this machine's id. Never slog.SetDefault
	// instead: see store.WithLogger for the CloudWatch filters that breaks. A role claim's grace
	// counts from the machine's service start, the delivery lease record's service_since, which
	// every pod reads alike; a listener with no lease takes its own start (store.WithServiceSince).
	registryOptions := []store.OpenOption{store.WithReplicas(cfg.NATSReplicas), store.WithLogger(logger.Slog())}
	if deliveryLease != nil {
		registryOptions = append(registryOptions, store.WithServiceSince(func() time.Time { return deliveryLease.Status().ServiceSince }))
	}
	registry := openStartStore(client, logger, "interest registry", []string{bus.InterestBucket, bus.RoleClaimBucket}, func(conn *nats.Conn) (*store.Registry, error) {
		return store.Open(conn, registryOptions...)
	})

	// Wait for both of the registry's views before accepting NATS messages or HTTP requests.
	// Without this gate, real events that arrive in the warm-up window get "no matching interests"
	// even when the durable KV entry has subscribers, and the two routes that report a session's
	// roles - GET /v1/sessions and GET /v1/registry/<session_id> - answer a live client the
	// bus-unavailable 503 while the bus is healthy, because the role claim view refuses every read
	// until its watcher has delivered every claim. So the gate waits for that view to be complete,
	// not merely ready: a first scan nats.go's idle timer ended releases readiness, and the
	// watcher's rescan ends this wait.
	//
	// Bounded at 30s: a healthy NATS cluster completes the scan in milliseconds. The wait also
	// ends before that bound with the role claim watcher's terminal error - a first start that
	// failed, a bucket recreated under it, the watcher's own end - or its stop, none of which the
	// watcher completes a scan after.
	// A warm-up that does not complete still serves, fail open: the warn below names the cache and
	// carries the cause, whichever of those it was, the registry keeps answering from whatever it
	// holds, and /healthz reports the role claim view as degraded and the buckets themselves while
	// NATS recovers.
	cacheReadyCtx, cacheReadyCancel := context.WithTimeout(context.Background(), 30*time.Second)
	if err := registry.WaitForCacheReady(cacheReadyCtx); err != nil {
		logger.Warn("interest or role claim cache warm-up did not complete; serving with possibly empty cache",
			slog.String("error", err.Error()))
	}
	cacheReadyCancel()

	// A session's last-seen time lives in the listener state bucket beside the lease, so every pod
	// answers it alike for a lapsed session; a listener with no lease keeps it in process
	// (session.WithLastSeenBucket). A departed session's last heartbeat is kept for the registry's
	// own session.LastSeenMemory (2 hours), which this listener does not shorten: it is the one
	// value, declared once, and it outlasts both the gone horizon, so a caller told a session is
	// gone can still be told when it last registered, and the interest reaper's allowance
	// (store.InterestReapAllowance(store.InterestReapGrace), 15 minutes), so /v1/interests/ can
	// report the real last heartbeat (the row's updated_at lags it by up to
	// store.InterestRefreshAge) of every row that reaper still lists.
	//
	// The windows every reader of the registry judges a record's age against. Each defaults to
	// the compiled constant (session.LiveWindow, session.GoneHorizon). The live window's is the
	// declaration contracts.SessionLiveWindow, which the Go test
	// TestDAEMONTESTTheSessionTimingRuleHoldsAtTheValuesTheBuildUses (packages/daemon/internal/daemon)
	// weighs to prove that three registration refreshes fit inside it; a deployment
	// that moves one moves what this listener runs with and nothing else. A value that is not a
	// positive duration is refused by name here rather than taken. Dispatch reads the same two
	// windows through the same helper and judges a role route's holder by them, so a deployment
	// gives both processes the same values (envconf.SessionWindows, cmd/dispatch's
	// roleHolderWindows).
	sessionLiveWindow, sessionGoneHorizon, err := envconf.SessionWindows(logger.Slog(), os.LookupEnv)
	if err != nil {
		log.Fatal(err)
	}
	// The two reaper loops below run on periods of their own: what the session reaper decides is
	// what the role claim reaper waits for, but each pass costs something different and a gone
	// holder's role is not freed sooner than the deployment means it to be (the two defaults say
	// why). Both run on the pod holding the delivery lease alone (holderLoops).
	sessionReapInterval, err := envconf.PositiveDuration(logger.Slog(), os.LookupEnv, "ENVOY_SESSION_REAP_INTERVAL", defaultSessionReapInterval)
	if err != nil {
		log.Fatal(err)
	}
	roleClaimReapInterval, err := envconf.PositiveDuration(logger.Slog(), os.LookupEnv, "ENVOY_ROLE_CLAIM_REAP_INTERVAL", defaultRoleClaimReapInterval)
	if err != nil {
		log.Fatal(err)
	}
	sessionOptions := []session.SessionRegistryOption{
		session.WithSessionReplicas(cfg.NATSReplicas),
		session.WithSessionLogger(logger.Slog()),
		session.WithSessionMaxAge(cfg.SessionMaxAge),
		session.WithSessionLiveWindow(sessionLiveWindow),
		session.WithSessionGoneHorizon(sessionGoneHorizon),
	}
	if deliveryLease != nil {
		sessionOptions = append(sessionOptions, session.WithLastSeenBucket(deliveryLease.kv))
	}
	sessions := openStartStore(client, logger, "session registry", []string{bus.SessionBucket}, func(conn *nats.Conn) (*session.SessionRegistry, error) {
		return session.OpenSessionRegistry(conn, sessionOptions...)
	})

	// The margin between a client's registration refresh and this lifetime can only be checked
	// against the defaults in the source, while the live value is whatever MaxAge the bucket
	// carries once ENVOY_SESSION_MAX_AGE has been applied to it. Say it at boot, with the windows
	// beside it, so an operator can read the values in force: a maximum age below the gone
	// horizon drops a stalled session's record before the horizon can be reached, and the horizon
	// then means nothing.
	logger.Info("session record lifetime resolved; a session record is dropped this long after its last write",
		slog.String("ttl", sessions.TTL().String()),
		slog.String("live_window", sessionLiveWindow.String()),
		slog.String("gone_horizon", sessionGoneHorizon.String()),
		slog.String("reap_interval", sessionReapInterval.String()),
		slog.String("role_claim_reap_interval", roleClaimReapInterval.String()))

	// Wait for the session cache to finish its initial scan before we accept
	// traffic, mirroring the interest-cache gate above. Bounded at 30s: a healthy
	// cluster completes in milliseconds. On timeout we fail open — Put
	// write-through keeps locally-registered sessions visible, while /healthz
	// reports a persistently unavailable registry.
	sessionCacheReadyCtx, sessionCacheReadyCancel := context.WithTimeout(context.Background(), 30*time.Second)
	if err := sessions.WaitForCacheReady(sessionCacheReadyCtx); err != nil {
		logger.Warn("session cache warm-up timed out; serving with possibly empty cache",
			slog.String("error", err.Error()))
	}
	sessionCacheReadyCancel()

	// KV watchers are not bus subscriptions: tie them to the connection, so a disconnect stops each
	// copy following its bucket, every recovered connection reads each bucket again, and the drain
	// ends them before the connection's own.
	caches := listenerCaches(registry, sessions, ciStore)
	hookListenerKVWatchers(client, caches)

	ledger := newDeliveryLedger(deliveryLease, cfg.MachineID, deliveryHandlingLimit)

	// The window an agent delivery this listener cannot place is retried within before it is
	// given up with a record (unplaceable_delivery.go). Zero is a value an operator may mean
	// here - retry nothing - so it is read through envconf.Duration and not refused.
	unplaceableRetryWindow := envconf.Duration(logger.Slog(), os.LookupEnv, "ENVOY_UNPLACEABLE_RETRY_WINDOW", defaultUnplaceableRetryWindow)

	// /healthz handler was registered early (before NATS) — no re-registration needed.

	// Wake reports are published off the paths that raise them, under a lifetime of the
	// listener's own — the shutdown ends every retry still running — and a cap on how many
	// retry at once (wakeReporter). It is built here because the delivery handlers below copy
	// this configuration by value, so every seam of it must be set before they are built.
	wakesCtx, stopWakeReports := context.WithCancel(context.Background())
	wakes := newWakeReporter(wakesCtx, client)

	// Phase 6: Open the role lane and /v1 onto the stores, then start the lease and the durable's
	// receive loop.
	deliveryConfig := listenerDeliveryHandlerConfig{
		client:            client,
		wakes:             wakes,
		forwardRole:       startRoleForward(client),
		registry:          registry,
		sessions:          sessions,
		machineID:         cfg.MachineID,
		ledger:            ledger,
		ledgerReconnected: ledgerConnected(leaseConn),
		logger:            logger,
		messagesReceived:  messagesReceived,
		messagesDelivered: messagesDelivered,
		messagesNAKed:     messagesNAKed,
		deliveryDuration:  deliveryDuration,
		roleKeptExpired:   roleKeptExpired,

		fanoutUnresponsiveSkips: fanoutUnresponsiveSkips,

		unplaceableRetryWindow: unplaceableRetryWindow,
		// One deferral set for the life of the process, shared by construction by every handler
		// built from this one value below - the role lane's and the durable's,
		// which the receive loop keeps across every resubscribe and every take-over of the lease -
		// so no handler opens a second budget beside the first (unplaceableDeferrals,
		// unplaceable_delivery.go).
		deferredUnplaceable: newUnplaceableDeferrals(),
		// One guard per role for the life of the process, held for a whole handover of that
		// role's kept lane, so the handovers a claim, a keep and the sweep start never walk the
		// lane together, and the sweep can tell a lane this listener is draining already
		// (roleKeepGuards, role_keep.go). Every handler and route built from this one value
		// shares it, as they share the deferral set above.
		roleKeepDrains: newRoleKeepGuards(),
	}
	// The role lane's forwards run off its handler (roleForwarder): the handler resolves each
	// role message's holder and hands the forward over, and the forwards to one holder are
	// published in order while each waits for its outcome on its own. It is wired here, last, so
	// every seam of the configuration it copies is set (wireRoleForwarder).
	deliveryConfig = wireRoleForwarder(deliveryConfig)
	roleForwards := deliveryConfig.roleForwards

	// The role lane is a core-NATS queue subscription on a connection of its own with finite pending
	// limits (bus.RoleLanePendingMsgs, bus.RoleLanePendingBytes; bus.ConnectRoleLane), in the queue
	// group every listener shares, or this machine's group on a hub whose grant predates it. Every
	// pod of this machine id, the lease holder and each standby, is a member, and during a rolling
	// deploy the task being replaced is one too; NATS hands each role message to one member of a
	// group, so no message is forwarded twice whichever pods run, and opening the lane before the
	// durable binds forwards none twice either.
	roleLaneClient, roleQueue, err := bus.ConnectRoleLane(cfg.NATSURLs, cfg.MachineID, coreNATSDeliveryHandler(deliveryConfig))
	if err != nil {
		logger.Error("core role subscription failed", slog.String("error", err.Error()))
		client.Conn.Close()
		os.Exit(1)
	}
	logger.Info("role lane subscribed", slog.String("subject", contracts.RoleTopicPrefix+">"), slog.String("queue", roleQueue))
	met.NewGaugeFunc("envoy_role_lane_slow_consumers", "Slow-consumer errors the role lane's connection has reported", func() int64 {
		return int64(roleLaneClient.SlowConsumers())
	})
	met.NewGaugeFunc("envoy_role_forwards_outstanding", "Role forwards queued behind their holder or waiting for their outcome", func() int64 {
		return int64(roleForwards.outstanding())
	})
	roleLaneWatchCtx, roleLaneWatchCancel := context.WithCancel(context.Background())
	go watchRoleLaneSlowConsumers(roleLaneWatchCtx, roleLaneClient, logger, roleLaneSlowConsumerInterval)
	lane := roleLane{client: roleLaneClient, forwards: roleForwards, stopWatch: roleLaneWatchCancel}

	// The session consumers a registration asks for are managed on a connection of their own
	// (bus.ConnectSessionConsumers). One that fails to connect leaves every session on core
	// delivery, which a listener of an earlier release serves too, rather than ending the start.
	delivery := connectSessionDelivery(cfg.NATSURLs, logger)
	// The relay of those consumers hears registrations from the moment /v1 opens, and relays once
	// this task serves its machine's receive term (below). Without its reply inbox no frame could
	// be answered, so a relay that cannot subscribe one leaves every session on core delivery too.
	var relays *sessionRelays
	if delivery.consumers != nil {
		relays, err = startSessionRelays(delivery, deliveryConfig)
		if err != nil {
			logger.Error("session relay reply subscription failed; every session stays on core delivery", slog.String("error", err.Error()))
			delivery.close()
			delivery = &sessionDelivery{logger: logger, now: time.Now}
		}
		if relays != nil && deliveryLease != nil {
			// A relay of this pod restarts a session's consumer at its ack floor only once a
			// former holder that released the lease has had its settle bound
			// (lease.FormerSettleWait): until then that pod may still be acking a frame the
			// session answered, which this one would otherwise publish again.
			relays.formerSettle = deliveryLease.FormerSettleWait
		}
	}
	// The parts a shutdown ends, gathered once: every end of a start that serves /v1 passes them
	// to shutdownListener.
	parts := listenerParts{
		server:    server,
		client:    client,
		lane:      lane,
		delivery:  delivery,
		relays:    relays,
		caches:    caches,
		stopWakes: stopWakeReports,
	}

	// From here a SIGTERM or SIGINT is an ordered shutdown: /v1 serves from the next step, and the
	// lease is released on the way out. Before here, during the NATS connect and the cache
	// warm-ups, a signal ends the process at once, as it always has.
	stopping, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	// The loops only the deliverer runs start when this pod starts receiving the durable and stop
	// when it stops: it lost or released the lease. A standby runs none of them. Each loop that
	// lists a bucket does so through watchers whose consumers nats.go creates and deletes, so the
	// end of receiving waits for a pass in flight, and the shutdown ends receiving before it
	// drains NATS.
	ciDebounce := envconf.Duration(logger.Slog(), os.LookupEnv, "ENVOY_CI_DEBOUNCE", 5*time.Second)
	// summaryDone closes once the last term's CI summary loop has detached from the CI store. A
	// store feeds one summary loop at a time, and a loop's pass in flight can outlast the end of
	// its term's wait (holderLoopsStopWait), so each term's loop starts only after it.
	summaryDone := make(chan struct{})
	close(summaryDone)
	holderLoops := func(ctx context.Context) []<-chan struct{} {
		// Relay ownership is no loop of this term's: the receive term itself sets and clears it
		// (durableReceiver.begin and durableReceiver.end, sessionRelays.setOwned), so among the
		// pods of one machine id only the one receiving relays its sessions' consumers.
		//
		// The interest reaper cross-references envoy_sessions with envoy_interests (permanent) to
		// prune orphaned interests from gone sessions, each kept until more than
		// store.InterestReapGrace plus store.InterestRefreshAge (15 minutes) has passed since its
		// last write; the role-claim reaper ends the claims of holders whose sessions are gone.
		//
		// A restored role claim's holder gets one live window to register again, not one bucket
		// maximum age: the window is what makes a session live to every other reader, and with a
		// maximum age raised past the gone horizon the age would hold a dead holder's claim for
		// an hour after the reaper had already found it gone.
		//
		// The role claim reaper runs at ENVOY_ROLE_CLAIM_REAP_INTERVAL, its own period and not
		// the session reaper's: what it waits for is what that reaper decides, but a pass of it
		// is what takes a gone holder's role away, and an operator who shortens the session
		// reaper's period to settle the bucket's maximum age sooner is not asking for that (the
		// two defaults say why). Each pass is judged against this listener's own live window,
		// not counted, so the period moves when a gone holder's role is freed and nothing else.
		sessionLive, _ := sessions.Windows()
		done := []<-chan struct{}{
			registry.StartReaper(ctx, func(sessionID string) (bool, error) { return sessionMayBeLive(sessions, sessionID) }, 5*time.Minute, store.InterestReapGrace),
			registry.StartRoleClaimReaper(ctx, func(sessionID string) bool { return roleHolderMayBeLive(logger, sessions, sessionID) }, roleClaimReapInterval, sessionLive),
			// The session reaper is the one loop that deletes a session's record, and the one
			// place the bucket's maximum age is settled while the listener runs
			// (session.ReapSessions). It runs here, on the lease holder alone, as every other
			// reaper of this machine does: a pass deletes each record at the revision it judged
			// it on and writes the same maximum age every pod would write, so two pods running
			// it would be safe, but each pass also walks the stream for every record it reaps,
			// and one pass per machine is what that is worth.
			sessions.StartSessionReaper(ctx, sessionReapInterval, session.SessionReaper{
				Client:              client,
				Reaped:              sessionsReaped,
				StoredAfterLastSeen: sessionStoredAfterLastSeen,
			}),
			// The collector removes the interest bucket's delete markers, which the reaper leaves
			// one of per dead session on a key that is never reused and which nothing else removes,
			// so every restart's cache warm-up replays them before it serves (LEGION-374). Its first
			// pass runs as receiving starts, off the readiness path; then on the reapers' cadence.
			// The purge is idempotent. The JetStream context comes from the client at each pass,
			// since a replaced connection reassigns it.
			registry.StartInterestMarkerCollector(ctx, client.JS, 5*time.Minute),
			// The sweep hands over the kept role lanes nothing is draining. A handover of a
			// role's kept lane runs in a goroutine of the pod that started it, and only a claim
			// or a keep starts one, so a pod restarted or replaced mid-handover leaves the
			// copies behind its cursor for a claim that seldom comes inside roleKeepWindow: a
			// live holder claims no second time. This is the backstop over them, on the reapers'
			// cadence. The lanes and their cursors are the stream's, so the pod receiving the
			// durable sweeps for the machine and its standby runs no pass of its own; whichever
			// pod a claim or a keep reaches still starts that role's handover itself. It costs
			// one stream read a pass while no role has a kept lane (startKeptRoleSweep,
			// role_keep_sweep.go).
			startKeptRoleSweep(ctx, deliveryConfig, 5*time.Minute),
		}
		// The CI summary loop, on a listener that opened the CI store, emits one pr.<n>.checks
		// event once a commit's checks settle, for every commit of a pull request, its head or
		// not; new runs re-arm settlement. The debounce window is ENVOY_CI_DEBOUNCE (default 5s).
		// The loop runs a pass only on a one-second tick by which a cached record changed or fell
		// due. It publishes through the same publisher the webhook routes do: a settlement is a
		// GitHub envelope like any other, and a subscriber of a renamed repository's former name
		// waits on its checks as much as on its comments.
		if ciStore != nil {
			previous, loopDone := summaryDone, make(chan struct{})
			summaryDone = loopDone
			go func() {
				defer close(loopDone)
				<-previous
				if ctx.Err() == nil {
					<-cistore.StartSummaryLoop(ctx, ciStore, publisher, ciDebounce, 1*time.Second, ciLegacyHeld, logger)
				}
			}()
			done = append(done, loopDone)
		}
		return done
	}
	receiver := newDurableReceiver(durableReceiverOptions{
		client:   client,
		consumer: consumer,
		lease:    deliveryLease,
		handle:   jetStreamDeliveryHandler(deliveryConfig),
		loops:    holderLoops,
		// The term elects the pod that relays its machine's session consumers: that ends with it.
		relays: relays,
		// A durable refused after the check above (an operator changed it since) can never be
		// received from, so the listener stops, as that check stops a start.
		refused: func(err error) {
			logger.Error(durableRefusedLine, slog.String("error", err.Error()))
			_ = syscall.Kill(os.Getpid(), syscall.SIGTERM)
		},
		logger: logger,
		timing: productionReceiveConfig(),
	})
	// A replaced connection invalidates the subscription; the receive loop subscribes again on the
	// connection the client now holds.
	client.AddReconnectHook(receiver.reconnected)
	// The lease judges this pod's receiving by that same connection: a holder whose receive
	// connection has been down for leaseReceiveLost releases the record, so a standby delivers
	// while this pod's lease connection, which is another connection, still renews; and no pod
	// takes the record while its receive connection is down (lease.SetReceiveSignal).
	if deliveryLease != nil {
		deliveryLease.SetReceiveSignal(receiver.receiveUp)
	}

	ready := &listenerDeps{
		client:     client,
		roleLane:   roleLaneClient,
		consumers:  delivery,
		registry:   registry,
		sessions:   sessions,
		ciStore:    ciStore,
		caches:     caches,
		receiver:   receiver,
		streamName: bus.Stream,
		wakes:      wakes,
		delivery:   &deliveryConfig,
		confirmed:  dedupe.New(10 * time.Minute),

		sendsToUnresponsive: sendsToUnresponsive,
	}
	openV1Routes(v1Gate, ready, &v1Deps, cfg.MachineID, logger, listeningAt)

	// The lease runs until the shutdown releases it (durableReceiver.Stop); leaseCtx is cancelled
	// after that, which ends a lease the release could not stop in time.
	leaseCtx, leaseCancel := context.WithCancel(context.Background())
	defer leaseCancel()
	if deliveryLease != nil {
		deliveryLease.Start(leaseCtx)
	}
	receiver.Start()
	deps.Store(ready)
	logger.Info("envoy-listener ready (NATS connected)")

	// Phase 6b: Say we are alive and reporting, now and every listenerReadyInterval, on the one
	// topic the Legion daemons read wakes of released issue pods from. Without it a daemon
	// cannot tell "the listener sends no reports" from "the listener has nothing to report", so
	// it releases nothing: this is the positive signal that lets release run at all. Every pod
	// of the machine runs it, the standby included, because every pod serves the publish route
	// and so raises role wakes of its own; a report says one listener is alive and reporting,
	// which is what the daemon's window asks, and two pods saying it is the same answer twice.
	// It runs under the wake reports' own lifetime, which the shutdown ends (parts.stopWakes).
	startListenerReadyReports(wakesCtx, client, logger, listenerReadyInterval)

	// Phase 6c: Keep transient JetStream timeouts observable without restarting
	// the listener. Terminal watcher/consumer failures are rebuilt immediately;
	// after three failed recovery intervals, SIGTERM enters the bounded shutdown
	// path and exits non-zero for Docker to replace the listener. Only the pod receiving the
	// durable checks it; a standby's probe checks the caches alone, so a standby runs on for as
	// long as its caches answer. The durable probe and a rebuild's resubscribe run under
	// monitorCtx, each JetStream read included, so the shutdown's monitorCancel cuts a read the
	// server has not answered and the 10 s shutdown budget is held by neither.
	monitorCtx, monitorCancel := context.WithCancel(context.Background())
	probeDurable := func() error { return receiver.probe(monitorCtx) }
	monitorDone := make(chan struct{})
	go func() {
		defer close(monitorDone)
		runSelfHealthMonitor(
			monitorCtx,
			logger,
			func() error {
				return checkSelfHealth(caches, probeDurable)
			},
			func(err error) bool {
				return isUnrecoverableSelfHealthFailure(err, client, caches)
			},
			client.Connected,
			func() error {
				return rebuildListenerDependencies(monitorCtx, client, caches, probeDurable, receiver.resubscribe)
			},
			func() { _ = syscall.Kill(os.Getpid(), syscall.SIGTERM) },
			30*time.Second,
			3,
		)
	}()

	// Phase 7: Block until SIGTERM/SIGINT or fatal error.
	select {
	case err := <-fatal:
		logger.Error("fatal error", slog.String("error", err.Error()))
	case <-stopping.Done():
		logger.Info("received signal, shutting down", slog.String("signal", context.Cause(stopping).Error()))
	}
	shutdownListener(logger, parts, listenerLoops{
		stop:        monitorCancel,
		monitorDone: monitorDone,
		receiver:    receiver,
	})
}

// listenerLoops are what main started that the shutdown ends before it drains NATS: stop ends the
// self-health monitor, monitorDone is closed once it has returned, and receiver is the durable's
// receive loop, which stops the holder-only loops itself.
type listenerLoops struct {
	stop        func()
	monitorDone <-chan struct{}
	receiver    *durableReceiver
}

// listenerParts are the parts of a started listener that shut down: server is the /v1 HTTP server,
// client the main bus connection, lane the role lane, delivery the session-consumer connection,
// relays the relay of its consumers (nil when it does not run), caches the KV caches whose
// watchers are retired last, and stopWakes ends the wake reports still retrying and the liveness
// report (wakeReporter, startListenerReadyReports).
type listenerParts struct {
	server    *http.Server
	client    *bus.Client
	lane      roleLane
	delivery  *sessionDelivery
	relays    *sessionRelays
	caches    []listenerCache
	stopWakes func()
}

// shutdownListener is the ordered shutdown every end of a start that serves /v1 comes to, a
// signal, a fatal error or a bind that did not complete, and exits non-zero so the runtime's
// restart policy restores the listener.
func shutdownListener(logger *logging.Logger, parts listenerParts, loops listenerLoops) {
	// 1. Stop the self-health monitor, so nothing re-subscribes or re-watches from here, and the
	// wake reports retrying off their callers' paths: each holds its goroutine for minutes
	// otherwise, and a report published into a draining connection reaches nothing. Both are a
	// context cancel and wait on nothing, so neither spends any of the budget below.
	loops.stop()
	parts.stopWakes()

	// 2. Stop receiving the durable, inside the 10 s budget the relays and the HTTP stop share:
	// the term's end stops this pod's session relays (durableReceiver.end), the holder-only
	// loops stop, the handler takes no more messages, the message in hand gets inflightGrace
	// (3 s) before its handling is cancelled and it is NAKed, with a second for the handler to
	// return, every message not handled is NAKed with a delay for the next holder, and the lease
	// is marked released, so a standby takes the lease at its next poll; the subscription then
	// stays until the standby receives, at least 1.25 s after the release (lingerUntaken and a
	// poll, the floor that holds the deliver subject while the standby subscribes) and up to
	// lingerMax (3 s) after the NAKs, then drains (2 s) and, when the whole hand-over went
	// through, writes the lease's clean-hand-back mark, one bucket call of 2 s at most, which
	// ends the standby's settle hold at 3 s (durableReceiver.handBack): 11 s at most, and
	// 12.25 s when the flush (1 s) and the release (2 s) both run to their bounds and leave that
	// floor past the cap. The relays' own settle is not in that sum: the term's end only cancels
	// them, and step 3 below waits for it. This budget bounds none of those waits: it is how
	// long this shutdown
	// waits for the receive stop. Past it the shutdown goes on to the relays and the HTTP stop
	// with no time left, then to the NATS drains below and os.Exit(1), while the hand-back may
	// still be running. The listener's own shutdown, not the runtime's kill, cuts that hand-back
	// off: client.Drain ends the subscription of a hand-back still lingering or draining, and
	// os.Exit(1) ends only a mark write still in flight on the lease's own connection. So a
	// longer grace period in the runtime keeps no mark, and the next holder then settles at its
	// 7 s cap, as it does after any stop that wrote none. HTTP keeps serving meanwhile.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	loops.receiver.Stop(shutdownCtx)

	// 3. The session relays: publish no frame and ack nothing more. Each relay the term's end
	// stopped first gives the frame it had unanswered relaySettleGrace (0.75 s) to be answered
	// and acks it (sessionRelays.settleStoppedFrames), so the pod taking over does not publish
	// it again; this step waits for that, out of what the receive stop left of the budget, and
	// is cut short with a warning when nothing is left. The messages the relays hold unacked
	// are delivered again once their ack wait runs out, to whichever pod of this machine
	// receives the durable then.
	parts.relays.stop(shutdownCtx)

	// 4. HTTP server — stop accepting new requests, drain in-flight, within what is left of the
	// budget.
	if err := parts.server.Shutdown(shutdownCtx); err != nil {
		logger.Warn("http shutdown error", slog.String("error", err.Error()))
	}
	// The session-consumer connection carries the consumer calls registrations make, which the HTTP
	// server has stopped serving, and the relays' acks, which its drain lets reach the server,
	// within what is left of the HTTP stop's budget.
	parts.delivery.drain(shutdownCtx)

	// 4. NATS — wait (within the same deadline) for a self-health rebuild the monitor may still be
	// running, so nothing re-subscribes or re-watches during the drain. Retire every cache's KV
	// watchers, so their end reads as the shutdown it is rather than a watcher failure, and a
	// reconnect hook still running cannot arm a new one; then drain in-flight deliveries without
	// reconnecting, end the retired watchers and delete their consumers (endListenerKVWatchers, the
	// drain hook main adds), and drain the connection, but never let a blocked NATS request pin the
	// process after its HTTP listener is gone.
	if awaitDone(shutdownCtx, loops.monitorDone) != nil {
		logger.Warn("self-health monitor still running at shutdown")
	}
	// The role lane drains before the main client: a delivery still in its handler, and each role
	// forward, publishes through the main client and reads the caches stopped below. It gets 3 s
	// for its handler and the receipt window on top for the forwards it handed over
	// (roleLane.drain), 5 s. Core NATS retries nothing, so a delivery still blocked at the
	// deadline is lost, as one sent while no listener runs is.
	parts.lane.drain(logger, 3*time.Second)
	for _, c := range parts.caches {
		c.cache.StopWatch()
	}
	// 10 s for the deliveries in their handlers, as before the drain hooks, and the hooks' 3 s on
	// top (bus's drainHookReserve): with the 10 s the receive stop, the relays' settle and the
	// HTTP stop share and the role lane's 5 s side by side, 28 s, inside every runtime's 30 s.
	if err := parts.client.Drain(13 * time.Second); err != nil {
		logger.Warn("nats drain error", slog.String("error", err.Error()))
	}

	logger.Info("envoy-listener shutdown complete")
	os.Exit(1)
}
