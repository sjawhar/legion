package contracts

// DedupeKeyNamesTheUpstreamEvent reports whether an envelope's dedupe key is the identity the
// upstream gave the event, so that a second envelope carrying it is the same event arriving again
// and the stream may drop it. For a webhook source that is the key the normalizers mint, the source
// plus the upstream's own delivery id, which the envelope also carries as SourceEventID; for
// Dispatch it is the idempotency key Dispatch supplies (LEGION-271). The source name alone is not
// enough, because other producers publish under these names with keys of their own shape that two
// distinct events can share (TestMCPBridge_TwoEventsSharingADedupeKeyBothLand). A deliberate replay
// inside the stream's 72-hour duplicate window is therefore stored once unless it carries a new
// delivery id. Storing is all the MsgId decides: a core-NATS subscriber is handed the replay anyway
// and recognises it by its dedupe key (DELIVERY_DUPLICATE_WINDOW_MS in packages/contracts).
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
