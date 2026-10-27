package main

import (
	"errors"
	"testing"
)

func testStack() *stackFile {
	return &stackFile{
		Name: "loadtest-abc", DispatchToken: "agent-secret", ListenerToken: "listener-secret",
		Ports: stackPorts{Postgres: 15432, Proxy: 15433, NATS: 14222, NATSMonitor: 18222, Listener: 19120, Dispatch: 18777, Pprof: 16061},
		PIDs:  stackPIDs{Postgres: 11, Dispatch: 12},
	}
}

func TestGuardRefusesAFlagThatDiffersFromTheStack(t *testing.T) {
	cases := map[string]endpointFlags{
		"-url":            {url: "http://127.0.0.1:9020"},
		"-listener":       {listener: "http://127.0.0.1:9020"},
		"-token":          {token: "some-other-token"},
		"-listener-token": {listenerToken: "some-other-token"},
		"-pg":             {pg: "postgres://postgres@127.0.0.1:5432/dispatch"},
	}
	for flag, flags := range cases {
		_, err := resolveEndpoints(flags, testStack())
		var foreign *foreignFlagError
		if !errors.As(err, &foreign) {
			t.Fatalf("%s: not refused as a foreign flag: %v", flag, err)
		}
		if foreign.Flag != flag {
			t.Errorf("refusal names %s, want %s", foreign.Flag, flag)
		}
	}
}

func TestGuardTakesEndpointsFromTheStack(t *testing.T) {
	e, err := resolveEndpoints(endpointFlags{url: "http://127.0.0.1:18777/"}, testStack())
	if err != nil {
		t.Fatalf("a flag equal to the stack's value was refused: %v", err)
	}
	if e.dispatch != "http://127.0.0.1:18777" || e.pg != "postgres://postgres@127.0.0.1:15432/dispatch?sslmode=disable" ||
		e.listenerToken != "listener-secret" || e.natsServerName != "loadtest-abc" {
		t.Fatalf("endpoints %+v do not come from the stack", e)
	}
	if e.pids["postgres"] != 11 || e.pids["dispatch"] != 12 {
		t.Fatalf("pids %v, want the stack's", e.pids)
	}
}

func TestGuardAllowForeignUsesTheFlags(t *testing.T) {
	e, err := resolveEndpoints(endpointFlags{url: "http://dispatch.example:8080", allowForeign: true}, testStack())
	if err != nil {
		t.Fatal(err)
	}
	if e.dispatch != "http://dispatch.example:8080" || e.listener != "http://127.0.0.1:19120" {
		t.Fatalf("endpoints %+v: want the flag's Dispatch and the stack's listener", e)
	}
	if len(e.pids) != 0 || e.natsServerName != "" {
		t.Fatalf("a foreign target keeps pids %v and server name %q, want neither", e.pids, e.natsServerName)
	}
}
