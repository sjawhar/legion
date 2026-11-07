package main

import (
	"log/slog"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/internal/roles"
	"github.com/sjawhar/envoy/roleholder"
)

// refusalExceptionBound is the most of a refused publish's machine payload its exception carries.
// The exception embeds the original payload, so without a bound a large refused publish would make
// an envelope past the server's max payload, which NATS refuses whole (bus.ErrTooLarge) on every
// attempt: the refusal would log exceptionPublishFailedLine, which the deployed publish-failure
// alarm matches, for a message nothing was wrong with. A payload past this is left out and the
// exception still carries the topic, the reason, the dedupe key and the one-line payload_summary,
// which is what a consumer of the refusal needs. 64 KiB keeps the exception far below NATS's
// default 1 MiB max payload, and every notice the Legion daemon publishes to a role is orders of
// magnitude smaller, so its own reports always carry their payload whole.
const refusalExceptionBound = 64 << 10

// publishRefusalException raises the role-lane exception of a publish this listener refused 404
// because no live session holds the role (roleSend.noHolder, after the keep lane kept nothing). It
// adds that bus signal; no Legion consumer acts on a peer's refusal today. The daemon's consumer
// handles only its own notices, and it treats these two reasons as a refused outbox row that
// retries on its own. A consumer that should act for a peer would have to wake or re-claim the
// claim behind the refused topic. The envelope is the delivery path's own
// (deliveryExceptionEnvelope), and the reason is the shared no-holder reason (roleholder). A state
// this function receives that roleholder does not list raises no exception, so a new listener
// state cannot become a reason the daemon calls malformed.
//
// It publishes on a goroutine of its own, after the caller has written the 404. net/http holds a
// handler's answer until the handler returns, so publishing in line would delay the 404 by as long
// as the publish takes -- up to the bus's own publish bound on a reconnecting connection, past this
// route's own budget and past the Envoy client's per-attempt deadline, whose retry would refuse a
// second time and raise a second exception. client.Publish keeps that bus bound itself, so this
// detached call adds no second one. Nothing waits for it, nothing retries it, and a failure is
// logged (exceptionPublishFailedLine) and nothing more.
func publishRefusalException(client *bus.Client, logger *logging.Logger, item contracts.Envelope, result roles.Resolution) {
	reason := string(result.State)
	if !roleholder.IsNoHolderReason(reason) {
		return
	}
	exception, err := deliveryExceptionEnvelope(item, reason, result.Holder, refusalExceptionBound)
	if err != nil {
		logger.Error(exceptionPublishFailedLine, slog.String("error", err.Error()), slog.String("topic", item.Topic))
		return
	}
	go func() {
		if err := client.Publish(exception); err != nil {
			logger.Error(exceptionPublishFailedLine, slog.String("error", err.Error()), slog.String("topic", item.Topic))
		}
	}()
}
