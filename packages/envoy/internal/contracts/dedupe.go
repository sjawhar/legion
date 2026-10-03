package contracts

import "regexp"

var mintedDedupeKey = regexp.MustCompile(MintedDedupeKeyPattern)

// DedupeKeyNamesTheUpstreamEvent reports whether an envelope's dedupe key names its event, so that a
// second envelope carrying it is the same event arriving again and the stream may drop it. It is
// dedupeKeyNamesItsEvent in packages/contracts, which every core-NATS host asks too: for Dispatch the
// idempotency key Dispatch supplies (LEGION-271); for a webhook source the key the normalizers mint,
// the source plus the upstream's own delivery id, which the envelope also carries as SourceEventID;
// and a key minted once for its message by the listener or the shared transport
// (MintedDedupeKeyPattern), which only a re-send of that message repeats. The source name alone is
// not enough, because other producers publish under these names with keys of their own shape that
// two distinct events can share (TestMCPBridge_TwoEventsSharingADedupeKeyBothLand). A deliberate
// replay inside the stream's 72-hour duplicate window is therefore stored once unless it carries a
// new key. Storing is all the MsgId decides: a core-NATS subscriber is handed the replay anyway and
// recognises it by its dedupe key (DELIVERY_DUPLICATE_WINDOW_MS in packages/contracts).
func DedupeKeyNamesTheUpstreamEvent(item Envelope) bool {
	if item.Source == "dispatch" || mintedDedupeKey.MatchString(item.DedupeKey) {
		return true
	}
	switch item.Source {
	case "github", "slack", "ghostwispr":
		return item.SourceEventID != "" && item.DedupeKey == item.Source+"."+item.SourceEventID
	default:
		return false
	}
}
