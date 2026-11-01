package bus

import (
	"errors"
	"fmt"
	"log/slog"

	"github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/contracts"
)

// The role lane's pending limits: what nats.go queues for the lane's subscription before it drops
// a message as a slow consumer. They are finite, so a publisher flood on notifications.role.> can
// never grow the client's buffer without bound, and far above any backlog the lane's handler can
// build: the handler hands each forward over and blocks only once 4,096 are open
// (cmd/listener/role_lane.go roleForwardWaitLimit), and every open forward ends within its receipt
// window of 2 seconds, so at the measured 40 role messages a second a blocked handler leaves about
// 80 messages queued, and the 400-message silent-holder test never blocks it at all. Reaching
// RoleLanePendingMsgs would take seven hours of arrivals at that rate with the handler blocked
// throughout; RoleLanePendingBytes, 1 GiB, is the same bound in memory for messages larger than a
// kilobyte. Client.SlowConsumers counts any slow-consumer error the connection reports.
const (
	RoleLanePendingMsgs  = 1_000_000
	RoleLanePendingBytes = 1 << 30
)

// errRoleLaneRefused is wrapped by openRoleLane's error when the server refused the role-lane
// subscription in the queue group it asked for.
var errRoleLaneRefused = errors.New("bus: NATS refused the role-lane subscription")

// ConnectRoleLane opens the listener's role lane: a Client of its own, named "envoy-role-lane"
// unless options name it, holding one core subscription to every role lane
// (contracts.RoleTopicPrefix + ">") under the role lane's pending limits (RoleLanePendingMsgs,
// RoleLanePendingBytes), so nats.go queues every role message the server delivers however far the
// handler falls behind and drops none under any load the lane meets, while the buffer it may grow
// to stays bounded. It connects as Connect does, as the NATS user, through the proxy and under the
// remote-NATS rule the environment names, and it reconnects, re-subscribes and applies the limits
// again on its own.
//
// It subscribes in the shared queue group RoleLaneQueue, so NATS hands each role message to one
// listener of all of them. A server whose grant predates that group refuses the subscription,
// before the flush's PONG, and nats.go records the refusal as the connection's last error: the lane
// then logs it once and subscribes in its machine's group, ListenerQueue(machineID), on a new
// connection, as listeners did before. It returns the client, once NATS has accepted the
// subscription, and the queue group it subscribed in.
func ConnectRoleLane(urls []string, machineID string, handler nats.MsgHandler, options ...ConnectOption) (*Client, string, error) {
	client, err := openRoleLane(urls, RoleLaneQueue, handler, options)
	if err == nil {
		return client, RoleLaneQueue, nil
	}
	if !errors.Is(err, errRoleLaneRefused) {
		return nil, "", err
	}
	queue := ListenerQueue(machineID)
	slog.Warn("envoy role lane: the NATS user may not subscribe in the shared queue group; subscribing in the machine's",
		slog.String("refused_queue", RoleLaneQueue),
		slog.String("queue", queue),
		slog.String("error", err.Error()))
	if client, err = openRoleLane(urls, queue, handler, options); err != nil {
		return nil, "", err
	}
	return client, queue, nil
}

// openRoleLane connects a role-lane client and subscribes it in queue (ConnectRoleLane). A refusal
// of that very subscription wraps errRoleLaneRefused; any other error the connection recorded by
// the flush is returned as it is. The client is closed on every error.
func openRoleLane(urls []string, queue string, handler nats.MsgHandler, options []ConnectOption) (*Client, error) {
	client, err := Connect(urls, append([]ConnectOption{WithName("envoy-role-lane")}, options...)...)
	if err != nil {
		return nil, fmt.Errorf("connect: %w", err)
	}
	subject := contracts.RoleTopicPrefix + ">"
	before := client.Conn.LastError()
	if _, err := client.subscribeCore(recoverableSubscription{
		transport:    coreSubscription,
		subject:      subject,
		queue:        queue,
		handler:      handler,
		pendingMsgs:  RoleLanePendingMsgs,
		pendingBytes: RoleLanePendingBytes,
	}); err != nil {
		client.Close()
		return nil, fmt.Errorf("subscribe: %w", err)
	}
	if err := client.Conn.Flush(); err != nil {
		client.Close()
		return nil, fmt.Errorf("flush: %w", err)
	}
	if after := client.Conn.LastError(); !sameError(after, before) {
		client.Close()
		if refusal, ok := readSubscriptionRefusal(after); ok && refusal.subject == subject && refusal.queue == queue {
			return nil, fmt.Errorf("%w in queue group %q: %w", errRoleLaneRefused, queue, after)
		}
		return nil, after
	}
	return client, nil
}
