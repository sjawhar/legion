package main

import "github.com/sjawhar/envoy/internal/session"

// subscribeBody is the POST /v1/interests/subscribe payload sent by each
// agent session's envoy extension, on subscribe and on every heartbeat.
type subscribeBody struct {
	SessionID    string   `json:"session_id"`
	Dir          string   `json:"dir"`
	Topics       []string `json:"topics"`
	Port         int      `json:"port"`
	Title        string   `json:"title"`
	Capabilities []string `json:"capabilities"`
	// Driving reports that this process is the one actually driving the session,
	// rather than a process that merely has it loaded from shared on-disk state.
	// Absent (older plugins) means "not driving", which is the safe default: a
	// claim without the flag cannot displace a live driving claim.
	Driving        bool `json:"driving"`
	SelfSubscribed bool `json:"self_subscribed"`
	// Host is envoy-client's machineID() (ENVOY_MACHINE_ID, falling back to the kernel
	// hostname): where the session process actually runs, as opposed to machineID below,
	// which is always the registering listener's own machine (unrelated to the caller). It is the
	// address the session's owning listener reaches its serving port at, unless the session runs
	// on that listener's machine (it registered through it, or Host is that listener's machine
	// id) and the listener has a host bridge (ENVOY_HOST_BRIDGE), which then wins. The owner is
	// the listener the session's stored subscription names (the one
	// store.Registry.Match delivers on), else the one it registered through; only the owner
	// delivers to the session, and any other listener leaves inbox messages to it silently
	// (session.ErrWrongMachine). Empty for a session whose plugin predates
	// this field; its owning listener then falls back to host.docker.internal when the session
	// registered through it, else to the machine id of the listener it registered through.
	Host string `json:"host"`
	// Delivery asks for consumer delivery (subscribeDelivery); absent from an older plugin's
	// registration, which stays on the core path.
	Delivery *subscribeDelivery `json:"delivery"`
}

// subscribeDelivery is a registration's delivery object. Consumer asks the listener for the
// session's consumer (bus.SessionConsumers), relayed on the session's relay subject
// (bus.DeliverSubject); Epoch is the plugin's random string for its current NATS connection; and
// Resume, when the session's consumer is missing, seeds the new one from the plugin's persisted
// resume point.
type subscribeDelivery struct {
	Consumer bool             `json:"consumer"`
	Epoch    string           `json:"epoch"`
	Resume   *subscribeResume `json:"resume"`
}

// subscribeResume is a session's resume point: the stream's identity (bus.StreamIDMetadataKey) and
// a sequence below which the plugin holds nothing unwritten, so a consumer created from it starts at
// Seq+1 when StreamID is still the stream's.
type subscribeResume struct {
	StreamID string `json:"stream_id"`
	Seq      uint64 `json:"seq"`
}

// sessionEntryFromSubscribe is the session entry a registration writes: the body's fields, the
// registering listener's machine id, and, when delivery says the listener holds the session's
// consumer for it, consumer delivery and the plugin's epoch.
func sessionEntryFromSubscribe(body subscribeBody, machineID string, delivery sessionDeliveryOutcome) session.SessionEntry {
	entry := session.SessionEntry{
		Port:           body.Port,
		MachineID:      machineID,
		Dir:            body.Dir,
		Title:          body.Title,
		Capabilities:   append([]string{}, body.Capabilities...),
		Driving:        body.Driving,
		SelfSubscribed: body.SelfSubscribed,
		Host:           body.Host,
	}
	if delivery.relayed {
		entry.Delivery = session.DeliveryConsumer
		entry.Epoch = delivery.epoch
	}
	return entry
}
