package main

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

// The dashboard streams a Dispatch build serves. A build from #2449 on gives a tab one live socket
// (/ws/live) and serves no SSE; one before it serves the SSE feed (GET /api/v1/events) and no live
// socket. The replay measures builds of both kinds, so the harness opens whichever the build under
// test serves and records it in summary.json as config.dashboard.
const (
	dashboardLive = "live"
	dashboardSSE  = "sse"
)

// probeDashboard asks the Dispatch at dispatch which dashboard stream it serves, once, before the
// window: it dials the live socket, and, when that is refused, asks for the SSE feed. It returns
// the kind (dashboardLive or dashboardSSE) and the URL the step's streams open. A build that
// serves neither is an error naming what each answered: the step would measure no frames.
func probeDashboard(ctx context.Context, dispatch string) (string, string, error) {
	liveURL, err := liveSocketURL(dispatch)
	if err != nil {
		return "", "", err
	}
	header := http.Header{"X-Dispatch-User": {"viewer-probe"}}
	dialer := &websocket.Dialer{HandshakeTimeout: 10 * time.Second}
	conn, resp, dialErr := dialer.DialContext(ctx, liveURL, header)
	if dialErr == nil {
		_ = conn.Close()
		return dashboardLive, liveURL, nil
	}
	liveAnswer := dialErr.Error()
	if resp != nil {
		liveAnswer = resp.Status
		_ = resp.Body.Close()
	}

	sseURL := dispatch + ssePath
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, sseURL, nil)
	if err != nil {
		return "", "", err
	}
	req.Header = header.Clone()
	req.Header.Set("Accept", "text/event-stream")
	client := &http.Client{Timeout: 30 * time.Second}
	sseResp, sseErr := client.Do(req)
	sseAnswer := "no answer"
	if sseErr != nil {
		sseAnswer = sseErr.Error()
	}
	if sseErr == nil {
		sseAnswer = sseResp.Status
		streams := sseResp.StatusCode == http.StatusOK &&
			strings.HasPrefix(sseResp.Header.Get("Content-Type"), "text/event-stream")
		_ = sseResp.Body.Close()
		if streams {
			return dashboardSSE, sseURL, nil
		}
	}
	return "", "", fmt.Errorf("the Dispatch at %s serves no dashboard stream: /ws/live answered %s, and GET %s answered %s",
		redactURL(dispatch), liveAnswer, ssePath, sseAnswer)
}
