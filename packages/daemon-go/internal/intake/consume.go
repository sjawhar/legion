package intake

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go/jetstream"
	"golang.org/x/sync/errgroup"
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
	Repositories []string
	AckWait      time.Duration
	NakDelay     time.Duration
	Logger       *slog.Logger
}

// Consume runs this project's two durable JetStream consumers. Every decoded fact enters
// ApplyFact; a committed transaction is acknowledged, a rolled-back transaction is nacked with a
// delay, poison is terminated, and a committed refusal is logged then acknowledged.
func Consume(ctx context.Context, js jetstream.JetStream, spec ConsumerSpec, pool *pgxpool.Pool, handlers ...Handler) error {
	spec, err := normalizedSpec(spec)
	if err != nil {
		return err
	}
	stream, err := js.Stream(ctx, notificationStream)
	if err != nil {
		return fmt.Errorf("open %s: %w", notificationStream, err)
	}

	dispatch, err := stream.CreateOrUpdateConsumer(ctx, jetstream.ConsumerConfig{
		Durable:       dispatchConsumerName(spec.Project),
		FilterSubject: "notifications.dispatch.issue.>",
		AckPolicy:     jetstream.AckExplicitPolicy,
		AckWait:       spec.AckWait,
	})
	if err != nil {
		return fmt.Errorf("create Dispatch durable consumer: %w", err)
	}
	github, err := stream.CreateOrUpdateConsumer(ctx, jetstream.ConsumerConfig{
		Durable:        githubConsumerName(spec.Project),
		FilterSubjects: githubFilters(spec.Repositories),
		AckPolicy:      jetstream.AckExplicitPolicy,
		AckWait:        spec.AckWait,
	})
	if err != nil {
		return fmt.Errorf("create GitHub durable consumer: %w", err)
	}

	group, consumeContext := errgroup.WithContext(ctx)
	group.Go(func() error { return consumeConsumer(consumeContext, dispatch, spec, pool, handlers) })
	group.Go(func() error { return consumeConsumer(consumeContext, github, spec, pool, handlers) })
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
	decoded, err := decodeMessage(message.Subject(), spec.Project, message.Data())
	if err != nil {
		logMessage(spec.Logger, slog.LevelError, "poison JetStream message", message, "error", err)
		if termErr := message.Term(); termErr != nil {
			logMessage(spec.Logger, slog.LevelError, "term poison JetStream message", message, "error", termErr)
		}
		return
	}
	if decoded.Fact == nil {
		ackMessage(spec.Logger, message)
		return
	}

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
	for _, repo := range spec.Repositories {
		owner, name, ok := strings.Cut(repo, "/")
		if !ok || owner == "" || name == "" || strings.Contains(name, "/") {
			return ConsumerSpec{}, fmt.Errorf("intake consumer repository %q must be owner/repository", repo)
		}
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

func githubFilters(repositories []string) []string {
	filters := make([]string, 0, len(repositories))
	for _, repo := range repositories {
		owner, name, _ := strings.Cut(repo, "/")
		filters = append(filters, "notifications.github."+owner+"."+name+".>")
	}
	return filters
}

func dispatchConsumerName(project string) string {
	return "legion-go-" + project + "-dispatch"
}

func githubConsumerName(project string) string {
	return "legion-go-" + project + "-github"
}
