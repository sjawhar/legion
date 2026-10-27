package main

// The browser-editor workload (README "Editors"): N Yjs peers on the document websocket of the
// first D seeded specs, each typing into a paragraph at R updates a second, open-loop Poisson. It
// measures the relay latency (one editor's send to another editor's receipt on the same document)
// and the durable latency (send to the marker's row in doc_updates), and counts what the rooms
// acknowledged, refused and closed.

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math/rand/v2"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	gws "github.com/gorilla/websocket"
	"github.com/jackc/pgx/v5"
	"github.com/reearth/ygo/crdt"
	"github.com/reearth/ygo/encoding"
	ygsync "github.com/reearth/ygo/sync"

	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// editorMarkerEvery is how often an editor's update carries a durable-latency marker: its 50th,
// 100th, ... update.
const editorMarkerEvery = 50

// editorMarker is the n-th marker: 16 bytes, "lge", n in 12 digits, "x". An update inserts it as
// one string, so it stands whole in the update's bytes, and in the doc_updates row that holds it.
func editorMarker(n int64) string { return fmt.Sprintf("lge%012dx", n) }

// proofFragment is the Yjs fragment a Dispatch document lives in (internal/dispatch/docs
// fragmentName).
const proofFragment = "prosemirror"

// The Hocuspocus frames an editor sends and reads (reearth/ygo provider/websocket server.go):
// each is VarString(document name), VarUint(tag), then the tag's payload.
const (
	hocuspocusSync       = 0
	hocuspocusAuth       = 2
	hocuspocusSyncStatus = 8

	hocuspocusAuthToken            = 0
	hocuspocusAuthPermissionDenied = 1
	hocuspocusAuthAuthenticated    = 2
)

// editorReconnectDelay is the wait before an editor reconnects after its connection ended or
// failed.
const editorReconnectDelay = time.Second

type editorConfig struct {
	editors int
	rate    float64 // updates per second per editor
	docs    int
}

// editorDoc is one document's editors, which relay one another's updates.
type editorDoc struct {
	key, artifactID string
	editors         int

	mu      sync.Mutex
	members []*editorConn
}

// editorStats are the workload's counts across every editor. Sent, acknowledged, refused, the
// relay latencies and the markers count only updates scheduled in the window; connects, connect
// failures and closes count the whole step.
type editorStats struct {
	mu              sync.Mutex
	sent            int
	acked           int
	refused         int
	notSent         map[string]int
	connects        int
	readOnly        int
	denied          int
	connectFailures map[string]int
	closes          map[string]int
	relayMs         []float64
	markers         int
}

func newEditorStats() *editorStats {
	return &editorStats{notSent: map[string]int{}, connectFailures: map[string]int{}, closes: map[string]int{}}
}

func (s *editorStats) add(f func(*editorStats)) {
	s.mu.Lock()
	f(s)
	s.mu.Unlock()
}

// editorConn is one editor's connection to its document's room: a browser's provider as the
// dashboard runs it (packages/dispatch/web/src/features/doc/connection.ts), on Hocuspocus framing.
type editorConn struct {
	doc    *editorDoc
	stats  *editorStats
	ws     *gws.Conn
	ydoc   *crdt.Doc
	client crdt.ClientID
	// ended is closed once the reader has stopped and the connection left its document.
	ended chan struct{}

	// writes holds one write at a time: gorilla/websocket allows one writer, and the reader
	// answers the room's sync step 1 while the editor sends.
	writes sync.Mutex
	// acks is what each SyncStatus the room owes answers, in send order: the room answers every
	// sync step 2 and update with exactly one, in order.
	ackMu sync.Mutex
	acks  []ackWait

	synced   atomic.Bool
	closing  atomic.Bool
	readOnly atomic.Bool

	// sent are the editor's updates, in send order, and cursors, per other editor of the
	// document, how many of that editor's sent updates this one has received; both under doc.mu.
	sent    []sentUpdate
	cursors map[*editorConn]int
}

type ackWait struct {
	update   bool
	inWindow bool
}

// sentUpdate is one update an editor sent: its client clock after the update (an editor holding
// that clock of the sender's client holds the update), when it was sent, and whether its schedule
// was in the window.
type sentUpdate struct {
	clock    uint64
	at       time.Time
	inWindow bool
}

// connectError is a failed connection, by a reason that groups it: "http <status>" for a refused
// upgrade, "dial" for any other.
type connectError struct {
	reason string
	err    error
}

func (e *connectError) Error() string { return e.reason + ": " + e.err.Error() }

// dialEditor connects an editor to d's room at url, the document websocket's full address with
// its schema_version, as login, and starts its reader. As @hocuspocus/provider does, it sends
// the token (the dashboard's is its schema version) and then sync step 1; the room's sync step 2
// makes it synced.
func dialEditor(ctx context.Context, url, login, token string, d *editorDoc, st *editorStats) (*editorConn, error) {
	dialer := gws.Dialer{HandshakeTimeout: 10 * time.Second}
	ws, resp, err := dialer.DialContext(ctx, url, http.Header{"X-Dispatch-User": []string{login}})
	if err != nil {
		reason := "dial"
		if resp != nil {
			reason = "http " + strconv.Itoa(resp.StatusCode)
		}
		return nil, &connectError{reason: reason, err: err}
	}
	ydoc := crdt.New()
	c := &editorConn{doc: d, stats: st, ws: ws, ydoc: ydoc, client: ydoc.ClientID(), ended: make(chan struct{}), cursors: map[*editorConn]int{}}
	go c.read()
	auth := encoding.EncodeBytes(func(e *encoding.Encoder) {
		e.WriteVarUint(hocuspocusAuthToken)
		e.WriteVarString(token)
	})
	if err := c.write(hocuspocusAuth, auth, nil); err != nil {
		c.close()
		return nil, &connectError{reason: "dial", err: err}
	}
	if err := c.write(hocuspocusSync, ygsync.EncodeSyncStep1(ydoc), nil); err != nil {
		c.close()
		return nil, &connectError{reason: "dial", err: err}
	}
	return c, nil
}

// write sends one frame; ack, when not nil, is what the room's SyncStatus for it will answer.
func (c *editorConn) write(tag uint64, payload []byte, ack *ackWait) error {
	frame := encoding.EncodeBytes(func(e *encoding.Encoder) {
		e.WriteVarString(c.doc.artifactID)
		e.WriteVarUint(tag)
		e.WriteRaw(payload)
	})
	c.writes.Lock()
	defer c.writes.Unlock()
	if ack != nil {
		c.ackMu.Lock()
		c.acks = append(c.acks, *ack)
		c.ackMu.Unlock()
	}
	_ = c.ws.SetWriteDeadline(time.Now().Add(10 * time.Second))
	return c.ws.WriteMessage(gws.BinaryMessage, frame)
}

// close ends the connection from the editor's side; its close is not counted.
func (c *editorConn) close() {
	c.closing.Store(true)
	_ = c.ws.Close()
	<-c.ended
}

// read handles every frame the room sends until the connection ends, then records how it ended
// and leaves the document.
func (c *editorConn) read() {
	defer close(c.ended)
	defer c.leave()
	for {
		_, message, err := c.ws.ReadMessage()
		if err != nil {
			if !c.closing.Load() {
				reason := "read error"
				var ce *gws.CloseError
				if errors.As(err, &ce) {
					reason = strconv.Itoa(ce.Code)
				}
				c.stats.add(func(s *editorStats) { s.closes[reason]++ })
			}
			return
		}
		if err := c.handle(message, time.Now()); err != nil {
			if !c.closing.Load() {
				c.stats.add(func(s *editorStats) { s.closes["unreadable frame"]++ })
			}
			c.closing.Store(true)
			_ = c.ws.Close()
			return
		}
	}
}

// handle applies one frame the room sent, which arrived at at.
func (c *editorConn) handle(message []byte, at time.Time) error {
	decoder := encoding.NewDecoder(message)
	if _, err := decoder.ReadVarString(); err != nil {
		return err
	}
	tag, err := decoder.ReadVarUint()
	if err != nil {
		return err
	}
	switch tag {
	case hocuspocusSync:
		payload := decoder.RemainingBytes()
		kind, _, err := ygsync.ReadSyncMessage(payload)
		if err != nil {
			return err
		}
		reply, err := ygsync.ApplySyncMessage(c.ydoc, payload, c)
		if err != nil {
			return err
		}
		switch kind {
		case ygsync.MsgSyncStep1:
			if reply != nil {
				// A failed write ends the connection, which the next read reports.
				_ = c.write(hocuspocusSync, reply, &ackWait{})
			}
		case ygsync.MsgSyncStep2:
			if !c.synced.Load() {
				c.join()
				c.synced.Store(true)
			}
			c.received(at)
		case ygsync.MsgUpdate:
			c.received(at)
		}
	case hocuspocusAuth:
		sub, err := decoder.ReadVarUint()
		if err != nil {
			return err
		}
		switch sub {
		case hocuspocusAuthPermissionDenied:
			c.stats.add(func(s *editorStats) { s.denied++ })
		case hocuspocusAuthAuthenticated:
			if scope, _ := decoder.ReadVarString(); scope == "readonly" && !c.readOnly.Swap(true) {
				c.stats.add(func(s *editorStats) { s.readOnly++ })
			}
		}
	case hocuspocusSyncStatus:
		applied, err := decoder.ReadVarUint()
		if err != nil {
			return err
		}
		c.ackMu.Lock()
		var w ackWait
		if len(c.acks) > 0 {
			w, c.acks = c.acks[0], c.acks[1:]
		}
		c.ackMu.Unlock()
		if w.update && w.inWindow {
			c.stats.add(func(s *editorStats) {
				if applied == 1 {
					s.acked++
				} else {
					s.refused++
				}
			})
		}
	}
	return nil
}

// join makes the editor one of its document's relaying editors: from now on it receives every
// update the others send, and they receive its.
func (c *editorConn) join() {
	d := c.doc
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, m := range d.members {
		m.cursors[c] = 0
		c.cursors[m] = len(m.sent)
	}
	d.members = append(d.members, c)
}

func (c *editorConn) leave() {
	d := c.doc
	d.mu.Lock()
	defer d.mu.Unlock()
	d.members = slices.DeleteFunc(d.members, func(m *editorConn) bool { return m == c })
	for _, m := range d.members {
		delete(m.cursors, c)
	}
}

// received records, at at, the receipt of every update another editor sent that the editor's
// copy now holds and had not.
func (c *editorConn) received(at time.Time) {
	sv := c.ydoc.StateVector()
	var latencies []float64
	c.doc.mu.Lock()
	for sender, next := range c.cursors {
		clock := sv.Clock(sender.client)
		for next < len(sender.sent) && sender.sent[next].clock <= clock {
			if u := sender.sent[next]; u.inWindow {
				latencies = append(latencies, float64(at.Sub(u.at))/1e6)
			}
			next++
		}
		c.cursors[sender] = next
	}
	c.doc.mu.Unlock()
	if len(latencies) > 0 {
		c.stats.add(func(s *editorStats) { s.relayMs = append(s.relayMs, latencies...) })
	}
}

// errNoParagraph is an edit the editor could not make: its copy holds no paragraph to type into.
var errNoParagraph = errors.New("no paragraph to edit")

// send types text into the editor's copy (editParagraph) and sends the room the update. beforeSend,
// when not nil, runs once the update is made and before it is sent, with the send time.
func (c *editorConn) send(r *rand.Rand, text string, inWindow bool, beforeSend func(at time.Time)) error {
	update := editParagraph(c.ydoc, r, text)
	if update == nil {
		return errNoParagraph
	}
	clock := c.ydoc.StateVector().Clock(c.client)
	at := time.Now()
	c.doc.mu.Lock()
	c.sent = append(c.sent, sentUpdate{clock: clock, at: at, inWindow: inWindow})
	c.doc.mu.Unlock()
	if beforeSend != nil {
		beforeSend(at)
	}
	return c.write(hocuspocusSync, ygsync.EncodeUpdate(update), &ackWait{update: true, inWindow: inWindow})
}

// editOrigin tags an editor's own transaction. It is not zero sized: two pointers to zero-sized
// values may be equal.
type editOrigin struct{ _ byte }

// editParagraph types text into doc as a browser's keystroke does: one transaction that inserts
// text, without marks, into the text of a top-level paragraph, at a position inside it (never at
// its start or end, so the paragraph neither starts nor ends with what was typed). A paragraph is
// eligible when it holds one text run of two characters or more and is not the document's first
// paragraph, which in a seeded spec is the "Revision token rN" line the event generator's edits
// replace. It reads the tree with the transaction's lock-free accessors only. It returns the
// update the transaction made, or nil when doc holds no eligible paragraph.
func editParagraph(doc *crdt.Doc, r *rand.Rand, text string) []byte {
	fragment := doc.GetXmlFragment(proofFragment)
	origin := &editOrigin{}
	var update []byte
	unsubscribe := doc.OnUpdate(func(encoded []byte, updateOrigin any) {
		if updateOrigin == origin {
			update = append([]byte(nil), encoded...)
		}
	})
	defer unsubscribe()
	doc.Transact(func(txn *crdt.Transaction) {
		var runs []*crdt.YXmlText
		first := true
		for _, child := range fragment.Children() {
			paragraph, ok := child.(*crdt.YXmlElement)
			if !ok || paragraph.NodeName != "paragraph" {
				continue
			}
			if first {
				first = false
				continue
			}
			children := paragraph.Children()
			if len(children) != 1 {
				continue
			}
			if run, ok := children[0].(*crdt.YXmlText); ok && run.Len() >= 2 {
				runs = append(runs, run)
			}
		}
		if len(runs) == 0 {
			return
		}
		run := runs[r.IntN(len(runs))]
		run.Insert(txn, 1+r.IntN(run.Len()-1), text, nil)
	}, origin)
	return update
}

// editorWorkload runs the editors of one step.
type editorWorkload struct {
	cfg      editorConfig
	docs     []*editorDoc
	stats    *editorStats
	probe    *durableProbe
	probeErr error
	markers  atomic.Int64
	// schedules is done once every editor's schedule has ended (the window's end); all once every
	// goroutine has stopped, after the step's context ended.
	schedules sync.WaitGroup
	all       sync.WaitGroup
}

// startEditors connects cfg.editors editors, editor i to the (i mod D)-th of the first D seeded
// specs in key order, as person-<i>, and runs each one's schedule from start to winEnd: updates
// at cfg.rate a second, open-loop Poisson from the step's RNG seed (stream 3+i). An update is
// scheduled whether or not its editor is connected; one whose editor is not connected and synced
// is counted as not sent. The durable probe reads its floors before the first editor connects.
func startEditors(ctx context.Context, rc runConfig, sf seedFile, start, winStart, winEnd time.Time, lg *log.Logger) *editorWorkload {
	cfg := rc.editors
	w := &editorWorkload{cfg: cfg, stats: newEditorStats()}
	ids := make([]string, cfg.docs)
	for i := range cfg.docs {
		is := sf.Issues[i]
		w.docs = append(w.docs, &editorDoc{key: is.Key, artifactID: is.ArtifactID})
		ids[i] = is.ArtifactID
	}
	for i := range cfg.editors {
		w.docs[i%cfg.docs].editors++
	}
	w.probe, w.probeErr = newDurableProbe(ctx, rc.ep.pg, ids)
	if w.probeErr != nil {
		lg.Printf("editors: durable probe: %v; durable latency is not measured", w.probeErr)
	} else {
		w.all.Add(1)
		go func() {
			defer w.all.Done()
			w.probe.loop(ctx)
		}()
	}
	base := "ws" + strings.TrimPrefix(rc.ep.dispatch, "http")
	schema := strconv.Itoa(pmdoc.SchemaVersion())
	inWindow := func(t time.Time) bool { return !t.Before(winStart) && t.Before(winEnd) }
	lg.Printf("editors: %d on %d documents at %g updates/s each, schema_version %s", cfg.editors, cfg.docs, cfg.rate, schema)
	for i := range cfg.editors {
		d := w.docs[i%cfg.docs]
		url := base + "/ws/doc/" + d.artifactID + "?schema_version=" + schema
		login := fmt.Sprintf("person-%d", i)
		var current atomic.Pointer[editorConn]
		w.all.Add(1)
		go func() {
			defer w.all.Done()
			for ctx.Err() == nil {
				c, err := dialEditor(ctx, url, login, schema, d, w.stats)
				if err != nil {
					if ctx.Err() == nil {
						var ce *connectError
						reason := "dial"
						if errors.As(err, &ce) {
							reason = ce.reason
						}
						w.stats.add(func(s *editorStats) { s.connectFailures[reason]++ })
					}
					sleepUntil(ctx, time.Now().Add(editorReconnectDelay))
					continue
				}
				w.stats.add(func(s *editorStats) { s.connects++ })
				current.Store(c)
				select {
				case <-c.ended:
				case <-ctx.Done():
					c.close()
				}
				current.Store(nil)
				sleepUntil(ctx, time.Now().Add(editorReconnectDelay))
			}
		}()
		w.schedules.Add(1)
		w.all.Add(1)
		go func() {
			defer w.all.Done()
			defer w.schedules.Done()
			// The schedule and the words come from one stream, which every scheduled update
			// draws from whether or not it is sent, so two runs offer the same arrivals; where an
			// update lands in the document comes from another.
			r := rand.New(rand.NewPCG(rc.rngSeed, 3+uint64(i)))
			positions := rand.New(rand.NewPCG(rc.rngSeed, 1<<32+uint64(i)))
			next := start
			updates := 0
			for {
				next = next.Add(time.Duration(r.ExpFloat64() / cfg.rate * float64(time.Second)))
				if !next.Before(winEnd) {
					return
				}
				word := words[r.IntN(len(words))]
				if !sleepUntil(ctx, next) {
					return
				}
				in := inWindow(next)
				c := current.Load()
				if c == nil || !c.synced.Load() {
					if in {
						w.stats.add(func(s *editorStats) { s.notSent["disconnected"]++ })
					}
					continue
				}
				updates++
				text, marker := " "+word, ""
				if updates%editorMarkerEvery == 0 {
					marker = editorMarker(w.markers.Add(1))
					text = " " + marker
				}
				err := c.send(positions, text, in, func(at time.Time) {
					if marker != "" && w.probe != nil {
						w.probe.add(d.artifactID, marker, at, in)
					}
				})
				switch {
				case !in:
				case errors.Is(err, errNoParagraph):
					w.stats.add(func(s *editorStats) { s.notSent["no paragraph"]++ })
				case err != nil:
					w.stats.add(func(s *editorStats) { s.notSent["write failed"]++ })
				default:
					w.stats.add(func(s *editorStats) {
						s.sent++
						if marker != "" {
							s.markers++
						}
					})
				}
			}
		}()
	}
	return w
}

type editorsSummary struct {
	Editors       int     `json:"editors"`
	RatePerEditor float64 `json:"rate_per_editor"`
	Docs          int     `json:"docs"`
	// SingleEditorDocs are the issue keys of the documents with one editor, which have no relay
	// latency.
	SingleEditorDocs []string `json:"single_editor_docs"`
	TargetPerS       float64  `json:"target_per_s"`
	Sent             int      `json:"sent"`
	SentPerS         float64  `json:"sent_per_s"`
	// Acknowledged and Refused are the sent updates the room answered with SyncStatus 1 and 0.
	Acknowledged int `json:"acknowledged"`
	Refused      int `json:"refused"`
	// NotSent are the updates scheduled in the window that were not sent, by reason.
	NotSent             map[string]int `json:"not_sent"`
	Connects            int            `json:"connects"`
	ConnectFailures     map[string]int `json:"connect_failures"`
	ReadOnlyConnections int            `json:"readonly_connections"`
	AuthDenied          int            `json:"auth_denied"`
	// Closes are the connections the room ended, by close code ("1006" for no close frame).
	Closes  map[string]int `json:"closes"`
	RelayMs stat           `json:"relay_ms"`
	// Markers are the in-window updates that carried a marker; MarkersFound those the durable
	// probe found in doc_updates; DurableMs the send-to-found times.
	Markers      int    `json:"markers"`
	MarkersFound int    `json:"markers_found"`
	DurableMs    stat   `json:"durable_ms"`
	DurableNote  string `json:"durable_note,omitempty"`
}

// summary reduces the workload once every goroutine has stopped.
func (w *editorWorkload) summary(durationS float64) *editorsSummary {
	s := w.stats
	s.mu.Lock()
	defer s.mu.Unlock()
	out := &editorsSummary{
		Editors: w.cfg.editors, RatePerEditor: w.cfg.rate, Docs: w.cfg.docs, SingleEditorDocs: []string{},
		TargetPerS: float64(w.cfg.editors) * w.cfg.rate, Sent: s.sent, Acknowledged: s.acked, Refused: s.refused,
		NotSent: s.notSent, Connects: s.connects, ConnectFailures: s.connectFailures, ReadOnlyConnections: s.readOnly,
		AuthDenied: s.denied, Closes: s.closes, RelayMs: mkStat(s.relayMs), Markers: s.markers,
	}
	if durationS > 0 {
		out.SentPerS = float64(s.sent) / durationS
	}
	for _, d := range w.docs {
		if d.editors == 1 {
			out.SingleEditorDocs = append(out.SingleEditorDocs, d.key)
		}
	}
	if w.probe == nil {
		out.DurableNote = fmt.Sprintf("not measured: %v", w.probeErr)
		return out
	}
	found := w.probe.foundMs()
	out.MarkersFound = len(found)
	out.DurableMs = mkStat(found)
	return out
}

// waitSchedules returns once every editor's schedule has ended, at the window's end.
func (w *editorWorkload) waitSchedules() { w.schedules.Wait() }

// wait returns once every goroutine of the workload has stopped; the step's context must have
// ended.
func (w *editorWorkload) wait() { w.all.Wait() }

// durableProbeQuery finds a marker's row: $1 the document, $2 the version floor, $3 the marker.
const durableProbeQuery = `select version from doc_updates where artifact_id = $1::text::uuid and version > $2 and position($3 in update) > 0`

// durableGiveUp is how long the probe looks for a marker before it counts it not found.
const durableGiveUp = 60 * time.Second

// durableProbe finds each marker's row in doc_updates, every 10 ms, over its own direct
// connection, on the documents that have a marker pending only. A marker's floor is the highest
// version the probe has seen on its document before the marker was sent: a row that committed
// before the send cannot hold it.
type durableProbe struct {
	conn *pgx.Conn

	mu      sync.Mutex
	floors  map[string]int64
	pending []pendingMarker
	found   []float64
}

type pendingMarker struct {
	doc      string
	marker   []byte
	sent     time.Time
	inWindow bool
	floor    int64
}

// newDurableProbe connects over url, the direct Postgres URL, and reads each document's highest
// version, before any editor sends.
func newDurableProbe(ctx context.Context, url string, docs []string) (*durableProbe, error) {
	config, err := pgx.ParseConfig(url)
	if err != nil {
		return nil, err
	}
	config.RuntimeParams["application_name"] = "loadgen-durable-probe"
	connectCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	conn, err := pgx.ConnectConfig(connectCtx, config)
	if err != nil {
		return nil, err
	}
	p := &durableProbe{conn: conn, floors: map[string]int64{}}
	rows, err := conn.Query(connectCtx, `select artifact_id::text, max(version) from doc_updates where artifact_id = any($1::text[]::uuid[]) group by artifact_id`, docs)
	if err != nil {
		conn.Close(context.Background())
		return nil, err
	}
	for rows.Next() {
		var doc string
		var version int64
		if err := rows.Scan(&doc, &version); err != nil {
			rows.Close()
			conn.Close(context.Background())
			return nil, err
		}
		p.floors[doc] = version
	}
	if err := rows.Err(); err != nil {
		conn.Close(context.Background())
		return nil, err
	}
	return p, nil
}

// add registers a marker sent at sent on doc.
func (p *durableProbe) add(doc, marker string, sent time.Time, inWindow bool) {
	p.mu.Lock()
	p.pending = append(p.pending, pendingMarker{doc: doc, marker: []byte(marker), sent: sent, inWindow: inWindow, floor: p.floors[doc]})
	p.mu.Unlock()
}

// loop looks for the pending markers every 10 ms, one pipelined batch of durableProbeQuery per
// look, until ctx ends.
func (p *durableProbe) loop(ctx context.Context) {
	defer p.conn.Close(context.Background())
	t := time.NewTicker(10 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		p.mu.Lock()
		look := slices.Clone(p.pending)
		p.mu.Unlock()
		if len(look) == 0 {
			continue
		}
		batch := &pgx.Batch{}
		for _, m := range look {
			batch.Queue(durableProbeQuery, m.doc, m.floor, m.marker)
		}
		results := p.conn.SendBatch(ctx, batch)
		versions := make([]int64, len(look))
		for i := range look {
			if err := results.QueryRow().Scan(&versions[i]); err != nil {
				versions[i] = -1
			}
		}
		_ = results.Close()
		if ctx.Err() != nil {
			return
		}
		now := time.Now()
		p.mu.Lock()
		// p.pending may have grown since look was taken; its first len(look) entries are look.
		kept := p.pending[:0]
		for i, m := range p.pending {
			if i < len(look) && versions[i] >= 0 {
				if m.inWindow {
					p.found = append(p.found, float64(now.Sub(m.sent))/1e6)
				}
				p.floors[m.doc] = max(p.floors[m.doc], versions[i])
				continue
			}
			if now.Sub(m.sent) > durableGiveUp {
				continue
			}
			kept = append(kept, m)
		}
		p.pending = kept
		p.mu.Unlock()
	}
}

func (p *durableProbe) foundMs() []float64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.found)
}
