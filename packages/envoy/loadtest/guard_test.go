package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

// isolatePG keeps a test's -pg values from the environment: pgconn.ParseConfig, which the guard
// and meta.json read -pg with, takes defaults from the PG* variables and from files under $HOME.
func isolatePG(t *testing.T) {
	t.Helper()
	for _, name := range []string{
		"PGHOST", "PGPORT", "PGDATABASE", "PGUSER", "PGPASSWORD", "PGSERVICE", "PGSERVICEFILE",
		"PGSSLMODE", "PGSSLKEY", "PGSSLCERT", "PGSSLROOTCERT", "PGSSLPASSWORD",
	} {
		t.Setenv(name, "")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("PGPASSFILE", filepath.Join(home, "absent.pgpass"))
}

func testStack() *stackFile {
	return &stackFile{
		Name: "loadtest-abc", DispatchToken: "agent-secret", ListenerToken: "listener-secret",
		Ports: stackPorts{Postgres: 15432, Proxy: 15433, NATS: 14222, NATSMonitor: 18222, Listener: 19120, Dispatch: 18777, Pprof: 16061, Metrics: 16062},
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
		"-metrics":        {metrics: "http://127.0.0.1:9090"},
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
	isolatePG(t)
	e, err := resolveEndpoints(endpointFlags{url: "http://127.0.0.1:18777/", metrics: "http://127.0.0.1:16062/"}, testStack())
	if err != nil {
		t.Fatalf("a flag equal to the stack's value was refused: %v", err)
	}
	if e.dispatch != "http://127.0.0.1:18777" || e.pg != "postgres://postgres@127.0.0.1:15432/dispatch?sslmode=disable" ||
		e.listenerToken != "listener-secret" || e.natsServerName != "loadtest-abc" || e.metrics != "http://127.0.0.1:16062" {
		t.Fatalf("endpoints %+v do not come from the stack", e)
	}
	if e.pids["postgres"] != 11 || e.pids["dispatch"] != 12 {
		t.Fatalf("pids %v, want the stack's", e.pids)
	}
	if got := redactEndpoint("-pg", e.pg); got != "postgres://127.0.0.1:15432/dispatch" {
		t.Fatalf("the stack's -pg recorded as %q, want its host, port and database", got)
	}
}

// Under -allow-foreign, -metrics is the one endpoint that may stay empty: the run then scrapes no
// metrics, and never the local stack's.
func TestGuardAllowForeignTakesMetricsOnlyFromItsFlag(t *testing.T) {
	flags := foreignFlags()
	e, err := resolveEndpoints(flags, testStack())
	if err != nil || e.metrics != "" {
		t.Fatalf("-allow-foreign without -metrics: metrics %q, %v; want empty and no refusal", e.metrics, err)
	}
	flags.metrics = "http://dispatch.example:9090/"
	if e, err = resolveEndpoints(flags, testStack()); err != nil || e.metrics != "http://dispatch.example:9090" {
		t.Fatalf("-allow-foreign with -metrics: metrics %q, %v", e.metrics, err)
	}
}

// foreignFlags gives every required endpoint flag a foreign value; -metrics stays empty.
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

// TestGuardPrintsNoURLPassword: a foreign URL that does not parse is refused naming its flag
// alone, and a failed guard check names the endpoint without its userinfo, user or password.
func TestGuardPrintsNoURLPassword(t *testing.T) {
	isolatePG(t)
	const password = "url-password-51d2"
	flags := foreignFlags()
	flags.pg = "host=db.example user=postgres password=" + password
	if _, err := resolveEndpoints(flags, testStack()); err == nil || !strings.Contains(err.Error(), "-pg") || strings.Contains(err.Error(), password) {
		t.Fatalf("a -pg that is not a URL: refusal %v, want one naming -pg without its password", err)
	}
	// pgx reads a -pg value as a URL only when it starts with postgres:// or postgresql://
	// (case-sensitive), and an invalid port fails its parse: each is refused by name, with no
	// part of the value and none of pgx's error.
	for _, pg := range []string{"POSTGRES://u:pw-frag1@h/db", "pg://u:pw-frag2@h/db", "postgres://u:pw-frag3@h:notaport/db"} {
		flags := foreignFlags()
		flags.pg = pg
		_, err := resolveEndpoints(flags, testStack())
		if err == nil || !strings.Contains(err.Error(), "-pg") {
			t.Errorf("-pg %q: refusal %v, want one naming -pg", pg, err)
			continue
		}
		for _, part := range []string{"pw-frag", "u:", "@h", "notaport", "parse"} {
			if strings.Contains(err.Error(), part) {
				t.Errorf("-pg %q: refusal %q holds %q", pg, err, part)
			}
		}
	}
	flags = foreignFlags()
	flags.nats = "nats://tok@127.0.0.1:3,nats://u:" + password + "@127.0.0.1:4"
	if _, err := resolveEndpoints(flags, testStack()); err != nil {
		t.Fatalf("a -nats server list: refused %v, want it accepted", err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	withPassword := strings.Replace(srv.URL, "://", "://loadgen:"+password+"@", 1)
	host := strings.TrimPrefix(srv.URL, "http://")
	for _, e := range []endpoints{
		{natsMon: withPassword, natsServerName: "loadtest-abc"},
		{dispatch: withPassword},
	} {
		err := checkStack(context.Background(), e)
		if err == nil || strings.Contains(err.Error(), password) || strings.Contains(err.Error(), "loadgen") || !strings.Contains(err.Error(), host) {
			t.Errorf("guard check against %+v: error %v, want one naming the endpoint's host without its userinfo", e, err)
		}
	}
}
