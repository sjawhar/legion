package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"strconv"
	"time"
)

// sseFrame is one data frame a dashboard SSE stream received. Dispatch writes each event as
// `id: <n>\nevent: <type>\ndata: <json>\n\n` (internal/dispatch/api/events.go writeSSEEvent);
// lines starting with ':' are comments (`: open`, `: heartbeat`).
type sseFrame struct {
	at         int64 // Unix ns when the frame's closing blank line arrived
	id         int64 // the event's id (data JSON "id"; the frame's id: field when the JSON has none)
	created    int64 // Unix ns of the event's created_at; 0 when absent or unparsable
	marker     int64 // the counter of the run's marker in the frame's data (findMarker); 0 when none
	bytes      int64 // the frame's bytes: its field lines and its closing blank line
	outOfOrder bool  // id <= the previous frame's id on the same connection
	// replay is set by a resuming stream (-sse-resume) on a frame its reconnect replayed: one
	// whose id is at or below the event log's max(id) read just before the reconnect.
	replay bool
}

// readSSE parses one SSE connection's body until it ends, calling onFrame for each data frame with
// at set from now() and the marker of the run with this nonce. It returns the bytes read and the
// error that ended the body (io.EOF at a clean end). A partial frame at the end is dropped: a
// frame counts only on its closing blank line.
func readSSE(body io.Reader, nonce int64, now func() time.Time, onFrame func(sseFrame)) (int64, error) {
	br := bufio.NewReaderSize(body, 64<<10)
	var n, frameBytes int64
	var data []byte
	var idField, prevID int64
	hasData, havePrev := false, false
	for {
		line, err := br.ReadBytes('\n')
		n += int64(len(line))
		if err != nil {
			return n, err
		}
		size := int64(len(line))
		line = bytes.TrimSuffix(bytes.TrimSuffix(line, []byte("\n")), []byte("\r"))
		if len(line) == 0 {
			if hasData {
				f := sseFrame{at: now().UnixNano(), id: idField, bytes: frameBytes + size}
				f.marker, _ = findMarker(data, nonce)
				var ev struct {
					ID        *int64    `json:"id"`
					CreatedAt time.Time `json:"created_at"`
				}
				if json.Unmarshal(data, &ev) == nil {
					if ev.ID != nil {
						f.id = *ev.ID
					}
					if !ev.CreatedAt.IsZero() {
						f.created = ev.CreatedAt.UnixNano()
					}
				}
				f.outOfOrder = havePrev && f.id <= prevID
				prevID, havePrev = f.id, true
				onFrame(f)
			}
			data, idField, hasData, frameBytes = data[:0], 0, false, 0
			continue
		}
		if line[0] == ':' {
			continue
		}
		frameBytes += size
		field, value, _ := bytes.Cut(line, []byte(":"))
		value = bytes.TrimPrefix(value, []byte(" "))
		switch string(field) {
		case "data":
			if hasData {
				data = append(data, '\n')
			}
			data = append(data, value...)
			hasData = true
		case "id":
			idField, _ = strconv.ParseInt(string(value), 10, 64)
		}
	}
}

// sseSummary times the dashboard SSE frames that arrived in the window.
type sseSummary struct {
	Frames int `json:"sse_frames"`
	// OrderViolations counts frames whose event id is not above the previous frame's on the same
	// connection.
	OrderViolations int `json:"sse_order_violations"`
	// FrameMsFromCreatedAt is the frame's arrival minus its event's created_at, over every live
	// frame with a created_at.
	FrameMsFromCreatedAt stat `json:"sse_frame_ms_from_created_at"`
	// FrameMsFromWriteResponse is the frame's arrival minus the response of the write whose
	// primary event the frame carries, over the live frames of in-window writes that got a 2xx
	// status and committed.
	FrameMsFromWriteResponse stat `json:"sse_frame_ms_from_write_response"`
}

// summarizeSSE reduces the in-window frames of every SSE connection. A frame is timed from its
// write's response when it carries the write's marker and is the write's primary event
// (primaryCommit) among the commit rows, as a NATS receipt is. A replayed frame (sseFrame.replay)
// counts, but is not timed: its delay is the stream's absence, not delivery.
func summarizeSSE(frames []sseFrame, writes []writeRec, commits []commitRow) sseSummary {
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
	out := sseSummary{Frames: len(frames)}
	var fromCreated, fromResponse []float64
	for _, f := range frames {
		if f.outOfOrder {
			out.OrderViolations++
		}
		if f.replay {
			continue
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
