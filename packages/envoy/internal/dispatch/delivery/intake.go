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

	"github.com/google/uuid"
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
	pool   *store.Pool
	github *githubapp.Client
	events *events.Broker
	// current is the settings the handler routes by. Read on every settingsPollInterval tick and
	// published here rather than captured by the handler: the durable's filter subject never
	// changes, so a settings change must reach the running handler without rebinding anything.
	// Only settings whose deploy repository splitRepo accepts are published, so route can compare
	// against it as it stands.
	current atomic.Pointer[DeliverySettings]
	// heldDurable is this intake's delivery from bus.DispatchDeliveryDurable under the durable's
	// hold: subscribe, StopDelivering and the take-over.
	heldDurable
}

// newIntake builds an Intake on natsClient, a connected client StartIntakes opens for it alone,
// holding its durable and taking it over under a hold and a lease it takes on leases.
func newIntake(natsClient *bus.Client, pool *store.Pool, leases intakeLeases, github *githubapp.Client, broker *events.Broker) *Intake {
	in := &Intake{pool: pool, github: github, events: broker}
	in.heldDurable = newHeldDurable(natsClient, leases, deliveryConsumerConfig, in.deliver)
	return in
}

// Run delivers GitHub events from the durable consumer while this process holds the durable's
// hold, and stands by for the hold while another process holds it, until ctx is cancelled. Idle
// until a delivery_settings row exists.
//
// The durable carries one filter subject, githubIntakeSubject, which no setting changes, so no
// setting rebinds it. delivery_settings is read on every settingsPollInterval tick and published
// to the running handler, so a settings change -- a different deploy repository or workflow path
// -- takes effect within one poll interval with no restart. route does all the filtering, from
// those settings.
//
// Several envoy-dispatch processes run at once during a rolling release, of this build and of the
// one before it, and exactly one of them delivers from the durable: the one holding its hold
// (heldDurable). The others stand by, trying for the hold every durableBindInterval for the life
// of the process, with no deadline, and take the durable over within seconds of the holder
// releasing its hold, or of the hold lapsing when the holder dies. A holder checks every
// intakeHoldRenew that the durable still delivers to it, and stops delivering the moment its hold
// is lost. When ctx ends it stops delivering, handing back what it holds, and releases the hold,
// so a standby takes over at once (heldDurable.stop).
//
// The PR-checks workflow path is live-reachable only for a fork-originated pull request: Envoy's
// own webhook normalizer (internal/contracts/normalize.go, untouched by this package) drops a
// workflow_run envelope whose pull_requests array is non-empty, which GitHub populates for a
// same-repo PR's run but reports empty for a cross-fork one. For the common same-repo case those
// runs never arrive here at all; the five-minute reconcile is the only path that ever records
// them. Routed rather than ignored because it is the only live-update path for the fork case,
// which does reach it.
func (in *Intake) Run(ctx context.Context) {
	reads := time.NewTicker(settingsPollInterval)
	defer reads.Stop()
	refresh := func() {
		settings, err := GetSettings(ctx, in.pool)
		if err != nil {
			if errors.Is(err, ErrNoSettings) {
				return
			}
			slog.Error("dispatch delivery: read settings for intake", "error", err)
			return
		}
		if _, _, err := splitRepo(settings.DeployRepo); err != nil {
			slog.Error("dispatch delivery: deploy_repo setting", "error", err)
			return
		}
		in.current.Store(&settings)
	}
	refresh()
	// The durable is tried only once a settings row has been read: the handler routes by those
	// settings, and an intake with none has nothing to do with a message.
	in.run(ctx, reads.C, refresh, func() bool { return in.current.Load() != nil })
}

// handle routes one envelope payload by the settings in force (route).
func (in *Intake) handle(ctx context.Context, payload string) (bool, error) {
	settings := in.current.Load()
	if settings == nil {
		return false, nil
	}
	return in.route(ctx, *settings, payload)
}

// settingsPollInterval is how often Run re-reads delivery_settings so a settings PUT reaches the
// running handler -- which repository and which workflow paths it routes by -- without an
// operator restarting the server. A var so a test can shrink it.
var settingsPollInterval = 30 * time.Second

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
	written, err := RecordDeployedEvents(ctx, in.pool, settings, in.events)
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
// handler decides what to do with each (route), and fetches nothing for the ones this slice does
// not want, which costs one decode of the payload per envelope.
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
// once. The handler does its GitHub calls inline on the subscription's own delivery goroutine,
// one message at a time, so the bound has to be what that one handler can acknowledge well
// inside intakeAckWait; the server's own default of 1,000 is far past it, and messages it
// redelivers under a handler still working accumulate in the client's pending buffer until that
// buffer drops them. A var so a test can shrink it.
var intakeMaxAckPending = 4

// intakeAckWait is how long NATS waits for an acknowledgement before redelivering. Derived from
// the two values above rather than chosen: the last of intakeMaxAckPending outstanding messages
// waits for the ones before it, so the bound is their total handling time, plus a minute for the
// acknowledgement itself to land. A function rather than a value so a test that shrinks either
// input gets an ack wait derived from it.
func intakeAckWait() time.Duration {
	return time.Duration(intakeMaxAckPending)*intakeMessageTimeout + time.Minute
}

// deliveryConsumerConfig is the policy a take-over creates the durable with
// (bus.DispatchDeliveryDurable, one push consumer over githubIntakeSubject), unless the deployed
// durable carries a higher generation (takeOverConfig). The server holds a backlog undelivered
// until a subscriber attaches, so a restart with events waiting loses none of them and none of
// them count against MaxAckPending in the meantime.
//
// FilterSubject, AckWait and MaxAckPending keep the values of the build before generations: that
// build's processes, still running during a rolling release, update a durable whose three differ
// from theirs in place on every settings poll, so a change to any of them waits until no such
// process is left. A change to this config takes a new consumerConfigGeneration and a new row in
// TestTheConsumerConfigEqualsItsGenerationsPinnedRow.
func deliveryConsumerConfig() natsgo.ConsumerConfig {
	return natsgo.ConsumerConfig{
		Durable:        bus.DispatchDeliveryDurable,
		DeliverSubject: natsgo.NewInbox(),
		FilterSubject:  githubIntakeSubject,
		AckPolicy:      natsgo.AckExplicitPolicy,
		AckWait:        intakeAckWait(),
		MaxAckPending:  intakeMaxAckPending,
		Metadata:       consumerGenerationMetadata(),
	}
}

// consumerConfigGeneration is the generation of the intakes' consumer configs
// (deliveryConsumerConfig, jobConsumerConfig), stamped on each durable as its
// consumerGenerationKey metadata. A take-over that finds a durable of a higher generation
// recreates it with the deployed config rather than this build's (takeOverConfig), so while two
// builds run, the older one never reverts what the newer one set.
const consumerConfigGeneration = 1

// consumerGenerationKey is the consumer metadata key that holds the generation of its config. A
// durable without it was last created by a build before generations: generation 0.
const consumerGenerationKey = "dispatch.consumer-generation"

// consumerGenerationMetadata is the metadata this build creates a durable with: its generation.
func consumerGenerationMetadata() map[string]string {
	return map[string]string{consumerGenerationKey: strconv.Itoa(consumerConfigGeneration)}
}

// takeOverConfig is the config a take-over recreates the durable deployed describes with: the
// deployed config when a newer build stamped it with a higher generation, this build's (wanted)
// otherwise, on a fresh deliver subject, starting at the stream sequence after the durable's ack
// floor. Starting there, the recreated durable redelivers at once every message the durable had
// not acknowledged -- what a dead holder had been handed, what a stopped one handed back --
// replays nothing it had, and skips nothing (TestBindReplacesAPluralFilterDurableAtItsOwnAckFloor).
// The take-over records that start in its hold's token before it deletes the durable, so a create
// that fails after the delete loses neither (intakeHoldToken).
// The deployed metadata keeps its own keys and drops the ones nats-server sets itself (the
// `_nats.` prefix), which the server sets again on create.
func takeOverConfig(deployed *natsgo.ConsumerInfo, wanted natsgo.ConsumerConfig) natsgo.ConsumerConfig {
	config := wanted
	if bus.MetadataGeneration(deployed.Config.Metadata, consumerGenerationKey) > consumerConfigGeneration {
		config = deployed.Config
		config.DeliverSubject = natsgo.NewInbox()
		config.OptStartTime = nil
		config.Metadata = bus.OwnMetadata(deployed.Config.Metadata, 0)
	}
	return startingAt(config, deployed.AckFloor.Stream+1)
}

// startingAt is config delivering from stream sequence start on.
func startingAt(config natsgo.ConsumerConfig, start uint64) natsgo.ConsumerConfig {
	config.DeliverPolicy = natsgo.DeliverByStartSequencePolicy
	config.OptStartSeq = start
	return config
}

// durableBindInterval is how often an intake that does not deliver tries for its durable's hold,
// and for the durable under it. It is cmd/listener/durable.go's interval: a standby polls every two
// seconds, so it takes the durable over within one interval of the holder releasing its hold, and
// within one interval of the hold's lapse when the holder dies (intakeHoldTTL).
//
// Unlike the listener, which gives up at its durableBindDeadline and exits so the runtime starts
// it again, Dispatch's standby polls for the life of the process: there is no deadline and no
// exit. A durable another envoy-dispatch process holds is not a failed start but the ordinary
// state of every process but one, which has every other thing a Dispatch server does to get on
// with meanwhile.
const durableBindInterval = 2 * time.Second

// standbyLogEvery bounds how often one intake says it is standing by for its durable. The first
// refusal is logged as it happens, so an operator sees at once which process stood by and why;
// the hold is then tried every durableBindInterval, and an unthrottled line would be 1,800 an hour
// per durable, each saying exactly what the first said.
const standbyLogEvery = 30 * time.Second

// The hold a process delivers from a durable under (intakeHoldName, duty.Hold): it lasts
// intakeHoldTTL on the database's clock after each renewal, is renewed every intakeHoldRenew, and
// reports itself lost intakeHoldMargin before it lapses, so the holder stops delivering before any
// standby can take it. A holder that dies is replaced within intakeHoldTTL plus one
// durableBindInterval. The holder checks that the durable still delivers to it on the same
// interval it renews (heldDurable.check).
const (
	intakeHoldTTL    = 15 * time.Second
	intakeHoldRenew  = 5 * time.Second
	intakeHoldMargin = 2 * time.Second
)

// intakeHoldName names the hold a process delivers from durable under. Its token records what the
// next holder needs (intakeHoldToken).
func intakeHoldName(durable string) string { return "delivery-intake-" + durable }

// intakeHoldToken is what an intake's hold records as its token:
//
//   - subject, the deliver subject the last holder made the durable deliver to, which tells the
//     next holder whether a binding it finds is a holder's (heldDurable.boundElsewhere);
//   - start, the stream sequence a take-over that deleted the durable creates it again at, or 0
//     for none. A take-over records it before its delete and clears it once its create succeeds.
//     A create that fails after the delete -- a JetStream API timeout, a stop cancelling its
//     context, a crash between the two -- leaves it in the row, so the next take-over, of this
//     intake or of the process that takes the hold next, creates the durable at the old ack floor
//     rather than at the default start (deliveryConsumerConfig names none, so the stream's first
//     message). A holder that stops with it set lapses its hold rather than deleting the row
//     (heldDurable.stop), so the next holder still reads it.
//
// The token is the subject alone, or the subject followed by intakeHoldTokenStart and the
// sequence in decimal. A deliver subject holds no space, so the two never mix. A token of any
// other shape reads as a subject alone.
type intakeHoldToken struct {
	subject string
	start   uint64
}

// intakeHoldTokenStart separates an intake hold token's subject from its start.
const intakeHoldTokenStart = " start="

func (t intakeHoldToken) String() string {
	if t.start == 0 {
		return t.subject
	}
	return t.subject + intakeHoldTokenStart + strconv.FormatUint(t.start, 10)
}

// parseIntakeHoldToken reads the token a hold's row records (intakeHoldToken).
func parseIntakeHoldToken(token string) intakeHoldToken {
	subject, start, found := strings.Cut(token, intakeHoldTokenStart)
	if !found {
		return intakeHoldToken{subject: token}
	}
	sequence, err := strconv.ParseUint(start, 10, 64)
	if err != nil || sequence == 0 {
		return intakeHoldToken{subject: token}
	}
	return intakeHoldToken{subject: subject, start: sequence}
}

// intakeLeases is the pool an intake takes its durable's hold and consumer lease on and renews the
// hold through: store.Pool.Leases, which StartIntakes hands each intake. A *pgxpool.Pool is both.
type intakeLeases interface {
	duty.Leases
	duty.Holds
}

// consumerLease names the lease every change to one durable runs under: this build's take-over
// (heldDurable.takeOver), and the configuration reconciliation of the build before holds, whose
// processes still run during a rolling release and take the same lease. So the two never delete,
// create or update one durable at once.
//
// The name carries the durable because that, and only that, is what the exclusion is about: two
// processes over one durable. One name over both delivery durables coupled two independent
// reconciliations, and INFRA-66 measured what that cost against a real nats-server and the real
// envoy-dispatch binary, with the Dispatch user denied exactly
// $JS.API.CONSUMER.CREATE.ENVOY_NOTIFICATIONS.delivery-events.> (envoy-infra's live row "without
// the delivery consumer create Dispatch leaves only that intake unbound"). A denied AddConsumer
// draws no reply at all, so the create blocks for the client's whole JetStream MaxWait -- 10 s
// (internal/bus/nats.go, nats.MaxWait) -- and the lease is held for every second of it. The events
// intake took the lease again on its next durableBindInterval poll and held it for another 10 s,
// round after round:
//
//	17:57:16.232 ERROR dispatch delivery: create NATS consumer name=delivery-events error="context deadline exceeded"
//	17:57:16.232 INFO  dispatch duty: leased pass ran  lease=delivery-consumers
//	17:57:26.235 ERROR dispatch delivery: create NATS consumer name=delivery-events error="context deadline exceeded"
//	17:57:26.236 INFO  dispatch duty: leased pass ran  lease=delivery-consumers
//
// Under that one name the job intake, whose own durable is created by a pass under the same lease,
// could only get in during the milliseconds between those holds: in CI's run of that row it never
// did, and the row failed with "consumer delivery-jobs was not push-bound within 20s". A lease per
// durable keeps a stuck change to the durable it is stuck on.
func consumerLease(durable string) string { return "delivery-consumer-" + durable }

// bindThrottle is one intake's own standby-log state for its own durable: when it last said it
// was standing by for it, and why. Per intake rather than a package-global map keyed by consumer
// name, because an intake is exactly what the window is about -- one process's repeated polls of
// one durable -- and a global outlives the durable it throttles: a second intake of the same name
// would stand by silently for a durable it has said nothing about.
type bindThrottle struct {
	mu sync.Mutex
	// standby is when the last standby line was logged and standbyReason the message it carried,
	// so a change of state -- the hold going from another process's to a binding no hold recorded
	// -- is said at once rather than waiting out the window of the line before it.
	standby       time.Time
	standbyReason string
}

// reportStandby logs message at INFO -- this durable is not this process's to deliver from yet,
// for the reason message names -- unless this intake logged that same message within
// standbyLogEvery.
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

// heldDurable is one intake's delivery from its durable, which it does only while it holds the
// durable's hold (intakeHoldName), so that of every envoy-dispatch process -- of this build and of
// the build before it, during a rolling release -- one delivers from the durable at a time:
//
//   - A standby tries for the hold every durableBindInterval and takes it when no holder holds it
//     live: released, lapsed, or never taken.
//   - Under the hold, and under the durable's consumer lease, it takes the durable over
//     (takeOver): it records the stream sequence after the durable's ack floor in the hold's
//     token, deletes the durable, subscribes to a fresh deliver subject, creates the durable again
//     at that sequence on that subject, and records the subject alone as the hold's token
//     (intakeHoldToken). The delete and create, rather than an update of the deliver subject,
//     redeliver a dead holder's unacked messages at once and fence the holder off: a
//     subscription its client sends again after a reconnect is on a subject the durable no
//     longer delivers to.
//     nats-server refuses that update anyway while the old subject still has interest, which a
//     dead connection keeps for minutes; a delete has no such check. Subscribing before the
//     create leaves the durable push-bound from the moment it exists. A durable bound to a
//     subject no hold recorded is a process of the build before holds, which binds by lookup and
//     holds nothing: it is never taken from it, and the take-over waits until it lets go.
//   - The holder stops delivering at once when its hold is lost, and when its check finds the
//     durable delivering to another subject (check); it hands a message back rather than handle
//     it once its hold's loss deadline has passed (intakeDelivery.receive).
//   - When its process stops, it stops delivering, handing back every message it was delivered
//     and did not handle, and then releases the hold (stop), so a standby takes over at once and
//     is redelivered those messages within seconds rather than after the ack wait.
//
// The run loop owns hold, recorded and subject; mu guards delivering, which StopDelivering reads
// from any goroutine.
type heldDurable struct {
	nats   *bus.Client
	leases intakeLeases
	// holder names this intake in its hold's row: each intake draws one as it is built.
	holder uuid.UUID
	// throttle is this intake's own standby-log state (bindThrottle).
	throttle *bindThrottle
	// config is the durable's config in this build; each call draws a fresh deliver subject.
	config func() natsgo.ConsumerConfig
	// name is the durable's name: config's Durable, which every call returns alike.
	name string
	// handle handles one message on the subscription's delivery goroutine and acknowledges it.
	handle natsgo.MsgHandler

	// hold is the durable's hold while this intake holds it.
	hold *duty.Hold
	// recorded is the token the hold's row records (intakeHoldToken): the deliver subject the last
	// holder made the durable deliver to, which may be this intake's own, and the start a take-over
	// that deleted the durable and did not create it again recorded.
	recorded intakeHoldToken
	// subject is the deliver subject this intake delivers at, and empty while it delivers nothing.
	subject string

	mu sync.Mutex
	// delivering is the latest subscription subscribe made, which StopDelivering stops.
	delivering *intakeDelivery
}

// newHeldDurable builds the delivery from config's durable on natsClient, taking its hold and
// consumer lease on leases and handing each message to handle.
func newHeldDurable(natsClient *bus.Client, leases intakeLeases, config func() natsgo.ConsumerConfig, handle natsgo.MsgHandler) heldDurable {
	return heldDurable{nats: natsClient, leases: leases, holder: uuid.New(), throttle: &bindThrottle{}, config: config, name: config().Durable, handle: handle}
}

// run delivers from the durable while this intake holds its hold, and stands by for it otherwise,
// until ctx ends; then it stops delivering and releases the hold (stop). Each durableBindInterval
// while it delivers nothing, and ready reports true, it tries for the hold and the durable
// (attempt); each intakeHoldRenew while it delivers, it checks the durable still delivers to it
// (check); and the moment its hold is lost it stops delivering. extra and onExtra are a caller's
// own ticker and what it does on each tick, nil for none.
func (d *heldDurable) run(ctx context.Context, extra <-chan time.Time, onExtra func(), ready func() bool) {
	defer d.stop()
	binds := time.NewTicker(durableBindInterval)
	defer binds.Stop()
	checks := time.NewTicker(intakeHoldRenew)
	defer checks.Stop()
	attempt := func() {
		if d.subject == "" && ready() {
			d.attempt(ctx)
		}
	}
	attempt()
	for {
		// lost stays nil, and so never ready, while this intake holds no hold.
		var lost <-chan struct{}
		if d.hold != nil {
			lost = d.hold.Lost()
		}
		select {
		case <-ctx.Done():
			return
		case <-extra:
			onExtra()
		case <-binds.C:
			attempt()
		case <-checks.C:
			d.check(ctx)
		case <-lost:
			slog.Warn("dispatch delivery: the intake's hold is lost; it stops delivering", "name", d.name)
			d.StopDelivering()
			d.hold, d.subject = nil, ""
		}
	}
}

// attempt tries to make this intake the one delivering from the durable: it takes the hold when
// no other process holds it live, then takes the durable over under it (takeOver). A durable
// bound to a subject no hold recorded is read first outside the consumer lease, so a holder that
// waits for a process of the build before holds to let go takes no lease on every poll.
func (d *heldDurable) attempt(ctx context.Context) {
	if d.hold == nil && !d.takeHold(ctx, d.name) {
		return
	}
	info, err := d.lookupDurable(ctx)
	if err == nil && d.boundElsewhere(info) {
		return
	}
	duty.Exclusive(ctx, d.leases, consumerLease(d.name), 0, d.takeOver)
}

// lookupDurable reads the durable's state from the server, bounded by intakeHoldRenew.
func (d *heldDurable) lookupDurable(ctx context.Context) (*natsgo.ConsumerInfo, error) {
	lookup, cancel := context.WithTimeout(ctx, intakeHoldRenew)
	defer cancel()
	return d.nats.JS().ConsumerInfo(bus.Stream, d.name, natsgo.Context(lookup))
}

// heldElsewhere is the standby line of an intake whose durable's hold another process holds live.
const heldElsewhere = "dispatch delivery: NATS consumer held by another process; standing by"

// takeHold takes the durable's hold when no holder holds it live, keeping the token its row
// records, and reports whether this intake holds it now. duty.TryHold records its own token as it
// takes a lapsed hold, so the lapsed hold's token is read first (duty.HoldToken).
func (d *heldDurable) takeHold(ctx context.Context, name string) bool {
	holdName := intakeHoldName(name)
	bounded, cancel := context.WithTimeout(ctx, intakeHoldRenew)
	defer cancel()
	token, live, err := duty.HoldToken(bounded, d.leases, holdName)
	switch {
	case err != nil:
		if ctx.Err() == nil {
			slog.Error("dispatch delivery: read the intake's hold", "name", name, "hold", holdName, "error", err)
		}
		return false
	case live:
		d.throttle.reportStandby(name, heldElsewhere)
		return false
	}
	hold, err := duty.TryHold(bounded, d.leases, holdName, d.holder, duty.HoldOptions{
		TTL: intakeHoldTTL, Renew: intakeHoldRenew, Margin: intakeHoldMargin, Token: token,
	})
	if err != nil {
		if ctx.Err() == nil {
			slog.Error("dispatch delivery: take the intake's hold", "name", name, "hold", holdName, "error", err)
		}
		return false
	}
	if hold == nil {
		d.throttle.reportStandby(name, heldElsewhere)
		return false
	}
	d.hold, d.recorded = hold, parseIntakeHoldToken(token)
	slog.Info("dispatch delivery: took the intake's hold", "name", name, "hold", holdName)
	return true
}

// boundElsewhere reports whether info is a durable bound to a subject the hold does not record: a
// process of the build before holds, which this intake never takes the durable from. It says so
// at INFO, throttled.
func (d *heldDurable) boundElsewhere(info *natsgo.ConsumerInfo) bool {
	if !info.PushBound || info.Config.DeliverSubject == d.recorded.subject {
		return false
	}
	d.throttle.reportStandby(info.Config.Durable, "dispatch delivery: NATS consumer bound to a process that holds no hold; standing by until it lets go")
	return true
}

// holds reports whether this intake may still act under its hold: the hold's loss deadline has
// not passed (duty.Hold.Deadline).
func (d *heldDurable) holds() bool {
	return d.hold != nil && time.Now().Before(d.hold.Deadline())
}

// takeOver makes the durable deliver to this intake, under the hold and the durable's consumer
// lease. A durable that does not exist is created with this build's config, at the start a
// take-over before this one recorded when it deleted the durable and did not create it again
// (intakeHoldToken). A durable bound to a subject no hold recorded is left as it is
// (boundElsewhere). Any other -- unbound, or bound to the subject the hold records, which is a
// holder that died or was cut off -- is deleted and created again at its ack floor on a fresh
// deliver subject (takeOverConfig), and that start is recorded as the hold's token before the
// delete. The durable's state is read again here, under the lease, since a process of the build
// before holds can bind it between attempt's read and this one.
//
// This intake subscribes to the fresh deliver subject before it creates the durable, so the
// durable is push-bound from the moment it exists. A process of the build before holds binds by
// looking the durable up and binding it when the lookup reports it unbound; a durable created
// before its subscriber would leave a moment in which that lookup finds it unbound, and both
// processes would then be handed every message on one subject. The subscribe sends its SUB on
// the client's connection before the create's request goes out on the same connection, and the
// server handles one connection's protocol in order, so the subject has interest before the
// create is even read (TestATakeOverCreatesTheDurableAlreadyBound). Then this intake records the
// subject alone as the hold's token, still under the lease.
func (d *heldDurable) takeOver(ctx context.Context) {
	js := d.nats.JS()
	wanted := d.config()
	name := wanted.Durable
	create := wanted
	info, err := js.ConsumerInfo(bus.Stream, name, natsgo.Context(ctx))
	switch {
	case errors.Is(err, natsgo.ErrConsumerNotFound):
		if d.recorded.start != 0 {
			create = startingAt(wanted, d.recorded.start)
		}
	case err != nil:
		slog.Error("dispatch delivery: look up NATS consumer", "name", name, "error", err)
		return
	case d.boundElsewhere(info):
		return
	default:
		create = takeOverConfig(info, wanted)
		if !d.holds() {
			return
		}
		pending := intakeHoldToken{subject: d.recorded.subject, start: create.OptStartSeq}
		if err := d.hold.SetToken(ctx, pending.String()); err != nil {
			slog.Error("dispatch delivery: record the take-over's start before deleting NATS consumer", "name", name, "start_seq", create.OptStartSeq, "error", err)
			return
		}
		d.recorded = pending
		if err := js.DeleteConsumer(bus.Stream, name, natsgo.Context(ctx)); err != nil && !errors.Is(err, natsgo.ErrConsumerNotFound) {
			slog.Error("dispatch delivery: delete NATS consumer to take it over", "name", name, "error", err)
			return
		}
	}
	if !d.holds() {
		return
	}
	if _, err := d.subscribe(create.DeliverSubject); err != nil {
		slog.Error("dispatch delivery: bind NATS consumer", "name", name, "subject", create.FilterSubject, "error", err)
		return
	}
	if _, err := createDurable(js, &create, natsgo.Context(ctx)); err != nil {
		slog.Error("dispatch delivery: create NATS consumer", "name", name, "subject", create.FilterSubject, "start_seq", create.OptStartSeq, "error", err)
		d.StopDelivering()
		return
	}
	bound := intakeHoldToken{subject: create.DeliverSubject}
	if err := d.hold.SetToken(ctx, bound.String()); err != nil {
		// The durable delivers here, but the hold does not say so: the next holder would take
		// the binding for a process that holds nothing and wait for it. Stop, and take the
		// durable over again on the next attempt.
		slog.Error("dispatch delivery: record the intake's deliver subject", "name", name, "error", err)
		d.StopDelivering()
		return
	}
	d.recorded, d.subject = bound, create.DeliverSubject
	slog.Info("dispatch delivery: NATS consumer bound", "name", name, "subject", create.FilterSubject,
		"start_seq", create.OptStartSeq, "generation", bus.MetadataGeneration(create.Metadata, consumerGenerationKey))
}

// createDurable creates the durable config describes on the notification stream. A var so a test
// can make a take-over's create fail after its delete.
var createDurable = func(js natsgo.JetStreamContext, config *natsgo.ConsumerConfig, opts ...natsgo.JSOpt) (*natsgo.ConsumerInfo, error) {
	return js.AddConsumer(bus.Stream, config, opts...)
}

// check stops delivering when the durable no longer delivers to this intake's deliver subject:
// something deleted it, or replaced it with one on another subject. The intake keeps its hold, and
// its next attempt takes the durable over again, or waits for a process that binds without a
// hold. A lookup that fails says nothing, and changes nothing.
func (d *heldDurable) check(ctx context.Context) {
	if d.subject == "" {
		return
	}
	info, err := d.lookupDurable(ctx)
	switch {
	case errors.Is(err, natsgo.ErrConsumerNotFound):
	case err != nil:
		return
	case info.Config.DeliverSubject == d.subject:
		return
	}
	slog.Warn("dispatch delivery: the NATS consumer no longer delivers to this intake; it stops delivering", "name", d.name)
	d.StopDelivering()
	d.subject = ""
}

// stop is the end of run: it stops delivering, then releases the hold, so a standby takes the
// durable over at once and is redelivered whatever this intake handed back. A hold whose token
// still records a take-over's start (intakeHoldToken) is lapsed instead, which frees it as
// Release does and keeps the row, so the next holder reads that start.
func (d *heldDurable) stop() {
	d.StopDelivering()
	if d.hold == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), intakeHoldRenew)
	defer cancel()
	release := d.hold.Release
	if d.recorded.start != 0 {
		release = d.hold.Lapse
	}
	if err := release(ctx); err != nil {
		slog.Error("dispatch delivery: release the intake's hold", "name", d.name, "error", err)
	}
	d.hold, d.subject = nil, ""
}

// subscribeDurable subscribes deliver to wanted's durable through client.Subscribe, acknowledging
// by hand. With no deliverSubject, nats.go looks the durable up and subscribes at the deliver
// subject the durable carries, refusing a durable another subscriber holds ("consumer is already
// bound"). With one, it subscribes at deliverSubject without the lookup, so the durable need not
// exist yet: nats.go sends the SUB and creates nothing (js.go, the SkipConsumerLookup branch of
// subscribe with Bind). It is for a caller that creates the durable with that deliver subject
// right after, under its consumer lease (takeOver).
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

// intakeDelivery is one subscription of an intake's: the client it subscribed on, the
// subscription, the handler its callback (receive) hands messages to, and the hold it delivers
// under. Each subscribe makes one of its own, so stopping one ends that subscription and nothing a
// later subscribe starts.
type intakeDelivery struct {
	nats   *bus.Client
	sub    *natsgo.Subscription
	handle natsgo.MsgHandler
	// hold is the hold the subscription delivers under: once its loss deadline has passed, every
	// message is handed back.
	hold *duty.Hold
	// handling is held while a message is in its handler, so stop waits for it.
	handling sync.Mutex
	stopping atomic.Bool
	stopped  sync.Once
}

// subscribe subscribes the intake's handling to its durable's push deliveries
// (subscribeDurable): at the deliver subject the durable carries when deliverSubject is empty, or
// at deliverSubject, for a caller that creates the durable with it right after. The subscription's
// callback (receive) handles each message on the subscription's delivery goroutine, one at a time,
// under the hold this intake holds. StopDelivering stops the subscription this made last, so a
// caller subscribing again stops the one before first.
func (d *heldDurable) subscribe(deliverSubject string) (*natsgo.Subscription, error) {
	delivery := &intakeDelivery{nats: d.nats, handle: d.handle, hold: d.hold}
	sub, err := subscribeDurable(d.nats, d.config(), deliverSubject, delivery.receive)
	if err != nil {
		return nil, err
	}
	delivery.sub = sub
	d.mu.Lock()
	d.delivering = delivery
	d.mu.Unlock()
	return sub, nil
}

// StopDelivering stops this intake's subscription so that another process can take the durable
// over with nothing waiting out the ack wait: it drains the subscription, so the server delivers
// it nothing more; negatively acknowledges every message the callback is handed from then on,
// those NATS had already delivered to the client's buffer included, so the server redelivers them
// at once; lets the message in its handler finish and be acknowledged; and returns once the drain
// has ended -- at the latest intakeDrainWait after that handler returned, when it unsubscribes
// instead -- having given up the subscription's registration on the client (bus.Client.Release),
// so that no reconnect binds it again and a later subscribe on the same client starts afresh. Run
// calls it when ctx ends and when the hold is lost. It is safe from any goroutine and idempotent: a
// second call returns once the first has stopped, and a call with nothing subscribed returns at
// once.
func (d *heldDurable) StopDelivering() {
	d.mu.Lock()
	delivery := d.delivering
	d.mu.Unlock()
	if delivery != nil {
		delivery.stop()
	}
}

// stop is StopDelivering for this subscription. The drain goes first, so the server has the
// subscription's interest gone before any negative acknowledgement reaches it, and redelivers
// what this one gives back to the next subscriber rather than straight back here.
func (d *intakeDelivery) stop() {
	d.stopped.Do(func() {
		d.stopping.Store(true)
		drained := true
		if err := d.sub.Drain(); err != nil {
			slog.Error("dispatch delivery: drain the intake's subscription", "error", err)
			drained = false
		}
		// The message in its handler, if any, finishes and is acknowledged; every one after it
		// finds stopping set.
		d.handling.Lock()
		d.handling.Unlock()
		// nats.go ends a drain on a goroutine of its own, once the callback has run every message
		// the client had buffered, and reports it only by the subscription turning invalid.
		deadline := time.Now().Add(intakeDrainWait)
		for drained && d.sub.IsValid() {
			if !time.Now().Before(deadline) {
				slog.Error("dispatch delivery: the intake's subscription did not finish draining; unsubscribing it", "waited", intakeDrainWait)
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
	})
}

// intakeDrainWait bounds how long stop waits for the drain to end once the message in its handler
// has finished. By then all that is left is nats.go's: the callback negatively acknowledging what
// the client had buffered, which never blocks, and the drain's flush to the server. So the bound is
// the one nats.go itself gives a connection's drain, natsgo.DefaultDrainTimeout -- bus connections
// never set their own -- rather than intakeMessageTimeout, which bounds a handler, and the handler
// is done.
const intakeDrainWait = natsgo.DefaultDrainTimeout

// receive is the subscription's callback. A message once stopping is set, or once the hold's loss
// deadline has passed, is negatively acknowledged: this intake may no longer act on the durable's
// messages, and the server redelivers them to the next holder. Any other goes to the handler.
func (d *intakeDelivery) receive(msg *natsgo.Msg) {
	d.handling.Lock()
	defer d.handling.Unlock()
	if d.stopping.Load() || (d.hold != nil && !time.Now().Before(d.hold.Deadline())) {
		negativelyAcknowledge(msg)
		return
	}
	d.handle(msg)
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

// deliver unwraps one NATS message into its envelope payload, routes it by the settings in force
// (handle) under intakeMessageTimeout, and always acks: a malformed envelope has nothing to retry,
// and a route error (a failed GitHub call, or that deadline passing) is caught up by the next
// reconcile pass rather than by NATS redelivery -- see the package doc comment. The deadline is
// what makes intakeAckWait a bound rather than a hope: without it one stuck GitHub call could hold
// an in-flight slot past the ack wait and have the message redelivered underneath it. An envelope
// route does not handle (an empty payload, or a kind route sends to no handler) is acknowledged
// and nothing else: no log line and no freshness write. One it handles records the event's time,
// whatever its handler decided.
func (in *Intake) deliver(msg *natsgo.Msg) {
	defer acknowledge(msg)
	ctx, cancel := context.WithTimeout(context.Background(), intakeMessageTimeout)
	defer cancel()

	var envelope contracts.Envelope
	if err := json.Unmarshal(msg.Data, &envelope); err != nil {
		slog.Error("dispatch delivery: decode envelope", "subject", msg.Subject, "error", err)
		return
	}
	handled, err := in.handle(ctx, envelope.Payload)
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
	if err := RecordEventAt(ctx, in.pool); err != nil {
		slog.Error("dispatch delivery: record event freshness", "error", err)
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
