package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

// liveSub is the one subscription a dashboard socket holds: the events view, the event feed a
// dashboard tab follows on its live socket.
const liveSub = "events"

// liveFrame is one event frame a dashboard live socket received. Dispatch sends each event of an
// events subscription as {"type":"event","sub":...,"id":"<n>","event":<the event's JSON>}
// (internal/dispatch/api/live_frames.go LiveEventFrame); every other frame type (events_start,
// heartbeat, a delivery view's frames, ended, error) is not an event frame.
type liveFrame struct {
	at         int64 // Unix ns when the frame's message arrived
	id         int64 // the event's id (the frame's decimal id)
	created    int64 // Unix ns of the event's created_at; 0 when absent or unparsable
	marker     int64 // the counter of the run's marker in the frame's event JSON (findMarker); 0 when none
	outOfOrder bool  // id <= the previous event frame's id on the same connection
}

// liveSubscribe is the subscribe frame a dashboard tab sends for the events view. It names no
// resume: a socket the load test opens follows the feed from its head.
func liveSubscribe() map[string]any {
	return map[string]any{"type": "subscribe", "sub": liveSub, "view": map[string]string{"kind": "events"}}
}

// liveMessage is the part of any frame Dispatch sends that the reader reads.
type liveMessage struct {
	Type    string          `json:"type"`
	Sub     string          `json:"sub"`
	ID      string          `json:"id"`
	Event   json.RawMessage `json:"event"`
	Reason  string          `json:"reason"`
	Code    string          `json:"code"`
	Message string          `json:"message"`
}

// readLive subscribes conn to the events view and reads it until the socket ends, calling onFrame
// for each event frame with at set from now() and the marker of the run with this nonce. The order
// check starts over with each call, one connection. A subscription Dispatch ends (an ended frame,
// when it fell behind) is subscribed again at once, as a dashboard tab does, once it had started;
// one ended before its events_start, or refused with an error frame, ends the read, and the caller
// reconnects after its pause, so a feed that cannot start is not asked again in a tight loop. It
// returns the bytes of every message read and the error that ended the socket.
func readLive(conn *websocket.Conn, nonce int64, now func() time.Time, onFrame func(liveFrame)) (int64, error) {
	if err := conn.WriteJSON(liveSubscribe()); err != nil {
		return 0, err
	}
	var n, prevID int64
	havePrev, started := false, false
	for {
		_, data, err := conn.ReadMessage()
		if err != nil {
			return n, err
		}
		at := now().UnixNano()
		n += int64(len(data))
		var m liveMessage
		if json.Unmarshal(data, &m) != nil || m.Sub != liveSub {
			continue
		}
		switch m.Type {
		case "events_start":
			started = true
		case "ended":
			if !started {
				return n, fmt.Errorf("the events subscription ended before it started (%s)", m.Reason)
			}
			started = false
			if err := conn.WriteJSON(liveSubscribe()); err != nil {
				return n, err
			}
		case "error":
			return n, fmt.Errorf("the events subscription was refused: %s: %s", m.Code, m.Message)
		case "event":
			f := liveFrame{at: at}
			f.id, _ = strconv.ParseInt(m.ID, 10, 64)
			f.marker, _ = findMarker(m.Event, nonce)
			var ev struct {
				CreatedAt time.Time `json:"created_at"`
			}
			if json.Unmarshal(m.Event, &ev) == nil && !ev.CreatedAt.IsZero() {
				f.created = ev.CreatedAt.UnixNano()
			}
			f.outOfOrder = havePrev && f.id <= prevID
			prevID, havePrev = f.id, true
			onFrame(f)
		}
	}
}

// followLive keeps one dashboard live socket open on url until ctx ends: it dials with header,
// reads the socket (readLive), and dials again retry after the socket ends or a dial fails, as
// the dashboard stream reconnected a second after its end. Dispatch closes every socket at a
// random 30-60 min age (4001) and at shutdown (1012). onBytes gets each socket's bytes read.
func followLive(ctx context.Context, dialer *websocket.Dialer, url string, header http.Header, nonce int64, retry time.Duration, onFrame func(liveFrame), onBytes func(int64)) {
	for ctx.Err() == nil {
		if conn, resp, err := dialer.DialContext(ctx, url, header); err == nil {
			stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
			n, _ := readLive(conn, nonce, time.Now, onFrame)
			stop()
			_ = conn.Close()
			onBytes(n)
		} else if resp != nil {
			_ = resp.Body.Close()
		}
		sleepUntil(ctx, time.Now().Add(retry))
	}
}

// liveSocketURL is the ws:// (or wss://) URL of Dispatch's GET /ws/live, from its http(s) base.
func liveSocketURL(dispatch string) (string, error) {
	if rest, ok := strings.CutPrefix(dispatch, "http://"); ok {
		return "ws://" + rest + "/ws/live", nil
	}
	if rest, ok := strings.CutPrefix(dispatch, "https://"); ok {
		return "wss://" + rest + "/ws/live", nil
	}
	return "", fmt.Errorf("the Dispatch URL %s is neither http:// nor https://", redactURL(dispatch))
}

// liveSummary times the dashboard live socket frames that arrived in the window.
type liveSummary struct {
	Frames int `json:"live_frames"`
	// OrderViolations counts frames whose event id is not above the previous frame's on the same
	// connection.
	OrderViolations int `json:"live_order_violations"`
	// FrameMsFromCreatedAt is the frame's arrival minus its event's created_at, over every frame
	// with a created_at.
	FrameMsFromCreatedAt stat `json:"live_frame_ms_from_created_at"`
	// FrameMsFromWriteResponse is the frame's arrival minus the response of the write whose
	// primary event the frame carries, over the frames of in-window writes that got a 2xx status
	// and committed.
	FrameMsFromWriteResponse stat `json:"live_frame_ms_from_write_response"`
}

// summarizeLive reduces the in-window frames of every live socket. A frame is timed from its
// write's response when it carries the write's marker and is the write's primary event
// (primaryCommit) among the commit rows, as a NATS receipt is.
func summarizeLive(frames []liveFrame, writes []writeRec, commits []commitRow) liveSummary {
	byMarker := commitsByMarker(commits)
	type timed struct{ primaryID, response int64 }
	committed := map[int64]timed{} // by marker
	for _, w := range writes {
		if !w.InWindow || w.TransportErr != "" || w.Status/100 != 2 {
			continue
		}
		if c := primaryCommit(byMarker[w.Marker], primaryEventType[w.Kind]); c != nil {
			committed[w.Marker] = timed{primaryID: c.EventID, response: w.Response}
		}
	}
	out := liveSummary{Frames: len(frames)}
	var fromCreated, fromResponse []float64
	for _, f := range frames {
		if f.outOfOrder {
			out.OrderViolations++
		}
		if f.created != 0 {
			fromCreated = append(fromCreated, float64(f.at-f.created)/1e6)
		}
		if t, ok := committed[f.marker]; ok && f.id == t.primaryID {
			fromResponse = append(fromResponse, float64(f.at-t.response)/1e6)
		}
	}
	out.FrameMsFromCreatedAt = mkStat(fromCreated)
	out.FrameMsFromWriteResponse = mkStat(fromResponse)
	return out
}
