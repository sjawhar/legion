package main

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dedupe"
	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/internal/session"
	"github.com/sjawhar/envoy/internal/store"
)

// sessionPort is a port session's serving port: an httptest server that answers every prompt_async
// 204 and records the address each prompt was sent to. Its client dials the server whatever address
// a prompt names, so a delivery to an address that is not loopback still lands here and is counted.
type sessionPort struct {
	server *httptest.Server
	mu     sync.Mutex
	hosts  []string
}

func newSessionPort(t *testing.T) *sessionPort {
	t.Helper()
	port := &sessionPort{}
	port.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		port.mu.Lock()
		port.hosts = append(port.hosts, r.Host)
		port.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(port.server.Close)
	return port
}

func (p *sessionPort) client() *http.Client {
	addr := p.server.Listener.Addr().String()
	return &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, addr)
	}}}
}

func (p *sessionPort) prompted() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.hosts...)
}

// podDeliveryConfig is the harness's delivery configuration as the pod podHost, whose bridge is
// bridge, sees it: its prompts go to port. The deliverer is a seam of the configuration the
// delivery workers keep by value, so the copy is wired again (wireTestDeliveryWorkers) and the
// durable path's pushes go through this pod's deliverer rather than the harness's.
func podDeliveryConfig(t *testing.T, harness listenerDeliveryHarness, port *sessionPort, bridge, podHost string) listenerDeliveryHandlerConfig {
	t.Helper()
	cfg := harness.config
	deliverer := *cfg.deliverer
	deliverer.HostBridge = bridge
	deliverer.PodHost = podHost
	deliverer.HTTPClient = port.client()
	cfg.deliverer = &deliverer
	return wireTestDeliveryWorkers(t, cfg)
}

// A port session whose address is local to the delivering pod (a loopback bridge) and whose
// registration another pod accepted is not prompted at this pod's loopback, where its port is not:
// the pod publishes delivery_failed for it, names the reason in its log, acks the message (no
// retry would reach the session from this pod) and leaves the pair undelivered in the ledger, so a
// resend after the session registers through this pod is prompted. One case per lane path: an inbox
// message, and a topic fanned out to the session.
func TestAPodLocalAddressOfASessionAnotherPodAcceptedIsReportedNotPrompted(t *testing.T) {
	for name, topic := range map[string]string{
		"inbox":  contracts.AgentSubject("ses_elsewhere"),
		"fanout": "notifications.github.owner.repo.issue.7",
	} {
		t.Run(name, func(t *testing.T) {
			harness := newListenerDeliveryHarness(t, nil)
			port := newSessionPort(t)
			cfg := podDeliveryConfig(t, harness, port, "127.0.0.1", "pod-b")
			if _, err := harness.registry.Upsert(store.Interest{SessionID: "ses_elsewhere", MachineID: "test-machine"}, []string{topic}); err != nil {
				t.Fatalf("register the interest: %v", err)
			}
			if err := harness.sessions.Put("ses_elsewhere", session.SessionEntry{Port: 4096, MachineID: "test-machine", AcceptedHost: "pod-a"}); err != nil {
				t.Fatalf("register the session through pod a: %v", err)
			}
			item := listenerTestEnvelope(topic, "pod-local-"+name)
			probe, err := harness.client.Conn.SubscribeSync("notifications.envoy.exceptions." + item.Topic)
			if err != nil {
				t.Fatalf("subscribe to the exception topic: %v", err)
			}
			t.Cleanup(func() { _ = probe.Unsubscribe() })

			msg, settlement := settledMessage(t, harness.client.Conn, marshalListenerEnvelope(t, item))
			jetStreamDeliveryHandler(cfg)(context.Background(), msg)

			if got := settlement(t); got != "+ACK" {
				t.Fatalf("settlement = %q, want an ack: no retry from this pod reaches the session", got)
			}
			if hosts := port.prompted(); len(hosts) != 0 {
				t.Fatalf("the session port was prompted at %q, want no prompt", hosts)
			}
			exception := assertDeliveryException(t, probe, item, "delivery_failed")
			if recipient := exceptionRecipient(t, exception); recipient != "ses_elsewhere" {
				t.Fatalf("exception recipient = %q, want ses_elsewhere", recipient)
			}
			if want := deliveryExceptionID(item.EventID, "ses_elsewhere", "delivery_failed"); exception.EventID != want {
				t.Fatalf("exception id = %q, want the deterministic %q", exception.EventID, want)
			}
			if logs := harness.logs.String(); !strings.Contains(logs, session.ErrAddressNotReachable.Error()) {
				t.Fatalf("the log does not name %q:\n%s", session.ErrAddressNotReachable, logs)
			}
			attempt, err := testDeliveryLedger(t, harness.client.Conn, "test-machine", "boot-pod-a").Attempt(item.DedupeKey, "ses_elsewhere")
			if err != nil || attempt.Verdict != dedupe.Proceed {
				t.Fatalf("the ledger's answer for the refused pair = %v, %v; want it neither delivered nor in flight", attempt.Verdict, err)
			}
		})
	}
}

// The refusal is only for a pod-local address on a pod that did not accept the registration: the
// accepting pod (pods on one host under host networking share its name), a registration an older
// build wrote without an accepting pod, and a bridge that is not loopback each prompt the session.
func TestAPortSessionIsPromptedWhereItsAddressReachesIt(t *testing.T) {
	for _, tc := range []struct {
		name     string
		bridge   string
		accepted string
		want     string
	}{
		{name: "the pod that accepted the registration", bridge: "127.0.0.1", accepted: "pod-b", want: "127.0.0.1:4096"},
		{name: "a registration without an accepting pod", bridge: "127.0.0.1", want: "127.0.0.1:4096"},
		{name: "a bridge that is not loopback", bridge: "10.0.0.5", accepted: "pod-a", want: "10.0.0.5:4096"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			harness := newListenerDeliveryHarness(t, nil)
			port := newSessionPort(t)
			cfg := podDeliveryConfig(t, harness, port, tc.bridge, "pod-b")
			item := listenerTestEnvelope(contracts.AgentSubject("ses_reachable"), "reachable")
			if _, err := harness.registry.Upsert(store.Interest{SessionID: "ses_reachable", MachineID: "test-machine"}, []string{item.Topic}); err != nil {
				t.Fatalf("register the interest: %v", err)
			}
			if err := harness.sessions.Put("ses_reachable", session.SessionEntry{Port: 4096, MachineID: "test-machine", AcceptedHost: tc.accepted}); err != nil {
				t.Fatalf("register the session: %v", err)
			}

			msg, settlement := settledMessage(t, harness.client.Conn, marshalListenerEnvelope(t, item))
			jetStreamDeliveryHandler(cfg)(context.Background(), msg)

			if got := settlement(t); got != "+ACK" {
				t.Fatalf("settlement = %q, want an ack", got)
			}
			if hosts := port.prompted(); len(hosts) != 1 || hosts[0] != tc.want {
				t.Fatalf("the session port was prompted at %q, want once at %s", hosts, tc.want)
			}
		})
	}
}

// The subscribe handler stamps a port registration with the host name of the pod that accepted it,
// and a heartbeat another pod accepts re-stamps it with that pod's: the latest pod to hear from the
// session. A role set keeps the stamp, and a registration with no port, which nobody dials, gets none.
func TestSubscribeStampsAPortRegistrationWithTheAcceptingPod(t *testing.T) {
	podA, podB := trailingListeners(t)
	podA.podHostname, podB.podHostname = "pod-a", "pod-b"
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	subscribe := func(pod *listenerDeps, body string) {
		t.Helper()
		recorder := httptest.NewRecorder()
		subscribeHandler(pod, "test-machine", listenerRigLogger(t, pod.podHostname)).ServeHTTP(recorder,
			httptest.NewRequest(http.MethodPost, "/v1/interests/subscribe", strings.NewReader(body)))
		if recorder.Code != http.StatusOK {
			t.Fatalf("subscribe through %s: status %d %s, want 200", pod.podHostname, recorder.Code, recorder.Body.String())
		}
	}
	accepted := func(sessionID string) string {
		t.Helper()
		entry, err := podA.sessions.Refresh(ctx, sessionID)
		if err != nil {
			t.Fatalf("read %s back: %v", sessionID, err)
		}
		return entry.AcceptedHost
	}

	subscribe(podA, `{"session_id":"ses_port","port":4096}`)
	if got := accepted("ses_port"); got != "pod-a" {
		t.Fatalf("accepted_host after a subscribe through pod a = %q, want pod-a", got)
	}
	subscribe(podB, `{"session_id":"ses_port","port":4096}`)
	if got := accepted("ses_port"); got != "pod-b" {
		t.Fatalf("accepted_host after a heartbeat through pod b = %q, want pod-b", got)
	}
	recorder := httptest.NewRecorder()
	roleSetHandler(podA, "test-machine", logging.New("test")).ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/roles/set",
		strings.NewReader(`{"session_id":"ses_port","role":"stamped"}`)))
	if recorder.Code != http.StatusOK {
		t.Fatalf("role set through pod a: status %d %s, want 200", recorder.Code, recorder.Body.String())
	}
	if got := accepted("ses_port"); got != "pod-b" {
		t.Fatalf("accepted_host after a role set = %q, want pod-b kept", got)
	}

	subscribe(podA, `{"session_id":"ses_portless","self_subscribed":true}`)
	if got := accepted("ses_portless"); got != "" {
		t.Fatalf("accepted_host of a registration with no port = %q, want none", got)
	}
}
