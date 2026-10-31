package main

import (
	"crypto/rand"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
)

// Consumer delivery of the simulated sessions: each session registers with `delivery: {consumer:
// true, epoch}`, gets each stored message of its subjects as a relay frame on its relay subject
// (bus.DeliverSubject), and answers each frame `received`, then `done`, on the frame's reply
// subject, as the pi plugin does (packages/envoy-client/src/consumer-delivery.ts). The frame is the
// session's receipt; its Envoy-Stored-At header, the stream's store time of the message, gives the
// delivery latency from publish. The session keeps its core subscriptions, as the plugin does: a
// core copy of a relayed subject answers its lane receipt on the agent topic and is not counted.

// sessionDeliveryConsumer is the mode the listener answers a registration it gave a consumer
// (cmd/listener/session_delivery.go deliveryModeConsumer), the only mode it answers at all: a
// registration it can give no consumer is refused, since nothing else delivers to a session.
const sessionDeliveryConsumer = "consumer"

// The headers of a relay frame and the replies to it (packages/envoy/cmd/listener/session_relay.go).
const (
	relayHeaderSubject   = "Envoy-Subject"
	relayHeaderStreamSeq = "Envoy-Stream-Seq"
	relayHeaderDelivered = "Envoy-Delivered"
	relayHeaderStoredAt  = "Envoy-Stored-At"
	relayHeaderGap       = "Envoy-Gap"
	relayHeaderStreamID  = "Envoy-Stream-Id"
)

var (
	relayReplyReceived = []byte("received")
	relayReplyDone     = []byte("done")
)

// relayFrameRec is one relay frame a session got, as relay-frames.csv lists it.
type relayFrameRec struct {
	Session   int
	Subject   string
	Seq       uint64
	Delivered int
	StoredAt  int64 // Unix ns; 0 when the header was missing or unreadable
	Recv      int64 // Unix ns
	EventID   int64 // 0 when the payload held none
	Duplicate bool  // a stream sequence the session had already received
	// Identity is the frame's dedupe identity as the plugin reads it
	// (packages/envoy-client/src/consumer-delivery.ts parseIdentity): the envelope's event_id,
	// else its dedupe_key, else "" for a frame the plugin's ledger could never recognise again.
	Identity string
	// Done is set when the session answered this frame `done`. The plugin answers done once the
	// frame is in its transcript and its ledger, so a later copy of a done-answered frame is one
	// the ledger hides (restart-report).
	Done bool
}

// relaySessions is the consumer-delivery side of the sessions: each NATS connection's epoch, each
// session's last registration answer and resume point, the stream sequences each session has
// received, and the counts of the frames and replies.
type relaySessions struct {
	epochs []string // by NATS connection
	// consumer is set for each session whose last registration answered consumer.
	consumer []atomic.Bool
	mu       sync.Mutex
	// answers holds each session's last answer: the delivery mode it named, "" before one and
	// for an answer that named no consumer delivery.
	answers []string
	// streams and written are each session's resume point, as the plugin keeps it
	// (packages/envoy-client/src/consumer-delivery.ts resumePoint): the stream the last answer or
	// frame named, and the highest sequence of it the session answered done; 0 before one.
	streams []string
	written []uint64
	// seen holds, per session, the stream sequences of the frames it received; each session's
	// relay subscription runs on one goroutine, so each map has one writer.
	seen   []map[uint64]struct{}
	frames *collector[relayFrameRec]

	modeChanges, frameCount, duplicates, redelivered, gaps, undecodable atomic.Int64
	replyErrors, coreCopies                                             atomic.Int64
}

func newRelaySessions(sessions, conns int) *relaySessions {
	r := &relaySessions{epochs: make([]string, conns), consumer: make([]atomic.Bool, sessions), answers: make([]string, sessions),
		streams: make([]string, sessions), written: make([]uint64, sessions),
		seen: make([]map[uint64]struct{}, sessions), frames: newCollector[relayFrameRec](1 << 16)}
	for i := range r.epochs {
		var b [12]byte
		_, _ = rand.Read(b[:])
		r.epochs[i] = hex.EncodeToString(b[:])
	}
	for i := range r.seen {
		r.seen[i] = map[uint64]struct{}{}
	}
	return r
}

// relayed reports whether session's last registration answered consumer.
func (r *relaySessions) relayed(session int) bool { return r.consumer[session].Load() }

// answered records session's registration answer; a consumer answer naming another stream than
// the session's resume point starts the resume point again on that stream.
func (r *relaySessions) answered(session int, mode, streamID string) {
	r.mu.Lock()
	prev := r.answers[session]
	r.answers[session] = mode
	if mode == sessionDeliveryConsumer {
		r.switchStream(session, streamID)
	}
	r.mu.Unlock()
	if prev != "" && prev != mode {
		r.modeChanges.Add(1)
	}
	r.consumer[session].Store(mode == sessionDeliveryConsumer)
}

// switchStream starts session's resume point on streamID when it names another stream. Under mu.
func (r *relaySessions) switchStream(session int, streamID string) {
	if streamID != "" && streamID != r.streams[session] {
		r.streams[session], r.written[session] = streamID, 0
	}
}

// wrote moves session's resume point past seq of streamID, once the session answered it done.
func (r *relaySessions) wrote(session int, streamID string, seq uint64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.switchStream(session, streamID)
	r.written[session] = max(r.written[session], seq)
}

// delivery is session's registration delivery field: consumer, the epoch of its NATS connection,
// and its resume point once it has one, as the plugin sends at every registration, unless
// LOADTEST_NO_RESUME=1 asks for registrations without one, as a plugin that never wrote a frame
// sends.
func (r *relaySessions) delivery(session int) map[string]any {
	d := map[string]any{"consumer": true, "epoch": r.epochs[session%len(r.epochs)]}
	if os.Getenv("LOADTEST_NO_RESUME") == "1" {
		return d
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.streams[session] != "" && r.written[session] > 0 {
		d["resume"] = map[string]any{"stream_id": r.streams[session], "seq": r.written[session]}
	}
	return d
}

// handler takes session's relay frames: it answers `received` at once, counts a first frame of
// each stream sequence as a receipt (an agent-topic one when the frame's subject is the session's
// agent subject), then answers `done`, which moves the session's resume point, and records the
// frame with that answer, as the plugin answers done once the frame is written.
func (r *relaySessions) handler(ns *natsSessions, session int, agentSubject string, nonce int64) nats.MsgHandler {
	return func(m *nats.Msg) {
		now := time.Now().UnixNano()
		r.reply(m, relayReplyReceived)
		r.frameCount.Add(1)
		rec := relayFrameRec{Session: session, Subject: m.Header.Get(relayHeaderSubject), Recv: now}
		rec.Seq, _ = strconv.ParseUint(m.Header.Get(relayHeaderStreamSeq), 10, 64)
		rec.Delivered, _ = strconv.Atoi(m.Header.Get(relayHeaderDelivered))
		if t, err := time.Parse(time.RFC3339Nano, m.Header.Get(relayHeaderStoredAt)); err == nil {
			rec.StoredAt = t.UnixNano()
		}
		if rec.Delivered > 1 {
			r.redelivered.Add(1)
		}
		if m.Header.Get(relayHeaderGap) == "true" {
			r.gaps.Add(1)
		}
		_, rec.Duplicate = r.seen[session][rec.Seq]
		r.seen[session][rec.Seq] = struct{}{}
		var env contracts.Envelope
		var ev struct {
			ID   int64  `json:"id"`
			Type string `json:"type"`
		}
		if json.Unmarshal(m.Data, &env) != nil || json.Unmarshal([]byte(env.Payload), &ev) != nil {
			r.undecodable.Add(1)
		} else {
			rec.EventID = ev.ID
		}
		if rec.Identity = env.EventID; rec.Identity == "" {
			rec.Identity = env.DedupeKey
		}
		switch {
		case rec.Duplicate:
			r.duplicates.Add(1)
		case rec.EventID != 0:
			agent := rec.Subject == agentSubject
			key := ev.Type
			if agent {
				key = "agent." + ev.Type
			}
			ns.envelopes.offer(key, m.Data, nonce)
			ns.receipts.add(receipt{EventID: ev.ID, Recv: now, Agent: agent, Published: rec.StoredAt})
		}
		rec.Done = r.reply(m, relayReplyDone)
		r.frames.add(rec)
		r.wrote(session, m.Header.Get(relayHeaderStreamID), rec.Seq)
	}
}

// reply answers m with word and reports whether the answer went out.
func (r *relaySessions) reply(m *nats.Msg, word []byte) bool {
	if m.Reply == "" {
		r.replyErrors.Add(1)
		return false
	}
	if err := m.Respond(word); err != nil {
		r.replyErrors.Add(1)
		return false
	}
	return true
}

// coreCopy takes a core copy of a relayed session's subject: the relay frame is the session's
// delivery, so the copy is counted and dropped. Nothing is answered on it, as a session answers
// no frame.
func (r *relaySessions) coreCopy() {
	r.coreCopies.Add(1)
}

// sessionDeliverySummary is how the sessions' delivery went.
type sessionDeliverySummary struct {
	// ConsumerSessions counts the sessions whose last registration answered consumer delivery;
	// NoConsumerSessions those whose last answer named another mode or none, which this listener
	// never answers 2xx (a registration it can give no consumer is refused, and the registrar
	// counts that refusal among its errors); and Unanswered those that never got an answer. A
	// step with any session not on consumer delivery measured a path that delivers nothing.
	ConsumerSessions   int `json:"consumer_sessions"`
	NoConsumerSessions int `json:"no_consumer_sessions"`
	Unanswered         int `json:"unanswered"`
	// ModeChanges counts the answers that differed from the session's answer before.
	ModeChanges int64 `json:"mode_changes"`
	// Frames counts the relay frames; Duplicates the frames of a stream sequence the session had
	// received already (not counted as receipts); Redelivered the frames whose Envoy-Delivered was
	// above 1; Gaps the frames marked Envoy-Gap; Undecodable those with no event id.
	Frames      int64 `json:"frames"`
	Duplicates  int64 `json:"duplicates"`
	Redelivered int64 `json:"redelivered"`
	Gaps        int64 `json:"gaps"`
	Undecodable int64 `json:"undecodable"`
	// ReplyErrors counts the replies that could not be sent (no reply subject, or a publish
	// error); CoreCopies the core copies of relayed sessions' subjects, not counted.
	ReplyErrors int64 `json:"reply_errors"`
	CoreCopies  int64 `json:"core_copies"`
}

func (r *relaySessions) summary() sessionDeliverySummary {
	out := sessionDeliverySummary{
		ModeChanges: r.modeChanges.Load(), Frames: r.frameCount.Load(), Duplicates: r.duplicates.Load(),
		Redelivered: r.redelivered.Load(), Gaps: r.gaps.Load(), Undecodable: r.undecodable.Load(),
		ReplyErrors: r.replyErrors.Load(), CoreCopies: r.coreCopies.Load()}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, a := range r.answers {
		switch a {
		case "":
			out.Unanswered++
		case sessionDeliveryConsumer:
			out.ConsumerSessions++
		default:
			out.NoConsumerSessions++
		}
	}
	return out
}

// writeFrames writes relay-frames.csv in dir: one row per relay frame, in arrival order per
// session, with the session id, the frame's dedupe identity and whether the session answered it
// done (restart-report reads them).
func (r *relaySessions) writeFrames(dir string, sessions []session, lg *log.Logger) {
	f, err := os.Create(filepath.Join(dir, "relay-frames.csv"))
	if err != nil {
		lg.Printf("write relay-frames.csv: %v", err)
		return
	}
	defer f.Close()
	w := csv.NewWriter(f)
	_ = w.Write([]string{"session", "subject", "stream_seq", "delivered", "stored_at_ns", "recv_ns", "event_id", "duplicate", "identity", "done"})
	for _, rec := range r.frames.snapshot() {
		_ = w.Write([]string{sessions[rec.Session].id, rec.Subject, strconv.FormatUint(rec.Seq, 10), strconv.Itoa(rec.Delivered),
			strconv.FormatInt(rec.StoredAt, 10), strconv.FormatInt(rec.Recv, 10), strconv.FormatInt(rec.EventID, 10),
			strconv.FormatBool(rec.Duplicate), rec.Identity, strconv.FormatBool(rec.Done)})
	}
	w.Flush()
	if err := w.Error(); err != nil {
		lg.Printf("write relay-frames.csv: %v", err)
	}
}

// pullSpy counts the pull requests on the stream's consumers: on a connection of its own it
// subscribes the subject a pull request is published on, $JS.API.CONSUMER.MSG.NEXT.<stream>.<consumer>,
// and gets a copy of each. Session consumers are those named s-<session id>
// (bus.SessionConsumerName).
type pullSpy struct {
	nc              *nats.Conn
	total, sessions atomic.Int64
}

func startPullSpy(url string) (*pullSpy, error) {
	nc, err := nats.Connect(url, nats.Name("loadgen-pull-spy"), nats.MaxReconnects(-1))
	if err != nil {
		return nil, fmt.Errorf("nats connect: %w", err)
	}
	p := &pullSpy{nc: nc}
	prefix := "$JS.API.CONSUMER.MSG.NEXT." + bus.Stream + "."
	if _, err := nc.Subscribe(prefix+">", func(m *nats.Msg) {
		p.total.Add(1)
		if strings.HasPrefix(m.Subject[len(prefix):], "s-") {
			p.sessions.Add(1)
		}
	}); err != nil {
		nc.Close()
		return nil, err
	}
	if err := nc.Flush(); err != nil {
		nc.Close()
		return nil, err
	}
	return p, nil
}

func (p *pullSpy) close() { p.nc.Close() }
