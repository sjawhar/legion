package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/logging"
)

// liveTopicsSubject is what the live topics lane subscribes to: every notification topic.
const liveTopicsSubject = bus.NotificationTopicPrefix + ">"

// streamSubjectsRefreshEvery is how often the live lane reads the stream's subjects from the server
// again (liveLane.follow), beside the read at start and after each reconnect.
const streamSubjectsRefreshEvery = 5 * time.Second

// streamSubjectsReadFailedLine is the WARN the live lane logs once for each run of failed reads of
// the stream's subjects; streamSubjectsReadRecoveredLine ends the run.
const (
	streamSubjectsReadFailedLine    = "live topics cannot read the stream's subjects; the lane keeps its last list"
	streamSubjectsReadRecoveredLine = "live topics read the stream's subjects again"
)

// openLiveTopics opens the live topics lane: a bus client of its own, named
// `envoy-live-topics <instance>`, since a client holds one core subscription and the main client's
// is the role lane, subscribed to every notification
// topic in the machine's live queue group (bus.ListenerLiveQueue), apart from the role lane's
// group, which NATS would merge across subjects and hand a role message to this lane. It connects
// as the main client does, as the NATS user, through the proxy and under the remote-NATS rule the
// environment names, and it reconnects and re-subscribes on its own. It reads the stream's subjects
// before it subscribes, so its first message is already split on the server's list, and keeps them
// current from then (liveLane.follow). It returns once NATS has accepted the subscription: a server
// that refuses it answers before the flush's PONG, and nats.go records the refusal as the
// connection's last error.
func openLiveTopics(urls []string, machineID, instance string, lane *liveLane) (*bus.Client, error) {
	live, err := bus.Connect(urls, bus.WithName("envoy-live-topics "+instance))
	if err != nil {
		return nil, fmt.Errorf("connect: %w", err)
	}
	lane.refresh(live.JS())
	if _, err := live.SubscribeCore(liveTopicsSubject, lane.handle, bus.ListenerLiveQueue(machineID)); err != nil {
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
	lane.follow(live)
	return live, nil
}

// liveLane is the live lane's handler (liveDeliveryHandler) and its view of the subjects the
// stream stores, which it leaves to the durable.
type liveLane struct {
	deliver func(deliveryMessage)
	logger  *logging.Logger
	// refreshEvery is how often follow reads the stream's subjects again: streamSubjectsRefreshEvery,
	// which a test shortens.
	refreshEvery time.Duration
	// subjects is the stream's subject list as the server last answered it, nil until a read
	// succeeds.
	subjects atomic.Pointer[[]string]
	// failing is set by a failed read and cleared by the next that succeeds, so a run of failures
	// logs once.
	failing  atomic.Bool
	stop     chan struct{}
	stopOnce sync.Once
}

// liveDeliveryHandler is the live lane's handler, over every notification topic (openLiveTopics).
// It leaves alone a subject another lane owns, read before anything is decoded
// and with no line logged, since the live lane receives each of those messages too: a role lane
// (the role lane arbitrates it) and a subject the stream stores (the durable delivers it, and a
// role forward travels on its holder's agent subject, which the stream stores, while keeping its
// role topic). Every other message goes through the delivery handler, whose fanout delivers it to
// every session on this machine subscribed to its topic.
//
// What the stream stores is the subject list the server holds now, not the one this build
// compiles in (streamCaptures): another build's start may have added subjects this build does not
// know, which the durable, filtering nothing, delivers.
func liveDeliveryHandler(cfg listenerDeliveryHandlerConfig) *liveLane {
	handler := listenerDeliveryHandler(cfg)
	return &liveLane{
		deliver:      handler,
		logger:       cfg.logger,
		refreshEvery: streamSubjectsRefreshEvery,
		stop:         make(chan struct{}),
	}
}

func (l *liveLane) handle(msg *nats.Msg) {
	if strings.HasPrefix(msg.Subject, contracts.RoleTopicPrefix) || l.streamCaptures(msg.Subject) {
		return
	}
	l.deliver(deliveryMessage{data: msg.Data, mode: liveDelivery, msg: msg})
}

// streamCaptures reports whether the stream stores a publish to topic, by the subject list last
// read from the server. Before any read has succeeded it answers by the subjects this build
// compiles in, which the stream carries from this build's start on (ensureStreamWithConfig), so
// the lane delivers nothing the durable also delivers. A list that has gone stale can only lack a
// subject added since, as the stream's subjects only grow across builds: the lane then also
// delivers that subject for at most one refresh, and the delivery ledger keeps the session from a
// second prompt.
func (l *liveLane) streamCaptures(topic string) bool {
	if subjects := l.subjects.Load(); subjects != nil {
		return bus.SubjectsCapture(*subjects, topic)
	}
	return bus.StreamCaptures(topic)
}

// refresh reads the stream's subjects from the server. A failed read keeps the list the lane holds
// and logs a WARN, once for each run of failures.
func (l *liveLane) refresh(js nats.JetStreamContext) {
	subjects, err := bus.ReadStreamSubjects(js)
	if err != nil {
		if !l.failing.Swap(true) {
			l.logger.Warn(streamSubjectsReadFailedLine, slog.String("stream", bus.Stream), slog.String("error", err.Error()))
		}
		return
	}
	l.subjects.Store(&subjects)
	if l.failing.Swap(false) {
		l.logger.Info(streamSubjectsReadRecoveredLine, slog.String("stream", bus.Stream))
	}
}

// follow keeps the lane's list current on live, the lane's client: it reads the subjects again
// every refreshEvery and after each reconnect, until live drains.
func (l *liveLane) follow(live *bus.Client) {
	live.AddReconnectHook(func(*nats.Conn) error {
		l.refresh(live.JS())
		return nil
	})
	live.AddDrainHook(func(context.Context) { l.stopOnce.Do(func() { close(l.stop) }) })
	go func() {
		ticker := time.NewTicker(l.refreshEvery)
		defer ticker.Stop()
		for {
			select {
			case <-l.stop:
				return
			case <-ticker.C:
				l.refresh(live.JS())
			}
		}
	}()
}
