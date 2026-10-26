package embedqueue

import (
	"context"
	"log/slog"

	"github.com/sjawhar/envoy/internal/dispatch/embed"
)

// embedCanary is a fixed, short, known-good text, never stored: the probe that finds out whether
// the embedder itself - the service, the credentials, the model - is working at that exact
// moment. confirmSoloFailure sends it right after a solo row's own embed call fails, and the
// coverage pass sends it (proveEmbedder) before it acts on rows another identity left while
// nothing this process embedded has proven the embedder yet. It is safe to send on every such
// occasion because it names nothing about any row's own content, costs a negligible, constant
// number of tokens (reserved from the same shared budget as any other background call), and is
// never written to embeddings or returned to a caller - its only use is the one Embed call's own
// success or failure.
const embedCanary = "dispatch embedqueue confirmation canary"

// confirmSoloFailure decides whether a solo row's own non-throttled failure is this row's own
// fault or a systemic condition, by immediately probing embedCanary - right after the row's own
// failed call, in the same call context, drawing its own token reservation like any other
// background call since it is a real (if tiny) request to the model service. A canary success
// proves the service, credentials and model were all working at that exact moment, so the row's
// own failure is confirmed permanent. A canary failure - including a throttle - means the
// condition is systemic, not this row's fault: nothing is confirmed, the row goes back to retry,
// and if the canary itself was throttled, throttled is reported too, so the caller backs off
// exactly as an outright-throttled call would. The confirmation is local to this one row and its
// own fresh canary call, never reasoning about any other row's result in the same batch or
// bisection.
func confirmSoloFailure(ctx context.Context, deps Deps, group []pendingRow, rowErr error, renewal *claimRenewal) bisectResult {
	renewal.renew(ctx)
	reserveBackgroundTokens(ctx, deps, []string{embedCanary}, renewal)
	_, canaryErr := deps.embedDocuments(ctx, []string{embedCanary})
	if canaryErr != nil {
		slog.Error("dispatch embedqueue: a known-good canary also failed right after this row's own failure - systemic, not this row's fault",
			"kind", group[0].kind, "id", group[0].id, "rowError", rowErr, "canaryError", canaryErr)
		return bisectResult{retry: group, throttled: embed.IsThrottled(canaryErr)}
	}
	slog.Error("dispatch embedqueue: a known-good canary embedded successfully right after this row's own failure - confirmed",
		"kind", group[0].kind, "id", group[0].id, "error", rowErr)
	return bisectResult{permanent: group}
}

// proveEmbedder embeds embedCanary, paced from the shared budget like any other background call,
// and returns the call's error: nil proves the configured embedder answers (and records it,
// Deps.proven), so Cover may drop the vectors and re-admit the dead rows another identity left.
// Any failure - a deployment that does not exist, a refused credential, a throttle - proves
// nothing, and the pass leaves those rows as they are until a later one proves it. The canary
// holds no claim, so it has nothing to renew.
func proveEmbedder(ctx context.Context, deps Deps) error {
	reserveBackgroundTokens(ctx, deps, []string{embedCanary}, nil)
	_, err := deps.embedDocuments(ctx, []string{embedCanary})
	return err
}
