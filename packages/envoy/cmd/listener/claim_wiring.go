package main

import (
	"strings"

	"github.com/sjawhar/envoy/internal/session"
)

// subscribeBody is the POST /v1/interests/subscribe payload sent by each
// agent session's envoy extension, on subscribe and on every heartbeat.
type subscribeBody struct {
	SessionID string   `json:"session_id"`
	Dir       string   `json:"dir"`
	Topics    []string `json:"topics"`
	// Port named the session's own HTTP serving port, which the listener pushed messages to.
	// It is still parsed so a registration that carries one above 0 can be refused by name
	// (subscribeHandler): the listener delivers to no port, and storing such a registration
	// would leave its session waiting for pushes nothing sends. A session receives from its own
	// consumer, or on its own NATS subscription.
	Port         int      `json:"port"`
	Title        string   `json:"title"`
	Capabilities []string `json:"capabilities"`
	// Driving reports that this process is the one actually driving the session,
	// rather than a process that merely has it loaded from shared on-disk state.
	// Nothing reads it: the claim rule that weighed it is gone with the serving port it
	// compared (session.mergeForClaim). It is stored with the session entry as the registering
	// process's own report of itself.
	Driving        bool `json:"driving"`
	SelfSubscribed bool `json:"self_subscribed"`
	// Host is envoy-client's machineID() (ENVOY_MACHINE_ID, falling back to the kernel
	// hostname): where the session process actually runs, as opposed to machineID below,
	// which is always the registering listener's own machine (unrelated to the caller). No
	// delivery reads it: an inbox message is the owner's to deliver - the listener the session's
	// stored subscription names, which this registration moves to the listener answering it
	// (store's mergeForUpsert), and any other listener leaves such a message to it silently -
	// and it reaches the session from its own consumer, relayed by that listener, or on the
	// session's own NATS subscription, wherever its process runs. Empty for a session whose
	// plugin predates this field.
	Host string `json:"host"`
	// Plugin and PluginVersion name the plugin the session registers through (its npm package
	// name) and that plugin's build, so the session list shows which build each session runs.
	// Empty for a session whose plugin predates the fields.
	Plugin        string `json:"plugin"`
	PluginVersion string `json:"plugin_version"`
	// ClientInstanceID is the id the client instance mints at its own start (one Oh My Pi Envoy
	// extension instance, one Claude Code channel-server process) and sends on every
	// registration, so a soft claim naming a predecessor session can tell that predecessor's
	// own instance from another instance's live session (session.InstanceMatches). Empty for a
	// client whose plugin predates the field, which keeps the old rule; absent here it is
	// written empty, clearing any id a previous registration stored, since the entry is built
	// fresh from this body.
	ClientInstanceID string `json:"client_instance_id"`
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
// consumer for it, consumer delivery, the plugin's epoch and the sequence that consumer starts at.
// The entry's Port is left 0: a registration naming one is refused before it reaches here
// (subscribeHandler).
func sessionEntryFromSubscribe(body subscribeBody, machineID string, delivery sessionDeliveryOutcome) session.SessionEntry {
	entry := session.SessionEntry{
		MachineID:        machineID,
		Dir:              body.Dir,
		Title:            body.Title,
		Capabilities:     append([]string{}, body.Capabilities...),
		Driving:          body.Driving,
		SelfSubscribed:   body.SelfSubscribed,
		Host:             body.Host,
		Plugin:           strings.TrimSpace(body.Plugin),
		PluginVersion:    strings.TrimSpace(body.PluginVersion),
		ClientInstanceID: strings.TrimSpace(body.ClientInstanceID),
	}
	if delivery.relayed() {
		entry.Delivery = session.DeliveryConsumer
		entry.Epoch = delivery.epoch
		entry.ConsumerStart = delivery.consumerStart
	}
	return entry
}
