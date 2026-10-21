// Package startup publishes Dispatch's own start on the bus (LEGION-781): one server.started
// envelope naming the build commit the binary was stamped with, which the Legion daemon compares
// against the merge commits of its open production checks to learn that a Dispatch change reached
// production. It goes directly through the bus client rather than the outbox: it is no issue's,
// document's or project's event, and the outbox publishes only those.
package startup

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/id"
)

// Subject is the one subject Dispatch publishes its start on, mirrored by
// DISPATCH_SERVER_STARTED_SUBJECT in packages/contracts/src/dispatch-api.ts and matched whole by the
// Legion daemon's intake. A subject of the notification stream (bus.StreamSubjects'
// notifications.dispatch.>), so the publish is a JetStream publish with an acknowledgement, and the
// stream's duplicate window recognises a retry of one that landed.
const Subject = "notifications.dispatch.server.started"

// Source is the envelope source of every Dispatch publish, this one included.
const Source = "dispatch"

// Payload is a server.started envelope's payload, mirrored by DispatchServerStartedPayload in
// packages/contracts/src/dispatch-api.ts: the build commit and when the process started, in UTC to
// the second.
type Payload struct {
	Commit    string    `json:"commit"`
	StartedAt time.Time `json:"started_at"`
}

// Backoff bounds the wait between two publish attempts: the first retry after initialBackoff,
// each later one after twice the wait before it, never past maxBackoff. A reconnect of the client
// ends the wait early, so the bound matters only while the client believes itself connected and
// the stream still refuses the publish.
const (
	initialBackoff = time.Second
	maxBackoff     = 30 * time.Second
)

// PublishStarted publishes Dispatch's start on Subject and returns once the stream has
// acknowledged it or ctx ends. With an empty commit, a binary nothing stamped
// (cmd/dispatch's buildCommit), it publishes nothing and says so at INFO: there is no build to
// name, and a start naming none would tell the daemon nothing. Otherwise it builds the envelope once
// (Envelope), at now, and publishes it through PublishReportingDuplicate, whose JetStream
// acknowledgement is the one proof the stream holds it; a core publish answers nothing. A publish
// that fails is retried after a backoff, or as soon as the client reconnects, whichever comes first,
// until the acknowledgement or ctx's end. The envelope's dedupe key names the start, so a retry of
// a publish that landed before its acknowledgement was lost is stored once, and comes back as a
// duplicate rather than a second start. The reconnect hook stays registered on the client for the
// process's life, as the client offers no removal; after the acknowledgement it does nothing.
func PublishStarted(ctx context.Context, client *bus.Client, commit string, now func() time.Time, log *slog.Logger) {
	if commit == "" {
		log.Info("dispatch publishes no start: no build commit")
		return
	}
	item, err := Envelope(commit, now())
	if err != nil {
		log.Error("dispatch publishes no start: envelope", "commit", commit, "error", err)
		return
	}
	reconnected := make(chan struct{}, 1)
	client.AddReconnectHook(func(*nats.Conn) error {
		select {
		case reconnected <- struct{}{}:
		default:
		}
		return nil
	})
	backoff := initialBackoff
	for attempt := 1; ; attempt++ {
		duplicate, err := client.PublishReportingDuplicate(item)
		if err == nil {
			log.Info("dispatch published its start", "commit", commit, "event_id", item.EventID, "attempt", attempt, "duplicate", duplicate)
			return
		}
		log.Warn("dispatch could not publish its start; it retries", "commit", commit, "attempt", attempt, "retry_in", backoff, "error", err)
		select {
		case <-ctx.Done():
			log.Info("dispatch stopped before it published its start", "commit", commit, "attempts", attempt)
			return
		case <-time.After(backoff):
		case <-reconnected:
			log.Debug("dispatch retries publishing its start on the reconnect", "commit", commit)
		}
		backoff = min(backoff*2, maxBackoff)
	}
}

// Envelope is the server.started envelope for a start of the build commit at startedAt: its event
// id, source event id and dedupe key dispatch-server-started-<commit>-<startedAt unix>, source
// dispatch, topic Subject, issued at startedAt, the summary `dispatch started on <sha7>`, a trace
// id minted as the webhook receivers mint theirs, and Payload as JSON text. Every field the daemon's
// envelope check requires is set; Validate holds it to the contract.
func Envelope(commit string, startedAt time.Time) (contracts.Envelope, error) {
	startedAt = startedAt.UTC().Truncate(time.Second)
	payload, err := json.Marshal(Payload{Commit: commit, StartedAt: startedAt})
	if err != nil {
		return contracts.Envelope{}, fmt.Errorf("encode server.started payload: %w", err)
	}
	eventID := fmt.Sprintf("dispatch-server-started-%s-%d", commit, startedAt.Unix())
	item := contracts.Envelope{
		EventID:        eventID,
		Source:         Source,
		SourceEventID:  eventID,
		Topic:          Subject,
		DedupeKey:      eventID,
		IssuedAt:       startedAt.UnixMilli(),
		PayloadSummary: "dispatch started on " + shortCommit(commit),
		Payload:        string(payload),
		TraceID:        id.New(),
	}
	if err := item.Validate(); err != nil {
		return contracts.Envelope{}, err
	}
	return item, nil
}

// shortCommit is the first seven characters of a commit sha, or the whole of a shorter string.
func shortCommit(commit string) string {
	if len(commit) > 7 {
		return commit[:7]
	}
	return commit
}
