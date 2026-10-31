package core

import (
	"slices"
	"strings"

	"github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/daemonpermissions"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
)

// Permissions are one NATS user's publish and subscribe rules, as a User takes them.
type Permissions struct {
	Publish, Subscribe Rules
}

// The subject spaces of NATS itself that nats.go sends JetStream and KV calls in. nats.go exports
// no name for them.
const (
	jsAPI      = "$JS.API." // the JetStream API
	jsAck      = "$JS.ACK." // a consumer's acknowledgements
	jsFlow     = "$JS.FC."  // an ordered consumer's flow-control replies
	kvStream   = "KV_"      // the stream behind a KV bucket, KV_<bucket>
	kvSubjects = "$KV."     // a KV bucket's keys, $KV.<bucket>.<key>
)

// inboxes matches every reply inbox nats.go makes.
const inboxes = nats.InboxPrefix + ">"

// listenerGrant is the grant every user that runs envoy-listener holds as machine machineID, built
// from Envoy's own names: ListenerPermissions adds the session consumers' calls to it
// (sessionConsumerCalls), ListenerHostPermissions those and the subscribe to every notification
// topic, DispatchPermissions what Dispatch itself needs, and HostPermissions the session's
// entries. Publish, in this order:
//   - every notification topic (bus.NotificationTopicPrefix): the stream's subjects, the role
//     lanes, every exception lane, the session relay subjects (bus.DeliverSubjectPrefix), and any
//     other topic the listener publishes over core NATS, a role lane's forward among them
//     (/v1/messages/publish refuses by name a notifications. topic no stream subject captures);
//   - the stream's info, create and update, its stored-message get, its durable's
//     (bus.ListenerDurable) info and create, the durable's acknowledgements, and JetStream's
//     account info. The stored-message get reads one stored message at a time and can create,
//     change or delete nothing; SessionPermissions does not hold it, so no session reads the
//     stream's stored messages itself and each takes its own from the consumer its listener
//     relays;
//   - for each bucket of bus.ListenerBuckets in turn (the buckets the listener's keeper raises),
//     the calls nats.go's KV makes and the raise a start sends (bucketPermissions): the bucket's
//     stream info, create and update, direct and stream message gets, writes to its keys, its
//     watchers' consumer create and delete, and their flow-control replies;
//   - the purge of the interest bucket, which its delete-marker collection sends.
//
// Subscribe: reply inboxes, then the role lanes in the machine's queue group (bus.ListenerQueue),
// which a listener of an earlier release subscribes in, and in the queue group every listener
// shares (bus.RoleLaneQueue). Every user built on this grant runs a listener that subscribes a
// role lane, Dispatch's user included (the envoy-listener beside envoy-dispatch), so each holds
// both groups.
func listenerGrant(machineID string) Permissions {
	durable := bus.ListenerDurable(machineID)
	publish := []string{
		bus.NotificationTopicPrefix + ">",
		jsAPI + "STREAM.INFO." + bus.Stream,
		jsAPI + "STREAM.CREATE." + bus.Stream,
		jsAPI + "STREAM.UPDATE." + bus.Stream,
		jsAPI + "STREAM.MSG.GET." + bus.Stream,
		jsAPI + "CONSUMER.INFO." + bus.Stream + "." + durable,
		jsAPI + "CONSUMER.CREATE." + bus.Stream + "." + durable,
		jsAck + bus.Stream + "." + durable + ".>",
		jsAPI + "INFO",
	}
	for _, bucket := range bus.ListenerBuckets() {
		publish = append(publish, bucketPermissions(bucket)...)
	}
	publish = append(publish, jsAPI+"STREAM.PURGE."+kvStream+bus.InterestBucket)
	return Permissions{
		Publish: Rules{Allow: publish},
		Subscribe: Rules{Allow: []string{
			inboxes,
			contracts.RoleTopicPrefix + "> " + bus.ListenerQueue(machineID),
			contracts.RoleTopicPrefix + "> " + bus.RoleLaneQueue,
		}},
	}
}

// sessionConsumerCalls are the calls bus.SessionConsumers makes, which only the sets of a user no
// session shares hold (ListenerPermissions and ListenerHostPermissions): consumer create, with and
// without a filter subject in its subject, info, delete, pull (CONSUMER.MSG.NEXT) and
// acknowledgements, each on any consumer of the stream, since a session consumer is named for its
// session (bus.SessionConsumerName), and the stream's consumer list, which a listener that starts
// relaying every session at once reads instead of one info each
// (bus.SessionConsumers.ListSessionConsumers). A NATS wildcard is a whole token, so these reach
// every consumer of the stream, other listeners' durables and Dispatch's included, and a consumer
// created through them can deliver the stream's messages to any subject. That reach is the
// listener user's alone: DispatchPermissions and HostPermissions keep their per-durable scope and
// hold none of these, so the listener those users run answers every registration that asks for
// consumer delivery core (bus.ErrConsumerNotGranted) and keeps only its own durable.
func sessionConsumerCalls() []string {
	return []string{
		jsAPI + "CONSUMER.CREATE." + bus.Stream + ".*",
		jsAPI + "CONSUMER.CREATE." + bus.Stream + ".*.>",
		jsAPI + "CONSUMER.INFO." + bus.Stream + ".*",
		jsAPI + "CONSUMER.DELETE." + bus.Stream + ".*",
		jsAPI + "CONSUMER.MSG.NEXT." + bus.Stream + ".*",
		jsAPI + "CONSUMER.LIST." + bus.Stream,
		jsAck + bus.Stream + ".*.>",
	}
}

// ListenerPermissions is the grant envoy-listener needs as machine machineID: the listener grant
// (listenerGrant), then the session consumers' calls (sessionConsumerCalls), which only this set
// and ListenerHostPermissions hold. A hub that renders this set for its own listener gives that
// listener consumer delivery; a listener connecting as a host or Dispatch user does not have it,
// and its sessions stay on the core path.
func ListenerPermissions(machineID string) Permissions {
	grant := listenerGrant(machineID)
	grant.Publish.Allow = append(grant.Publish.Allow, sessionConsumerCalls()...)
	return grant
}

// ListenerHostPermissions is the grant of a listener's own NATS user, one no session shares, as
// machine machineID: the listener's set (ListenerPermissions), the session consumers' calls
// included, then the subscribe to every notification topic (bus.NotificationTopicPrefix) that
// ENVOY_SUBSCRIBE_ALL_TOPICS needs, which the role-lane entries of listenerGrant do not cover: a
// listener opted in holds a core subscription on notifications.> in its machine's queue group, so
// a session it serves receives a topic the stream does not store. That subscribe is all this set
// adds to the listener's: it holds none of SessionPermissions' publish entries, so a process
// connecting as this user can neither publish a session's agent stream frames nor subscribe to a
// session's control subject. HostPermissions stays the set of a user a host's sessions share.
func ListenerHostPermissions(machineID string) Permissions {
	grant := ListenerPermissions(machineID)
	grant.Subscribe.Allow = append(grant.Subscribe.Allow, bus.NotificationTopicPrefix+">")
	return grant
}

// bucketPermissions are the calls nats.go's KV makes on one bucket, in the order the grants list
// them: listenerGrant lists them for each of bus.ListenerBuckets, DispatchPermissions for the
// redelivery bucket. Beside them is the update of the bucket's stream: a start, and the program's
// keeper every 30 seconds after it, raises a bucket found with fewer copies than the deployment's
// replica count (bus.ReplicasEnvVar) with a stream update (bus.EnsureKeyValue,
// bus.WithKeptBuckets). Without that grant the raise times out and the start stops naming the
// bucket.
func bucketPermissions(bucket string) []string {
	stream := kvStream + bucket
	return []string{
		jsAPI + "STREAM.INFO." + stream,
		jsAPI + "STREAM.CREATE." + stream,
		jsAPI + "STREAM.UPDATE." + stream,
		jsAPI + "DIRECT.GET." + stream + ".>",
		jsAPI + "STREAM.MSG.GET." + stream,
		kvSubjects + bucket + ".>",
		jsAPI + "CONSUMER.CREATE." + stream + ".>",
		jsAPI + "CONSUMER.DELETE." + stream + ".>",
		jsFlow + stream + ".>",
	}
}

// DispatchDurables are the fixed durable consumers of the stream that Dispatch's grant lets it
// bind, beside the listener durable of its machine id. Two users holding DispatchPermissions would
// bind the same ones.
func DispatchDurables() []string {
	return []string{bus.DispatchDeliveryDurable, bus.DispatchJobDurable}
}

// DispatchPermissions is the grant of Dispatch's one NATS user, which envoy-dispatch and the
// envoy-listener beside it share: the listener grant (listenerGrant) of the listener's machine id,
// then what Dispatch itself needs. It holds no session-consumer call (sessionConsumerCalls), so
// the listener beside envoy-dispatch keeps its own durable alone and answers every registration
// that asks for consumer delivery core. No session connects as this user either, so it holds none
// of SessionPermissions: it is a direct publish to a session's frames, or a subscription to its
// control subject, that is refused. The seed's reach is the whole server even so: its _INBOX.>
// subscription receives every reply any client gets, other listeners' durable push deliveries
// included; the filtered create on delivery-events lets it create that durable on any filter, so
// it can read any retained notification; and a consumer it creates can deliver any payload onto a
// session's frames, since NATS checks no push consumer's deliver subject. Publish, in this
// order: the stream calls and acknowledgements of each durable Dispatch binds, the KV calls for its
// webhook-redelivery bucket, and every session's agent stream control subject. Subscribe: every
// session's agent stream frames.
func DispatchPermissions(machineID string) Permissions {
	listener := listenerGrant(machineID)
	publish := listener.Publish.Allow
	for _, durable := range DispatchDurables() {
		publish = append(publish,
			jsAPI+"CONSUMER.INFO."+bus.Stream+"."+durable,
			jsAPI+"CONSUMER.CREATE."+bus.Stream+"."+durable+".>",
			jsAPI+"CONSUMER.DELETE."+bus.Stream+"."+durable,
			jsAck+bus.Stream+"."+durable+".>",
		)
	}
	publish = append(publish, bucketPermissions(bus.WebhookRedeliveryBucket)...)
	publish = append(publish, contracts.AgentStreamControlSubject("*"))
	return Permissions{
		Publish:   Rules{Allow: publish},
		Subscribe: Rules{Allow: append(listener.Subscribe.Allow, contracts.AgentStreamFramesSubject("*"))},
	}
}

// SessionPermissions is the grant any agent session needs, with * for the session id. Publish:
// reply inboxes (the receipt a role delivery waits for, and the receipt of each relay frame),
// then its agent stream's frames (contracts.AgentStreamFramesSubject). Subscribe: its own relay
// subject (bus.DeliverSubject), the subject the listener that owns the session publishes every
// message of the session's consumer (bus.SessionConsumerName) on, then its agent stream's control
// subject (contracts.AgentStreamControlSubject). A session subscribes to no notification topic:
// it receives what its consumer delivers, on that one subject, and nothing else.
//
// A Legion controller session needs nothing added for its project's notice lane
// (notifications.legion.<project>.controller, the daemon's notify.ControllerTopic): it registers
// that lane as an interest while it holds the project's controller role
// (bus.ReachesPrivateStreamSubject), and the listener's relay hands it the notices on the relay
// subject above, as it hands it every other message. No session subscribes to the lane itself,
// which this set rightly refuses.
//
// What this set does not solve: every session on a shared user holds the same set, so the relay
// subject carries * and one session can read another's relay frames until the per-session
// credentials of dispatch://INFRA-840. dispatch://INFRA-778 is what hands a deployment's sessions
// this grant; nothing here does.
func SessionPermissions() Permissions {
	return Permissions{
		Publish:   Rules{Allow: []string{inboxes, contracts.AgentStreamFramesSubject("*")}},
		Subscribe: Rules{Allow: []string{bus.DeliverSubject("*"), contracts.AgentStreamControlSubject("*")}},
	}
}

// DaemonPermissions is the NATS grant one Legion daemon needs. The grant itself lives with
// Envoy's subject names so the daemon's real-NATS proof and the hub's rendered user share it.
func DaemonPermissions() Permissions {
	grant := daemonpermissions.Grant()
	return Permissions{
		Publish:   Rules{Allow: grant.Publish},
		Subscribe: Rules{Allow: grant.Subscribe},
	}
}

// HostPermissions is the grant of a host's one NATS user, which its listener and every session on
// the host share: in each direction the listener grant (listenerGrant), then each entry of the
// session's it lacks, and in the subscribe direction the plain notifications.> entry this set
// names itself. It holds no session-consumer call (sessionConsumerCalls), so a listener
// connecting as a host user keeps its own durable alone and can give no session a consumer. A
// listener whose user no session shares takes ListenerHostPermissions instead, which holds those
// calls beside the same notifications.> subscribe.
//
// That subscribe is this set's own, not one it takes from the session's: SessionPermissions holds
// it no longer, and a listener run with ENVOY_SUBSCRIBE_ALL_TOPICS=1, which a host user may be,
// is refused the subscription without it and ends its start. Naming it here also leaves the
// sessions sharing a host user the reach they have today, which the per-session credentials of
// dispatch://INFRA-840 take away.
func HostPermissions(machineID string) Permissions {
	listener, agent := listenerGrant(machineID), SessionPermissions()
	// notifications.> admits every notification subject the session's set names, its relay subject
	// among them, so only the session entries outside that family are added: a second entry for
	// subjects already allowed would re-render every deployed host user, and a hub's users are
	// rendered into the VM's cloud-config, which changes only by replacing the VM.
	subscribe := union(listener.Subscribe.Allow, []string{bus.NotificationTopicPrefix + ">"})
	outside := slices.DeleteFunc(agent.Subscribe.Allow, func(entry string) bool {
		return strings.HasPrefix(entry, bus.NotificationTopicPrefix)
	})
	return Permissions{
		Publish:   Rules{Allow: union(listener.Publish.Allow, agent.Publish.Allow)},
		Subscribe: Rules{Allow: union(subscribe, outside)},
	}
}

// union is a followed by each entry of b it does not hold yet.
func union(a, b []string) []string {
	for _, entry := range b {
		if !slices.Contains(a, entry) {
			a = append(a, entry)
		}
	}
	return a
}
