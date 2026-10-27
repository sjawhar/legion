package main

import (
	"errors"
	"fmt"
	"strings"

	"github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
)

// liveTopicsSubject is what the live topics lane subscribes to: every notification topic.
const liveTopicsSubject = bus.NotificationTopicPrefix + ">"

// openLiveTopics opens the live topics lane: a bus client of its own, named
// `envoy-live-topics <instance>`, since a client holds one core subscription and the main client's
// is the role lane, subscribed to every notification
// topic in the machine's live queue group (bus.ListenerLiveQueue), apart from the role lane's
// group, which NATS would merge across subjects and hand a role message to this lane. It connects
// as the main client does, as the NATS user, through the proxy and under the remote-NATS rule the
// environment names, and it reconnects and re-subscribes on its own. It returns once NATS has
// accepted the subscription: a server that refuses it answers before the flush's PONG, and
// nats.go records the refusal as the connection's last error.
func openLiveTopics(urls []string, machineID, instance string, handler nats.MsgHandler) (*bus.Client, error) {
	live, err := bus.Connect(urls, bus.WithName("envoy-live-topics "+instance))
	if err != nil {
		return nil, fmt.Errorf("connect: %w", err)
	}
	if _, err := live.SubscribeCore(liveTopicsSubject, handler, bus.ListenerLiveQueue(machineID)); err != nil {
		live.Close()
		return nil, fmt.Errorf("subscribe: %w", err)
	}
	if err := live.Conn.Flush(); err != nil {
		live.Close()
		return nil, fmt.Errorf("flush: %w", err)
	}
	if err := live.Conn.LastError(); errors.Is(err, nats.ErrPermissionViolation) {
		live.Close()
		return nil, err
	}
	return live, nil
}

// liveDeliveryHandler is the live lane's handler, over every notification topic (openLiveTopics).
// It leaves alone a subject another lane owns, read before anything is decoded
// and with no line logged, since the live lane receives each of those messages too: a role lane
// (the role lane arbitrates it) and a subject the stream stores (the durable delivers it, and a
// role forward travels on its holder's agent subject, which the stream stores, while keeping its
// role topic). Every other message goes through the delivery handler, whose fanout delivers it to
// every session on this machine subscribed to its topic.
func liveDeliveryHandler(cfg listenerDeliveryHandlerConfig) nats.MsgHandler {
	handler := listenerDeliveryHandler(cfg)
	return func(msg *nats.Msg) {
		if strings.HasPrefix(msg.Subject, contracts.RoleTopicPrefix) || bus.StreamCaptures(msg.Subject) {
			return
		}
		handler(deliveryMessage{data: msg.Data, mode: liveDelivery, msg: msg})
	}
}
