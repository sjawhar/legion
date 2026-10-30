package main

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"net/http"
	"strconv"
	"time"
)

// ssePath is the dashboard's server-sent-event feed, GET /api/v1/events. Dispatch served it until
// every live update moved onto one live socket per tab (#2449), which removed the route. The
// replay runs against builds of both kinds, so the harness keeps this reader for the builds that
// serve no /ws/live (probeDashboard).
const ssePath = "/api/v1/events"

// readSSE parses one SSE connection's body until it ends, calling onFrame for each data frame with
// at set from now() and the marker of the run with this nonce. Dispatch writes each event as
// `id: <n>\nevent: <type>\ndata: <json>\n\n`; lines starting with ':' are comments (`: open`,
// `: heartbeat`). It returns the bytes read and the error that ended the body (io.EOF at a clean
// end). A partial frame at the end is dropped: a frame counts only on its closing blank line.
func readSSE(body io.Reader, nonce int64, now func() time.Time, onFrame func(liveFrame)) (int64, error) {
	br := bufio.NewReaderSize(body, 64<<10)
	var n int64
	var data []byte
	var idField int64
	fr := liveFrames{nonce: nonce}
	hasData := false
	for {
		line, err := br.ReadBytes('\n')
		n += int64(len(line))
		if err != nil {
			return n, err
		}
		line = bytes.TrimSuffix(bytes.TrimSuffix(line, []byte("\n")), []byte("\r"))
		if len(line) == 0 {
			if hasData {
				onFrame(fr.frame(now().UnixNano(), idField, data, true))
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

// followSSE keeps one dashboard SSE stream open on url until ctx ends: it sends the request with
// header, reads the stream (readSSE), and asks again retry after the stream ends or the request
// fails, as the dashboard reconnected a second after its stream ended. onBytes gets each stream's
// bytes read.
func followSSE(ctx context.Context, client *http.Client, url string, header http.Header, nonce int64, retry time.Duration, onFrame func(liveFrame), onBytes func(int64)) {
	for ctx.Err() == nil {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return
		}
		req.Header = header
		if resp, err := client.Do(req); err == nil {
			n, _ := readSSE(resp.Body, nonce, time.Now, onFrame)
			_ = resp.Body.Close()
			onBytes(n)
		}
		sleepUntil(ctx, time.Now().Add(retry))
	}
}
