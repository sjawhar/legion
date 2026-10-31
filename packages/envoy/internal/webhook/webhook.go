package webhook

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
)

// Publisher publishes a webhook's envelopes, each within the context its handler gives it, which
// carries the request's own end and the route's budget for its caller (*bus.Client.PublishWithin,
// which also keeps the bus's own five-second bound).
type Publisher interface {
	PublishWithin(context.Context, contracts.Envelope) error
}

// deliveryFailed answers a delivery whose envelope the listener did not publish, and logs
// `<action> refused` or `<action> failed` (failureOutcome). Slack and Ghost Wispr deliveries log
// it on every attempt; a GitHub delivery logs it once (failedDeliveries).
func deliveryFailed(w http.ResponseWriter, action string, err error) {
	result, status, answer := failureOutcome(err)
	log.Print(failureLine(action, result, err))
	http.Error(w, answer, status)
}

// failureLine is the line a failed delivery logs, `<action> <result>: <error>`, which the
// listener's `webhook-refused` and `publish-failure` alarms count. A GitHub delivery that fails
// again logs this same line behind its own `redelivery <delivery>: ` prefix
// (*failedDeliveries.deliveryFailed), which no alarm counts, so both lines are written here.
func failureLine(action, result string, err error) string {
	return fmt.Sprintf("%s %s: %v", action, result, err)
}

// failureOutcome is how a delivery whose envelope, or CI observation, the listener did not publish
// is logged and answered. A refusal (bus.ErrRefused: too large to publish whole, or a subject NATS
// does not accept) is refused the same way on every redelivery while the server's limits stand,
// so it is a 422 `refused`, and its answer is the error, which says why; Dispatch's redelivery
// sweep asks for it again like any failed delivery. Any other failure may pass on a redelivery,
// so it is a 503 `failed`. A stream at its byte limit (bus.ErrStreamFull) is such a failure: it
// takes the redelivery once age has made room, on a clustered stream too, where the server
// acknowledges a redelivery under the refused delivery's MsgId as a duplicate it does not hold and
// the bus publishes it once more without the id (bus.ErrNotStored); until then that is a 503 as
// well. The listener's `webhook-refused` and `publish-failure` alarms count these lines.
func failureOutcome(err error) (result string, status int, answer string) {
	if errors.Is(err, bus.ErrRefused) {
		return "refused", http.StatusUnprocessableEntity, err.Error()
	}
	return "failed", http.StatusServiceUnavailable, "service unavailable"
}
