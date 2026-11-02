package bus

// The wake subject: how the listener tells the Legion daemons of a message that reached nobody
// because the issue pod its recipient runs in was released, and how it tells them it is listening
// at all.

import "time"

// WakeTopic is the one subject the listener reports a wake on. It is a leaf of
// notifications.legion.>, which the stream already carries (streamSubjects) and already classifies
// private for replay (privateStreamSubjects), so it needs no stream change, no replay class of its
// own and no new NATS grant. Every Legion daemon consumes it on a durable of its own and acts only
// on a role token or a session id among its own released claims: the listener does not know which
// daemon owns a session and must not learn.
const WakeTopic = NotificationTopicPrefix + "legion.wake"

// The kinds a WakeReport carries.
const (
	// WakeKindRoleMessage: a message was published to a role topic whose role has no live holder,
	// a keep: false one included. The holder is gone because its pod was released, so the role is
	// woken and the sender's next try reaches it.
	WakeKindRoleMessage = "role_message"
	// WakeKindAgentMessage: a message sent to the agent subject of a session the listener could
	// place nowhere — an Envoy direct message, or a person's answer, comment or message a Dispatch
	// claim routes to that session.
	WakeKindAgentMessage = "agent_message"
	// WakeKindListenerReady: the listener is listening. It is published at start and every
	// ListenerReadyInterval after, and a daemon that has seen none for fifteen minutes releases
	// no pod: "the listener sends no reports" and "the listener has nothing to report" are the
	// same silence, and only this report tells them apart.
	WakeKindListenerReady = "listener_ready"
)

// ListenerReadyInterval is how often a running listener says so on WakeTopic. The daemon's own
// window is three of these, so a listener that misses one report, or a daemon that misses one
// message, releases pods as before.
const ListenerReadyInterval = 5 * time.Minute

// WakeReport is the body of a wake report, carried as the envelope's payload on WakeTopic. The
// daemon reads it by these member names and by nothing else.
type WakeReport struct {
	// Kind is one of the three above. A report of any other kind is read as no wake at all, so a
	// listener ahead of a daemon can add one without poisoning its stream.
	Kind string `json:"kind"`
	// Role is the role token a role publish named (WakeKindRoleMessage), without the topic
	// prefix: "legion-INFRA-945-tester".
	Role string `json:"role,omitempty"`
	// Session is the session id an agent delivery was addressed to (WakeKindAgentMessage).
	Session string `json:"session,omitempty"`
	// ReportID is the listener's own id for the message this report is about — the envelope's
	// event id — so every redelivery of one message reports the same id and wakes the pod once.
	ReportID string `json:"reportId,omitempty"`
	// At is the listener's own clock when it made the report, in epoch milliseconds.
	At int64 `json:"at"`
	// Listener is the machine id of the listener that made the report, for an operator reading
	// the stream.
	Listener string `json:"listener,omitempty"`
	// Reason is why the message reached nobody, in the listener's own words.
	Reason string `json:"reason,omitempty"`
}
