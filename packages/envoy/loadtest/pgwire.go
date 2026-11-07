package main

// The latency proxy's reading of the Postgres wire protocol (README "Document rooms"): it counts
// the round trips Dispatch's connections make, tells room work from the rest by the SQL of the
// statements in each round trip, and times the round trips that insert a whole-markdown version.
// The relay hands each direction's chunks to a wireTap, which parses them on its own goroutine
// from the chunk the relay already copied, so the relay's delay queue never waits for the parser.

import (
	"bytes"
	"encoding/binary"
	"regexp"
	"sync"
	"sync/atomic"
	"time"
)

// The untyped requests a client sends before or instead of its startup message, by their codes.
const (
	sslRequestCode    = 80877103
	gssEncRequestCode = 80877104
	cancelRequestCode = 80877102
)

// roomWorkPattern matches the SQL of a document room's database work: a statement that names one
// of the room tables, or takes, tries or frees the document's own advisory lock, which Dispatch
// takes as pg_advisory_(xact_)lock(hashtext($1)) on the room id (internal/dispatch/docs). Other
// locks keyed by hashtext of a literal ('issue-rank:' || $1, two-key locks) are not a room's.
var roomWorkPattern = regexp.MustCompile(`(?i)\b(doc_updates|doc_checkpoints|doc_pending_authors|doc_settlements_pending)\b` +
	`|pg_(try_)?advisory_(xact_)?(un)?lock\s*\(\s*hashtext\s*\(\s*\$[0-9]+\s*\)\s*\)`)

// versionInsertPattern matches the insert of a whole-markdown document version.
var versionInsertPattern = regexp.MustCompile(`(?i)\binsert\s+into\s+(public\.)?artifact_versions\b`)

// stmtClass is what a statement's SQL is, as the counters tell round trips apart.
type stmtClass uint8

const (
	classRoom stmtClass = 1 << iota
	classVersionInsert
)

func classifySQL(sql []byte) stmtClass {
	var c stmtClass
	if roomWorkPattern.Match(sql) {
		c |= classRoom
	}
	if versionInsertPattern.Match(sql) {
		c |= classVersionInsert
	}
	return c
}

// maxVersionInsertSamples bounds the proxy's ring of timed version inserts. 16,384 is more than a
// 180 s window at 10x writes (about 12 versions a second).
const maxVersionInsertSamples = 1 << 14

// wireCounters are the proxy's counts across every connection it relayed, from its start. The
// stats endpoint serves them (proxyStats); loadgen run reads them at the window's start and end
// and takes the difference.
type wireCounters struct {
	connections     atomic.Int64
	unparsed        atomic.Int64
	roundTrips      atomic.Int64
	roomRoundTrips  atomic.Int64
	flushRoundTrips atomic.Int64
	versionInserts  atomic.Int64
	versionInsertNs atomic.Int64

	mu sync.Mutex
	// recent is a ring of the latest timed version inserts; next is where the next one goes.
	recent []versionInsert
	next   int
}

// proxyStats is the stats endpoint's answer: the counters at At.
type proxyStats struct {
	At time.Time `json:"at"`
	// Connections are the client connections accepted; UnparsedConnections those whose stream the
	// parser gave up on (a frame it could not read, an encrypted stream, or a parser that fell
	// 4,096 chunks behind the relay): their round trips are not counted from then on.
	Connections         int64 `json:"connections"`
	UnparsedConnections int64 `json:"unparsed_connections"`
	// RoundTrips counts every Sync, simple Query, function call and reply-awaiting Flush;
	// RoomRoundTrips those whose statements are room work; FlushRoundTrips those that were a Flush.
	RoundTrips      int64 `json:"round_trips"`
	RoomRoundTrips  int64 `json:"room_round_trips"`
	FlushRoundTrips int64 `json:"flush_round_trips"`
	// VersionInserts and VersionInsertNs are the round trips that insert into artifact_versions,
	// and their time in all, each from the client's Sync or Query reaching Postgres to Postgres's
	// ReadyForQuery reaching the proxy.
	VersionInserts  int64 `json:"version_inserts"`
	VersionInsertNs int64 `json:"version_insert_ns"`
	// RecentVersionInserts are the latest of them, oldest first, up to maxVersionInsertSamples.
	RecentVersionInserts []versionInsert `json:"recent_version_inserts"`
}

// versionInsert is one timed version-insert round trip: when its ReadyForQuery arrived, and its
// time in ns.
type versionInsert struct {
	At time.Time `json:"at"`
	Ns int64     `json:"ns"`
}

func (w *wireCounters) snapshot() proxyStats {
	s := proxyStats{
		At:                  time.Now(),
		Connections:         w.connections.Load(),
		UnparsedConnections: w.unparsed.Load(),
		RoundTrips:          w.roundTrips.Load(),
		RoomRoundTrips:      w.roomRoundTrips.Load(),
		FlushRoundTrips:     w.flushRoundTrips.Load(),
		VersionInserts:      w.versionInserts.Load(),
		VersionInsertNs:     w.versionInsertNs.Load(),
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	s.RecentVersionInserts = make([]versionInsert, 0, len(w.recent))
	if len(w.recent) == maxVersionInsertSamples {
		s.RecentVersionInserts = append(s.RecentVersionInserts, w.recent[w.next:]...)
		s.RecentVersionInserts = append(s.RecentVersionInserts, w.recent[:w.next]...)
	} else {
		s.RecentVersionInserts = append(s.RecentVersionInserts, w.recent...)
	}
	return s
}

func (w *wireCounters) recordVersionInsert(at time.Time, d time.Duration) {
	w.versionInserts.Add(1)
	w.versionInsertNs.Add(int64(d))
	w.mu.Lock()
	defer w.mu.Unlock()
	v := versionInsert{At: at, Ns: int64(d)}
	if len(w.recent) < maxVersionInsertSamples {
		w.recent = append(w.recent, v)
		return
	}
	w.recent[w.next] = v
	w.next = (w.next + 1) % maxVersionInsertSamples
}

// wireConn is one relayed connection's parse: the client's stream (frontend) and the server's
// (backend), each fed in order by its own goroutine. Postgres answers every Sync, simple Query and
// function call, and the startup, with exactly one ReadyForQuery, in order, so the two streams are
// paired by queue position (awaiting, ready) whichever goroutine runs first.
type wireConn struct {
	counters *wireCounters
	lost     atomic.Bool

	mu       sync.Mutex
	awaiting []awaitingReady
	ready    []time.Time

	front frontendParser
	back  backendParser
}

// awaitingReady is a message that Postgres answers with one ReadyForQuery: when it reached
// Postgres, and whether its round trip inserts a version.
type awaitingReady struct {
	at     time.Time
	insert bool
}

func newWireConn(counters *wireCounters) *wireConn {
	counters.connections.Add(1)
	return &wireConn{counters: counters, front: frontendParser{stmts: map[string]stmtClass{}}}
}

// lose stops counting the connection, once: its stream can no longer be followed.
func (c *wireConn) lose() {
	if c.lost.CompareAndSwap(false, true) {
		c.counters.unparsed.Add(1)
	}
}

// frontend parses data, the next bytes of the client's stream, which reached Postgres at at.
func (c *wireConn) frontend(data []byte, at time.Time) {
	if c.lost.Load() {
		return
	}
	if !c.front.feed(c, data, at) {
		c.lose()
	}
}

// backend parses data, the next bytes of the server's stream, which reached the proxy at at.
func (c *wireConn) backend(data []byte, at time.Time) {
	if c.lost.Load() {
		return
	}
	if !c.back.feed(c, data, at) {
		c.lose()
	}
}

// roundTrip counts one round trip of class cls; a flush is one that a Flush ended.
func (c *wireConn) roundTrip(cls stmtClass, flush bool) {
	if c.lost.Load() {
		return
	}
	c.counters.roundTrips.Add(1)
	if cls&classRoom != 0 {
		c.counters.roomRoundTrips.Add(1)
	}
	if flush {
		c.counters.flushRoundTrips.Add(1)
	}
}

func (c *wireConn) awaitReady(a awaitingReady) {
	c.mu.Lock()
	c.awaiting = append(c.awaiting, a)
	c.pairLocked()
	c.mu.Unlock()
}

func (c *wireConn) readyForQuery(at time.Time) {
	c.mu.Lock()
	c.ready = append(c.ready, at)
	c.pairLocked()
	c.mu.Unlock()
}

// pairLocked pairs each ReadyForQuery with the message it answers, in order, and times the
// version inserts among them.
func (c *wireConn) pairLocked() {
	for len(c.awaiting) > 0 && len(c.ready) > 0 {
		a, at := c.awaiting[0], c.ready[0]
		c.awaiting, c.ready = c.awaiting[1:], c.ready[1:]
		if a.insert && !c.lost.Load() {
			c.counters.recordVersionInsert(at, max(at.Sub(a.at), 0))
		}
	}
}

// frameReader holds a stream's partial frame across chunks. A frame whose body the parser does
// not read is skipped as its bytes arrive, without being held.
type frameReader struct {
	buf  []byte
	skip int
}

// frames calls each for every whole frame in data, after what earlier chunks left, with the
// frame's type and body. want says, from its type, whether a frame's body is read: a frame whose
// body is not read is handed over with a nil body as soon as its header is in, and its body is
// skipped. enough says, of a read frame whose body is still arriving, whether the part of it in so
// far holds all the caller reads: that part is handed over as the body at once and the rest is
// skipped, so the frame is not held whole. It is asked only of a frame want reads. It returns
// false when a length field cannot be a frame's or each returns false.
func (f *frameReader) frames(data []byte, want func(byte) bool, enough func(typ byte, partial []byte) bool, each func(typ byte, body []byte) bool) bool {
	if f.skip > 0 {
		n := min(f.skip, len(data))
		f.skip -= n
		data = data[n:]
	}
	// owned is whether data is the reader's own buffer, which it may keep and grow; otherwise it
	// is the caller's chunk, kept only with its capacity clipped, so a later append copies it.
	owned := false
	if len(f.buf) > 0 && len(data) > 0 {
		data, owned = append(f.buf, data...), true
	} else if len(f.buf) > 0 {
		data, owned = f.buf, true
	}
	f.buf = nil
	for len(data) >= 5 {
		typ := data[0]
		n := int(binary.BigEndian.Uint32(data[1:5]))
		if n < 4 {
			return false
		}
		if !want(typ) {
			if !each(typ, nil) {
				return false
			}
			if 1+n > len(data) {
				f.skip = 1 + n - len(data)
				return true
			}
			data = data[1+n:]
			continue
		}
		if 1+n > len(data) {
			if !enough(typ, data[5:]) {
				break
			}
			if !each(typ, data[5:]) {
				return false
			}
			f.skip = 1 + n - len(data)
			return true
		}
		if !each(typ, data[5:1+n]) {
			return false
		}
		data = data[1+n:]
	}
	switch {
	case len(data) == 0:
	case owned:
		f.buf = data
	default:
		f.buf = data[:len(data):len(data)]
	}
	return true
}

// frontendParser follows the client's stream: the startup (with SSLRequest or GSSENCRequest
// before it), then typed frames.
type frontendParser struct {
	frames  frameReader
	started bool
	cancel  bool
	// stmts maps each prepared statement's name (the unnamed one is "") to its SQL's class.
	stmts map[string]stmtClass
	// seg is the class of the statements since the last round trip ended; pending is whether a
	// message since then awaits a reply, so a Flush now ends a round trip. sinceSync is the class
	// since the last Sync, which its ReadyForQuery answers.
	seg       stmtClass
	pending   bool
	sinceSync stmtClass
}

func (p *frontendParser) feed(c *wireConn, data []byte, at time.Time) bool {
	if p.cancel {
		return true
	}
	if !p.started {
		data = append(p.frames.buf, data...)
		p.frames.buf = nil
		for !p.started {
			if len(data) < 8 {
				p.frames.buf = append([]byte(nil), data...)
				return true
			}
			n := int(binary.BigEndian.Uint32(data[0:4]))
			if n < 8 || n > 1<<16 {
				return false
			}
			if len(data) < n {
				p.frames.buf = append([]byte(nil), data...)
				return true
			}
			switch code := binary.BigEndian.Uint32(data[4:8]); {
			case code == sslRequestCode || code == gssEncRequestCode:
				// The server answers with one byte (backendParser.refusals) and the client
				// sends its startup next.
			case code == cancelRequestCode:
				p.cancel = true
				return true
			case code>>16 == 3:
				p.started = true
				// The startup's authentication ends with one ReadyForQuery, which is no round
				// trip of the client's work.
				c.awaitReady(awaitingReady{at: at})
			default:
				return false
			}
			data = data[n:]
		}
	}
	return p.frames.frames(data, frontendWants, frontendEnough, func(typ byte, body []byte) bool {
		return p.message(c, typ, body, at)
	})
}

// frontendWants is the client frames whose bodies the parser reads: Parse, Bind and Query.
func frontendWants(typ byte) bool { return typ == 'P' || typ == 'B' || typ == 'Q' }

// frontendEnough is whether partial, the body so far of a client frame still arriving, holds all
// the parser reads of it: a Bind's portal and statement names, its first two strings, and not the
// parameters after them, which can be a batch's whole updates. A Parse or a Query is read whole.
func frontendEnough(typ byte, partial []byte) bool {
	return typ == 'B' && bytes.Count(partial, []byte{0}) >= 2
}

func (p *frontendParser) message(c *wireConn, typ byte, body []byte, at time.Time) bool {
	switch typ {
	case 'P': // Parse: statement name, SQL, parameter types
		name, rest, ok := cstring(body)
		if !ok {
			return false
		}
		sql, _, ok := cstring(rest)
		if !ok {
			return false
		}
		cls := classifySQL(sql)
		p.stmts[string(name)] = cls
		// Preparing a room statement is room work; preparing an insert inserts nothing, so only
		// the Bind that runs it marks its round trip as a version insert.
		p.add(cls &^ classVersionInsert)
	case 'B': // Bind: portal name, statement name, ...
		_, rest, ok := cstring(body)
		if !ok {
			return false
		}
		name, _, ok := cstring(rest)
		if !ok {
			return false
		}
		p.add(p.stmts[string(name)])
	case 'D', 'E', 'C': // Describe, Execute, Close: each is answered
		p.pending = true
	case 'H': // Flush: the client waits for the answers to what it sent since the last round trip
		if p.pending {
			c.roundTrip(p.seg, true)
			p.seg, p.pending = 0, false
		}
	case 'S': // Sync
		c.roundTrip(p.seg, false)
		c.awaitReady(awaitingReady{at: at, insert: p.sinceSync&classVersionInsert != 0})
		p.seg, p.pending, p.sinceSync = 0, false, 0
	case 'Q': // simple Query: one round trip, whatever it holds
		sql, _, ok := cstring(body)
		if !ok {
			return false
		}
		cls := classifySQL(sql)
		c.roundTrip(cls|p.seg, false)
		c.awaitReady(awaitingReady{at: at, insert: cls&classVersionInsert != 0})
		p.seg, p.pending, p.sinceSync = 0, false, 0
	case 'F': // function call: answered with one ReadyForQuery
		c.roundTrip(0, false)
		c.awaitReady(awaitingReady{at: at})
	}
	return true
}

func (p *frontendParser) add(cls stmtClass) {
	p.seg |= cls
	p.sinceSync |= cls
	p.pending = true
}

// cstring splits b at its first NUL.
func cstring(b []byte) ([]byte, []byte, bool) {
	if i := bytes.IndexByte(b, 0); i >= 0 {
		return b[:i], b[i+1:], true
	}
	return nil, nil, false
}

// backendParser follows the server's stream for its ReadyForQuery frames: first any one-byte
// refusals of SSLRequest or GSSENCRequest ('N'), then typed frames.
type backendParser struct {
	frames frameReader
	typed  bool
}

func (p *backendParser) feed(c *wireConn, data []byte, at time.Time) bool {
	for !p.typed && len(data) > 0 {
		switch data[0] {
		case 'N':
			data = data[1:]
		case 'S', 'G':
			return false // an encrypted stream
		default:
			p.typed = true
		}
	}
	return p.frames.frames(data, func(byte) bool { return false }, nil, func(typ byte, _ []byte) bool {
		if typ == 'Z' {
			c.readyForQuery(at)
		}
		return true
	})
}

// wireTap hands one direction's chunks to its parser on the parser's own goroutine. send never
// blocks: a parser 4,096 chunks behind gives the connection up (wireConn.lose).
type wireTap struct {
	ch   chan wireChunk
	conn *wireConn
}

type wireChunk struct {
	data []byte
	at   time.Time
}

func newWireTap(conn *wireConn, parse func([]byte, time.Time)) *wireTap {
	t := &wireTap{ch: make(chan wireChunk, 4096), conn: conn}
	go func() {
		for c := range t.ch {
			parse(c.data, c.at)
		}
	}()
	return t
}

// send hands data, which the caller never writes to again, to the parser. A nil tap does nothing.
func (t *wireTap) send(data []byte, at time.Time) {
	if t == nil || t.conn.lost.Load() {
		return
	}
	select {
	case t.ch <- wireChunk{data: data, at: at}:
	default:
		t.conn.lose()
	}
}

func (t *wireTap) close() {
	if t != nil {
		close(t.ch)
	}
}
