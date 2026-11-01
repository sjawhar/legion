package main

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	gws "github.com/gorilla/websocket"
	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/cmdtest"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/startup"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/testnats"
)

// natsDownCommit is the commit the process started while NATS is down is stamped with, which its
// server.started announcement names.
const natsDownCommit = "5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a"

// A process started while NATS is unreachable serves: /healthz answers 200 with `ok` true and
// `nats` false, an API read answers, an agent view on the live socket ends at once as `dropped`,
// which the page answers by subscribing again, rather than holding a view nothing reaches, and the
// webhook sweeper's first pass runs and says why it swept nothing. Once the server answers, every
// client connects with no restart: `nats` reads true, an issue written afterwards has its event on
// the stream, the announcement of the stamped build is on it, both delivery intakes bind their
// durables, and an agent view relays a session's frame. Before INFRA-555 the boot exited on the
// first failed connect.
func TestAProcessStartedWhileNATSIsDownServesAndConnectsLater(t *testing.T) {
	database := storetest.Open(t)
	seedDeliverySettings(t, database)
	srv, natsURL := testnats.Start(t)
	srv.Stop(t)
	github := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, "[]")
	}))
	t.Cleanup(github.Close)
	process := startDispatchProcessWith(t, database.Pool.Config().ConnString(), dispatchOptions{
		NATSURL: natsURL,
		Binary:  cmdtest.BuildStamped(t, "envoy-dispatch-nats-down", natsDownCommit),
		Env:     githubAppEnv(t, github.URL),
	})

	process.waitHealthy(t)
	if status, health := process.health(t); status != http.StatusOK || !health.OK || health.NATS == nil || *health.NATS {
		t.Fatalf("/healthz while NATS is down = %d %+v, want 200 with ok true and nats false", status, health)
	}
	process.call(t, http.MethodGet, "/api/v1/projects", "", http.StatusOK)
	if frame := process.openAgentView(t); frame.Type != "ended" || frame.Reason != "dropped" {
		t.Fatalf("an agent view while NATS is down first sent %+v, want it ended as dropped", frame)
	}
	process.waitForLog(t, 30*time.Second, `msg="webhook redelivery sweep failed"`, "NATS is not connected")

	srv.Start(t)
	process.waitForHealth(t, 30*time.Second, "nats true", func(health healthBody) bool {
		return health.OK && health.NATS != nil && *health.NATS
	})
	js, err := testnats.Connect(t, natsURL).JetStream()
	if err != nil {
		t.Fatalf("JetStream context: %v", err)
	}
	waitUntil(t, 30*time.Second, "the stream the process owns", func() bool {
		_, err := js.StreamInfo(bus.Stream)
		return err == nil
	})
	key, _ := process.createIssue(t)
	issueTopic := "notifications.dispatch.issue." + key + "."
	waitUntil(t, 30*time.Second, "the issue's event and the build's announcement on the stream", func() bool {
		var issued, announced bool
		for _, item := range readStreamFrom(t, js, 0) {
			if strings.HasPrefix(item.Topic, issueTopic) {
				issued = true
			}
			var payload startup.Payload
			if item.Topic == startup.Subject && json.Unmarshal([]byte(item.Payload), &payload) == nil && payload.Commit == natsDownCommit {
				announced = true
			}
		}
		return issued && announced
	})
	for _, durable := range deliveryDurables {
		process.waitForLog(t, 30*time.Second, `msg="dispatch delivery: NATS consumer bound"`, "name="+durable)
	}
	process.requireAgentViewRelays(t, testnats.Connect(t, natsURL))
}

// The server's NATS client declares a connection whose server stops answering without closing it
// lost within seconds, so /healthz reports `nats` false within five seconds of a silent cut, with
// 200 throughout, and true again once the traffic moves. The deploy's measuring job counts its
// limits from the cut; with nats.go's default pings the probe read true for minutes into it.
func TestAProcessReportsASilentNATSCutWithinSeconds(t *testing.T) {
	database := storetest.Open(t)
	srv, natsURL := testnats.Start(t)
	process := startDispatchProcessWith(t, database.Pool.Config().ConnString(), dispatchOptions{NATSURL: natsURL})
	process.waitHealthy(t)
	process.waitForHealth(t, 30*time.Second, "nats true", func(health healthBody) bool {
		return health.NATS != nil && *health.NATS
	})

	thaw := srv.Freeze(t)
	// Cleanups run last-in first-out, so this thaw precedes the server's shutdown.
	t.Cleanup(thaw)
	cut := time.Now()
	for {
		status, health := process.health(t)
		if status != http.StatusOK || !health.OK {
			t.Fatalf("/healthz %s into a silent cut = %d %+v, want 200 with ok true", time.Since(cut), status, health)
		}
		if health.NATS != nil && !*health.NATS {
			break
		}
		if time.Since(cut) > 5*time.Second {
			t.Fatalf("/healthz still reports nats true %s into a silent cut", time.Since(cut))
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Logf("/healthz reported the cut %s after it began", time.Since(cut))

	thaw()
	process.waitForHealth(t, 30*time.Second, "nats true after the cut heals", func(health healthBody) bool {
		return health.OK && health.NATS != nil && *health.NATS
	})
}

// healthBody is the part of /healthz these tests read.
type healthBody struct {
	OK            bool  `json:"ok"`
	NATS          *bool `json:"nats"`
	SignIn        *bool `json:"signin"`
	ServiceTokens *bool `json:"service_tokens"`
}

// health reads /healthz once, bounded so a probe that hangs fails the test.
func (p *dispatchProcess) health(t *testing.T) (int, healthBody) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, p.url("/healthz"), nil)
	if err != nil {
		t.Fatalf("build the /healthz request: %v", err)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	defer response.Body.Close()
	var health healthBody
	if err := json.NewDecoder(response.Body).Decode(&health); err != nil {
		t.Fatalf("decode /healthz: %v", err)
	}
	return response.StatusCode, health
}

// waitForHealth waits up to within for /healthz to answer 200 with a body ready accepts.
func (p *dispatchProcess) waitForHealth(t *testing.T, within time.Duration, what string, ready func(healthBody) bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		status, health := p.health(t)
		if status == http.StatusOK && ready(health) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("/healthz never reported %s within %v; last %d %+v:\n%s", what, within, status, health, p.Output.String())
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// waitUntil polls done until it holds, failing the test once within has passed.
func waitUntil(t *testing.T, within time.Duration, what string, done func() bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for !done() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %v waiting for %s", within, what)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// agentViewFrame is a frame the live socket sends an agent view: an agent frame (Event, Data), the
// view's end (Reason) or its refusal (Code).
type agentViewFrame struct {
	Type   string          `json:"type"`
	Sub    string          `json:"sub"`
	Event  string          `json:"event"`
	Data   json.RawMessage `json:"data"`
	Reason string          `json:"reason"`
	Code   string          `json:"code"`
}

// startAgentView opens GET /ws/live as the test's login and subscribes it to streamedSessionID's
// agent view, as an agent's page does. The socket closes when the test ends.
func (p *dispatchProcess) startAgentView(t *testing.T) *gws.Conn {
	t.Helper()
	conn, response, err := gws.DefaultDialer.Dial("ws://127.0.0.1:"+strconv.Itoa(p.port)+"/ws/live", http.Header{
		"X-Dispatch-User": {dispatchTestLogin},
	})
	if err != nil {
		status := "no response"
		if response != nil {
			status = response.Status
		}
		t.Fatalf("open the live socket: %v (%s)", err, status)
	}
	t.Cleanup(func() { _ = conn.Close() })
	if err := conn.WriteJSON(map[string]any{"type": "subscribe", "sub": "agent", "view": map[string]string{"kind": "agent", "session_id": streamedSessionID}}); err != nil {
		t.Fatalf("subscribe the live socket to the agent view: %v", err)
	}
	return conn
}

// nextAgentViewFrame reads conn up to the next frame of its agent view, passing over heartbeats, and
// fails the test when none arrives within ten seconds, so a view held open with nothing in it fails
// rather than hangs.
func (p *dispatchProcess) nextAgentViewFrame(t *testing.T, conn *gws.Conn) agentViewFrame {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	defer func() { _ = conn.SetReadDeadline(time.Time{}) }()
	for {
		var frame agentViewFrame
		if err := conn.ReadJSON(&frame); err != nil {
			t.Fatalf("read the agent view: %v\n%s", err, p.Output.String())
		}
		if frame.Sub == "agent" {
			return frame
		}
	}
}

// openAgentView subscribes a new live socket to the agent view and returns the view's first frame.
func (p *dispatchProcess) openAgentView(t *testing.T) agentViewFrame {
	t.Helper()
	conn := p.startAgentView(t)
	frame := p.nextAgentViewFrame(t, conn)
	_ = conn.Close()
	return frame
}

// requireAgentViewRelays subscribes agent views, as the page does after each `dropped`, until the
// relay's own connection has connected and a view opens with the session's responder verdict, then
// requires a frame the session publishes on conn to reach that view.
func (p *dispatchProcess) requireAgentViewRelays(t *testing.T, conn *natsgo.Conn) {
	t.Helper()
	var view *gws.Conn
	waitUntil(t, 30*time.Second, "an agent view to open with the session's responder verdict", func() bool {
		view = p.startAgentView(t)
		frame := p.nextAgentViewFrame(t, view)
		if frame.Type == "agent" && frame.Event == "responder" {
			return true
		}
		_ = view.Close()
		return false
	})
	const frame = `{"v":1,"kind":"message","seq":1}`
	if err := conn.Publish(contracts.AgentStreamFramesSubject(streamedSessionID), []byte(frame)); err != nil {
		t.Fatalf("publish the session's frame: %v", err)
	}
	if err := conn.Flush(); err != nil {
		t.Fatalf("flush the session's frame: %v", err)
	}
	for {
		got := p.nextAgentViewFrame(t, view)
		if got.Type == "agent" && got.Event == "replay" {
			continue
		}
		if got.Type != "agent" || got.Event != "frame" || string(got.Data) != frame {
			t.Fatalf("the agent view relayed %+v (data %s), want the session's frame", got, got.Data)
		}
		return
	}
}

// ───── shared with multiprocess_test.go and the two-build tests (processes_test.go) ──

// logLines is every line of the process's output that holds all of parts, which is how a slog
// text line is matched: the message and the attribute that narrows it.
func (p *dispatchProcess) logLines(parts ...string) []string {
	var matched []string
	for line := range strings.Lines(p.Output.String()) {
		if holdsAll(line, parts...) {
			matched = append(matched, strings.TrimRight(line, "\n"))
		}
	}
	return matched
}

func holdsAll(line string, parts ...string) bool {
	for _, part := range parts {
		if !strings.Contains(line, part) {
			return false
		}
	}
	return true
}

// logged reports whether the process has written a line holding all of parts.
func (p *dispatchProcess) logged(parts ...string) bool {
	return len(p.logLines(parts...)) > 0
}

// waitForLog waits up to within for a line holding all of parts, polling rather than sleeping a
// fixed time, and fails the test with the process's whole output when none arrives, at once when
// the process exits without it: a process that has exited writes no more lines.
func (p *dispatchProcess) waitForLog(t *testing.T, within time.Duration, parts ...string) {
	t.Helper()
	deadline := time.Now().Add(within)
	for !p.logged(parts...) {
		select {
		case <-p.Exited:
			// Exited closes only once the output is whole, so this read is final.
			if p.logged(parts...) {
				return
			}
			t.Fatalf("the process exited without a line holding %v:\n%s", parts, p.Output.String())
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("no line holding %v within %v:\n%s", parts, within, p.Output.String())
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// readStreamFrom is every envelope the stream retains past head, read by sequence rather than
// through a consumer so the read creates nothing on the server and cannot miss a message a
// consumer's filter would skip. A sequence the stream no longer holds is passed over.
func readStreamFrom(t *testing.T, js natsgo.JetStreamContext, head uint64) []contracts.Envelope {
	t.Helper()
	info, err := js.StreamInfo(bus.Stream)
	if err != nil {
		t.Fatalf("read the notification stream: %v", err)
	}
	var items []contracts.Envelope
	for seq := head + 1; seq <= info.State.LastSeq; seq++ {
		msg, err := js.GetMsg(bus.Stream, seq)
		if err != nil {
			if errors.Is(err, natsgo.ErrMsgNotFound) {
				continue
			}
			t.Fatalf("read message %d of the notification stream: %v", seq, err)
		}
		var item contracts.Envelope
		if err := json.Unmarshal(msg.Data, &item); err != nil {
			t.Fatalf("decode message %d of the notification stream: %v", seq, err)
		}
		items = append(items, item)
	}
	return items
}

// deliveryDurables are the two push durables the delivery intakes bind, one subscriber each.
var deliveryDurables = []string{"delivery-events", "delivery-jobs"}

// seedDeliverySettings writes the one delivery_settings row, the same write
// PUT /api/v1/settings/delivery makes (delivery.PutSettings). The intakes read it on their first
// poll and bind nothing until it is there, so a row about the durables seeds it before the
// processes start rather than driving the route, which would first have the GitHub App prove it
// can read the repository.
func seedDeliverySettings(t *testing.T, database *store.Store) {
	t.Helper()
	if _, err := delivery.PutSettings(context.Background(), database.Pool, delivery.DeliverySettings{
		DeployRepo:           "acme/deploy",
		DeployWorkflowPath:   ".github/workflows/deploy.yaml",
		PRChecksWorkflowPath: ".github/workflows/pr.yaml",
		ProductionJobNames:   []string{"production"},
	}, model.Actor{Kind: "user", ID: dispatchTestLogin}); err != nil {
		t.Fatalf("seed the delivery settings: %v", err)
	}
}

// githubAppEnv is a GitHub App a Dispatch process boots with: a throwaway RSA key generated for
// this test, and apiBase as the only origin its App JWT is ever sent to. Without an App the
// delivery intakes never connect and the webhook redelivery sweep never runs, so a row about
// either needs these rows.
func githubAppEnv(t *testing.T, apiBase string) map[string]string {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate the test App key: %v", err)
	}
	encoded := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	return map[string]string{
		"DISPATCH_APP_CLIENT_ID":   "Iv1.multiprocess",
		"DISPATCH_APP_ID":          "4242",
		"DISPATCH_APP_SLUG":        "multiprocess",
		"DISPATCH_APP_PEM_B64":     base64.StdEncoding.EncodeToString(encoded),
		"DISPATCH_GITHUB_API_BASE": apiBase,
	}
}
