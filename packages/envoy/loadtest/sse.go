package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strconv"
	"time"
)

// sseMediaType is the media type of Dispatch's SSE endpoint, which a dashboard stream both asks
// for (Accept) and reads (Content-Type).
const sseMediaType = "text/event-stream"

// sseEventsPath is Dispatch's SSE endpoint, the event feed a dashboard tab followed before the
// dashboard live socket replaced it. A Dispatch from before that change serves this path and no
// /ws/live, so a step against one follows the feed here (liveTransportSSE).
const sseEventsPath = "/api/v1/events"

// sseEventsURL is the URL of Dispatch's SSE endpoint, from its http(s) base.
func sseEventsURL(dispatch string) string { return dispatch + sseEventsPath }

// readSSE parses one SSE connection's body until it ends, calling onFrame for each data frame with
// the very fields the live socket's reader gives (readLive): at set from now(), the event's id and
// created_at, the marker of the run with this nonce, and the order check of this connection.
// Dispatch writes each event as `id: <n>\nevent: <type>\ndata: <json>\n\n`
// (internal/dispatch/api/events.go writeSSEEvent); lines starting with ':' are comments (`: open`,
// `: heartbeat`). It returns the bytes read and the error that ended the body (io.EOF at a clean
// end). A partial frame at the end is dropped: a frame counts only on its closing blank line.
func readSSE(body io.Reader, nonce int64, now func() time.Time, onFrame func(liveFrame)) (int64, error) {
	br := bufio.NewReaderSize(body, 64<<10)
	var n int64
	var data []byte
	var idField, prevID int64
	hasData, havePrev := false, false
	for {
		line, err := br.ReadBytes('\n')
		n += int64(len(line))
		if err != nil {
			return n, err
		}
		line = bytes.TrimSuffix(bytes.TrimSuffix(line, []byte("\n")), []byte("\r"))
		if len(line) == 0 {
			if hasData {
				f := liveFrame{at: now().UnixNano(), id: idField}
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
			data, idField, hasData = data[:0], 0, false
			continue
		}
		if line[0] == ':' {
			continue
		}
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

// followSSE keeps one dashboard SSE stream open on url until ctx ends: it asks for the event
// stream with header, reads the body (readSSE), and asks again retry after the stream ends or the
// request fails, as the dashboard stream reconnected a second after its end. onBytes gets each
// stream's bytes read.
func followSSE(ctx context.Context, client *http.Client, url string, header http.Header, nonce int64, retry time.Duration, onFrame func(liveFrame), onBytes func(int64)) {
	h := header.Clone()
	if h == nil {
		h = http.Header{}
	}
	h.Set("Accept", sseMediaType)
	for ctx.Err() == nil {
		if req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil); err == nil {
			req.Header = h
			if resp, err := client.Do(req); err == nil {
				n, _ := readSSE(resp.Body, nonce, time.Now, onFrame)
				_ = resp.Body.Close()
				onBytes(n)
			}
		}
		sleepUntil(ctx, time.Now().Add(retry))
	}
}

// probeSSE asks the SSE endpoint for one stream and closes it at once: ok is set when it answered
// 200 with an event-stream body, and answer says what it did, for the refusal that names it.
func probeSSE(ctx context.Context, url string, header http.Header) (ok bool, answer string) {
	ctx, cancel := context.WithTimeout(ctx, liveDialTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return false, "could not be asked: " + err.Error()
	}
	req.Header = header.Clone()
	req.Header.Set("Accept", sseMediaType)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false, "could not be reached: " + err.Error()
	}
	defer resp.Body.Close()
	media, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	switch {
	case resp.StatusCode != http.StatusOK:
		return false, fmt.Sprintf("answered HTTP %s with content type %q", resp.Status, media)
	case media != sseMediaType:
		return false, fmt.Sprintf("answered HTTP %s with content type %q, not %s", resp.Status, media, sseMediaType)
	}
	return true, fmt.Sprintf("answered HTTP %s with content type %s", resp.Status, media)
}
