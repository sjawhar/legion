package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// stackFile is $LOADTEST_ROOT/stack.json. stack.sh setup writes it; run-step.sh rewrites it once
// it has started the step's Dispatch, so pids.dispatch is that process:
//
//	{
//	  "name": "loadtest-<random>",   NATS server_name (nats-server -n) and the listener's machine id
//	  "root": "<LOADTEST_ROOT>",
//	  "dispatch_token": "<random>",  Dispatch's DISPATCH_AGENT_TOKEN
//	  "listener_token": "<random>",  the listener's ENVOY_API_TOKEN
//	  "ports": {"postgres": 15432, "proxy": 15433, "nats": 14222, "nats_monitor": 18222,
//	            "listener": 19120, "dispatch": 18777, "pprof": 16061},
//	  "pids": {"postgres": <postmaster>, "proxy": 0, "nats": 0, "listener": 0, "dispatch": 0},
//	  "pg_direct": false             true when Dispatch reaches Postgres without the latency proxy
//	}
//
// Every process listens on 127.0.0.1. The loadgen samplers and the lock probe always reach
// Postgres directly (ports.postgres, database dispatch), never through the proxy.
type stackFile struct {
	Name          string     `json:"name"`
	Root          string     `json:"root"`
	DispatchToken string     `json:"dispatch_token"`
	ListenerToken string     `json:"listener_token"`
	Ports         stackPorts `json:"ports"`
	PIDs          stackPIDs  `json:"pids"`
	PGDirect      bool       `json:"pg_direct"`
}

type stackPorts struct {
	Postgres    int `json:"postgres"`
	Proxy       int `json:"proxy"`
	NATS        int `json:"nats"`
	NATSMonitor int `json:"nats_monitor"`
	Listener    int `json:"listener"`
	Dispatch    int `json:"dispatch"`
	Pprof       int `json:"pprof"`
}

type stackPIDs struct {
	Postgres int `json:"postgres"`
	Proxy    int `json:"proxy"`
	NATS     int `json:"nats"`
	Listener int `json:"listener"`
	Dispatch int `json:"dispatch"`
}

func loadStack(path string) (*stackFile, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var st stackFile
	if err := json.Unmarshal(b, &st); err != nil {
		return nil, fmt.Errorf("stack file %s: %w", path, err)
	}
	missing := []string{}
	for _, field := range []struct {
		name  string
		empty bool
	}{
		{"name", st.Name == ""}, {"dispatch_token", st.DispatchToken == ""}, {"listener_token", st.ListenerToken == ""},
		{"ports.postgres", st.Ports.Postgres == 0}, {"ports.nats", st.Ports.NATS == 0},
		{"ports.nats_monitor", st.Ports.NATSMonitor == 0}, {"ports.listener", st.Ports.Listener == 0},
		{"ports.dispatch", st.Ports.Dispatch == 0}, {"ports.pprof", st.Ports.Pprof == 0},
	} {
		if field.empty {
			missing = append(missing, field.name)
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("stack file %s lacks %s", path, strings.Join(missing, ", "))
	}
	return &st, nil
}

// endpoints is every address and credential a seed or a step uses.
type endpoints struct {
	dispatch, listener, nats, natsMon, pprof, pg string
	token, listenerToken                         string
	// natsServerName is the /varz server_name the guard expects; empty skips that check.
	natsServerName string
	// pids are the processes the sampler reads from /proc, by name ("postgres" is the
	// postmaster); empty under -allow-foreign.
	pids     map[string]int
	stack    string
	pgDirect bool
}

// endpointFlags are the endpoint flags seed and run share. Each defaults empty, so the stack's
// value applies; one that differs from it is refused unless allowForeign is set.
type endpointFlags struct {
	stack                                   string
	url, listener, nats, natsMon, pprof, pg string
	token, listenerToken                    string
	natsServerName                          string
	allowForeign                            bool
}

func (f *endpointFlags) register(fs *flag.FlagSet) {
	fs.StringVar(&f.stack, "stack", "", "stack.json path (default $LOADTEST_ROOT/stack.json)")
	fs.StringVar(&f.url, "url", "", "Dispatch base URL (default: the stack's)")
	fs.StringVar(&f.listener, "listener", "", "listener base URL (default: the stack's)")
	fs.StringVar(&f.nats, "nats", "", "NATS URL (default: the stack's)")
	fs.StringVar(&f.natsMon, "nats-mon", "", "NATS monitoring URL (default: the stack's)")
	fs.StringVar(&f.pprof, "pprof", "", "Dispatch pprof base URL (default: the stack's)")
	fs.StringVar(&f.pg, "pg", "", "direct Postgres URL of the dispatch database (default: the stack's)")
	fs.StringVar(&f.token, "token", "", "Dispatch agent bearer (default: the stack's)")
	fs.StringVar(&f.listenerToken, "listener-token", "", "listener bearer (default: the stack's)")
	fs.StringVar(&f.natsServerName, "nats-server-name", "", "with -allow-foreign: the NATS server_name to expect (empty skips the check)")
	fs.BoolVar(&f.allowForeign, "allow-foreign", false, "allow endpoint flags that differ from stack.json (another stack, such as a cluster copy)")
}

// foreignFlagError refuses an endpoint flag that names something other than the stack's own.
type foreignFlagError struct {
	Flag string
}

func (e *foreignFlagError) Error() string {
	return fmt.Sprintf("%s differs from stack.json; it would target something other than the local stack, so it is refused (pass -allow-foreign to target another stack on purpose)", e.Flag)
}

// resolve reads stack.json and resolves the endpoints. It opens no connection.
func (f endpointFlags) resolve() (endpoints, error) {
	path := f.stack
	if path == "" {
		path = filepath.Join(loadtestRoot(), "stack.json")
	}
	st, err := loadStack(path)
	if err != nil {
		if !f.allowForeign || f.stack != "" || !errors.Is(err, fs.ErrNotExist) {
			return endpoints{}, fmt.Errorf("read stack: %w", err)
		}
		st = nil
	}
	e, err := resolveEndpoints(f, st)
	e.stack = path
	return e, err
}

// resolveEndpoints takes every endpoint from st, refusing a flag that differs from it, unless
// f.allowForeign: then a given flag wins, an empty one falls back to st, and pids stay empty.
func resolveEndpoints(f endpointFlags, st *stackFile) (endpoints, error) {
	if st == nil && !f.allowForeign {
		return endpoints{}, fmt.Errorf("no stack.json, and -allow-foreign is not set")
	}
	var e endpoints
	stackValue := map[string]string{}
	if st != nil {
		local := func(scheme string, port int) string { return fmt.Sprintf("%s://127.0.0.1:%d", scheme, port) }
		stackValue = map[string]string{
			"-url":            local("http", st.Ports.Dispatch),
			"-listener":       local("http", st.Ports.Listener),
			"-nats":           local("nats", st.Ports.NATS),
			"-nats-mon":       local("http", st.Ports.NATSMonitor),
			"-pprof":          local("http", st.Ports.Pprof),
			"-pg":             fmt.Sprintf("postgres://postgres@127.0.0.1:%d/dispatch?sslmode=disable", st.Ports.Postgres),
			"-token":          st.DispatchToken,
			"-listener-token": st.ListenerToken,
		}
		e.pgDirect = st.PGDirect
	}
	fields := []struct {
		flag  string
		given string
		dst   *string
	}{
		{"-url", strings.TrimRight(f.url, "/"), &e.dispatch},
		{"-listener", strings.TrimRight(f.listener, "/"), &e.listener},
		{"-nats", f.nats, &e.nats},
		{"-nats-mon", strings.TrimRight(f.natsMon, "/"), &e.natsMon},
		{"-pprof", strings.TrimRight(f.pprof, "/"), &e.pprof},
		{"-pg", f.pg, &e.pg},
		{"-token", f.token, &e.token},
		{"-listener-token", f.listenerToken, &e.listenerToken},
	}
	for _, field := range fields {
		want := stackValue[field.flag]
		switch {
		case !f.allowForeign && field.given != "" && field.given != want:
			return endpoints{}, &foreignFlagError{Flag: field.flag}
		case field.given != "":
			*field.dst = field.given
		default:
			*field.dst = want
		}
		if *field.dst == "" {
			return endpoints{}, fmt.Errorf("%s is required: stack.json does not supply it", field.flag)
		}
	}
	if f.natsServerName != "" && !f.allowForeign {
		return endpoints{}, fmt.Errorf("-nats-server-name applies only with -allow-foreign; the stack's name is expected otherwise")
	}
	if f.allowForeign {
		e.natsServerName = f.natsServerName
		e.pids = map[string]int{}
		return e, nil
	}
	e.natsServerName = st.Name
	e.pids = map[string]int{}
	for name, pid := range map[string]int{
		"postgres": st.PIDs.Postgres, "proxy": st.PIDs.Proxy, "nats": st.PIDs.NATS,
		"listener": st.PIDs.Listener, "dispatch": st.PIDs.Dispatch,
	} {
		if pid > 0 {
			e.pids[name] = pid
		}
	}
	return e, nil
}

// checkStack confirms, before the first write, that each endpoint is the stack's own: NATS
// /varz names the stack, Dispatch answers an authenticated read with the stack's agent token, and
// the listener answers /v1/sessions with the stack's API token and refuses any other token (a
// listener with no token configured would answer every caller). The pod's live listener and
// production Dispatch hold other tokens, so a mistaken target fails here.
func checkStack(ctx context.Context, e endpoints) error {
	client := &http.Client{Timeout: 10 * time.Second}
	if e.natsServerName != "" {
		var varz struct {
			ServerName string `json:"server_name"`
		}
		status, body, err := guardGet(ctx, client, e.natsMon+"/varz", "")
		if err == nil && status != http.StatusOK {
			err = fmt.Errorf("GET %s/varz answered %d", e.natsMon, status)
		}
		if err == nil {
			err = json.Unmarshal(body, &varz)
		}
		if err != nil {
			return fmt.Errorf("guard: nats check failed: %w", err)
		}
		if varz.ServerName != e.natsServerName {
			return fmt.Errorf("guard: nats check failed: %s/varz server_name is %q, want %q", e.natsMon, varz.ServerName, e.natsServerName)
		}
	}
	status, body, err := guardGet(ctx, client, e.dispatch+"/api/v1/projects", e.token)
	if err != nil {
		return fmt.Errorf("guard: dispatch check failed: %w", err)
	}
	if status != http.StatusOK {
		return fmt.Errorf("guard: dispatch check failed: GET %s/api/v1/projects with the stack's agent token answered %d %s", e.dispatch, status, trunc(body))
	}
	status, body, err = guardGet(ctx, client, e.listener+"/v1/sessions", e.listenerToken)
	if err != nil {
		return fmt.Errorf("guard: listener check failed: %w", err)
	}
	if status != http.StatusOK {
		return fmt.Errorf("guard: listener check failed: GET %s/v1/sessions with the stack's listener token answered %d %s", e.listener, status, trunc(body))
	}
	other := make([]byte, 16)
	_, _ = rand.Read(other)
	status, _, err = guardGet(ctx, client, e.listener+"/v1/sessions", hex.EncodeToString(other))
	if err != nil {
		return fmt.Errorf("guard: listener check failed: %w", err)
	}
	if status/100 == 2 {
		return fmt.Errorf("guard: listener check failed: %s/v1/sessions answers %d to a token that is not the stack's, so it does not hold the stack's token", e.listener, status)
	}
	return nil
}

func guardGet(ctx context.Context, client *http.Client, url, bearer string) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Accept", "application/json")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, body, err
}
