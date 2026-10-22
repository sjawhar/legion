package core

import (
	"slices"

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
	jsAck      = "$JS.ACK." // a push consumer's acknowledgements
	jsFlow     = "$JS.FC."  // an ordered consumer's flow-control replies
	kvStream   = "KV_"      // the stream behind a KV bucket, KV_<bucket>
	kvSubjects = "$KV."     // a KV bucket's keys, $KV.<bucket>.<key>
)

// inboxes matches every reply inbox nats.go makes.
const inboxes = nats.InboxPrefix + ">"

// listenerBuckets are the KV buckets a listener opens, in the order its grant lists them: the
// interest and role-claim buckets, the session registry, and the CI state bucket, which only a
// listener with the GitHub webhook route opens.
var listenerBuckets = []string{bus.InterestBucket, bus.RoleClaimBucket, bus.SessionBucket, bus.CIStateBucket}

// ListenerPermissions is the grant envoy-listener needs as machine machineID, built from Envoy's own
// names. Publish, in this order:
//   - every notification topic (bus.NotificationTopicPrefix): the stream's subjects, the role
//     lanes, every exception lane, and any other topic a caller of /v1/messages/publish names,
//     which the listener publishes over core NATS (an ad-hoc topic such as
//     notifications.team.deploy-squad);
//   - the stream's info, create and update, its durable's (bus.ListenerDurable) info and create,
//     the durable's acknowledgements, and JetStream's account info;
//   - for each bucket of listenerBuckets in turn, the calls nats.go's KV makes: the bucket's stream
//     info and create, direct and stream message gets, writes to its keys, its watchers' consumer
//     create and delete, and their flow-control replies;
//   - the purge of the interest bucket, which its delete-marker collection sends.
//
// Subscribe: reply inboxes, then the role lanes in the machine's queue group (bus.ListenerQueue).
func ListenerPermissions(machineID string) Permissions {
	durable := bus.ListenerDurable(machineID)
	publish := []string{
		bus.NotificationTopicPrefix + ">",
		jsAPI + "STREAM.INFO." + bus.Stream,
		jsAPI + "STREAM.CREATE." + bus.Stream,
		jsAPI + "STREAM.UPDATE." + bus.Stream,
		jsAPI + "CONSUMER.INFO." + bus.Stream + "." + durable,
		jsAPI + "CONSUMER.CREATE." + bus.Stream + "." + durable,
		jsAck + bus.Stream + "." + durable + ".>",
		jsAPI + "INFO",
	}
	for _, bucket := range listenerBuckets {
		publish = append(publish, bucketPermissions(bucket)...)
	}
	publish = append(publish, jsAPI+"STREAM.PURGE."+kvStream+bus.InterestBucket)
	return Permissions{
		Publish:   Rules{Allow: publish},
		Subscribe: Rules{Allow: []string{inboxes, contracts.RoleTopicPrefix + "> " + bus.ListenerQueue(machineID)}},
	}
}

// bucketPermissions are the calls nats.go's KV makes on one bucket, in the order the grants list
// them: ListenerPermissions lists them for each of listenerBuckets, DispatchPermissions for the
// redelivery bucket.
func bucketPermissions(bucket string) []string {
	stream := kvStream + bucket
	return []string{
		jsAPI + "STREAM.INFO." + stream,
		jsAPI + "STREAM.CREATE." + stream,
		jsAPI + "DIRECT.GET." + stream + ".>",
		jsAPI + "STREAM.MSG.GET." + stream,
		kvSubjects + bucket + ".>",
		jsAPI + "CONSUMER.CREATE." + stream + ".>",
		jsAPI + "CONSUMER.DELETE." + stream + ".>",
		jsFlow + stream + ".>",
	}
}

// DispatchPermissions is the grant of Dispatch's one NATS user, which envoy-dispatch and the
// envoy-listener beside it share: HostPermissions(machineID), the listener's machine id, then what
// Dispatch itself needs. Publish, in this order: the stream calls and acknowledgements of each
// durable Dispatch binds, the KV calls for its webhook-redelivery bucket, and every session's agent
// stream control subject. Subscribe: every session's agent stream frames.
func DispatchPermissions(machineID string) Permissions {
	host := HostPermissions(machineID)
	publish := host.Publish.Allow
	for _, durable := range []string{bus.DispatchDeliveryDurable, bus.DispatchJobDurable} {
		publish = append(publish,
			jsAPI+"CONSUMER.INFO."+bus.Stream+"."+durable,
			jsAPI+"CONSUMER.CREATE."+bus.Stream+"."+durable+".>",
			jsAPI+"CONSUMER.DELETE."+bus.Stream+"."+durable,
			jsAck+bus.Stream+"."+durable+".>",
		)
	}
	publish = append(publish,
		bucketPermissions(bus.WebhookRedeliveryBucket)...,
	)
	publish = append(publish, contracts.AgentStreamControlSubject("*"))
	return Permissions{
		Publish:   Rules{Allow: publish},
		Subscribe: Rules{Allow: append(host.Subscribe.Allow, contracts.AgentStreamFramesSubject("*"))},
	}
}

// SessionPermissions is the grant any agent session needs, with * for the session id. Publish:
// reply inboxes (the receipt a role delivery waits for), then its agent stream's frames
// (contracts.AgentStreamFramesSubject). Subscribe: every notification topic, then its agent
// stream's control subject (contracts.AgentStreamControlSubject).
func SessionPermissions() Permissions {
	return Permissions{
		Publish:   Rules{Allow: []string{inboxes, contracts.AgentStreamFramesSubject("*")}},
		Subscribe: Rules{Allow: []string{bus.NotificationTopicPrefix + ">", contracts.AgentStreamControlSubject("*")}},
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
// the host share: in each direction the listener's set, then each entry of the session's it lacks.
func HostPermissions(machineID string) Permissions {
	listener, agent := ListenerPermissions(machineID), SessionPermissions()
	return Permissions{
		Publish:   Rules{Allow: union(listener.Publish.Allow, agent.Publish.Allow)},
		Subscribe: Rules{Allow: union(listener.Subscribe.Allow, agent.Subscribe.Allow)},
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
