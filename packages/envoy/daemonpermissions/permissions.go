// Package daemonpermissions defines the NATS subjects a Legion daemon needs from Envoy.
package daemonpermissions

import (
	"github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
)

// Permissions is one NATS user's publish and subscribe allow lists.
type Permissions struct {
	Publish, Subscribe []string
}

// Grant returns the NATS grant for one Legion daemon. Its two durable consumers inspect and create
// consumers in Envoy's notification stream, fetch their messages, and acknowledge them; the
// consumer subjects end in `>`, so the grant reaches every consumer of the stream, not only those
// two. It also receives role-lane delivery exceptions and JetStream API replies. Those replies
// arrive on `_INBOX.>`, so the daemon's seed receives every reply any client on the server gets,
// key-value reads and watch updates included, such as the listener's envoy_roles and
// envoy_sessions watchers.
//
// Each call owns its lists so a consumer can narrow a grant without changing another caller's.
func Grant() Permissions {
	return Permissions{
		Publish: []string{
			"$JS.API.STREAM.INFO." + bus.Stream,
			"$JS.API.CONSUMER.INFO." + bus.Stream + ".>",
			"$JS.API.CONSUMER.CREATE." + bus.Stream + ".>",
			"$JS.API.CONSUMER.MSG.NEXT." + bus.Stream + ".>",
			"$JS.ACK." + bus.Stream + ".>",
		},
		Subscribe: []string{
			bus.ExceptionTopicPrefix + contracts.RoleTopicPrefix + ">",
			nats.InboxPrefix + ">",
		},
	}
}
