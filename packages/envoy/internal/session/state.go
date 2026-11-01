package session

import "time"

// The windows every reader of the session registry judges a record's age against. They are
// declarations and not settings on purpose: .github/scripts/check-heartbeat-margin.sh reads
// LiveWindow here to prove that three registration refreshes and one listener call fit inside it,
// and a gate that read a deployment's environment instead would prove nothing about the tree.
// WithSessionLiveWindow and WithSessionGoneHorizon move what one registry runs with; these are
// what a registry with no option, and every caller holding a record and no registry (IsLive), use.
const (
	// LiveWindow is how long a session's last registration keeps it live. A pane refreshes its
	// registration every DEFAULT_HEARTBEAT_MS (packages/envoy-client/src/envoy-settings.ts), so
	// this leaves two whole refreshes to fail before a working session stops reading live.
	LiveWindow = 5 * time.Minute
	// GoneHorizon is how long past its last registration a session's record still stands for a
	// session that is not dead but unresponsive: one that keeps its role and issue claims, has
	// messages stored for it, and gets no replacement started for it. Only past this is the
	// session gone, which is the only state in which the session reaper deletes its record and
	// the role reaper releases its claims. A box under load stalls a session for minutes at a
	// time, and reading such a stall as death is what this horizon stops.
	GoneHorizon = time.Hour
	// LastSeenMemory is how long after a session left the cache the registry still answers its
	// final registration time (WithLastSeenRetention). It is longer than the gone horizon, so a
	// caller told a session is gone can still say when that session was last seen.
	LastSeenMemory = 2 * time.Hour
)

// State is what a session's last registration says about it. Every reader of the registry that
// used to read a record's absence as death reads a state instead: a push to a serving port, topic
// fan-out and the active-sessions gauge want Live, while the role reaper and the session reaper
// act only on Gone.
type State string

const (
	// Live is a session whose last registration is within the live window.
	Live State = "live"
	// Unresponsive is a session past the live window and within the gone horizon. It is not dead:
	// nothing of its is taken away, and no replacement is started for it.
	Unresponsive State = "unresponsive"
	// Gone is a session past the gone horizon, or one the registry holds no record for.
	Gone State = "gone"
)

// StateOf judges entry by its last registration (UpdatedAt) and by nothing else: not by its
// presence in any cache, not by the bucket's maximum age, and not by any claim it holds. A record
// the copy kept through a re-read is judged again from its own age, so a stale cache cannot hold
// a dead session live, and a cache that could not see a live session's heartbeats does not read
// their absence as death either.
//
// Each window is the age a record may reach, not the age it may not: a record exactly live old is
// still live, and one exactly horizon old is still unresponsive. The boundaries fall that way so
// that a record refreshed exactly one window ago — which is what a refresh that arrives at its
// own deadline leaves — is not the moment a session is taken for stalled.
//
// A record with no registration time at all is gone: there is no moment to measure it from, and
// a session a caller cannot date must not be taken for live.
func StateOf(entry SessionEntry, now time.Time, live, horizon time.Duration) State {
	if entry.UpdatedAt <= 0 {
		return Gone
	}
	switch age := now.Sub(time.UnixMilli(entry.UpdatedAt)); {
	case age <= live:
		return Live
	case age <= horizon:
		return Unresponsive
	}
	return Gone
}

// IsLive reports whether entry's last registration is within the package's own live window, for a
// caller that holds a record and has no registry to ask. A caller that has one asks it
// (SessionRegistry.Lookup), since an option may have moved its windows.
func IsLive(entry SessionEntry, now time.Time) bool {
	return StateOf(entry, now, LiveWindow, GoneHorizon) == Live
}
