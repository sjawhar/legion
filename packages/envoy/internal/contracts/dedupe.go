package contracts

import (
	"regexp"
	"strings"
)

// RoleForwardDedupePrefix starts the dedupe key of a role forward: an envelope sent to a role's
// holder on the holder's agent subject, its topic still the role's, its dedupe key this prefix and
// the original's. The listener's role arbiter and Dispatch's outbox both forward under it, and an
// envelope carrying it is never arbitrated again.
const RoleForwardDedupePrefix = "envoy.role.forward."

var mintedDedupeKey = regexp.MustCompile(MintedDedupeKeyPattern)

// AgentSendDedupeKey is the dedupe key of a direct send to sessionID's own topic
// (notifications.agent.<session>) under idempotencyKey: the listener keys every send by it, and
// Dispatch's outbox gives it to the copy of a comment it publishes to a session whose mention of
// that comment failed, so a host that already holds the mention drops the copy.
func AgentSendDedupeKey(sessionID, idempotencyKey string) string {
	return "agent." + sessionID + "." + idempotencyKey
}

// RoleForwardDedupeKey is the dedupe key a role forward of item to holder carries: the key
// holder's own mention of the comment went under when the envelope names one
// (Envelope.MentionDedupeKeys, which only a copy kept on a role's kept lane carries), else the
// message's own key unchanged when that key is already holder's own agent send key
// (AgentSendDedupeKey, a key beginning agent.<holder>.), and RoleForwardDedupePrefix in front of
// it for every other key, which is what a role forward has always carried.
//
// The exceptions are there because such a key is one the holder's host has claimed already: the
// send that minted it - Dispatch's outbox gives the copy of a comment the key of the mention of
// that session whose send failed - may have landed before the attempt failed, so a copy carrying
// that key is dropped at that host and the session is shown the comment once, while the prefix
// would make it a key no host has seen and show the comment a second time. A key naming another
// session names no claim of this holder's, and an empty holder names no host at all, so both
// take the prefix. The carried keys are what a copy kept for a role nobody holds has in place of
// the holder it cannot know: which of the comment's mentioned sessions claims the role next is
// decided after the copy is stored, so the key of that holder's own mention is chosen here, by
// whoever forwards the copy on, rather than stored with it.
//
// Both forwarders of a role message ask it - the listener's role lane and the drain of a role's
// kept lane (roleForward.forwarded in cmd/listener/role_lane.go), and Dispatch's outbox, for a
// route's own forward and for the handover behind a keep (roleForward.forward in
// internal/dispatch/outbox/role.go) - so one message reaches one holder under one key, and so
// under one bus.ForwardMsgID of that key and the holder's agent subject, which is what keeps the
// stream to one stored copy of it however often it is forwarded.
func RoleForwardDedupeKey(item Envelope, holder string) string {
	if key := item.MentionDedupeKeys[holder]; key != "" {
		return key
	}
	if holder != "" && strings.HasPrefix(item.DedupeKey, AgentSendDedupeKey(holder, "")) {
		return item.DedupeKey
	}
	return RoleForwardDedupePrefix + item.DedupeKey
}

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
