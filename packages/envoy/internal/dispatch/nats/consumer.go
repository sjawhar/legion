// Package nats subscribes to the Envoy GitHub topics and forwards each
// event to the SSE hub, filtered per-client by watched repos.
//
// Subject shape (from envoy webhook publishers):
//
//	notifications.github.<owner>.<repo>.<kind>[.<sub>...]
//
// We subscribe to notifications.github.> (everything), extract <owner>/<repo>
// from each message, and call hub.BroadcastRepo(slug, …). The hub then
// fans out only to clients whose watched-repos set contains that slug.
//
// Filtering at the hub keeps the consumer agnostic of who's watching what —
// users add/remove repos in their dashboard without us needing to resubscribe
// at the NATS layer.
package nats

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	natsclient "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/dispatch/sse"
)

var defaultNatsURLs = []string{"nats://127.0.0.1:4222"}

// Connect opens a tuned core NATS connection for dispatch. We use bus.Dial
// instead of bus.Connect because dispatch only needs transient pub/sub
// (forward live GitHub events to live SSE clients). No JetStream durability,
// no replay — dashboards refetch state from GitHub on connect.
func Connect(urls []string) (*natsclient.Conn, error) {
	if len(urls) == 0 {
		urls = defaultNatsURLs
	}
	return bus.Dial("dispatch-server", urls)
}

// SubscribeGithub subscribes to every notifications.github.* topic and
// forwards messages as "github_event" SSE broadcasts, filtered per-client
// by watched-repos.
func SubscribeGithub(ctx context.Context, nc *natsclient.Conn, hub *sse.Hub) (*natsclient.Subscription, error) {
	const subject = "notifications.github.>"
	sub, err := nc.Subscribe(subject, func(msg *natsclient.Msg) {
		repo := repoFromSubject(msg.Subject)
		if repo == "" {
			slog.Debug("dispatch: skipping nats message — could not extract repo", "subject", msg.Subject)
			return
		}
		var payload any
		if err := json.Unmarshal(msg.Data, &payload); err != nil {
			slog.Warn("dispatch: failed to decode nats payload", "subject", msg.Subject, "error", err)
			return
		}
		hub.BroadcastRepo(repo, sse.Event{
			Type: "github_event",
			Data: map[string]any{
				"subject": msg.Subject,
				"repo":    repo,
				"payload": payload,
			},
		})
	})
	if err != nil {
		return nil, err
	}
	go func() {
		<-ctx.Done()
		_ = sub.Unsubscribe()
	}()
	slog.Info("dispatch: nats subscribed", "subject", subject)
	return sub, nil
}

// repoFromSubject returns the <owner>/<repo> slug from a subject of the
// form notifications.github.<owner>.<repo>.<kind>... Returns "" if the
// subject doesn't have at least the expected first four segments.
func repoFromSubject(subject string) string {
	parts := strings.Split(subject, ".")
	const (
		prefixLen  = 2 // "notifications.github"
		ownerIdx   = 2
		repoIdx    = 3
		minParts   = 4
	)
	if len(parts) < minParts {
		return ""
	}
	if parts[0] != "notifications" || parts[1] != "github" {
		return ""
	}
	owner := parts[ownerIdx]
	repo := parts[repoIdx]
	if owner == "" || repo == "" {
		return ""
	}
	return fmt.Sprintf("%s/%s", owner, repo)
}
