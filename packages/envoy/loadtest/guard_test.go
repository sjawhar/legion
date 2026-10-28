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

// foreignFlags gives every endpoint flag a foreign value.
func foreignFlags() endpointFlags {
	return endpointFlags{
		url: "http://dispatch.example:8080/", listener: "http://listener.example:9120", nats: "nats://nats.example:4222",
		natsMon: "http://nats.example:8222", pprof: "http://dispatch.example:6061", pg: "postgres://postgres@db.example:5432/dispatch",
		token: "cluster-agent-token", listenerToken: "cluster-listener-token", allowForeign: true,
	}
}

func TestGuardAllowForeignUsesTheFlags(t *testing.T) {
	e, err := resolveEndpoints(foreignFlags(), testStack())
	if err != nil {
		t.Fatal(err)
	}
	if e.dispatch != "http://dispatch.example:8080" || e.listener != "http://listener.example:9120" || e.pg != "postgres://postgres@db.example:5432/dispatch" ||
		e.pprof != "http://dispatch.example:6061" || e.natsMon != "http://nats.example:8222" || e.token != "cluster-agent-token" {
		t.Fatalf("endpoints %+v: want every one from the flags", e)
	}
	if len(e.pids) != 0 || e.natsServerName != "" {
		t.Fatalf("a foreign target keeps pids %v and server name %q, want neither", e.pids, e.natsServerName)
	}
}

func TestGuardAllowForeignRefusesAMissingFlag(t *testing.T) {
	unsetters := map[string]func(*endpointFlags){
		"-url":            func(f *endpointFlags) { f.url = "" },
		"-listener":       func(f *endpointFlags) { f.listener = "" },
		"-nats":           func(f *endpointFlags) { f.nats = "" },
		"-nats-mon":       func(f *endpointFlags) { f.natsMon = "" },
		"-pprof":          func(f *endpointFlags) { f.pprof = "" },
		"-pg":             func(f *endpointFlags) { f.pg = "" },
		"-token":          func(f *endpointFlags) { f.token = "" },
		"-listener-token": func(f *endpointFlags) { f.listenerToken = "" },
	}
	for flag, unset := range unsetters {
		for _, st := range []*stackFile{testStack(), nil} {
			flags := foreignFlags()
			unset(&flags)
			_, err := resolveEndpoints(flags, st)
			var missing *missingForeignFlagError
			if !errors.As(err, &missing) {
				t.Fatalf("%s left empty (stack.json present: %t): not refused as a missing flag: %v", flag, st != nil, err)
			}
			if missing.Flag != flag {
				t.Errorf("refusal names %s, want %s", missing.Flag, flag)
			}
		}
	}
	// With -url and -token only, the refusal names the first flag left empty.
	_, err := resolveEndpoints(endpointFlags{url: "http://127.0.0.1:18777", token: "x", allowForeign: true}, testStack())
	var missing *missingForeignFlagError
	if !errors.As(err, &missing) || missing.Flag != "-listener" {
		t.Fatalf("refusal %v, want -listener named", err)
	}
}
