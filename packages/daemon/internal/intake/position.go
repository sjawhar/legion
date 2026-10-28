package intake

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go/jetstream"
)

// streamPosition is where a durable consumer's message sits on the notification stream: the
// consumer that delivered it, its stream sequence and the time JetStream stored it.
type streamPosition struct {
	consumer string
	seq      uint64
	time     time.Time
}

type streamPositionKey struct{}

// withStreamPosition is ctx carrying message's stream position, for ApplyFact to record in the
// fact's transaction (recordStreamPosition). A message whose metadata does not parse carries none.
func withStreamPosition(ctx context.Context, message jetstream.Msg) context.Context {
	metadata, err := message.Metadata()
	if err != nil {
		return ctx
	}
	return context.WithValue(ctx, streamPositionKey{}, streamPosition{consumer: metadata.Consumer, seq: metadata.Sequence.Stream, time: metadata.Timestamp})
}

// recordStreamPosition writes the stream position ctx carries (withStreamPosition) as its
// consumer's position, in tx, so it commits with the fact or not at all. A position only advances:
// a redelivery of an older message, applied after a newer one, leaves the newer one standing. The
// time is truncated to the microseconds Postgres keeps, never rounded up, so a consumer created
// at it (openConsumer) starts at or before the message, never after it. A context with no
// position (a fact from the API or supervision) records nothing.
func recordStreamPosition(ctx context.Context, tx pgx.Tx) error {
	position, ok := ctx.Value(streamPositionKey{}).(streamPosition)
	if !ok {
		return nil
	}
	if _, err := tx.Exec(ctx, `insert into consumer_positions (consumer, stream_seq, stream_time) values ($1, $2, $3)
		on conflict (consumer) do update set
			stream_seq = greatest(consumer_positions.stream_seq, excluded.stream_seq),
			stream_time = greatest(consumer_positions.stream_time, excluded.stream_time)`,
		position.consumer, int64(position.seq), position.time.Truncate(time.Microsecond)); err != nil {
		return fmt.Errorf("record the position of durable consumer %s at stream sequence %d: %w", position.consumer, position.seq, err)
	}
	return nil
}

// storedStreamTime is the stream time of the last message consumer applied, and false when it
// has applied none.
func storedStreamTime(ctx context.Context, pool *pgxpool.Pool, consumer string) (time.Time, bool, error) {
	var at time.Time
	err := pool.QueryRow(ctx, `select stream_time from consumer_positions where consumer = $1`, consumer).Scan(&at)
	if errors.Is(err, pgx.ErrNoRows) {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, fmt.Errorf("read the stored position of durable consumer %s: %w", consumer, err)
	}
	return at, true, nil
}
