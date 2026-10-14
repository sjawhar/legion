package core

import (
	"slices"

	"github.com/nats-io/nats.go"

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
//   - the notification stream's subjects (bus.StreamSubjects), the role lanes and every exception
//     lane;
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
	publish := append(bus.StreamSubjects(),
		contracts.RoleTopicPrefix+">",
		bus.ExceptionTopicPrefix+bus.NotificationTopicPrefix+">",
		jsAPI+"STREAM.INFO."+bus.Stream,
		jsAPI+"STREAM.CREATE."+bus.Stream,
		jsAPI+"STREAM.UPDATE."+bus.Stream,
		jsAPI+"CONSUMER.INFO."+bus.Stream+"."+durable,
		jsAPI+"CONSUMER.CREATE."+bus.Stream+"."+durable,
		jsAck+bus.Stream+"."+durable+".>",
		jsAPI+"INFO",
	)
	for _, bucket := range listenerBuckets {
		stream := kvStream + bucket
		publish = append(publish,
			jsAPI+"STREAM.INFO."+stream,
			jsAPI+"STREAM.CREATE."+stream,
			jsAPI+"DIRECT.GET."+stream+".>",
			jsAPI+"STREAM.MSG.GET."+stream,
			kvSubjects+bucket+".>",
			jsAPI+"CONSUMER.CREATE."+stream+".>",
			jsAPI+"CONSUMER.DELETE."+stream+".>",
			jsFlow+stream+".>",
		)
	}
	publish = append(publish, jsAPI+"STREAM.PURGE."+kvStream+bus.InterestBucket)
	return Permissions{
		Publish:   Rules{Allow: publish},
		Subscribe: Rules{Allow: []string{inboxes, contracts.RoleTopicPrefix + "> " + bus.ListenerQueue(machineID)}},
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
