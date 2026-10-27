// Intake (LEGION-567): a Dispatch server consumer of the GitHub events Envoy already relays over
// NATS (`notifications.github.>`), for live updates between the five-minute reconcile's passes.
// Every write here goes through the same upsert functions reconcile uses (store.go), so a NATS
// redelivery or a reconcile re-reading the same fact is a no-op, not a duplicate -- and intake
// never relies on NATS redelivery for correctness: a GitHub call that fails here is logged and
// the message is still acked, because the reconcile that runs every five minutes is the actual
// catch-up mechanism ("a missed event appears by the next five-minute reconcile", LEGION-567).
package delivery

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/duty"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/githubapp"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// ompSessionTrailer matches a commit message's `Omp-Session: <id>` trailer line (the same shape
// LEGION-294's own collector reads).
var ompSessionTrailer = regexp.MustCompile(`(?m)^Omp-Session:\s*(\S+)\s*$`)

// issueKeyCandidate matches a bare Dispatch issue key mention (e.g. ACME-1072), the same shape
// this codebase's own issueKeyPattern uses elsewhere (packages/envoy/internal/dispatch/api/server.go,
// packages/envoy/internal/dispatch/text/refs.go) -- duplicated here rather than imported because
// both are unexported in their own packages and this package does not depend on either.
var issueKeyCandidate = regexp.MustCompile(`\b[A-Z][A-Z0-9]{1,9}-[1-9][0-9]*\b`)

// Intake wires the NATS consumer to the database, the GitHub App client and the event broker the
// issue.deployed events go through (RecordDeployedEvents). StartIntakes builds it, and only with
// the App configured.
type Intake struct {
	nats   *bus.Client
	pool   *store.Pool
	github *githubapp.Client
	events *events.Broker
	// settings is the delivery settings every message is routed by, shared with the job intake
	// (StartIntakes).
	settings *SettingsCache
	// leases is the pool bind takes this durable's consumer lease on (consumerLease), so that one
	// process at a time reconciles this durable's configuration (bindDurableConsumer).
	leases duty.Leases
	// throttle is this intake's own bind-poll state: how recently it reconciled its durable and
	// how recently it said it was standing by for it (bindThrottle).
	throttle *bindThrottle

	mu sync.Mutex
	// delivering is the latest subscription subscribe made, which StopDelivering stops.
	delivering *intakeDelivery
}

// newIntake builds an Intake on natsClient, a connected client StartIntakes opens for it alone,
// reconciling its durable under a lease it takes on leases and routing by settings.
func newIntake(natsClient *bus.Client, pool *store.Pool, leases duty.Leases, github *githubapp.Client, broker *events.Broker, settings *SettingsCache) *Intake {
	return &Intake{nats: natsClient, pool: pool, github: github, events: broker, settings: settings, leases: leases, throttle: &bindThrottle{}}
}

// Run binds the durable consumer and processes GitHub events until ctx is cancelled, then stops
// delivering (StopDelivering) under intakeStopBudget before it returns. Idle until the settings
// cache holds settings.
//
// The durable carries one filter subject, githubIntakeSubject, which no setting changes, so it is
// bound once and never rebound. Each message is routed by the settings the cache holds when its
// handler runs, so a settings change -- a different deploy repository or workflow path -- takes
// effect as soon as the cache has read it, with no restart and no rebind. route does all the
// filtering, from those settings.
//
// Two things wake an unbound intake: the cache storing settings, since an intake with none has
// nothing to route by, and, once it has settings, durableBindInterval's ticker. The durable admits
// one subscriber, so a second envoy-dispatch process stands by on that ticker -- for the life of
// the process, with no deadline -- and takes the durable over within seconds of the process
// holding it going away. The bind poll stops once this process is bound.
//
// The PR-checks workflow path is live-reachable only for a fork-originated pull request: Envoy's
// own webhook normalizer (internal/contracts/normalize.go, untouched by this package) drops a
// workflow_run envelope whose pull_requests array is non-empty, which GitHub populates for a
// same-repo PR's run but reports empty for a cross-fork one. For the common same-repo case those
// runs never arrive here at all; the five-minute reconcile is the only path that ever records
// them. Routed rather than ignored because it is the only live-update path for the fork case,
// which does reach it.
func (in *Intake) Run(ctx context.Context) {
	binds := time.NewTicker(durableBindInterval)
	defer binds.Stop()
	// changed is taken before each Load, so settings stored after that Load wake the loop.
	for changed := in.settings.Changed(); ; {
		// poll stays nil, and so never ready, while the cache holds no settings: a tick then has
		// nothing to bind by, and the cache's Changed signal wakes the first bind.
		var poll <-chan time.Time
		if in.settings.Load() != nil {
			if in.bind(ctx) == bindBound {
				break
			}
			poll = binds.C
		}
		select {
		case <-ctx.Done():
			return
		case <-changed:
			changed = in.settings.Changed()
		case <-poll:
		}
	}
	// Nothing rebinds a bound intake, so the poll stops here rather than waking every two seconds
	// for the life of the process.
	binds.Stop()
	<-ctx.Done()
	stopCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), intakeStopBudget)
	defer cancel()
	in.StopDelivering(stopCtx)
}

// intakeStopBudget is how long Run's stop waits, from the moment its ctx ends, for the messages in
// their handlers to finish and be acknowledged before it gives them up (StopDelivering). In
// cmd/dispatch that ctx is the signal context, so the stop begins at SIGTERM and runs beside the
// HTTP drain and the document shutdown, which take up to stopGrace (30 s) less a 5 s reserve:
// httpDrainBeforeDocuments then documentShutdownTimeout (cmd/dispatch/shutdown.go). main's own
// stop of the intakes gives their handlers up at intakeStopDeadline (stopGrace less the same
// reserve, 25 s) after the signal at the latest. 15 s leaves 10 s before that for what follows the
// wait -- the handlers given up returning, which they do as soon as their context is cancelled,
// the negative acknowledgements and the drain's end -- and 15 s before the kubelet's kill. It is
// also well past what a handler GitHub answers needs, a pull request and its commit pages with a
// page GitHub answered 502 asked again (after 1 s, then 2 s: githubPageRetryWait), so an ordinary
// handler finishes and is acknowledged rather than redone. A call GitHub does not answer holds one
// attempt for up to githubapp.RequestTimeout (20 s), and is then asked again, which no budget
// inside stopGrace could wait out.
const intakeStopBudget = 15 * time.Second

// route dispatches one envelope payload by its kind field: a pull request to
// handlePullRequestEnvelope, a workflow run to routeWorkflowEnvelope, any other kind nowhere. It is
// the whole filter: the durable is handed every GitHub notification on the bus
// (githubIntakeSubject), CI settlements, comments and the events Envoy builds no payload for among
// them. The payload is decoded once, into its fields. An empty payload, or one of a kind route does
// not handle, is discarded before any GitHub call or write, whatever its other fields hold (a CI
// settlement's hold arrays, cistore.Summary); only a pull request's or a workflow run's fields are
// read as the flat string map its handler takes. handled reports whether the kind is one route
// hands to a handler, whatever that handler then decides, so deliver records the event's time for
// a pull request that was not merged or another repository's run as well. A payload that is not a
// JSON object, a kind that is not a string, and a handled kind's payload holding anything but
// strings are errMalformedPayload.
//
// An envelope whose handler stored a fact the issue.deployed rule reads (a pull request's row, a
// run, its jobs) ends with recordDeployed, whether or not the handler then failed: a run written
// before its jobs fetch failed may still be the run that shipped a pull request stored earlier.
func (in *Intake) route(ctx context.Context, settings DeliverySettings, raw string) (handled bool, err error) {
	if raw == "" {
		return false, nil
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &fields); err != nil {
		return false, fmt.Errorf("%w: %w", errMalformedPayload, err)
	}
	var kind string
	if field, ok := fields["kind"]; ok {
		if err := json.Unmarshal(field, &kind); err != nil {
			return false, fmt.Errorf("%w: kind: %w", errMalformedPayload, err)
		}
	}
	var handle func(context.Context, DeliverySettings, map[string]string) (bool, error)
	switch kind {
	case "pr":
		handle = in.handlePullRequestEnvelope
	case "workflow":
		handle = in.routeWorkflowEnvelope
	default:
		return false, nil
	}
	payload, err := flatPayload(fields)
	var wrote bool
	if err == nil {
		wrote, err = handle(ctx, settings, payload)
	}
	if wrote {
		in.recordDeployed(ctx, settings)
	}
	return true, err
}

// recordDeployed writes the issue.deployed of every pull request the envelope just handled may have
// shipped or completed (RecordDeployedEvents). A failure is logged and does not fail the envelope,
// which is acknowledged as every handled envelope is: the reconcile records at the end of every
// pass too, so a row this call could not write gets its event within five minutes.
func (in *Intake) recordDeployed(ctx context.Context, settings DeliverySettings) {
	written, err := RecordDeployedEvents(ctx, in.pool, settings, in.events, time.Now())
	if err != nil {
		slog.Error("dispatch delivery: record issue.deployed after an envelope", "error", err)
	}
	if written > 0 {
		slog.Info("dispatch delivery: recorded issue.deployed", "events", written, "by", "intake")
	}
}

// routeWorkflowEnvelope hands a workflow run to handleWorkflowEnvelope as the run kind its file is
// configured as. The runs this slice records are the deploy repository's own: another repository's
// run of a file with the same path is not one of them, and routing it would fetch a run this slice
// never stores. A run of a file neither setting names is not fetched either.
func (in *Intake) routeWorkflowEnvelope(ctx context.Context, settings DeliverySettings, payload map[string]string) (bool, error) {
	if payload["repo"] != settings.DeployRepo {
		return false, nil
	}
	switch payload["path"] {
	case settings.DeployWorkflowPath:
		return in.handleWorkflowEnvelope(ctx, settings, DeliveryRunKindDeploy, payload)
	case settings.PRChecksWorkflowPath:
		// A pr_checks run is no part of the shipping rule (shippingRunJoin reads deploy runs
		// alone), so storing one can ship nothing: the envelope ends with no deployed pass.
		_, err := in.handleWorkflowEnvelope(ctx, settings, DeliveryRunKindPRChecks, payload)
		return false, err
	default:
		return false, nil
	}
}

// errMalformedPayload is an envelope payload route cannot read: not a JSON object, a kind that is
// not a string, or a pull request's or workflow run's payload holding anything but strings.
var errMalformedPayload = errors.New("malformed envelope payload")

// flatPayload is a pull request's or workflow run's fields as the flat string map its handler
// reads, or errMalformedPayload when a field is not a string. The field's name is left out of the
// error, since an envelope's keys are its publisher's to choose.
func flatPayload(fields map[string]json.RawMessage) (map[string]string, error) {
	payload := make(map[string]string, len(fields))
	for name, field := range fields {
		var value string
		if err := json.Unmarshal(field, &value); err != nil {
			return nil, fmt.Errorf("%w: %w", errMalformedPayload, err)
		}
		payload[name] = value
	}
	return payload, nil
}

// githubIntakeSubject is the durable's one filter subject: every GitHub event Envoy relays. The
// subscription's callback reads each message's subject first (intakeKey) and acknowledges every
// subject that is not a pull request's own or a workflow run's without decoding it; the handler
// decides what to do with the rest (route), and fetches nothing for the ones this slice does not
// want.
//
// One subject rather than the set this once carried -- the wildcard pull-request subject
// notifications.github.*.*.pr.* beside one notifications.github.<owner>.<repo>.workflow.<file>.>
// per configured workflow file, through FilterSubjects -- because nats-server 2.10.29 does not
// hand that set's workflow messages over. Measured on one stream holding five pull-request and
// twelve workflow-run messages (TestWhichFilterShapeDeliversAWorkflowSubject): a durable carrying
// the set reports all of them in NumPending, then delivers the five pull requests and none of the
// runs, by pull fetch or push subscription alike. Each filter alone delivers its own messages,
// and so do two workflow filters together, or a concrete pull-request subject beside the
// workflow filter; only the wildcard pull-request filter beside it withholds the workflow
// messages, in either order. A withheld message is silent -- the consumer reports it pending and
// never errors -- so this takes the one shape that delivers everything and filters in the
// handler. It also means the filter no longer depends on the settings row, so a settings change
// needs no rebind.
const githubIntakeSubject = "notifications.github.>"

// intakeMessageTimeout bounds how long one envelope's own handling may take -- its GitHub calls
// (a pull request plus its commit pages, or a workflow run plus its jobs pages, each with this
// package's own bounded page retries) and its Postgres writes. A handler that runs past it is
// cancelled, logged and acked: the five-minute reconcile is this package's catch-up mechanism,
// so one slow envelope must not hold the consumer. A var so a test can shrink it.
var intakeMessageTimeout = time.Minute

// intakeMaxAckPending bounds how many messages NATS may have outstanding with this consumer at
// once, and so how many handlers run at once: one per key (intakeKey), and every message of a key
// waits for the one before it. All of them can be one key's, so the bound has to be what one
// handler can acknowledge in turn well inside intakeAckWait; the server's own default of 1,000 is
// far past it, and messages it redelivers under a handler still working accumulate in the
// client's pending buffer until that buffer drops them. A var so a test can shrink it.
var intakeMaxAckPending = 4

// intakeAckWait is how long NATS waits for an acknowledgement before redelivering. Derived from
// the two values above rather than chosen: when every outstanding message is one key's, the last
// of intakeMaxAckPending waits for the ones before it, so the bound is their total handling time,
// plus a minute for the acknowledgement itself to land. A function rather than a value so a test
// that shrinks either input gets an ack wait derived from it.
func intakeAckWait() time.Duration {
	return time.Duration(intakeMaxAckPending)*intakeMessageTimeout + time.Minute
}

// deliveryConsumerConfig is the policy bind stamps on the durable (bus.DispatchDeliveryDurable,
// one push consumer over githubIntakeSubject), whether it creates it or finds one an earlier
// release left with another filter or the server's own flow control. The server holds a backlog
// undelivered until a subscriber attaches, so a restart with events waiting loses none of them
// and none of them count against MaxAckPending in the meantime.
func deliveryConsumerConfig() natsgo.ConsumerConfig {
	return natsgo.ConsumerConfig{
		Durable:        bus.DispatchDeliveryDurable,
		DeliverSubject: natsgo.NewInbox(),
		FilterSubject:  githubIntakeSubject,
		AckPolicy:      natsgo.AckExplicitPolicy,
		AckWait:        intakeAckWait(),
		MaxAckPending:  intakeMaxAckPending,
	}
}

// durableBindInterval is how often an intake with nothing bound tries its durable again. It is
// cmd/listener/durable.go's interval, for the reason measured there: a push durable's binding is
// its holder's until that process stops, which took 35 to 71 s in production, so a two-second
// poll lands the bind within one interval of the holder going away. The dead holder's work
// resumes in seconds rather than up to the 30-second settings poll the bind once rode on.
//
// Unlike the listener, which gives up at its durableBindDeadline and exits so the runtime starts
// it again, Dispatch's standby polls for the life of the process: there is no deadline and no
// exit. A durable another envoy-dispatch process holds is not a failed start but the ordinary
// state of the second process, which has every other thing a Dispatch server does to get on with
// meanwhile.
const durableBindInterval = 2 * time.Second

// standbyLogEvery bounds how often one intake says it is standing by for its durable, and for
// the same reason. The first refusal is logged as it happens, so an operator sees at once which
// process stood by and why; the bind is then retried every durableBindInterval, and an
// unthrottled line would be 1,800 an hour per durable, each saying exactly what the first said.
const standbyLogEvery = 30 * time.Second

// consumerLease names the lease one durable's consumer reconciliation runs under: one process at
// a time reads that durable's configuration and corrects it. Two processes -- two builds during a
// rolling update, each re-running the reconciliation on every bind poll -- would otherwise
// reconcile one durable against each other, the second undoing what the first had just done.
//
// The name carries the durable because that, and only that, is what the exclusion is about: two
// processes over one durable. One name over both delivery durables coupled two independent
// reconciliations, and INFRA-66 measured what that cost against a real nats-server and the real
// envoy-dispatch binary, with the Dispatch user denied exactly
// $JS.API.CONSUMER.CREATE.ENVOY_NOTIFICATIONS.delivery-events.> (envoy-infra's live row "without
// the delivery consumer create Dispatch leaves only that intake unbound"). A denied AddConsumer
// draws no reply at all, so the create blocks for the client's whole JetStream MaxWait -- 10 s
// (internal/bus/nats.go, nats.MaxWait) -- and the lease is held for every second of it. A pass
// that failed is deliberately left unthrottled (bindDurableConsumer records no pass then), so the
// events intake took the lease again on its next durableBindInterval poll and held it for another
// 10 s, round after round:
//
//	17:57:16.232 ERROR dispatch delivery: create NATS consumer name=delivery-events error="context deadline exceeded"
//	17:57:16.232 INFO  dispatch duty: leased pass ran  lease=delivery-consumers
//	17:57:26.235 ERROR dispatch delivery: create NATS consumer name=delivery-events error="context deadline exceeded"
//	17:57:26.236 INFO  dispatch duty: leased pass ran  lease=delivery-consumers
//
// Under that one name the job intake, whose own durable is created by a pass under the same lease,
// could only get in during the milliseconds between those holds: in CI's run of that row it never
// did, and the row failed with "consumer delivery-jobs was not push-bound within 20s". A lease per
// durable keeps a stuck reconciliation to the durable it is stuck on.
func consumerLease(durable string) string { return "delivery-consumer-" + durable }

// bindOutcome is what one attempt at a durable did, which is three things rather than two: a
// durable another process holds, or has not created yet, is neither a bind nor a fault.
type bindOutcome int

const (
	// bindFailed is an attempt that got nowhere and says nothing about who holds the durable: the
	// lookup, the create, the update or the subscribe failed, and the failure is logged at ERROR.
	// The zero value, so an outcome nothing set is never read as a bind.
	bindFailed bindOutcome = iota
	// bindBound is this process holding the durable's push binding, with its subscription live.
	bindBound
	// bindStandby is NATS refusing the bind for a reason that is about the durable rather than
	// about this process: another process is bound to it (consumerHeldElsewhere), or it does not
	// exist yet (natsgo.ErrConsumerNotFound, the leased reconciliation not having created it).
	// Neither is an error: the caller polls on, and binds once the durable is free and there.
	bindStandby
)

// consumerHeldElsewhere reports whether err is NATS refusing a bind because the push durable
// already has a subscription on it ("consumer is already bound to a subscription"): the one
// refusal that means another process holds the durable rather than something being wrong with it.
// Matched on the message because the server sends it as a plain error with no code the client
// surfaces -- cmd/listener/durable.go and cmd/listener/main.go rely on the same string.
func consumerHeldElsewhere(err error) bool {
	return err != nil && strings.Contains(err.Error(), "consumer is already bound")
}

// bindThrottle is one intake's own bind-poll state for its own durable: when this process last
// reconciled that durable's configuration, and when it last said it was standing by for it.
//
// Per intake rather than a package-global map keyed by consumer name, because an intake is
// exactly what these windows are about -- one process's repeated bind polls of one durable --
// and a global outlives the durable it throttles: a second intake of the same name (a test's, or
// a process that reconnected to another NATS account) would skip the reconciliation its own
// durable has never had and never create it, and would stand by silently for a durable it has
// said nothing about.
type bindThrottle struct {
	mu sync.Mutex
	// reconciled is when this intake's last reconciliation pass over its durable was recorded.
	reconciled time.Time
	// standby is when the last standby line was logged and standbyReason the message it carried,
	// so a change of state -- the durable going from not created yet to held by another process
	// -- is said at once rather than waiting out the window of the line before it.
	standby       time.Time
	standbyReason string
}

// consumerReconcileInterval is the least time between two reconciliations of one durable's
// configuration by one intake (bindThrottle.reconcileDue): 30 s, the period the intake once polled
// its settings at, which is how often the loop the bind poll replaced reconciled at all.
const consumerReconcileInterval = 30 * time.Second

// reconcileDue reports whether this durable's reconciliation is to run now: the first attempt is,
// and a later one within consumerReconcileInterval of the last pass recorded is not.
//
// Throttled because the reconciliation runs on every bind poll, and a standby polls every
// durableBindInterval (2 s) for the life of the process: each poll of each durable would
// otherwise take that durable's consumer lease (consumerLease) -- two Postgres round trips --
// and write a "dispatch duty: leased pass ran" line, 1,800 of each an hour per durable, for a
// configuration that changes only with a release. The poll itself stays at durableBindInterval,
// which is what makes a hand-over from a dead holder fast.
func (t *bindThrottle) reconcileDue() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.reconciled.IsZero() || time.Since(t.reconciled) >= consumerReconcileInterval
}

// recordReconcile records a pass over this durable, which throttles the next
// consumerReconcileInterval of attempts.
func (t *bindThrottle) recordReconcile() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.reconciled = time.Now()
}

// reportStandby logs message at INFO -- this durable is not this process's to bind yet, for the
// reason message names -- unless this intake logged that same message within standbyLogEvery.
func (t *bindThrottle) reportStandby(name, message string) {
	t.mu.Lock()
	if t.standbyReason == message && time.Since(t.standby) < standbyLogEvery {
		t.mu.Unlock()
		return
	}
	t.standby, t.standbyReason = time.Now(), message
	t.mu.Unlock()
	slog.Info(message, "name", name)
}

// bind creates (on first run) or resumes (on a restart) the delivery durable and subscribes to it
// (subscribe, at the deliver subject the durable carries), throttling its own reconciliation and
// standby line on this intake's own bindThrottle.
func (in *Intake) bind(ctx context.Context) bindOutcome {
	_, outcome := bindDurableConsumer(ctx, in.nats, in.leases, in.throttle, deliveryConsumerConfig(), func() (*natsgo.Subscription, error) {
		return in.subscribe("")
	})
	return outcome
}

// bindDurableConsumer creates (on first run) or resumes (on a restart) a durable and subscribes to
// it through subscribe, and reports which of three things happened: this process bound it, another
// process holds it (bindStandby), or the attempt failed (bindFailed).
//
// The configuration reconciliation -- the ConsumerInfo lookup and every create, update and delete
// it leads to -- runs under this durable's own consumer lease (consumerLease,
// reconcileDurableConsumer), so one process at a time reconciles a durable -- and a durable whose
// reconciliation is stuck blocks no other durable's. A process that does not take the lease skips
// the reconciliation altogether and goes straight to the subscribe: the process holding the lease
// is doing that work, and this one's job is only to bind the durable or stand by for it. So does
// a process that reconciled this durable within the last consumerReconcileInterval (throttle, the
// caller's own bindThrottle), which is what keeps a two-second bind poll from leasing and logging
// a pass it has just made.
//
// The durable admits one subscriber, which is how exactly one process does this durable's work
// and how that process dying hands the work on: NATS refuses a second binder with "consumer is
// already bound" and releases the binding once the holder's interest goes away, so a caller that
// keeps polling (durableBindInterval) takes it over. It subscribes through client.Subscribe
// (subscribeDurable), which refuses a second durable on one bus.Client (bus.ErrSubscriptionHeld),
// so each durable bound here takes a client of its own.
//
// A durable that does not exist yet is the other refusal a caller waits out rather than fails on
// (natsgo.ErrConsumerNotFound): the reconciliation that creates it is leased, so on a fresh bus
// exactly one process creates the durable and every other one skips the reconciliation the
// holder is doing and asks the server for a durable whose creation has not landed yet. That is a
// "not yet", like the standby above, not a fault in this process -- so it polls on at
// durableBindInterval and binds the durable as soon as it is there.
func bindDurableConsumer(ctx context.Context, client *bus.Client, leases duty.Leases, throttle *bindThrottle, wanted natsgo.ConsumerConfig, subscribe func() (*natsgo.Subscription, error)) (*natsgo.Subscription, bindOutcome) {
	name, subject := wanted.Durable, wanted.FilterSubject
	if throttle.reconcileDue() {
		// reconciled is true when the pass does not run -- the lease is held elsewhere, or could
		// not be taken: a reconciliation that never ran failed nothing, and the subscribe below
		// says what this process's own state is.
		//
		// No period (0): this is not a periodic loop but a step of every bind poll, and what
		// keeps it from running on each of them is the throttle below, which is this process's
		// own and deliberately so -- a durable each process binds for itself. So the lease is
		// asked for mutual exclusion and nothing else.
		reconciled := true
		ran := duty.Exclusive(ctx, leases, consumerLease(wanted.Durable), 0, func(context.Context) {
			reconciled = reconcileDurableConsumer(client.JS(), wanted)
		})
		if !reconciled {
			return nil, bindFailed
		}
		// Recorded for a pass that ran and succeeded alone: one the lease refused reconciled
		// nothing, and the window is how long this durable's configuration is taken as settled.
		// Recording a skip would hold this process off a durable for consumerReconcileInterval after a
		// pass it never made: the process that did not take the lease would wait out that window
		// before it could create a durable the holder's pass did not create -- a pass of the
		// holder's that failed, or a holder that stopped between taking the lease and creating it
		// -- standing by meanwhile for a durable nobody is making. A pass that ran and failed is
		// made again on the next poll rather than skipped for the window. A pass shutdown cut
		// short reports false too (duty.Exclusive), so the window does not start then either:
		// this is the one caller that reads that answer, and at shutdown the process is going
		// anyway.
		if ran {
			throttle.recordReconcile()
		}
	}
	sub, err := subscribe()
	switch {
	case consumerHeldElsewhere(err):
		throttle.reportStandby(name, "dispatch delivery: NATS consumer held by another process; standing by")
		return nil, bindStandby
	case errors.Is(err, natsgo.ErrConsumerNotFound):
		// Matched with errors.Is rather than on the message, since nats.go exports the sentinel
		// its own subscribe returns for a bind of a durable the server does not have.
		throttle.reportStandby(name, "dispatch delivery: NATS consumer not created yet; standing by")
		return nil, bindStandby
	case err != nil:
		slog.Error("dispatch delivery: bind NATS consumer", "name", name, "subject", subject, "error", err)
		return nil, bindFailed
	}
	slog.Info("dispatch delivery: NATS consumer bound", "name", name, "subject", subject)
	return sub, bindBound
}

// reconcileDurableConsumer brings a durable's configuration to wanted and reports whether it
// could: it creates the durable when there is none, replaces one an earlier release left carrying
// a filter-subject set, and corrects a filter or flow control in place. It checks ConsumerInfo
// before AddConsumer, like cmd/listener/durable.go's startListenerSubscription: a durable's
// DeliverSubject is a random inbox chosen once at creation, and calling AddConsumer again with a
// freshly generated one on every restart would try to change a durable's config out from under
// itself rather than resuming its cursor.
//
// Its caller runs it under that durable's consumer lease (consumerLease), so one process at a
// time is in here for one durable.
//
// PushBound is read once, by the ConsumerInfo immediately before the delete, and even that read
// leaves a window of one round trip: a process whose own reconciliation was skipped -- the lease
// was held, or its throttle refused the pass -- goes straight to the subscribe in
// bindDurableConsumer without passing through here, so one that binds between that read and the
// delete has its durable deleted under it. No message is lost, since this pass creates
// the replacement and binds it; what the loser loses is its standby, silently. Intake.Run stops
// its bind poll as soon as it is bound ("Nothing rebinds a bound intake"), so that process
// delivers nothing for the life of the process with "dispatch delivery: NATS consumer bound" as
// its last word, and only its next restart clears it.
func reconcileDurableConsumer(js natsgo.JetStreamContext, wanted natsgo.ConsumerConfig) bool {
	name, subject := wanted.Durable, wanted.FilterSubject
	info, err := js.ConsumerInfo(bus.Stream, name)
	switch {
	case errors.Is(err, natsgo.ErrConsumerNotFound):
		if _, err := js.AddConsumer(bus.Stream, &wanted); err != nil {
			slog.Error("dispatch delivery: create NATS consumer", "name", name, "subject", subject, "error", err)
			return false
		}
	case err != nil:
		slog.Error("dispatch delivery: look up NATS consumer", "name", name, "error", err)
		return false
	case len(info.Config.FilterSubjects) > 0:
		// A durable an earlier release left carrying a filter-subject set, which withholds the
		// workflow runs this slice exists to record (githubIntakeSubject). It is replaced rather
		// than updated in place, and replaced at its own ack floor, so the replacement neither
		// replays what the old durable acknowledged nor skips what it had not
		// (TestBindReplacesAPluralFilterDurableAtItsOwnAckFloor).
		//
		// Re-read the durable immediately before the delete: the read above is two round trips
		// back, and a process that skipped this reconciliation binds without taking the lease,
		// so the durable can have been bound since. The ack floor comes from this read for the
		// same reason -- it is the last thing the durable acknowledged before the delete.
		//
		// That fresh read is the only thing that decides whether another process is bound to
		// this durable: one question, answered once, by the read closest to the delete, so the
		// standby line below reports a state no read of this pass contradicts. It also decides
		// it in the direction of acting: a durable whose binder went away between the two reads
		// is replaced on this pass rather than left plural for another consumerReconcileInterval,
		// which is what the second read was added for. The cost is the ConsumerInfo itself, one
		// per durable per pass the throttle admits (reconcileDue), not one per bind poll.
		bound, err := js.ConsumerInfo(bus.Stream, name)
		switch {
		case err != nil:
			slog.Error("dispatch delivery: look up NATS consumer", "name", name, "error", err)
			return false
		case bound.PushBound:
			slog.Info("dispatch delivery: another process is bound to the NATS consumer; leaving its configuration alone", "name", name)
			return true
		}
		recreate := wanted
		recreate.DeliverPolicy = natsgo.DeliverByStartSequencePolicy
		recreate.OptStartSeq = bound.AckFloor.Stream + 1
		if err := js.DeleteConsumer(bus.Stream, name); err != nil {
			slog.Error("dispatch delivery: replace multi-subject NATS consumer", "name", name, "error", err)
			return false
		}
		if _, err := js.AddConsumer(bus.Stream, &recreate); err != nil {
			slog.Error("dispatch delivery: create replacement NATS consumer", "name", name, "start_seq", recreate.OptStartSeq, "error", err)
			return false
		}
		slog.Info("dispatch delivery: multi-subject NATS consumer replaced", "name", name, "subject", subject, "start_seq", recreate.OptStartSeq)
	case info.Config.FilterSubject != wanted.FilterSubject ||
		info.Config.AckWait != wanted.AckWait ||
		info.Config.MaxAckPending != wanted.MaxAckPending:
		// The durable carries an earlier release's filter or flow control: update it in place,
		// preserving its delivery cursor, so this needs no operator to notice a log line and
		// restart the server. Every field here is one NATS lets an existing consumer change, on a
		// consumer another process is bound to as much as on a free one; nothing touches its
		// deliver policy or ack policy, which it does not.
		config := info.Config
		config.FilterSubject = wanted.FilterSubject
		config.AckWait = wanted.AckWait
		config.MaxAckPending = wanted.MaxAckPending
		if _, err := js.UpdateConsumer(bus.Stream, &config); err != nil {
			slog.Error("dispatch delivery: update NATS consumer policy", "name", name, "configured_subject", info.Config.FilterSubject, "wanted_subject", wanted.FilterSubject, "error", err)
			return false
		}
		slog.Info("dispatch delivery: NATS consumer policy updated", "name", name, "subject", wanted.FilterSubject, "ack_wait", wanted.AckWait, "max_ack_pending", wanted.MaxAckPending)
	}
	return true
}

// subscribeDurable subscribes deliver to wanted's durable through client.Subscribe, acknowledging
// by hand. With no deliverSubject, nats.go looks the durable up and subscribes at the deliver
// subject the durable carries, refusing a durable another subscriber holds ("consumer is already
// bound"). With one, it subscribes at deliverSubject without the lookup: for a caller that has just
// created the durable with that deliver subject, under its consumer lease.
func subscribeDurable(client *bus.Client, wanted natsgo.ConsumerConfig, deliverSubject string, deliver natsgo.MsgHandler) (*natsgo.Subscription, error) {
	options := []natsgo.SubOpt{natsgo.Bind(bus.Stream, wanted.Durable), natsgo.ManualAck()}
	if deliverSubject != "" {
		options = append(options, natsgo.DeliverSubject(deliverSubject), natsgo.SkipConsumerLookup())
	}
	// The subject must be the consumer's own filter subject: nats.go checks a bound
	// subscription's subject against it, and refuses an empty one ("subject does not match
	// consumer").
	return client.Subscribe(wanted.FilterSubject, deliver, options...)
}

// intakeDelivery is one subscription of the intake's: the client it subscribed on, the
// subscription, the keyed runner its callback (receive) hands messages to, and the context every
// handler of it runs under. Each subscribe makes one of its own, so stopping one ends that
// subscription and nothing a later subscribe starts.
type intakeDelivery struct {
	nats *bus.Client
	sub  *natsgo.Subscription
	keys *keyedRunner[intakeMessage]
	// handling is the parent of every handler's context (deliver), and abandon cancels it: the stop
	// gives up the handlers still running when its context ends.
	handling context.Context
	abandon  context.CancelFunc
	stopping atomic.Bool
	stopped  sync.Once

	mu sync.Mutex
	// held is what the stop hands back to NATS once no handler of this delivery is running: the
	// messages that were waiting behind their keys, and those the callback was handed since.
	held []*natsgo.Msg
	// released is set once held has been handed back; the callback then hands a message back at
	// once.
	released bool
}

// intakeMessage is one message on its way to its key's handler, with its envelope when keying it
// decoded the envelope (a workflow run's), so the handler does not decode it again.
type intakeMessage struct {
	msg      *natsgo.Msg
	envelope *contracts.Envelope
}

// subscribe subscribes the intake's handling to its durable's push deliveries
// (subscribeDurable): at the deliver subject the durable carries when deliverSubject is empty, as
// bind does, or at deliverSubject, for a caller that has just created the durable with it. The
// subscription's callback only routes each message (receive); its handler runs on the goroutine of
// the message's key (keyedRunner) and acknowledges it there (deliver). StopDelivering stops the
// subscription this made last, so a caller subscribing again stops the one before first.
func (in *Intake) subscribe(deliverSubject string) (*natsgo.Subscription, error) {
	handling, abandon := context.WithCancel(context.Background())
	delivery := &intakeDelivery{nats: in.nats, handling: handling, abandon: abandon}
	delivery.keys = newKeyedRunner(func(message intakeMessage) { in.deliver(handling, message) })
	sub, err := subscribeDurable(in.nats, deliveryConsumerConfig(), deliverSubject, delivery.receive)
	if err != nil {
		abandon()
		return nil, err
	}
	delivery.sub = sub
	in.mu.Lock()
	in.delivering = delivery
	in.mu.Unlock()
	return sub, nil
}

// StopDelivering stops this intake's subscription so that another process can take the durable
// over with nothing waiting out the ack wait. It drains the subscription, so the server delivers it
// nothing more, and waits for the messages in their handlers until ctx ends: one that finishes by
// then is acknowledged, and the handlers still running when ctx ends are given up -- their context
// is cancelled, so a GitHub call that does not answer is abandoned -- and their messages
// negatively acknowledged as they return. Only once no handler is running does it negatively
// acknowledge every message that was waiting behind its key and every message the callback was
// handed meanwhile, those NATS had already delivered to the client's buffer included, so the server
// redelivers them at once -- to the next holder, which can then not start a pull request's or run's
// next message while this process still works on the one before it. A message the callback is
// handed after that is handed back at once. It returns once the drain has ended -- at the latest
// intakeDrainWait after the last handler returned, or as ctx ends if that is sooner, when it
// unsubscribes instead -- having given up the subscription's registration on the client
// (bus.Client.Release), so that no reconnect binds it again and a later subscribe on the same
// client starts afresh.
//
// Run calls it as its ctx ends, under intakeStopBudget. It is safe from any goroutine and
// idempotent: a second call returns once the first has stopped, and a call with nothing subscribed
// returns at once. Every call's ctx bounds the handler wait of the stop under way, so a caller with
// a nearer deadline than the first (StartIntakes' stop) has the handlers given up when its own ctx
// ends.
func (in *Intake) StopDelivering(ctx context.Context) {
	in.mu.Lock()
	delivery := in.delivering
	in.mu.Unlock()
	if delivery != nil {
		delivery.stop(ctx)
	}
}

// stop is StopDelivering for this subscription. The drain goes first, so the server has the
// subscription's interest gone before any negative acknowledgement reaches it, and redelivers
// what this one gives back to the next subscriber rather than straight back here.
func (d *intakeDelivery) stop(ctx context.Context) {
	giveUp := context.AfterFunc(ctx, d.abandon)
	defer giveUp()
	d.stopped.Do(func() {
		d.stopping.Store(true)
		drained := true
		if err := d.sub.Drain(); err != nil {
			slog.Error("dispatch delivery: drain the intake's subscription", "error", err)
			drained = false
		}
		for _, waiting := range d.keys.stop() {
			d.giveBack(waiting.msg)
		}
		// Returns once every handler has, which a handler given up (abandon) does promptly: its
		// GitHub calls and its writes all run under its context.
		d.keys.wait()
		d.mu.Lock()
		d.released = true
		held := d.held
		d.held = nil
		d.mu.Unlock()
		for _, msg := range held {
			negativelyAcknowledge(msg)
		}
		// nats.go ends a drain on a goroutine of its own, once the callback has run every message
		// the client had buffered, and reports it only by the subscription turning invalid.
		waitedFrom := time.Now()
		for drained && d.sub.IsValid() {
			if d.handling.Err() != nil || time.Since(waitedFrom) >= intakeDrainWait {
				slog.Error("dispatch delivery: the intake's subscription did not finish draining; unsubscribing it", "waited", time.Since(waitedFrom))
				if err := d.sub.Unsubscribe(); err != nil {
					slog.Error("dispatch delivery: unsubscribe the intake's subscription", "error", err)
				}
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		// The client binds again a registration whose subscription is no longer valid, a drained
		// one included, so without this the next reconnect would bind this stopped callback, which
		// negatively acknowledges every message it is handed, straight back to the durable.
		d.nats.Release(d.sub)
		d.abandon()
	})
}

// giveBack hands msg back to NATS for the next holder: once no handler of this delivery is
// running, at once; until then it is held, and the stop hands it back with the rest. Handed back
// earlier, a message of a key whose handler is still running here could be redelivered to the
// next holder and run there beside it, and two fetches of one pull request or run at once can
// write the older answer last.
func (d *intakeDelivery) giveBack(msg *natsgo.Msg) {
	d.mu.Lock()
	if !d.released {
		d.held = append(d.held, msg)
		d.mu.Unlock()
		return
	}
	d.mu.Unlock()
	negativelyAcknowledge(msg)
}

// intakeDrainWait bounds how long stop waits for the drain to end once every handler has returned.
// By then all that is left is nats.go's: the callback handing back what the client had buffered,
// which never blocks, and the drain's flush to the server. So the bound is the one nats.go itself
// gives a connection's drain, natsgo.DefaultDrainTimeout -- bus connections never set their own --
// rather than intakeMessageTimeout, which bounds a handler, and the handlers are done. The stop's
// context ending cuts it shorter: by then the drain has had the whole handler wait to end.
const intakeDrainWait = natsgo.DefaultDrainTimeout

// receive is the subscription's callback, and only routes: a message once stopping is set is
// handed back (giveBack), a subject intakeKey does not want is acknowledged, and any other message
// goes to its key's handler (deliver).
func (d *intakeDelivery) receive(msg *natsgo.Msg) {
	if d.stopping.Load() {
		d.giveBack(msg)
		return
	}
	key, envelope, wanted := intakeKey(msg)
	if !wanted {
		acknowledge(msg)
		return
	}
	if !d.keys.run(key, intakeMessage{msg: msg, envelope: envelope}) {
		d.giveBack(msg)
	}
}

// intakeKey reads from a message's subject, notifications.github.<owner>.<repo>.<kind>..., whether
// the intake wants it, and the key its handler runs under. Only two shapes can carry a kind route
// handles: a pull request's own subject, pr.<n> and nothing after it (pr.<n>.review and
// pr.<n>.comment carry other kinds, contracts' githubTopic), and a workflow run's,
// workflow.<file>.<action>. Every other subject -- checks, comments, issues, pushes, job events --
// is not wanted, and is decided with no decode at all.
//
// A pull request is keyed by its subject's repository and number, with no decode. A workflow
// run's subject names its file and not its run, so its key is its payload's repo and run_id, and
// the envelope decoded for them is handed on. A workflow message whose envelope or payload does
// not decode is keyed by its subject, and its handler logs it.
func intakeKey(msg *natsgo.Msg) (key string, envelope *contracts.Envelope, wanted bool) {
	tokens := strings.Split(msg.Subject, ".")
	if len(tokens) < 5 || tokens[0] != "notifications" || tokens[1] != "github" {
		return "", nil, false
	}
	switch {
	case tokens[4] == "pr" && len(tokens) == 6:
		return "pr " + tokens[2] + "/" + tokens[3] + "#" + tokens[5], nil, true
	case tokens[4] == "workflow" && len(tokens) == 7:
		decoded, err := decodeEnvelope(msg.Data)
		if err != nil {
			return "workflow " + msg.Subject, nil, true
		}
		var run struct {
			Repo  string `json:"repo"`
			RunID string `json:"run_id"`
		}
		if err := json.Unmarshal([]byte(decoded.Payload), &run); err != nil {
			return "workflow " + msg.Subject, &decoded, true
		}
		return "workflow " + run.Repo + "#" + run.RunID, &decoded, true
	}
	return "", nil, false
}

// decodeEnvelope decodes one message's envelope. A var so a test can count the decodes.
var decodeEnvelope = func(data []byte) (contracts.Envelope, error) {
	var envelope contracts.Envelope
	err := json.Unmarshal(data, &envelope)
	return envelope, err
}

// deliver handles one message on its key's goroutine: it routes the envelope's payload by the
// settings the cache holds under intakeMessageTimeout, and acks: a malformed envelope has nothing
// to retry, and a route error (a failed GitHub call, or that deadline passing) is caught up by the
// next reconcile pass rather than by NATS redelivery -- see the package doc comment. The deadline
// is what makes intakeAckWait a bound rather than a hope: without it one stuck GitHub call could
// hold an in-flight slot past the ack wait and have the message redelivered underneath it. An
// envelope route does not handle (an empty payload, or a kind route sends to no handler) is
// acknowledged and nothing else: no log line and no freshness write. One it handles records the
// event's time, whatever its handler decided.
//
// The one message it does not ack is one whose handling the stop gave up (handling cancelled,
// StopDelivering): that is negatively acknowledged, so the next holder of the durable redoes it at
// once rather than this event waiting five minutes for the reconcile. Its writes are the upserts
// reconcile makes too, so redoing what had in fact finished just before the stop gave it up
// writes the same facts again.
func (in *Intake) deliver(handling context.Context, message intakeMessage) {
	msg := message.msg
	defer func() {
		if handling.Err() != nil {
			slog.Warn("dispatch delivery: the intake stopped before the event's handling finished; handing it back", "subject", msg.Subject)
			negativelyAcknowledge(msg)
			return
		}
		acknowledge(msg)
	}()
	ctx, cancel := context.WithTimeout(handling, intakeMessageTimeout)
	defer cancel()

	envelope := message.envelope
	if envelope == nil {
		decoded, err := decodeEnvelope(msg.Data)
		if err != nil {
			slog.Error("dispatch delivery: decode envelope", "subject", msg.Subject, "error", err)
			return
		}
		envelope = &decoded
	}
	settings := in.settings.Load()
	if settings == nil {
		return
	}
	handled, err := in.route(ctx, *settings, envelope.Payload)
	switch {
	case errors.Is(err, errMalformedPayload):
		slog.Error("dispatch delivery: decode envelope payload", "subject", msg.Subject, "error", err)
		return
	case err != nil:
		slog.Warn("dispatch delivery: process event", "subject", msg.Subject, "error", err)
		return
	case !handled:
		return
	}
	if err := RecordEventAt(ctx, in.pool, time.Now()); err != nil {
		slog.Error("dispatch delivery: record event freshness", "error", err)
	}
}

// acknowledge acks msg. An acknowledgement that does not reach the server is never discarded: it
// leaves the message outstanding, and intakeMaxAckPending of those stop the consumer being given
// anything until the ack wait expires. The Nak gives the slot back at once; when it fails too (the
// connection is down, which is why the ack failed), the ack wait is the backstop.
func acknowledge(msg *natsgo.Msg) {
	if err := msg.Ack(); err != nil {
		slog.Error("dispatch delivery: acknowledge message", "subject", msg.Subject, "error", err)
		negativelyAcknowledge(msg)
	}
}

// negativelyAcknowledge hands msg back to the server for redelivery at once, logging a Nak that
// fails: the ack wait then redelivers it.
func negativelyAcknowledge(msg *natsgo.Msg) {
	if err := msg.Nak(); err != nil {
		slog.Error("dispatch delivery: negatively acknowledge message", "subject", msg.Subject, "error", err)
	}
}

// handlePullRequestEnvelope processes one `kind:"pr"` envelope. Only a merge (action "closed",
// merged "true") does anything; author and excluded-repository checks run on the envelope's own
// fields first, with no GitHub call, so a merge by anyone outside the population costs nothing.
// It reports whether it stored the pull request's row: a row it deleted, a pull request outside
// the population, ships with nothing.
func (in *Intake) handlePullRequestEnvelope(ctx context.Context, settings DeliverySettings, payload map[string]string) (wrote bool, err error) {
	if payload["kind"] != "pr" || payload["action"] != "closed" || payload["merged"] != "true" {
		return false, nil
	}
	repoFull := payload["repo"]
	number, err := strconv.Atoi(payload["number"])
	if err != nil {
		return false, fmt.Errorf("merged-PR envelope: malformed number %q: %w", payload["number"], err)
	}
	author := payload["author"]

	if containsString(settings.ExcludedRepos, repoFull) {
		return false, nil
	}
	if !containsString(settings.PopulationAuthors, author) {
		return false, nil
	}

	owner, repo, err := splitRepo(repoFull)
	if err != nil {
		return false, err
	}

	fetched, fetchErr := FetchPullRequest(ctx, in.github, owner, repo, number)
	if fetchErr != nil {
		// The population/task-label decision needs the fetch (labels aren't in the webhook
		// payload); without it we cannot tell whether this PR belongs in the population at all,
		// so nothing is written here. The reconcile's merged-PR search will find and complete
		// this PR on its own within five minutes.
		return false, fmt.Errorf("complete merged PR %s#%d: %w", repoFull, number, fetchErr)
	}
	if fetched.MergedAt == nil {
		return false, fmt.Errorf("merged-PR envelope for %s#%d, but GitHub reports it unmerged", repoFull, number)
	}

	raw := RawPullRequest{Repo: repoFull, Number: number, Title: fetched.Title, Author: fetched.Author, Labels: fetched.Labels, MergedAt: *fetched.MergedAt}
	isPopulation, err := IsPopulationPR(raw, settings, liveIntakeWindow())
	if err != nil {
		// IsTaskPR's "neither label" error: a deploy-repo PR GitHub's own classifier workflow has
		// not labelled yet. Surfaced, not guessed; the reconcile retries this PR's label on its
		// next pass.
		return false, fmt.Errorf("classify %s#%d: %w", repoFull, number, err)
	}
	if !isPopulation {
		return false, DeletePullRequest(ctx, in.pool, repoFull, number)
	}

	if err := completePullRequest(ctx, in.pool, in.github, owner, repo, repoFull, number, fetched); err != nil {
		return false, err
	}
	return true, nil
}

// completePullRequest finishes writing one merged pull request's complete row -- session
// trailers, attribution inputs, issue resolution, and the upsert -- once its GitHub facts
// (FetchPullRequest's answer) are in hand. The shared tail both intake's live
// handlePullRequestEnvelope and reconcile's completePartialPullRequest call, so "a live webhook
// completes a PR" and "reconcile completes a partial row" write through the exact same last
// steps, never two copies that can drift from each other. A commit fetch that fails, for any
// reason, returns its error without writing anything: completing the row from what was read
// would record its sessions and commit-message keys short for good. The row stays as it was
// (still partial, for reconcile's own caller; unwritten, for intake's, whose merged-PR search then
// finds it), so the next pass retries the whole completion. A rate limit comes back as the
// *githubapp.RateLimitError, which stops reconcile's batch.
func completePullRequest(ctx context.Context, pool *store.Pool, github *githubapp.Client, owner, repo, repoFull string, number int, fetched FetchedPullRequest) error {
	messages, err := fetchCommitMessages(ctx, github, owner, repo, number)
	if err != nil {
		if limited, ok := githubapp.AsRateLimit(err); ok {
			return limited
		}
		return fmt.Errorf("complete %s#%d: %w", repoFull, number, err)
	}
	inputs := attributionInputsFrom(attributionFacts{
		Repo: repoFull, URL: fetched.URL, Title: fetched.Title, Body: fetched.Body,
		HeadRef: fetched.HeadRef, CommitMessages: messages,
	})
	issueKey, _, err := resolveStoredIssueKey(ctx, pool, fetched.URL, inputs)
	if err != nil {
		return err
	}
	return UpsertPullRequest(ctx, pool, DeliveryPullRequest{
		Repo: repoFull, Number: number, Title: fetched.Title, URL: fetched.URL, Author: fetched.Author,
		CreatedAt: &fetched.CreatedAt, MergedAt: fetched.MergedAt, FirstCommitAt: fetched.FirstCommitAt,
		MergeCommitSHA: fetched.MergeCommitSHA, Additions: fetched.Additions, Deletions: fetched.Deletions,
		Rework: IsRework(fetched.Title), IssueKey: issueKey, Sessions: sessionTrailers(messages),
		Attribution: &inputs, Partial: false,
	})
}

// liveIntakeWindow is the window IsPopulationPR is checked against for a live merge event. The
// 28-day rolling window is the reconcile backfill's concern (which historical PRs to import), not
// a live event's: a merge event is, by construction, happening now, so this window is wide enough
// to always contain it and exists only to satisfy IsPopulationPR's signature.
func liveIntakeWindow() TimeWindow {
	return TimeWindow{Start: time.Unix(0, 0), End: time.Now().Add(time.Hour)}
}

// handleWorkflowEnvelope processes one `kind:"workflow"` envelope for the workflow bound to kind
// (the deploy workflow or the PR-checks workflow, whichever subject this consumer was bound to).
// The envelope's repo/run_id are used only to know which run to ask GitHub about: every stored
// field (head SHA, timing, conclusion, URL, PR number) comes from FetchWorkflowRun's own answer,
// never trusted from the envelope directly -- an Envoy /v1 bearer (any agent session) can publish
// an arbitrary envelope naming any repo/run_id/conclusion/timing, and this is the one place a
// forged value could otherwise rewrite a real run's stored facts. A run row is written whatever
// its current state (so an in-progress run appears before it concludes, per CONTRACT.md); jobs
// are fetched and written only once GitHub reports the run concluded. It reports whether it stored
// the run, with or without its jobs.
func (in *Intake) handleWorkflowEnvelope(ctx context.Context, settings DeliverySettings, kind DeliveryRunKind, payload map[string]string) (wrote bool, err error) {
	if payload["kind"] != "workflow" {
		return false, nil
	}
	repoFull := payload["repo"]
	owner, repo, err := splitRepo(repoFull)
	if err != nil {
		return false, err
	}
	runID, err := strconv.ParseInt(payload["run_id"], 10, 64)
	if err != nil {
		return false, fmt.Errorf("workflow envelope: malformed run_id %q: %w", payload["run_id"], err)
	}

	fetched, err := FetchWorkflowRun(ctx, in.github, owner, repo, runID)
	if err != nil {
		return false, fmt.Errorf("verify run %d against GitHub: %w", runID, err)
	}

	if err := UpsertRun(ctx, in.pool, StoredRun(repoFull, kind, fetched)); err != nil {
		return false, fmt.Errorf("upsert run %d: %w", runID, err)
	}

	if fetched.CompletedAt == nil {
		return true, nil
	}
	fetchedJobs, err := ListWorkflowRunJobs(ctx, in.github, owner, repo, runID)
	if err != nil {
		// The run row is already written; the reconcile's own run listing fetches this run's
		// jobs too and will complete them.
		return true, fmt.Errorf("fetch jobs of run %d: %w", runID, err)
	}
	jobs := RunJobs(repoFull, runID, fetchedJobs, settings.Production())
	return true, UpsertRunJobs(ctx, in.pool, repoFull, runID, jobs, JobsComplete(jobs))
}

// commitMessagePayload is one element of GET /repos/{owner}/{repo}/pulls/{number}/commits,
// limited to the message field fetchCommitMessages reads (distinct from github_prs.go's
// commitPayload, which reads the same endpoint's dates instead).
type commitMessagePayload struct {
	Commit struct {
		Message string `json:"message"`
	} `json:"commit"`
}

// maxCommitPages bounds fetchCommitMessages' pagination: 500 commits is already an enormous pull
// request, and a PR beyond that is not worth the API cost of chasing its full history.
const maxCommitPages = 5

// fetchCommitMessages reads a pull request's commit messages, in commit order: what its session
// trailers (sessionTrailers) and its commit-message issue keys (AttributionInputs.CommitKeys) are
// read from. Shared by Intake and Reconcile (both complete a PR the same way; reconcile must read
// them itself rather than carrying forward whatever a stale row already had, see store.go's
// UpsertPullRequest doc comment on why a partial row is never allowed to regress a complete one's
// attribution). On an error it answers the messages of the pages it read.
func fetchCommitMessages(ctx context.Context, client *githubapp.Client, owner, repo string, number int) ([]string, error) {
	token := func() (string, error) { return client.RepositoryToken(ctx, owner, repo) }
	messages, _, err := fetchCommitMessagesWithToken(ctx, client, token, owner, repo, number)
	return messages, err
}

// fetchCommitMessagesWithToken is fetchCommitMessages asking token for each page's installation
// token, by readGitHubPage's rule. It also answers how many commit pages it asked GitHub for, the
// failed one included, so a caller can count what a read cost (reconcileAttributionInputs' call
// budget).
func fetchCommitMessagesWithToken(ctx context.Context, client *githubapp.Client, token func() (string, error), owner, repo string, number int) ([]string, int, error) {
	var messages []string
	for page := 1; page <= maxCommitPages; page++ {
		current, err := token()
		if err != nil {
			return messages, page - 1, fmt.Errorf("mint installation token for %s/%s PR #%d commits page %d: %w", owner, repo, number, page, err)
		}
		commitsPath := fmt.Sprintf("/repos/%s/%s/pulls/%d/commits?per_page=100&page=%d", url.PathEscape(owner), url.PathEscape(repo), number, page)
		body, status, header, err := readGitHubPage(ctx, client, current, commitsPath)
		if err != nil {
			return messages, page, fmt.Errorf("fetch commits of PR #%d page %d: %w", number, page, err)
		}
		if err := githubapp.CheckResponse(status, header, body); err != nil {
			return messages, page, fmt.Errorf("fetch commits of PR #%d page %d: %w", number, page, err)
		}
		var commits []commitMessagePayload
		if err := json.Unmarshal(body, &commits); err != nil {
			return messages, page, fmt.Errorf("decode commits of PR #%d page %d: %w", number, page, err)
		}
		for _, c := range commits {
			messages = append(messages, c.Commit.Message)
		}
		if len(commits) < 100 {
			return messages, page, nil
		}
	}
	return messages, maxCommitPages, nil
}

// sessionTrailers is every `Omp-Session:` commit trailer across messages (LEGION-294's rule,
// ported from the prototype's `session_ids`), first-seen order, no repeats.
func sessionTrailers(messages []string) []string {
	sessions := []string{}
	for _, message := range messages {
		for _, match := range ompSessionTrailer.FindAllStringSubmatch(message, -1) {
			sessions = appendUnique(sessions, match[1])
		}
	}
	return sessions
}

// mapRunConclusion maps GitHub's raw run conclusion to this schema's narrower check constraint
// (success, failure, cancelled only); any other value (including an empty string for a run still
// in progress) is nil, matching FetchedRun's documented policy.
func mapRunConclusion(raw string) *DeliveryRunConclusion {
	switch raw {
	case "success":
		c := DeliveryRunConclusionSuccess
		return &c
	case "failure":
		c := DeliveryRunConclusionFailure
		return &c
	case "cancelled":
		c := DeliveryRunConclusionCancelled
		return &c
	default:
		return nil
	}
}

// mapJobConclusion is mapRunConclusion's job-level counterpart: a job's schema accepts a wider
// set (skipped, timed_out in addition to the three a run accepts).
func mapJobConclusion(raw *string) *DeliveryJobConclusion {
	if raw == nil {
		return nil
	}
	switch *raw {
	case "success":
		c := DeliveryJobConclusionSuccess
		return &c
	case "failure":
		c := DeliveryJobConclusionFailure
		return &c
	case "cancelled":
		c := DeliveryJobConclusionCancelled
		return &c
	case "skipped":
		c := DeliveryJobConclusionSkipped
		return &c
	case "timed_out":
		c := DeliveryJobConclusionTimedOut
		return &c
	default:
		return nil
	}
}

// splitRepo splits "owner/repo" into its two segments.
func splitRepo(repoFull string) (owner, repo string, err error) {
	parts := strings.SplitN(repoFull, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("malformed repository %q: want owner/repo", repoFull)
	}
	return parts[0], parts[1], nil
}

// containsString reports whether values contains s, exactly.
func containsString(values []string, s string) bool {
	for _, v := range values {
		if v == s {
			return true
		}
	}
	return false
}
