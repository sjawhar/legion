package intake

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go/jetstream"
	"golang.org/x/sync/errgroup"

	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/record"
)

const notificationStream = "ENVOY_NOTIFICATIONS"

const (
	defaultAckWait  = 30 * time.Second
	defaultNakDelay = 30 * time.Second
	// defaultHandlerLimit is the floor the derived limit never goes below (HandlerLimitFor). The
	// derivation gives one window to each bounded read and one more to the rest, four in all, so
	// every window at or below 2m30s yields these ten minutes. No caller outside this package
	// needs the number: HandlerLimitFor's second answer says whether the window lifted the limit
	// above it.
	defaultHandlerLimit = 10 * time.Minute
)

// boundedReadsPerFact is the most bounded reads one fact's handling makes, each of them bounded
// at AckWait. A review makes three — whether its pull request is recorded (recordedPullRequest),
// its truncated body (resolveReviewBody) and its author's repository permission
// (resolveReviewPermission) — and handleMessage makes them one after another, so they add up
// rather than overlap. Every other fact makes one read or none.
const boundedReadsPerFact = 3

// HandlerLimitCeiling is the derived limit above which a wedged handler's message no longer
// returns to the stream within one operator's look, which is what the limit is for. Nothing
// refuses a configuration whose window derives a limit above it; the daemon's boot line and
// `legion start --check-config` say so instead (daemon.IntakeHandlerLimitLine), which is why it
// is exported.
const HandlerLimitCeiling = time.Hour

// HandlerLimitFor is the limit a caller that sets no HandlerLimit gets for a given AckWait:
// (boundedReadsPerFact + 1) * ackWait, floored at defaultHandlerLimit — derived true only when
// the window lifted the limit above that floor. The first term, boundedReadsPerFact * ackWait, is
// the bounded reads one fact may make, each of which handleMessage bounds at ackWait; the one
// window more is for what nothing bounds, ApplyFact's wait on the one advisory lock plus the
// transaction itself. So the bounded reads never take more than three quarters of the limit.
//
// That one window of headroom is a judgement, and what it is a judgement about is the lock wait:
// ApplyFact's wait on pg_advisory_xact_lock(hashtext('legion.apply_fact')) cannot be derived from
// the window at all, because Run works three durable consumers at once and the daemon's own
// lifecycle writes take the same lock, so what a fact waits for is what else is applying, not how
// long a read is given. The limit therefore states a share — a quarter of it left for the
// unbounded part — instead of a duration.
//
// The floor is there so that every window at or below 2m30s keeps the ten minutes this daemon has
// always had, which leaves every existing caller and the default configuration unchanged. An
// ackWait of zero or less, which normalizedSpec has defaulted away before it asks, derives a
// bound at or below zero and so yields the floor too.
func HandlerLimitFor(ackWait time.Duration) (limit time.Duration, derived bool) {
	bound := (boundedReadsPerFact + 1) * ackWait
	return max(bound, defaultHandlerLimit), bound > defaultHandlerLimit
}

// ConsumerSpec is the caller-supplied durable-consumer configuration. The daemon owns its project
// and the repositories GitHub intake hears at boot (SetRepositories changes them at runtime);
// intake owns only the fixed stream and consumer identities.
type ConsumerSpec struct {
	Project      string
	Repositories []ghrepo.Repository
	// Renames is the deployment's repository equivalence (`repository_renames`,
	// config.Config.RepositoryRenames): a pull request recorded under the name its repository
	// moved from is the one a fact naming the repository it moved to names (recordedPullRequest,
	// ghrepo.Under). The zero value, which answers as a comparison
	// without case does, while the deployment configures none.
	Renames  ghrepo.Renames
	AckWait  time.Duration
	NakDelay time.Duration
	// HandlerLimit is how long one message's handling may run before it is cut. A caller that sets
	// none gets HandlerLimitFor(AckWait): the bounded reads one fact may make
	// (boundedReadsPerFact), each of AckWait — a review is the one fact that makes all three,
	// whether its pull request is recorded (recordedPullRequest), its truncated body
	// (resolveReviewBody) and its author's permission (resolveReviewPermission), while every other
	// fact makes one read or none — plus one window more for the part nothing bounds, ApplyFact's
	// advisory-lock wait and its transaction, and never below defaultHandlerLimit. Production sets
	// none, so the daemon's worker_rpc_timeout_seconds, which it passes as AckWait, decides it;
	// tests set a short one. There is no legion.yaml key for it. What the cut relies on is the
	// handler honouring its context: consumeMessage cancels the handling's context and then waits
	// for the handler to return, so a wedged handler's message goes back to the stream once the
	// step that is running honours that cancelled context, which every step today — a pgx call or
	// a bounded HTTP read — does.
	HandlerLimit time.Duration
	Logger       *slog.Logger
	// Now is the clock Consumers reads when it records a consumer opened or a message handled,
	// and when Lag measures a stall and the oldest waiting message's age; nil is time.Now.
	Now func() time.Time
	// ReviewPermission answers whether review's author has write access or higher to its
	// repository, which is what lets a review decide a review round
	// (PullRequestReview.AuthorCanWrite). The workflow decides inside a transaction and performs no
	// I/O, so the answer is read here, before the fact is applied, only for a review that decides
	// (PullRequestReview.Decides) and has an author, and within AckWait, which bounds this one read
	// and not the delivery: intake reports the message in progress for as long as the handler runs
	// (keepInProgress), so the delivery is held and only HandlerLimit ends it, and the bound here
	// is what keeps one slow GitHub answer from holding the fact. An author GitHub gives no write
	// access is false; a lookup that fails is an error, and the message is retried, after NakDelay
	// or the longer wait a RetryLater names, rather than applied with a permission nobody read. No
	// resolver (a test, or a daemon without one) leaves every review's AuthorCanWrite false, so
	// only the review App's own reviews decide.
	ReviewPermission func(ctx context.Context, review PullRequestReview) (bool, error)
	// ReviewBody restores a review-App review's full body from GitHub when Envoy's normalizer
	// capped it (PullRequestReview.BodyTruncated), since the capped body can cut the trailing
	// Legion footer the workflow reads to tell the reviewer's review from any other review-App
	// session's (PullRequestReview.LegionSession). It is read here, before the fact is applied,
	// within AckWait, the same bound on the one read rather than on the delivery (ReviewPermission),
	// and only for a review the review App submitted, with an id and a truncated body
	// (resolveReviewBody); a failed read is returned, and the message is retried the same way a
	// failed ReviewPermission read is. No resolver (a test, or a daemon without one) leaves every
	// truncated review's body capped.
	ReviewBody func(ctx context.Context, review PullRequestReview) (string, error)
	// RepositoryMapping reads the project's repository mapping again, for Dispatch's
	// settings.repo_project.updated on the project (RepoMappingChanged): it runs before the event
	// is acknowledged, within AckWait, which bounds this read alone (ReviewPermission), and in no
	// transaction, and a failed read is retried after NakDelay. No reader (a test) acknowledges the
	// event.
	RepositoryMapping func(ctx context.Context) error
	// PrepareRepository readies what the workflow reads in memory of the repository a Dispatch
	// issue event resolves to (DispatchIssue.ResolvedRepo), before the fact is applied, within
	// AckWait, which bounds this one preparation rather than the delivery (ReviewPermission), and
	// returns the fact to apply: the workflow performs no I/O, and takes the repository
	// and its default branch when it first records an issue, so a retained event's missing
	// repository (ResolvedRepoMissing) is filled here. A failed preparation is retried after
	// NakDelay rather than applied without it. No preparer (a test) applies the fact as decoded.
	PrepareRepository func(ctx context.Context, issue DispatchIssue) (DispatchIssue, error)
	// PrepareArtifact readies what the workflow reads of the issue a Dispatch review event is on
	// (DispatchArtifact.Issue), before the fact is applied, within AckWait, which bounds this one
	// preparation rather than the delivery (ReviewPermission), and returns the fact to
	// apply: the workflow performs no I/O, and decides from Dispatch's view of the issue whether a
	// person's approval of a parked labelled root's primary document queues that root, so the view
	// is read here, for a person's approval that is not a carried one. A failed preparation is
	// retried after NakDelay rather than applied without it. No preparer (a test) applies the fact
	// as decoded, with no view, which queues nothing.
	PrepareArtifact func(ctx context.Context, artifact DispatchArtifact) (DispatchArtifact, error)
}

// RetryLater is an error ConsumerSpec.ReviewPermission returns when its read must not be made again
// sooner than After, which GitHub names when it answers with its rate limit: the delivery is nacked
// with a delay of After, or of NakDelay when that is longer.
type RetryLater struct {
	After time.Duration
	Err   error
}

func (r *RetryLater) Error() string { return r.Err.Error() }
func (r *RetryLater) Unwrap() error { return r.Err }

// Consumers are this project's three durable JetStream consumers, created before intake runs so a
// daemon whose stream is missing refuses to boot instead of booting with no intake. repos is the
// set GitHub intake hears and decodes, which SetRepositories replaces under mu. pool is the
// Postgres every fact is applied on and every consumer's position is kept in (consumer_positions).
// progress is, per durable consumer, what JetStream does not keep: when this daemon opened it and
// when it last handled a message (Lag).
type Consumers struct {
	spec     ConsumerSpec
	pool     *pgxpool.Pool
	stream   jetstream.Stream
	dispatch jetstream.Consumer
	github   jetstream.Consumer
	// wake is the durable on the Envoy listener's wake reports (legionWakeSubject): a message
	// published to a role topic of a released pod, a message sent to the agent subject of a
	// released session, and the listener's own `listener_ready`. It is a durable of its own
	// because every daemon consumes the subject: the listener does
	// not know which daemon owns a session, so each reads every report and acts only on a
	// released claim of its own.
	wake jetstream.Consumer

	progress struct{ dispatch, github, wake *consumerProgress }

	mu    sync.Mutex
	repos []ghrepo.Repository
}

// OpenConsumers creates or updates this project's Dispatch, GitHub and listener-wake durable
// consumers. A consumer created here for the first time starts at the next message, so a first
// daemon on a stream that already holds history (production keeps 72 hours of it) does not replay
// it into admission. Boot opens the consumers before it reads its Dispatch listing, so the listing
// covers the state before them and the consumers everything published since, including what lands
// while the listing is read. A consumer that exists keeps its position and the policy that created
// it, and takes the rest of this boot's configuration. A consumer that applied messages before and
// is now missing starts at the last one it applied (openConsumer). pool is required: it is where
// facts are applied and positions kept.
func OpenConsumers(ctx context.Context, js jetstream.JetStream, pool *pgxpool.Pool, spec ConsumerSpec) (*Consumers, error) {
	if pool == nil {
		return nil, errors.New("intake consumers need the Postgres pool facts are applied on")
	}
	spec, err := normalizedSpec(spec)
	if err != nil {
		return nil, err
	}
	stream, err := js.Stream(ctx, notificationStream)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", notificationStream, err)
	}

	dispatch, err := openConsumer(ctx, stream, pool, spec.Logger, jetstream.ConsumerConfig{
		Durable:        dispatchConsumerName(spec.Project),
		FilterSubjects: []string{"notifications.dispatch.issue.>", mappingSubject(spec.Project)},
		AckPolicy:      jetstream.AckExplicitPolicy,
		AckWait:        spec.AckWait,
	})
	if err != nil {
		return nil, fmt.Errorf("open the Dispatch durable consumer: %w", err)
	}
	github, err := openConsumer(ctx, stream, pool, spec.Logger, githubConsumerConfig(spec, spec.Repositories))
	if err != nil {
		return nil, fmt.Errorf("open the GitHub durable consumer: %w", err)
	}
	wake, err := openConsumer(ctx, stream, pool, spec.Logger, jetstream.ConsumerConfig{
		Durable:       wakeConsumerName(spec.Project),
		FilterSubject: legionWakeSubject,
		AckPolicy:     jetstream.AckExplicitPolicy,
		AckWait:       spec.AckWait,
	})
	if err != nil {
		return nil, fmt.Errorf("open the listener wake durable consumer: %w", err)
	}
	consumers := &Consumers{spec: spec, pool: pool, stream: stream, dispatch: dispatch, github: github, wake: wake,
		repos: slices.Clone(spec.Repositories)}
	opened := spec.Now()
	consumers.progress.dispatch = &consumerProgress{name: dispatchConsumerName(spec.Project), opened: opened}
	consumers.progress.github = &consumerProgress{name: githubConsumerName(spec.Project), opened: opened}
	consumers.progress.wake = &consumerProgress{name: wakeConsumerName(spec.Project), opened: opened}
	return consumers, nil
}

func githubConsumerConfig(spec ConsumerSpec, repositories []ghrepo.Repository) jetstream.ConsumerConfig {
	return jetstream.ConsumerConfig{
		Durable:        githubConsumerName(spec.Project),
		FilterSubjects: githubFilters(repositories),
		AckPolicy:      jetstream.AckExplicitPolicy,
		AckWait:        spec.AckWait,
	}
}

// SetRepositories replaces the repositories GitHub intake hears: the GitHub consumer's filters,
// then, once JetStream has taken them, the set the decoder accepts an event's repository from. Both
// change under one lock every decode reads the set under, so no event is decoded against the one
// while the consumer filters on the other: an event of a repository added here is decoded from the
// moment the consumer delivers it, and one of a repository taken out is acknowledged untouched. An
// update JetStream refuses changes neither, so the caller can retry it. No repository is a daemon
// that serves none yet (githubHearsNoRepository); a zero one is refused.
func (c *Consumers) SetRepositories(ctx context.Context, repositories []ghrepo.Repository) error {
	if slices.ContainsFunc(repositories, ghrepo.Repository.IsZero) {
		return fmt.Errorf("GitHub intake cannot hear a zero repository")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	github, err := openConsumer(ctx, c.stream, c.pool, c.spec.Logger, githubConsumerConfig(c.spec, repositories))
	if err != nil {
		return fmt.Errorf("update the GitHub durable consumer's repositories: %w", err)
	}
	c.repos, c.github = slices.Clone(repositories), github
	return nil
}

// repositories is the set GitHub intake hears now.
func (c *Consumers) repositories() []ghrepo.Repository {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.repos
}

// openConsumer updates the durable consumer config names, keeping the start position it was created
// with (JetStream refuses to change it), or creates it. A consumer created where pool keeps a
// position for it (recordStreamPosition) starts at that position's stream time, so the messages
// published while it was missing are delivered; the one at that time, which it applied, is
// delivered again and dropped as a duplicate (processed_events). The start is logged; one older
// than the stream's first message (the stream keeps 72 hours) is warned of too. The position moves
// only when a fact is applied, never for a message acknowledged without a transaction, so on a
// quiet project an old position is no sign of loss: the messages between it and the stream's first
// may have been handled by the consumer before it went missing, or may have been missed. The
// daemon applied none of them, and the pass heals those it would have acted on. A consumer with no
// position starts at the next message.
func openConsumer(ctx context.Context, stream jetstream.Stream, pool *pgxpool.Pool, logger *slog.Logger, config jetstream.ConsumerConfig) (jetstream.Consumer, error) {
	existing, err := stream.Consumer(ctx, config.Durable)
	if errors.Is(err, jetstream.ErrConsumerNotFound) {
		return createConsumer(ctx, stream, pool, logger, config)
	}
	if err != nil {
		return nil, err
	}
	created := existing.CachedInfo().Config
	config.DeliverPolicy, config.OptStartSeq, config.OptStartTime = created.DeliverPolicy, created.OptStartSeq, created.OptStartTime
	return stream.UpdateConsumer(ctx, config)
}

// createConsumer creates the missing durable consumer config names, at its stored position when
// pool keeps one and at the next message otherwise (openConsumer).
func createConsumer(ctx context.Context, stream jetstream.Stream, pool *pgxpool.Pool, logger *slog.Logger, config jetstream.ConsumerConfig) (jetstream.Consumer, error) {
	start, stored, err := storedStreamTime(ctx, pool, config.Durable)
	if err != nil {
		return nil, err
	}
	if !stored {
		config.DeliverPolicy = jetstream.DeliverNewPolicy
		return stream.CreateConsumer(ctx, config)
	}
	info, err := stream.Info(ctx)
	if err != nil {
		return nil, fmt.Errorf("read %s info: %w", notificationStream, err)
	}
	config.DeliverPolicy, config.OptStartTime = jetstream.DeliverByStartTimePolicy, &start
	consumer, err := stream.CreateConsumer(ctx, config)
	if err != nil {
		return nil, err
	}
	logger.Info("intake: a missing durable consumer is created at the last message it applied",
		"consumer", config.Durable, "start", start.UTC().Format(time.RFC3339Nano))
	// The stored time keeps microseconds; the stream's, nanoseconds. A start at the stream's first
	// message itself is not before it.
	if first := info.State.FirstTime; start.Before(first.Truncate(time.Microsecond)) {
		logger.Warn("intake: a missing durable consumer's last applied message is older than the stream's first message; events published between may have been missed: the daemon applied none of them, and the pass heals those it would have acted on",
			"consumer", config.Durable, "start", start.UTC().Format(time.RFC3339Nano), "stream_first", first.UTC().Format(time.RFC3339Nano))
	}
	return consumer, nil
}

// DispatchTarget is the notification stream's own current last sequence, read fresh from
// JetStream. Boot reads it after Reconcile's own Dispatch listing (not before: a message
// published between an earlier read and the listing would be in the listing but not counted),
// so any record the listing shows behind Dispatch's log is caught up only once the Dispatch
// consumer's ack floor reaches this position — a stream position, not a per-issue or per-message
// count, so it needs no correction for redelivery, a nak, or messages the consumer's filter never
// matches.
func (c *Consumers) DispatchTarget(ctx context.Context) (int64, error) {
	info, err := c.stream.Info(ctx)
	if err != nil {
		return 0, fmt.Errorf("read notification stream info: %w", err)
	}
	return int64(info.State.LastSeq), nil
}

// DispatchPosition is the Dispatch consumer's own current position, read fresh from JetStream:
// the measurement Reconcile holds against DispatchTarget (DispatchConsumerPosition.Reached). It
// reads through a handle of its own (stream.Consumer: one CONSUMER.INFO request, the handle's
// cached info that answer) rather than c.dispatch.Info: nats.go's Info writes the handle's cached
// info under no lock and Run's Consume reads it as it starts, so a position read while Run starts
// raced on the shared handle (the race detector's find in the daemon's retained-event test), and a
// fresh handle shares nothing with the one Run consumes. Nothing here may wait for Run instead:
// the boot's reconcile reads the position before Run starts, and pollHoldRelease beside it.
func (c *Consumers) DispatchPosition(ctx context.Context) (DispatchConsumerPosition, error) {
	consumer, err := c.stream.Consumer(ctx, dispatchConsumerName(c.spec.Project))
	if err != nil {
		return DispatchConsumerPosition{}, fmt.Errorf("read Dispatch consumer info: %w", err)
	}
	return positionOf(consumer.CachedInfo()), nil
}

func positionOf(info *jetstream.ConsumerInfo) DispatchConsumerPosition {
	return DispatchConsumerPosition{
		AckFloorStream: int64(info.AckFloor.Stream),
		Idle:           info.NumPending == 0 && info.NumAckPending == 0,
	}
}

const (
	// consumerLagBound bounds one consumer's Lag read: the state route waits on it, so a NATS server
	// that does not answer costs a state read two seconds and an unavailable line, not the
	// request's life.
	consumerLagBound = 2 * time.Second
	// consumerOldestBound bounds the read of one consumer's oldest waiting message, all its
	// STREAM.MSG.GET together, from when the consumer's info answered. A NATS server that refuses a
	// publish answers only with an asynchronous -ERR, so a get the daemon's user lacks the grant
	// for never answers and waits this bound out; it ends within consumerLagBound, so the waiting
	// count and stalled flag the info gave are still the consumer's answer.
	consumerOldestBound = time.Second
	// consumerStallAfter is how long a consumer with messages waiting may go without handling one
	// before Lag calls it stalled: a healthy consumer handles a message in well under a second, and
	// a stalled one holds every event behind the message it is stuck on.
	consumerStallAfter = 60 * time.Second
)

// ConsumerLag is one durable consumer's lag as Lag reads it.
//
// Waiting is the messages the consumer has not handled, JetStream's NumPending (not yet delivered)
// plus NumAckPending (delivered and not yet acknowledged); it counts the messages the daemon will
// acknowledge without a transaction at decode too. OldestAt is the stream time of the oldest of
// them, the first message after the consumer's ack floor on one of its filter subjects, and
// OldestAge how long before the read it was stored; both are zero when nothing waits, and when that
// message could not be read. LastHandledAt is when this daemon last handled a message of the
// consumer — acknowledged it, or terminated it as poison, with JetStream taking either — and is zero
// before the first; a nak is not handled. Stalled is true when messages wait and none was handled
// for consumerStallAfter, counted from the consumer's open while none has been.
//
// Unavailable names what NATS did not answer or refused: `read the consumer's info: <error>` when
// CONSUMER.INFO failed, and then Waiting is 0 and Stalled false, since neither is known; `read the
// oldest waiting message: <error>` when only the stored-message read (STREAM.MSG.GET) failed, and
// then Waiting and Stalled stand and OldestAt is zero. Empty when every read answered.
type ConsumerLag struct {
	Name          string
	Waiting       uint64
	OldestAt      time.Time
	OldestAge     time.Duration
	LastHandledAt time.Time
	Stalled       bool
	Unavailable   string
}

// consumerProgress is what Lag needs of one durable consumer that JetStream does not keep: when
// this daemon opened it (OpenConsumers) and when it last handled a message of it (Run). Run's
// consumer loop writes handled while a state read reads it, so it is kept under mu.
type consumerProgress struct {
	name   string
	opened time.Time

	mu      sync.Mutex
	handled time.Time
}

func (p *consumerProgress) handle(at time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.handled = at
}

func (p *consumerProgress) lastHandled() time.Time {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.handled
}

// Lag is the lag of the Dispatch, GitHub and listener-wake durable consumers, in
// that order (ConsumerLag), read fresh from JetStream: per consumer one CONSUMER.INFO and, while
// messages wait, one STREAM.MSG.GET per filter subject for the first message at or after the
// sequence above its ack floor on that subject, the smallest sequence's time being the oldest; a
// message at that sequence ends the reads, since no filter can give an older one. The consumers
// the same time, each within consumerLagBound of its own, and each consumer's gets within
// consumerOldestBound of their own, so a read that does not answer costs only its own consumer
// its answer: never another consumer's info, nor its own consumer's waiting count.
// Each consumer's info is read through a handle of its own, as DispatchPosition reads, so nothing
// races Run's. A read that fails or runs out of its bound is that consumer's Unavailable; Lag
// itself never fails, so the state that shows it is still served.
func (c *Consumers) Lag(ctx context.Context) []ConsumerLag {
	progress := []*consumerProgress{c.progress.dispatch, c.progress.github, c.progress.wake}
	lags := make([]ConsumerLag, len(progress))
	var reads sync.WaitGroup
	for i, consumer := range progress {
		reads.Go(func() { lags[i] = c.lagOf(ctx, consumer) })
	}
	reads.Wait()
	return lags
}

// lagOf reads one consumer's lag (Lag) within consumerLagBound, its oldest waiting message within
// consumerOldestBound of the info's answer.
func (c *Consumers) lagOf(ctx context.Context, progress *consumerProgress) ConsumerLag {
	ctx, cancel := context.WithTimeout(ctx, consumerLagBound)
	defer cancel()
	handled := progress.lastHandled()
	lag := ConsumerLag{Name: progress.name, LastHandledAt: handled}
	consumer, err := c.stream.Consumer(ctx, progress.name)
	if err != nil {
		lag.Unavailable = "read the consumer's info: " + err.Error()
		return lag
	}
	info := consumer.CachedInfo()
	lag.Waiting = info.NumPending + uint64(info.NumAckPending)
	if lag.Waiting == 0 {
		return lag
	}
	now := c.spec.Now()
	since := handled
	if since.IsZero() {
		since = progress.opened
	}
	lag.Stalled = now.Sub(since) >= consumerStallAfter
	read, cancelRead := context.WithTimeout(ctx, consumerOldestBound)
	defer cancelRead()
	oldest, err := c.oldestWaiting(read, info)
	if err != nil {
		lag.Unavailable = "read the oldest waiting message: " + err.Error()
		return lag
	}
	if !oldest.IsZero() {
		lag.OldestAt, lag.OldestAge = oldest, now.Sub(oldest)
	}
	return lag
}

// oldestWaiting is the stream time of the first message after info's ack floor on any of its
// consumer's filter subjects, or zero when the stream holds none (the stream's MaxAge removed it
// since the info was read). A message at the sequence right after the ack floor is the oldest any
// filter can give, so the filters after it are not read.
func (c *Consumers) oldestWaiting(ctx context.Context, info *jetstream.ConsumerInfo) (time.Time, error) {
	filters := slices.Clone(info.Config.FilterSubjects)
	if info.Config.FilterSubject != "" {
		filters = append(filters, info.Config.FilterSubject)
	}
	var oldest *jetstream.RawStreamMsg
	for _, filter := range filters {
		message, err := c.stream.GetMsg(ctx, info.AckFloor.Stream+1, jetstream.WithGetMsgSubject(filter))
		if errors.Is(err, jetstream.ErrMsgNotFound) {
			continue
		}
		if err != nil {
			return time.Time{}, err
		}
		if oldest == nil || message.Sequence < oldest.Sequence {
			oldest = message
		}
		if oldest.Sequence == info.AckFloor.Stream+1 {
			break
		}
	}
	if oldest == nil {
		return time.Time{}, nil
	}
	return oldest.Time, nil
}

// Run consumes the three durable consumers until ctx ends, and returns the error of the one that
// stops first. Every decoded fact enters ApplyFact on the consumers' pool; a committed transaction
// is acknowledged, a rolled-back transaction is nacked with a delay, poison is terminated, and a
// committed refusal is logged then acknowledged. Each message acknowledged or terminated is
// recorded as its consumer's last handled (Lag). The Dispatch consumer's own position, and
// admission's hold on it, are the daemon's boot-owned concern (workflowRuntime.pollHoldRelease),
// not intake's: this loop knows nothing about either.
func (c *Consumers) Run(ctx context.Context, handlers ...Handler) error {
	c.mu.Lock()
	dispatch, github := c.dispatch, c.github
	c.mu.Unlock()
	group, consumeContext := errgroup.WithContext(ctx)
	group.Go(func() error {
		return c.consumeConsumer(consumeContext, dispatch, c.progress.dispatch, handlers)
	})
	group.Go(func() error {
		return c.consumeConsumer(consumeContext, github, c.progress.github, handlers)
	})
	group.Go(func() error {
		return c.consumeConsumer(consumeContext, c.wake, c.progress.wake, handlers)
	})
	return group.Wait()
}

// consumeConsumer works one message at a time, and asks JetStream for one at a time
// (jetstream.PullMaxMessages(1)). The acknowledgement window (AckWait) starts when JetStream
// delivers a message, not when this loop reaches it, so a message the client library had already
// fetched into its buffer spends that window queued behind the messages before it: with the
// library's default batch of 500, a consumer working a backlog has JetStream redeliver the
// messages it is holding, and each redelivery is prepared again (a Dispatch read) and takes the
// fact lock again before it is found already processed. One message in flight costs one pull
// request per message and leaves the window to the message's own work. Each message handled is
// recorded in progress at spec.Now.
func (c *Consumers) consumeConsumer(ctx context.Context, consumer jetstream.Consumer, progress *consumerProgress, handlers []Handler) error {
	lag := &lagWatch{consumer: consumer.CachedInfo().Name, logger: c.spec.Logger, now: time.Now}
	consuming, err := consumer.Consume(func(message jetstream.Msg) {
		lag.observe(message)
		if consumeMessage(ctx, message, c.spec, c.repositories(), c.pool, handlers) {
			progress.handle(c.spec.Now())
		}
	}, jetstream.PullMaxMessages(1))
	if err != nil {
		return err
	}
	select {
	case <-ctx.Done():
		consuming.Stop()
		<-consuming.Closed()
		return nil
	case <-consuming.Closed():
		if ctx.Err() != nil {
			return nil
		}
		return fmt.Errorf("durable consumer %s stopped unexpectedly", consumer.CachedInfo().Name)
	}
}

const (
	// consumerLagWarn is how long after its publish a message may reach its durable consumer before
	// lagWatch says the consumer is behind: well past the seconds a healthy consumer takes, and well
	// short of the hours a backlog of no-op events once held phase moves and pull request records
	// back with nothing in the daemon's log to say so.
	consumerLagWarn = 5 * time.Minute
	// consumerLagEvery bounds lagWatch to one line a minute per consumer, however many messages a
	// backlog delivers.
	consumerLagEvery = time.Minute
)

// lagWatch tells, at most once every consumerLagEvery, that its durable consumer is handling a
// message JetStream stored more than consumerLagWarn before: how far behind the consumer is, how
// many messages still wait behind that one, and how many times this one was delivered, since a
// message retried after a failure keeps the time it was first stored. Every event a consumer has
// not reached waits for it - a pull request's opened or synchronize, a Dispatch status change - so
// the line is the operator's sign that the daemon acts on old news. Nothing else watches the GitHub
// consumer's position; the Dispatch consumer's is watched only while admission holds roots for it.
// JetStream's Consume calls its handler for one message at a time, so observe takes no lock.
type lagWatch struct {
	consumer string
	logger   *slog.Logger
	now      func() time.Time
	last     time.Time
}

func (l *lagWatch) observe(message jetstream.Msg) {
	metadata, err := message.Metadata()
	if err != nil {
		return
	}
	now := l.now()
	behind := now.Sub(metadata.Timestamp)
	if behind < consumerLagWarn || (!l.last.IsZero() && now.Sub(l.last) < consumerLagEvery) {
		return
	}
	l.last = now
	l.logger.Warn("intake: a durable consumer is behind its stream; every event it has not reached yet waits for it",
		"consumer", l.consumer, "behindSeconds", int64(behind/time.Second), "pending", metadata.NumPending, "delivered", metadata.NumDelivered,
		"subject", message.Subject(), "stream_seq", metadata.Sequence.Stream, "published_at", metadata.Timestamp.UTC().Format(time.RFC3339))
}

// consumeMessage applies one message and reports whether it was handled: acknowledged, or
// terminated as poison, with JetStream taking the acknowledgement or the termination, the two ways
// a message leaves its consumer's waiting count. A nak is not handled: the message waits again.
//
// The order the work is done in is the point. A poison message is decoded and terminated before
// anything else starts, so it pays for neither a goroutine nor a ticker. Everything after it runs
// under work, a context of spec.HandlerLimit, with keepInProgress reporting the message in
// progress every third of AckWait for as long as that work runs, so JetStream does not deliver the
// message again and have it prepared a second time. The work ends the message in no way: it
// returns a messageOutcome, stop() ends the keeper, and only then does the one acknowledgement or
// nak that outcome asks for follow.
//
// The keeper has to stop before that one call rather than in a defer after it. A progress signal
// that reached the server after a nak would stretch the retry the nak asked for: nats-server's
// processNak keeps the entry in o.pending with a timestamp that expires after the nak's delay,
// and progressUpdate resets that timestamp to now, so a one-second retry would become a whole
// AckWait (server/consumer.go). nats.go stops that tick on its own today — a nak marks the
// message acknowledged, so every later signal for it is refused and published nowhere — but that
// guard is the client library's, and what keeps the window between the work and the terminal call
// free of signals is this order.
//
// HandlerLimit is what the held delivery costs: a message reported in progress for ever is a
// message nothing redelivers, so a handler that hangs is cut at that limit, its message naked and
// delivered again rather than held for good.
func consumeMessage(ctx context.Context, message jetstream.Msg, spec ConsumerSpec, repositories []ghrepo.Repository, pool *pgxpool.Pool, handlers []Handler) bool {
	decoded, err := decodeMessage(message.Subject(), spec.Project, repositories, message.Data())
	if err != nil {
		logMessage(spec.Logger, slog.LevelError, "poison JetStream message", message, "error", err)
		if termErr := message.Term(); termErr != nil {
			logMessage(spec.Logger, slog.LevelError, "term poison JetStream message", message, "error", termErr)
			return false
		}
		return true
	}
	for _, field := range decoded.Unread {
		logMessage(spec.Logger, slog.LevelWarn, "unreadable field taken as absent", message, "event_id", decoded.EventID, "field", field)
	}

	// A message that decodes to no fact is acknowledged here rather than inside the work, for the
	// reason a poison message is terminated above it: it does nothing, and the keeper, its ticker
	// and the deadline would be the whole of its cost. Every consumer hears more of these than of
	// anything else — the Dispatch durable takes every project's issue events, and decodeMessage
	// answers with no fact for another project's key and for every event type the workflow does
	// not read.
	if decoded.Fact == nil {
		return ackMessage(spec.Logger, message)
	}

	work, cancel := context.WithTimeout(ctx, spec.HandlerLimit)
	defer cancel()
	stop := keepInProgress(work, message, spec.AckWait/3, spec.Logger)
	started := time.Now()
	outcome := handleMessage(work, message, spec, pool, decoded, handlers)
	took := time.Since(started)
	stop()

	// A fact outliving its acknowledgement window is what the progress signal makes harmless, so
	// the daemon says when one did: this is the only line that reports how long a fact took,
	// nothing else times one, and the consumer-lag warning (lagWatch) speaks only five minutes
	// late.
	outlived := !outcome.nak && took > spec.AckWait
	// A handler the limit cut is otherwise hard to see: held in progress, it shows neither as
	// repeated preparation work nor as a rising delivery count, and the nak warning nakResolve
	// writes after it names the step and `context deadline exceeded`, which a single read cut at
	// its own AckWait bound also says. This line is the one that names the cut.
	// A live parent context is what tells the limit from the daemon shutting its consumers down.
	// The outcome belongs in the test because the line claims the message was naked and will be
	// delivered again: a deadline can pass after the work committed its fact, or on a read that
	// ignores the context it was handed, and the handling still asks for the acknowledgement
	// below. Such a message was never cut, and saying it was would send an operator looking for a
	// redelivery that is not coming of a fact that is already applied.
	cut := outcome.nak && ctx.Err() == nil && errors.Is(work.Err(), context.DeadlineExceeded)
	if outlived || cut {
		// Metadata is parsed, never read: it tokenizes the acknowledgement subject and allocates.
		// Neither line is the ordinary message's, so neither is that work.
		var consumer string
		var delivered uint64
		if metadata, metadataErr := message.Metadata(); metadataErr == nil {
			consumer, delivered = metadata.Consumer, metadata.NumDelivered
		}
		if outlived {
			logMessage(spec.Logger, slog.LevelInfo, "an intake fact outlived its acknowledgement window and was held in progress while it ran", message,
				"consumer", consumer, "event_id", decoded.EventID, "delivered", delivered, "took", took)
		}
		if cut {
			logMessage(spec.Logger, slog.LevelWarn, "an intake handler outran its limit and was cut", message,
				"consumer", consumer, "event_id", decoded.EventID, "fact", fmt.Sprintf("%T", decoded.Fact), "limit", spec.HandlerLimit)
		}
	}

	if outcome.nak {
		nakResolve(spec, message, decoded.EventID, outcome.what, outcome.nakWhat, outcome.err)
		return false
	}
	return ackMessage(spec.Logger, message)
}

// messageOutcome is what one message's handling asks of JetStream, with nothing of it taken yet:
// acknowledge the message, or nak it with what nakResolve names the failure by. handleMessage
// returns one rather than ending the message itself, so its caller can stop the progress keeper
// before the one terminal call.
type messageOutcome struct {
	nak     bool
	what    string
	nakWhat string
	err     error
}

// ackOutcome asks for the acknowledgement: the fact was applied, or there was none to apply.
func ackOutcome() messageOutcome { return messageOutcome{} }

// nakOutcome asks for the nak nakResolve makes: what failed, which it logs at warn, nakWhat, which
// it logs at error if the nak itself fails, and the failure, whose RetryLater can widen the delay.
// The event both lines name is the caller's own, which holds the decoded message.
func nakOutcome(what, nakWhat string, err error) messageOutcome {
	return messageOutcome{nak: true, what: what, nakWhat: nakWhat, err: err}
}

// handleMessage does one decoded message's work — the reads its fact needs, then the fact's
// transaction — under work, the deadline context of consumeMessage's handling, and says what
// should become of the message. It acknowledges, naks and terminates nothing. Each read keeps its
// own AckWait bound, nested under work, so one dependency that is slow to answer is cut while the
// handling as a whole runs to HandlerLimit.
func handleMessage(work context.Context, message jetstream.Msg, spec ConsumerSpec, pool *pgxpool.Pool, decoded decodedMessage, handlers []Handler) messageOutcome {
	// decoded.Fact is never nil here: consumeMessage acknowledges a message that decodes to none
	// before it starts the keeper this work runs under.
	if _, ok := decoded.Fact.(RepoMappingChanged); ok {
		if err := readMapping(work, spec); err != nil {
			return nakOutcome("read the project's repository mapping again", "nak the mapping read", err)
		}
		return ackOutcome()
	}
	recorded, err := recordedPullRequest(work, spec, pool, decoded.Fact)
	if err != nil {
		return nakOutcome("read whether the pull request is recorded", "nak the pull request read", err)
	}
	if !recorded {
		logMessage(spec.Logger, slog.LevelDebug, "a GitHub event of a pull request the daemon does not record is acknowledged without a transaction", message,
			"event_id", decoded.EventID)
		return ackOutcome()
	}
	prepared, err := prepareRepository(work, spec, decoded.Fact)
	if err != nil {
		return nakOutcome("prepare the issue's repository", "nak the repository preparation", err)
	}
	decoded.Fact = prepared
	prepared, err = prepareArtifact(work, spec, decoded.Fact)
	if err != nil {
		return nakOutcome("prepare the review's issue", "nak the review's issue preparation", err)
	}
	decoded.Fact = prepared
	withBody, err := resolveReviewBody(work, spec, decoded.Fact)
	if err != nil {
		return nakOutcome("restore the review's truncated body", "nak the review body restore", err)
	}
	fact, err := resolveReviewPermission(work, spec, withBody)
	if err != nil {
		return nakOutcome("read the reviewer's repository permission", "nak the reviewer's permission read", err)
	}
	decoded.Fact = fact

	result, err := ApplyFact(withStreamPosition(work, message), pool, decoded.Source, decoded.EventID, decoded.Fact, handlers...)
	if err != nil {
		return nakOutcome("retryable intake processing failure", "nak intake processing failure", err)
	}
	if result.Refusal != nil {
		logMessage(spec.Logger, slog.LevelWarn, "committed refusal", message,
			"event_id", decoded.EventID,
			"status", result.Refusal.Status,
			"code", result.Refusal.Code,
			"error", result.Refusal.Message,
		)
	}
	return ackOutcome()
}

// recordedPullRequest says whether the daemon records the pull request a review or a checks
// settlement names (PullRequestReview, PullRequestChecks), by repository and number as the workflow
// looks it up (record.PullRequestRecorded), and is true for every other fact. It asks under the
// name the fact carries and then, when that finds nothing, under the other name of the pair that
// names it (ghrepo.Under, ConsumerSpec.Renames), as the workflow reads the row: a pull request
// already open when its repository was transferred is recorded under the name it moved from,
// while the fact names the one it moved to. It is one read of the pool per name within the one
// AckWait, in no transaction and under no lock, made before
// the review's GitHub reads (resolveReviewBody, resolveReviewPermission): the workflow applies
// either fact only to a recorded pull request, so one of any other pull request is acknowledged
// with no transaction under the fact lock (ApplyFact) and no GitHub read, whatever the number of
// other pull requests a mapped repository reviews and checks. A pull request recorded after this
// read, by a later opened, gets its checks and reviews from their next event or from the pass's
// read.
func recordedPullRequest(ctx context.Context, spec ConsumerSpec, pool *pgxpool.Pool, fact Fact) (bool, error) {
	var repo string
	var number int
	switch fact := fact.(type) {
	case PullRequestReview:
		repo, number = fact.Repo, fact.Number
	case PullRequestChecks:
		repo, number = fact.Repo, fact.Number
	default:
		return true, nil
	}
	read, cancel := context.WithTimeout(ctx, spec.AckWait)
	defer cancel()
	return ghrepo.Under(spec.Renames, repo, func(name string) (bool, error) {
		return record.PullRequestRecorded(read, pool, name, number)
	})
}

// readMapping runs ConsumerSpec.RepositoryMapping within AckWait; no reader reads nothing.
func readMapping(ctx context.Context, spec ConsumerSpec) error {
	if spec.RepositoryMapping == nil {
		return nil
	}
	read, cancel := context.WithTimeout(ctx, spec.AckWait)
	defer cancel()
	return spec.RepositoryMapping(read)
}

// prepareRepository runs ConsumerSpec.PrepareRepository for a Dispatch issue fact, within AckWait,
// and is the fact to apply. Every other fact passes through untouched.
func prepareRepository(ctx context.Context, spec ConsumerSpec, fact Fact) (Fact, error) {
	issue, ok := fact.(DispatchIssue)
	if !ok || spec.PrepareRepository == nil {
		return fact, nil
	}
	read, cancel := context.WithTimeout(ctx, spec.AckWait)
	defer cancel()
	return spec.PrepareRepository(read, issue)
}

// prepareArtifact runs ConsumerSpec.PrepareArtifact for a Dispatch review or version fact, within
// AckWait, and is the fact to apply. Every other fact passes through untouched.
func prepareArtifact(ctx context.Context, spec ConsumerSpec, fact Fact) (Fact, error) {
	artifact, ok := fact.(DispatchArtifact)
	if !ok || spec.PrepareArtifact == nil {
		return fact, nil
	}
	read, cancel := context.WithTimeout(ctx, spec.AckWait)
	defer cancel()
	return spec.PrepareArtifact(read, artifact)
}

// resolveReviewPermission fills a review's AuthorCanWrite from ConsumerSpec.ReviewPermission
// (ResolveAuthorCanWrite), before the fact enters its transaction, within AckWait. Every other fact
// passes through untouched.
func resolveReviewPermission(ctx context.Context, spec ConsumerSpec, fact Fact) (Fact, error) {
	review, ok := fact.(PullRequestReview)
	if !ok {
		return fact, nil
	}
	read, cancel := context.WithTimeout(ctx, spec.AckWait)
	defer cancel()
	review, err := ResolveAuthorCanWrite(read, review, spec.ReviewPermission)
	if err != nil {
		return nil, err
	}
	return review, nil
}

// ResolveAuthorCanWrite fills review's AuthorCanWrite from permission, the daemon's reader of the
// author's repository permission, for a webhook's review (resolveReviewPermission) and the daemon's
// own read of a pull request's reviews alike, so the workflow's DecidesRound judges both on the same
// answer. Only a review that decides (Decides) and names its author is looked up: a comment decides
// nothing whoever writes it, so it costs no GitHub call. No reader (a test, or a daemon without one)
// leaves AuthorCanWrite false. A failed read is returned with the review unchanged.
func ResolveAuthorCanWrite(ctx context.Context, review PullRequestReview, permission func(context.Context, PullRequestReview) (bool, error)) (PullRequestReview, error) {
	if permission == nil || review.Author == "" || !review.Decides() {
		return review, nil
	}
	canWrite, err := permission(ctx, review)
	if err != nil {
		return review, err
	}
	review.AuthorCanWrite = canWrite
	return review, nil
}

// resolveReviewBody restores a truncated review-App review's body from GitHub
// (ConsumerSpec.ReviewBody), before the fact enters its transaction and before
// resolveReviewPermission reads its author's permission, since a restored body carries the Legion
// footer the workflow needs. Only a review the normalizer capped (BodyTruncated), with an id to
// look it up by and an author, is read: one with no id (a listener that predates it) keeps its
// capped body and BodyTruncated true, so the workflow sets it aside as footer-less rather than
// guessing at a footer it cannot restore. Every other fact, and a review the normalizer did not
// cap, passes through untouched.
func resolveReviewBody(ctx context.Context, spec ConsumerSpec, fact Fact) (Fact, error) {
	review, ok := fact.(PullRequestReview)
	if !ok || spec.ReviewBody == nil || !review.BodyTruncated || review.ID == 0 || review.Author == "" {
		return fact, nil
	}
	read, cancel := context.WithTimeout(ctx, spec.AckWait)
	defer cancel()
	body, err := spec.ReviewBody(read, review)
	if err != nil {
		return nil, err
	}
	review.Body = body
	review.BodyTruncated = false
	return review, nil
}

// nakResolve widens message's nak delay to a RetryLater's wait when it names one longer than
// spec.NakDelay, logs what failed and the delay at warn, then naks the message with that delay,
// logging nakWhat at error if the nak itself fails.
func nakResolve(spec ConsumerSpec, message jetstream.Msg, eventID, what, nakWhat string, err error) {
	delay := spec.NakDelay
	var later *RetryLater
	if errors.As(err, &later) && later.After > delay {
		delay = later.After
	}
	logMessage(spec.Logger, slog.LevelWarn, what, message, "event_id", eventID, "retry_in", delay, "error", err)
	if nakErr := message.NakWithDelay(delay); nakErr != nil {
		logMessage(spec.Logger, slog.LevelError, nakWhat, message, "event_id", eventID, "error", nakErr)
	}
}

// ackMessage acknowledges message and reports whether the client sent the acknowledgement, logging
// the failure when it did not.
func ackMessage(logger *slog.Logger, message jetstream.Msg) bool {
	if err := message.Ack(); err != nil {
		logMessage(logger, slog.LevelError, "ack intake message", message, "error", err)
		return false
	}
	return true
}

func logMessage(logger *slog.Logger, level slog.Level, message string, delivery jetstream.Msg, attributes ...any) {
	metadata, err := delivery.Metadata()
	if err == nil {
		attributes = append(attributes,
			"subject", delivery.Subject(),
			"stream_seq", metadata.Sequence.Stream,
			"delivery_seq", metadata.Sequence.Consumer,
		)
	} else {
		attributes = append(attributes, "subject", delivery.Subject())
	}
	logger.Log(context.Background(), level, message, attributes...)
}

func normalizedSpec(spec ConsumerSpec) (ConsumerSpec, error) {
	if strings.TrimSpace(spec.Project) == "" {
		return ConsumerSpec{}, fmt.Errorf("intake consumer project is required")
	}
	if slices.ContainsFunc(spec.Repositories, ghrepo.Repository.IsZero) {
		return ConsumerSpec{}, fmt.Errorf("intake consumer repository is required")
	}
	if spec.AckWait <= 0 {
		spec.AckWait = defaultAckWait
	}
	if spec.NakDelay <= 0 {
		spec.NakDelay = defaultNakDelay
	}
	if spec.HandlerLimit <= 0 {
		spec.HandlerLimit, _ = HandlerLimitFor(spec.AckWait)
	}
	if spec.Logger == nil {
		spec.Logger = slog.Default()
	}
	if spec.Now == nil {
		spec.Now = time.Now
	}
	return spec, nil
}

// githubHearsNoRepository is the GitHub consumer's one filter while GitHub intake hears no
// repository: a subject Envoy publishes no repository's event on, since a GitHub owner's login
// never begins with a hyphen. A consumer with no filter would hear every subject of the stream.
const githubHearsNoRepository = "notifications.github.-.-"

func githubFilters(repositories []ghrepo.Repository) []string {
	if len(repositories) == 0 {
		return []string{githubHearsNoRepository}
	}
	filters := make([]string, 0, len(repositories))
	for _, repo := range repositories {
		filters = append(filters, githubRepositoryPrefix(repo)+".>")
	}
	return filters
}

// githubRepositoryPrefix is what every GitHub subject Envoy publishes for repo begins with: the
// owner and the name each one segment, every dot written `_`, since a repository name may hold a
// dot and a subject splits on dots. It is this module's copy of the contracts' githubRepositoryPrefix
// (packages/contracts/src/subject.ts), which the Go coordinator cannot import from Envoy; the golden
// tests hold the two to Envoy's published subjects. The spelling is lossy, `a.b` and `a_b` alike, so
// the payload's repository, not the subject, says whose event it is.
func githubRepositoryPrefix(repo ghrepo.Repository) string {
	return "notifications.github." + strings.ReplaceAll(repo.Owner(), ".", "_") + "." + strings.ReplaceAll(repo.Name(), ".", "_")
}

func dispatchConsumerName(project string) string {
	return "legion-go-" + project + "-dispatch"
}

func githubConsumerName(project string) string {
	return "legion-go-" + project + "-github"
}

func wakeConsumerName(project string) string {
	return "legion-go-" + project + "-wake"
}
