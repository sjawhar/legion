package main

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
)

// The simulated sessions: on NATS, each subscribes its agent topic and its watched issues'
// topics; on the listener, each registers and sends its heartbeat. Under consumer delivery
// (-session-delivery consumer) each also subscribes its relay subject first and asks the listener
// for consumer delivery (relay.go).

// natsSessions is the sessions' NATS side: the shared connections and their subscriptions, every
// receipt, and one sample envelope per event type.
type natsSessions struct {
	conns     []*nats.Conn
	subs      int
	receipts  *collector[receipt]
	envelopes envelopeSamples
	// relay is the consumer-delivery side: nil on the core path (-session-delivery core).
	relay *relaySessions
	// pulls counts the pull requests on the stream's consumers (pullSpy); nil when the spy could
	// not subscribe.
	pulls *pullSpy
}

// subscribeSessions connects natsConns connections the sessions share; each session subscribes
// its own agent topic and its watched issues' topics, as the pi plugin's self-subscription does.
// Under consumer delivery each session subscribes its relay subject before those, as the plugin
// does before it registers. The connections it made are closed by close, also when it fails.
func (st *step) subscribeSessions() (*natsSessions, error) {
	ns := &natsSessions{receipts: newCollector[receipt](1 << 16), envelopes: envelopeSamples{data: map[string][]byte{}, marked: map[string]bool{}}}
	for i := range st.cfg.natsConns {
		nc, err := nats.Connect(st.cfg.ep.nats, nats.Name(fmt.Sprintf("loadgen-%d", i)), nats.MaxReconnects(-1))
		if err != nil {
			return ns, fmt.Errorf("nats connect: %w", err)
		}
		ns.conns = append(ns.conns, nc)
	}
	if spy, err := startPullSpy(st.cfg.ep.nats); err != nil {
		st.lg.Printf("pull spy: %v; the pull-request rate stays empty", err)
	} else {
		ns.pulls = spy
	}
	if st.cfg.sessionDelivery == sessionDeliveryConsumer {
		ns.relay = newRelaySessions(len(st.sessions), len(ns.conns))
	}
	for i, s := range st.sessions {
		nc := ns.conns[i%len(ns.conns)]
		if ns.relay != nil {
			if _, err := nc.Subscribe(bus.DeliverSubject(s.id), ns.relay.handler(ns, i, contracts.AgentSubject(s.id), st.nonce)); err != nil {
				return ns, err
			}
			ns.subs++
		}
		if _, err := nc.Subscribe(contracts.AgentSubject(s.id), ns.handler(i, true, st.nonce)); err != nil {
			return ns, err
		}
		ns.subs++
		for _, w := range s.watched {
			if _, err := nc.Subscribe(st.issueTopic(w), ns.handler(i, false, st.nonce)); err != nil {
				return ns, err
			}
			ns.subs++
		}
	}
	for _, nc := range ns.conns {
		_ = nc.Flush()
	}
	st.lg.Printf("nats: %d subscriptions on %d connections (session delivery %s)", ns.subs, len(ns.conns), st.cfg.sessionDelivery)
	return ns, nil
}

// handler records each envelope as a receipt of the event id in its payload, on an agent topic
// or an issue topic, and offers it as its event type's sample envelope. For a session the listener
// relays (its last registration answered consumer), the relay frame is the receipt, as in the pi
// plugin: the core copy only answers its lane receipt, on the agent topic, and is not counted.
func (ns *natsSessions) handler(session int, agent bool, nonce int64) nats.MsgHandler {
	return func(m *nats.Msg) {
		now := time.Now().UnixNano()
		if ns.relay != nil && ns.relay.relayed(session) {
			ns.relay.coreCopy(m, agent)
			return
		}
		var env contracts.Envelope
		if json.Unmarshal(m.Data, &env) != nil {
			return
		}
		var ev struct {
			ID   int64  `json:"id"`
			Type string `json:"type"`
		}
		if json.Unmarshal([]byte(env.Payload), &ev) != nil {
			return
		}
		// A payload with no event type is a listener send (a mention), not a Dispatch event.
		key := cmp.Or(ev.Type, "send")
		if agent {
			key = "agent." + key
		}
		ns.envelopes.offer(key, m.Data, nonce)
		ns.receipts.add(receipt{EventID: ev.ID, Recv: now, Agent: agent})
	}
}

func (ns *natsSessions) close() {
	for _, nc := range ns.conns {
		nc.Close()
	}
	if ns.pulls != nil {
		ns.pulls.close()
	}
}

// envelopeSamples keeps one envelope per event type (agent topics apart), for envelope-*.json.
type envelopeSamples struct {
	mu     sync.Mutex
	data   map[string][]byte
	marked map[string]bool // whether each kept envelope carries the run's marker
}

// offer keeps the first envelope of key, then replaces it once with one that carries the run's
// marker.
func (e *envelopeSamples) offer(key string, data []byte, nonce int64) {
	e.mu.Lock()
	defer e.mu.Unlock()
	_, seen := e.data[key]
	if seen && e.marked[key] {
		return
	}
	_, marked := findMarker(data, nonce)
	if seen && !marked {
		return
	}
	e.data[key] = bytes.Clone(data)
	e.marked[key] = marked
}

// write saves each kept envelope as envelope-<key>.json in dir.
func (e *envelopeSamples) write(dir string, lg *log.Logger) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for key, data := range e.data {
		name := "envelope-" + strings.Map(func(r rune) rune {
			if r == '.' || r == '-' || r == '_' || (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
				return r
			}
			return '_'
		}, key) + ".json"
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o644); err != nil {
			lg.Printf("write %s: %v", name, err)
		}
	}
}

// registrar registers the sessions with the listener as the pi plugin does. Each session's body
// is the same at every heartbeat, but for its delivery field under consumer delivery, which
// carries the session's current resume point.
type registrar struct {
	url, token    string
	client        *http.Client
	bodies        []map[string]any
	ok, errs      atomic.Int64 // the first registration of each session
	heartbeatErrs atomic.Int64
	// relay hears each answer's delivery mode, under consumer delivery; nil on the core path.
	relay *relaySessions
}

// registerAnswer is the part of a registration's answer the registrar reads: the delivery mode
// and stream (a missing field means core).
type registerAnswer struct {
	Delivery *struct {
		Mode     string `json:"mode"`
		Reason   string `json:"reason"`
		StreamID string `json:"stream_id"`
	} `json:"delivery"`
}

func (r *registrar) register(ctx context.Context, i int) error {
	body := r.bodies[i]
	if r.relay != nil {
		body = maps.Clone(body)
		body["delivery"] = r.relay.delivery(i)
	}
	data, _ := json.Marshal(body)
	req, _ := http.NewRequestWithContext(ctx, "POST", r.url+"/v1/interests/subscribe", bytes.NewReader(data))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+r.token)
	resp, err := r.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("%d %s", resp.StatusCode, trunc(b))
	}
	if r.relay != nil {
		var ans registerAnswer
		mode, reason, streamID := sessionDeliveryCore, "not_answered", ""
		if json.Unmarshal(b, &ans) != nil {
			reason = "unreadable_answer"
		} else if ans.Delivery != nil {
			mode, reason, streamID = ans.Delivery.Mode, ans.Delivery.Reason, ans.Delivery.StreamID
		}
		r.relay.answered(i, mode, reason, streamID)
	}
	return nil
}

// registrationFields is session s's registration with the listener: its directory, title and
// the topics of the issues it watches. With -mention-frac, it advertises the default mention
// delivery, so Dispatch delivers a mention of the session. registerSessions adds a delivery field
// under consumer delivery (registrar.register).
func (st *step) registrationFields(s session) map[string]any {
	topics := []string{}
	for _, w := range s.watched {
		topics = append(topics, st.issueTopic(w))
	}
	capabilities := []string{}
	if st.cfg.mentionFrac > 0 {
		capabilities = []string{mentionDelivery}
	}
	return map[string]any{"session_id": s.id, "dir": "/work/" + s.id, "title": "load " + s.id,
		"topics": topics, "port": 0, "self_subscribed": true, "driving": false, "capabilities": capabilities}
}

// registrationBody is registrationFields encoded, as the listener receives it.
func (st *step) registrationBody(s session) []byte {
	b, _ := json.Marshal(st.registrationFields(s))
	return b
}

// registerSessions registers every session, 16 at a time. Under consumer delivery each body asks
// for consumer delivery with the epoch of the session's NATS connection and, once the session has
// one, its resume point (relaySessions.delivery).
func (st *step) registerSessions(ctx context.Context, ns *natsSessions) *registrar {
	reg := &registrar{url: st.cfg.ep.listener, token: st.cfg.ep.listenerToken, client: &http.Client{Timeout: 10 * time.Second},
		bodies: make([]map[string]any, len(st.sessions)), relay: ns.relay}
	for i, s := range st.sessions {
		reg.bodies[i] = st.registrationFields(s)
	}
	var wg sync.WaitGroup
	sem := make(chan struct{}, 16)
	for i, s := range st.sessions {
		sem <- struct{}{}
		wg.Go(func() {
			defer func() { <-sem }()
			if err := reg.register(ctx, i); err != nil {
				if reg.errs.Add(1) <= 5 {
					st.lg.Printf("register %s: %v", s.id, err)
				}
				return
			}
			reg.ok.Add(1)
		})
	}
	wg.Wait()
	st.lg.Printf("registered %d sessions (%d errors)", reg.ok.Load(), reg.errs.Load())
	if ns.relay != nil {
		d := ns.relay.summary()
		st.lg.Printf("session delivery: %d sessions on consumer delivery, %d on core %v", d.ConsumerSessions, d.CoreSessions, d.CoreReasons)
	}
	return reg
}

// startHeartbeat re-registers one session at a time, so that each one's 90 s heartbeat is spread
// evenly (packages/envoy-client/src/envoy-settings.ts), until ctx ends.
func (st *step) startHeartbeat(ctx context.Context, reg *registrar) {
	go func() {
		t := time.NewTicker(90 * time.Second / time.Duration(len(st.sessions)))
		defer t.Stop()
		for i := 0; ; i++ {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				go func(i int) {
					if err := reg.register(ctx, i); err != nil && ctx.Err() == nil {
						if reg.heartbeatErrs.Add(1) <= 5 {
							st.lg.Printf("heartbeat %s: %v", st.sessions[i].id, err)
						}
					}
				}(i % len(st.sessions))
			}
		}
	}()
}
