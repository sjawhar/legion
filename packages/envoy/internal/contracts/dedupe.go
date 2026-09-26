package contracts

// DedupeKeyNamesTheUpstreamEvent reports whether an envelope's dedupe key is built from the
// identity the upstream gave the event itself, rather than from this attempt at it, from a key a
// caller chose, or from the event's own content. The stream may drop a repeat of such a key,
// because a second envelope carrying it is the same event arriving again.
//
// An ingested event qualifies when its key is its source and the upstream's own delivery id, which
// is what the normalizers mint and what the envelope carries beside it as SourceEventID:
//
//   - github: "github.<delivery>", the X-GitHub-Delivery GUID, which GitHub documents as
//     identifying the event; a redelivery of it carries the same GUID (the deliveries API lists a
//     per-attempt id, a per-event guid, and a redelivery flag beside each other).
//   - slack: "slack.<event_id>", which Slack repeats on each retry of one event.
//   - ghostwispr: "ghostwispr.<delivery>", the sender's own delivery id.
//
// Reading the key rather than the source alone is what keeps this exact, because a source name is
// not evidence of anything: two other producers publish under a source an operator or a caller
// chooses, with keys of their own shape, and neither may be deduped.
//
//   - The MCP bridge publishes with the source its configuration names, where `github` is a legal
//     value, and a key of "<source>." plus a sha256 over the resource URI and the summary
//     (`internal/mcpbridge/envelope.go`). That summary is one constant sentence whenever the
//     resource read returns no text, so two distinct events on one URI share a key. Each is a
//     fresh report that something happened, never a redelivery of the one before it, so both must
//     be published.
//   - The CI store publishes check settlements as `github` too, with a key carrying the head and
//     the record's generation and a SourceEventID minted per publish
//     (`internal/cistore/loop.go`). A record recreated under one head restarts its generation, so
//     an equal key can carry a different snapshot.
//
// Dispatch qualifies by its own rule: the key is the idempotency key Dispatch supplies for a
// targeted message, "<message>:<mode>", stable across every attempt of that pair (LEGION-271).
//
// Everything else is left out. A WhatsApp key is composed from a timestamp
// ("whatsapp.<phone>.<chat>.<millis>"), which two messages in one chat can share; an agent's or an
// API caller's key is minted per call or chosen by the caller.
//
// What it costs where it applies: replaying an event on purpose inside the stream's duplicate
// window (72 h) publishes nothing, because the stream recognises the key. A replay must therefore
// carry a new upstream delivery id, or wait the window out.
func DedupeKeyNamesTheUpstreamEvent(item Envelope) bool {
	switch item.Source {
	case "github", "slack", "ghostwispr":
		return item.SourceEventID != "" && item.DedupeKey == item.Source+"."+item.SourceEventID
	case "dispatch":
		return true
	default:
		return false
	}
}
