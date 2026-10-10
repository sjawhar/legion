package intake

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go/jetstream"
	"golang.org/x/sync/errgroup"

	"github.com/sjawhar/legion/daemon/internal/ghrepo"
)

const notificationStream = "ENVOY_NOTIFICATIONS"

const (
	defaultAckWait  = 30 * time.Second
	defaultNakDelay = 30 * time.Second
)

// ConsumerSpec is the caller-supplied durable-consumer configuration. The daemon owns its project
// and configured repositories; intake owns only the fixed stream and consumer identities.
type ConsumerSpec struct {
	Project      string
	Repositories []ghrepo.Repository
	AckWait      time.Duration
	NakDelay     time.Duration
	Logger       *slog.Logger
	// ReviewPermission answers whether review's author has write access or higher to its
	// repository, which is what lets a review decide a review round
	// (PullRequestReview.AuthorCanWrite). The workflow decides inside a transaction and performs no
	// I/O, so the answer is read here, before the fact is applied, only for a review that decides
	// (PullRequestReview.Decides) and has an author, and within AckWait, so a read GitHub is slow to
	// answer is cut and retried rather than outlasting the delivery it reads for. An author GitHub
	// gives no write access is false; a lookup that fails is an error, and the message is retried,
	// after NakDelay or the longer wait a RetryLater names, rather than applied with a permission
	// nobody read. No resolver (a test, or a daemon without one) leaves every review's
	// AuthorCanWrite false, so only the review App's own reviews decide.
	ReviewPermission func(ctx context.Context, review PullRequestReview) (bool, error)
	// ReviewBody restores a review-App review's full body from GitHub when Envoy's normalizer
	// capped it (PullRequestReview.BodyTruncated), since the capped body can cut the trailing
	// Legion footer the workflow reads to tell the reviewer's review from any other review-App
	// session's (PullRequestReview.LegionSession). It is read here, before the fact is applied,
	// within AckWait, and only for a review the review App submitted, with an id and a truncated
	// body (resolveReviewBody); a failed read is returned, and the message is retried the same way
	// a failed ReviewPermission read is. No resolver (a test, or a daemon without one) leaves every
	// truncated review's body capped.
	ReviewBody func(ctx context.Context, review PullRequestReview) (string, error)
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

// Consumers are this project's two durable JetStream consumers, created before intake runs so a
// daemon whose stream is missing refuses to boot instead of booting with no intake.
type Consumers struct {
	spec     ConsumerSpec
	stream   jetstream.Stream
	dispatch jetstream.Consumer
	github   jetstream.Consumer
}

// OpenConsumers creates or updates this project's Dispatch and GitHub durable consumers. A consumer
// created here starts at the next message, so a first daemon on a stream that already holds history
// (production keeps 72 hours of it) does not replay it into admission. Boot opens the consumers before
// it reads its Dispatch listing, so the listing covers the state before them and the consumers
// everything published since, including what lands while the listing is read. A consumer that exists keeps its position and the policy that
// created it, and takes the rest of this boot's configuration.
func OpenConsumers(ctx context.Context, js jetstream.JetStream, spec ConsumerSpec) (*Consumers, error) {
	spec, err := normalizedSpec(spec)
	if err != nil {
		return nil, err
	}
	stream, err := js.Stream(ctx, notificationStream)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", notificationStream, err)
	}

	dispatch, err := openConsumer(ctx, stream, jetstream.ConsumerConfig{
		Durable:       dispatchConsumerName(spec.Project),
		FilterSubject: "notifications.dispatch.issue.>",
		AckPolicy:     jetstream.AckExplicitPolicy,
		AckWait:       spec.AckWait,
	})
	if err != nil {
		return nil, fmt.Errorf("open the Dispatch durable consumer: %w", err)
	}
	github, err := openConsumer(ctx, stream, jetstream.ConsumerConfig{
		Durable:        githubConsumerName(spec.Project),
		FilterSubjects: githubFilters(spec.Repositories),
		AckPolicy:      jetstream.AckExplicitPolicy,
		AckWait:        spec.AckWait,
	})
	if err != nil {
		return nil, fmt.Errorf("open the GitHub durable consumer: %w", err)
	}
	return &Consumers{spec: spec, stream: stream, dispatch: dispatch, github: github}, nil
}

// openConsumer updates the durable consumer config names, keeping the start position it was created
// with (JetStream refuses to change it), or creates it delivering only new messages.
func openConsumer(ctx context.Context, stream jetstream.Stream, config jetstream.ConsumerConfig) (jetstream.Consumer, error) {
	existing, err := stream.Consumer(ctx, config.Durable)
	if errors.Is(err, jetstream.ErrConsumerNotFound) {
		config.DeliverPolicy = jetstream.DeliverNewPolicy
		return stream.CreateConsumer(ctx, config)
	}
	if err != nil {
		return nil, err
	}
	created := existing.CachedInfo().Config
	config.DeliverPolicy, config.OptStartSeq, config.OptStartTime = created.DeliverPolicy, created.OptStartSeq, created.OptStartTime
	return stream.UpdateConsumer(ctx, config)
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
// the measurement Reconcile holds against DispatchTarget (DispatchConsumerPosition.Reached).
func (c *Consumers) DispatchPosition(ctx context.Context) (DispatchConsumerPosition, error) {
	info, err := c.dispatch.Info(ctx)
	if err != nil {
		return DispatchConsumerPosition{}, fmt.Errorf("read Dispatch consumer info: %w", err)
	}
	return positionOf(info), nil
}

func positionOf(info *jetstream.ConsumerInfo) DispatchConsumerPosition {
	return DispatchConsumerPosition{
		AckFloorStream: int64(info.AckFloor.Stream),
		Idle:           info.NumPending == 0 && info.NumAckPending == 0,
	}
}

// Run consumes both durable consumers until ctx ends, and returns the error of either one that
// stops first. Every decoded fact enters ApplyFact; a committed transaction is acknowledged, a
// rolled-back transaction is nacked with a delay, poison is terminated, and a committed refusal is
// logged then acknowledged. The Dispatch consumer's own position, and admission's hold on it, are
// the daemon's boot-owned concern (workflowRuntime.pollHoldRelease), not intake's: this loop knows
// nothing about either.
func (c *Consumers) Run(ctx context.Context, pool *pgxpool.Pool, handlers ...Handler) error {
	group, consumeContext := errgroup.WithContext(ctx)
	group.Go(func() error {
		return consumeConsumer(consumeContext, c.dispatch, c.spec, pool, handlers)
	})
	group.Go(func() error { return consumeConsumer(consumeContext, c.github, c.spec, pool, handlers) })
	return group.Wait()
}

func consumeConsumer(ctx context.Context, consumer jetstream.Consumer, spec ConsumerSpec, pool *pgxpool.Pool, handlers []Handler) error {
	consuming, err := consumer.Consume(func(message jetstream.Msg) {
		consumeMessage(ctx, message, spec, pool, handlers)
	})
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

func consumeMessage(ctx context.Context, message jetstream.Msg, spec ConsumerSpec, pool *pgxpool.Pool, handlers []Handler) {
	decoded, err := decodeMessage(message.Subject(), spec.Project, spec.Repositories, message.Data())
	if err != nil {
		logMessage(spec.Logger, slog.LevelError, "poison JetStream message", message, "error", err)
		if termErr := message.Term(); termErr != nil {
			logMessage(spec.Logger, slog.LevelError, "term poison JetStream message", message, "error", termErr)
		}
		return
	}
	for _, field := range decoded.Unread {
		logMessage(spec.Logger, slog.LevelWarn, "unreadable field taken as absent", message, "event_id", decoded.EventID, "field", field)
	}
	if decoded.Fact == nil {
		ackMessage(spec.Logger, message)
		return
	}
	withBody, err := resolveReviewBody(ctx, spec, decoded.Fact)
	if err != nil {
		nakResolve(spec, message, decoded.EventID, "restore the review's truncated body", "nak the review body restore", err)
		return
	}
	fact, err := resolveReviewPermission(ctx, spec, withBody)
	if err != nil {
		nakResolve(spec, message, decoded.EventID, "read the reviewer's repository permission", "nak the reviewer's permission read", err)
		return
	}
	decoded.Fact = fact

	result, err := ApplyFact(ctx, pool, decoded.Source, decoded.EventID, decoded.Fact, handlers...)
	if err != nil {
		logMessage(spec.Logger, slog.LevelWarn, "retryable intake processing failure", message, "event_id", decoded.EventID, "error", err)
		if nakErr := message.NakWithDelay(spec.NakDelay); nakErr != nil {
			logMessage(spec.Logger, slog.LevelError, "nak intake processing failure", message, "event_id", decoded.EventID, "error", nakErr)
		}
		return
	}
	if result.Refusal != nil {
		logMessage(spec.Logger, slog.LevelWarn, "committed refusal", message,
			"event_id", decoded.EventID,
			"status", result.Refusal.Status,
			"code", result.Refusal.Code,
			"error", result.Refusal.Message,
		)
	}
	ackMessage(spec.Logger, message)
}

// resolveReviewPermission fills a review's AuthorCanWrite from ConsumerSpec.ReviewPermission,
// before the fact enters its transaction. Only a review that decides (PullRequestReview.Decides)
// and names its author is looked up: a comment decides nothing whoever writes it, so it costs no
// GitHub call. Every other fact passes through untouched.
func resolveReviewPermission(ctx context.Context, spec ConsumerSpec, fact Fact) (Fact, error) {
	review, ok := fact.(PullRequestReview)
	if !ok || spec.ReviewPermission == nil || review.Author == "" || !review.Decides() {
		return fact, nil
	}
	read, cancel := context.WithTimeout(ctx, spec.AckWait)
	defer cancel()
	canWrite, err := spec.ReviewPermission(read, review)
	if err != nil {
		return nil, err
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

func ackMessage(logger *slog.Logger, message jetstream.Msg) {
	if err := message.Ack(); err != nil {
		logMessage(logger, slog.LevelError, "ack intake message", message, "error", err)
	}
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
	if len(spec.Repositories) == 0 {
		return ConsumerSpec{}, fmt.Errorf("intake consumer repositories are required")
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
	if spec.Logger == nil {
		spec.Logger = slog.Default()
	}
	return spec, nil
}

func githubFilters(repositories []ghrepo.Repository) []string {
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
