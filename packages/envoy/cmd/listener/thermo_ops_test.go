package main

import (
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/config"
)

func TestThermoOpsListenerConfigDefaultsToLoopback(t *testing.T) {
	t.Setenv("ENVOY_MACHINE_ID", "thermo-ops")
	t.Setenv("NATS_URLS", "nats://127.0.0.1:4222")
	t.Setenv("ENVOY_LISTEN_HOST", "")

	service, err := config.Load(0)
	if err != nil {
		t.Fatalf("load listener config: %v", err)
	}
	address := service.ListenAddress()
	if address != "127.0.0.1:0" {
		t.Fatalf("default listen address = %q, want 127.0.0.1:0", address)
	}

	listener, err := net.Listen("tcp", address)
	if err != nil {
		t.Fatalf("bind default listener address: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	bound, ok := listener.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatalf("listener address type = %T, want *net.TCPAddr", listener.Addr())
	}
	if !bound.IP.IsLoopback() {
		t.Fatalf("default listener bound non-loopback address %s", bound.IP)
	}
}

func TestThermoOpsListenerConfigAllowsExplicitWidening(t *testing.T) {
	t.Setenv("ENVOY_MACHINE_ID", "thermo-ops")
	t.Setenv("NATS_URLS", "nats://127.0.0.1:4222")
	t.Setenv("ENVOY_LISTEN_HOST", "0.0.0.0")

	service, err := config.Load(9020)
	if err != nil {
		t.Fatalf("load listener config: %v", err)
	}
	if address := service.ListenAddress(); address != "0.0.0.0:9020" {
		t.Fatalf("explicit listen address = %q, want 0.0.0.0:9020", address)
	}
}

func TestThermoOpsRoleSetRejectsUnregisteredSession(t *testing.T) {
	registry, sessions := setupSessionsTest(t, nil, nil)
	var state atomic.Pointer[listenerDeps]
	state.Store(&listenerDeps{registry: registry, sessions: sessions})

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(
		http.MethodPost,
		"/v1/roles/set",
		strings.NewReader(`{"session_id":"ses_unregistered","role":"legion-controller"}`),
	)
	roleSetHandler(&state, "test-machine").ServeHTTP(recorder, request)

	if recorder.Code != http.StatusNotFound {
		t.Fatalf("unregistered role claimant status = %d, want 404: %s", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), "registered") {
		t.Fatalf("unregistered role claimant response = %q, want registration error", recorder.Body.String())
	}
	if _, err := registry.Get("ses_unregistered"); !errors.Is(err, nats.ErrKeyNotFound) {
		t.Fatalf("unregistered role claimant created an interest: %v", err)
	}
}
