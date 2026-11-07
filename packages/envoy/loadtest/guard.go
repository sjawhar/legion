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
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// stackFile is $LOADTEST_ROOT/stack.json. stack.sh setup writes it; run-step.sh rewrites it once
// it has started the step's Dispatch, so pids.dispatch is that process:
//
//	{
//	  "name": "loadtest-<random>",   NATS server_name (nats-server -n) and the listener's machine id
//	  "root": "<LOADTEST_ROOT>",
//	  "dispatch_token": "<random>",  Dispatch's DISPATCH_AGENT_TOKEN
//	  "listener_token": "<random>",  the listener's ENVOY_API_TOKEN
//	  "ports": {"postgres": 10000, "proxy": 10001, "nats": 10002, "nats_monitor": 10003,
//	            "listener": 10004, "dispatch": 10005, "pprof": 10006, "metrics": 10007},
//	  "pids": {"postgres": <postmaster>, "proxy": 0, "nats": 0, "listener": 0, "dispatch": 0},
//	  "pg_direct": false             true when Dispatch reaches Postgres without the latency proxy
//	}
//
// The ports above are a root whose port block is 0: each default is 10000 + 10k + the key's
// offset, for the root's block k, and a LOADTEST_*_PORT variable replaces one (common.sh).
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
	Metrics     int `json:"metrics"` // Dispatch's DISPATCH_METRICS_ADDR port
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
		{"ports.dispatch", st.Ports.Dispatch == 0}, {"ports.pprof", st.Ports.Pprof == 0}, {"ports.metrics", st.Ports.Metrics == 0},
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
	// metrics is Dispatch's metrics base URL (GET <metrics>/metrics); empty under -allow-foreign
	// without -metrics, and then the sampler scrapes nothing.
	metrics              string
	token, listenerToken string
	// natsServerName is the /varz server_name the guard expects; empty skips that check.
	natsServerName string
	// pids are the processes the sampler reads from /proc, by name ("postgres" is the
	// postmaster); empty under -allow-foreign.
	pids     map[string]int
	stack    string
	pgDirect bool
}

// endpointFlags are the endpoint flags seed and run share. Each defaults empty, so the stack's
// value applies; one that differs from it is refused unless allowForeign is set, and then every
// one but -metrics is required.
type endpointFlags struct {
	stack                                   string
	url, listener, nats, natsMon, pprof, pg string
	metrics                                 string
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
	fs.StringVar(&f.metrics, "metrics", "", "Dispatch metrics base URL, which serves /metrics (default: the stack's; optional with -allow-foreign)")
	fs.StringVar(&f.pg, "pg", "", "direct Postgres URL of the dispatch database (default: the stack's)")
	fs.StringVar(&f.token, "token", "", "Dispatch agent bearer (default: the stack's)")
	fs.StringVar(&f.listenerToken, "listener-token", "", "listener bearer (default: the stack's)")
	fs.StringVar(&f.natsServerName, "nats-server-name", "", "with -allow-foreign: the NATS server_name to expect (empty skips the check)")
	fs.BoolVar(&f.allowForeign, "allow-foreign", false, "allow endpoint flags that differ from stack.json (another stack, such as a cluster copy)")
}

// tokenFlags are the endpoint flags whose values are bearer tokens, and urlFlags those whose values
// are URLs. meta.json records the run's flags with each token replaced by redacted and each URL
// without its credentials (recordedArgs, redactEndpoint).
var (
	tokenFlags = []string{"-token", "-listener-token"}
	urlFlags   = []string{"-url", "-listener", "-nats", "-nats-mon", "-pprof", "-metrics", "-pg"}
)

// redactURL is raw without its userinfo and without a password query parameter, for every place
// that records or prints an HTTP endpoint (redactEndpoint does each -nats server the same way).
// The whole userinfo goes, the user name too: nats.go takes a user name with no password as the
// auth token. raw is read whole by url.Parse, as net/http reads it. A value that does not parse,
// or names no scheme, is redacted whole, since no credential in it can be found for sure. A -pg
// value is never read here (redactPG).
func redactURL(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Scheme == "" {
		return redacted
	}
	return withoutCredentials(u).String()
}

// redactEndpoint is the value of endpoint flag flag as it is recorded or printed: a -nats value is
// a server list that nats.go splits at commas (processUrlString), each server with userinfo of its
// own, so each loses its own; a -pg value is redactPG's; any other flag's value is one URL
// (redactURL).
func redactEndpoint(flag, raw string) string {
	if flag == "-pg" {
		return redactPG(raw)
	}
	if flag != "-nats" {
		return redactURL(raw)
	}
	servers := strings.Split(raw, ",")
	for i, server := range servers {
		u, scheme, err := parseNATSServer(server)
		if err != nil {
			servers[i] = redacted
			continue
		}
		servers[i] = withoutCredentials(u).String()
		if !scheme {
			servers[i] = strings.TrimPrefix(servers[i], natsScheme)
		}
	}
	return strings.Join(servers, ",")
}

// withoutCredentials is u with no userinfo and no password query parameter.
func withoutCredentials(u *url.URL) *url.URL {
	u.User = nil
	if q := u.Query(); q.Has("password") {
		q.Del("password")
		u.RawQuery = q.Encode()
	}
	return u
}

// parsePG is pgx's own parse of a -pg value, and ok is false when pgx would not read raw as a
// URL: raw must start with postgres:// or postgresql:// (the prefixes pgconn.ParseConfig checks,
// case-sensitive; any other value goes to its keyword/value parser, whose error can quote the
// value) and pgconn.ParseConfig, the parser pgx.Connect uses, must accept it. ParseConfig also
// reads the PG* environment variables and may read the passfile, as pgx.Connect does.
func parsePG(raw string) (config *pgconn.Config, ok bool) {
	if !strings.HasPrefix(raw, "postgres://") && !strings.HasPrefix(raw, "postgresql://") {
		return nil, false
	}
	config, err := pgconn.ParseConfig(raw)
	return config, err == nil
}

// redactPG is a -pg value as it is recorded or printed, built from parsePG's result alone:
// postgres://, each host:port pgx would try (Host and Port, then each of Fallbacks, a repeat of
// the one before it dropped: sslmode prefer and allow give each host twice), comma-separated,
// then / and the database. It holds no user, password, sslpassword or other parameter. A value
// parsePG refuses is redacted whole.
func redactPG(raw string) string {
	config, ok := parsePG(raw)
	if !ok {
		return redacted
	}
	addrs := []string{net.JoinHostPort(config.Host, strconv.Itoa(int(config.Port)))}
	for _, fb := range config.Fallbacks {
		addrs = append(addrs, net.JoinHostPort(fb.Host, strconv.Itoa(int(fb.Port))))
	}
	return "postgres://" + strings.Join(slices.Compact(addrs), ",") + "/" + url.PathEscape(config.Database)
}

// natsScheme stands in for the scheme of a NATS server that names none ("host:4222", which
// nats.go reads as nats://host:4222), so that url.Parse reads its host.
const natsScheme = "nats://"

// parseNATSServer parses one server of a -nats list; scheme reports whether it named its own.
func parseNATSServer(server string) (u *url.URL, scheme bool, err error) {
	server = strings.TrimSpace(server)
	scheme = strings.Contains(server, "://")
	if !scheme {
		server = natsScheme + server
	}
	u, err = url.Parse(server)
	return u, scheme, err
}

// isEndpointURL reports whether v is a valid value of endpoint flag flag: for -pg, a value
// parsePG accepts; for -nats, a comma-separated server list whose every server names a host; for
// any other flag, one URL with a scheme and a host, as url.Parse reads it.
func isEndpointURL(flag, v string) bool {
	switch flag {
	case "-pg":
		_, ok := parsePG(v)
		return ok
	case "-nats":
		for _, server := range strings.Split(v, ",") {
			if u, _, err := parseNATSServer(server); err != nil || u.Host == "" {
				return false
			}
		}
		return true
	default:
		u, err := url.Parse(strings.TrimSpace(v))
		return err == nil && u.Scheme != "" && u.Host != ""
	}
}

// foreignFlagError refuses an endpoint flag that names something other than the stack's own.
type foreignFlagError struct {
	Flag string
}

func (e *foreignFlagError) Error() string {
	return fmt.Sprintf("%s differs from stack.json; it would target something other than the local stack, so it is refused (pass -allow-foreign to target another stack on purpose)", e.Flag)
}

// missingForeignFlagError refuses a run under -allow-foreign that leaves an endpoint flag empty:
// it would fall back to the local stack's value, so the run would write to one stack and count,
// sample or profile another. All lists every required endpoint and token flag.
type missingForeignFlagError struct {
	Flag string
	All  []string
}

func (e *missingForeignFlagError) Error() string {
	return fmt.Sprintf("%s is required with -allow-foreign: a foreign run takes no endpoint or token from stack.json, so give every one of %s", e.Flag, strings.Join(e.All, ", "))
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
	}
	e, err := resolveEndpoints(f, st)
	e.stack = path
	return e, err
}

// resolveEndpoints takes every endpoint from st, refusing a flag that differs from it, unless
// f.allowForeign: then every endpoint flag but -metrics is required, none falls back to st (an
// empty -metrics stays empty), and pids stay empty.
func resolveEndpoints(f endpointFlags, st *stackFile) (endpoints, error) {
	if st == nil && !f.allowForeign {
		return endpoints{}, fmt.Errorf("no stack.json, and -allow-foreign is not set")
	}
	var e endpoints
	// Under -allow-foreign without stack.json, the stack values below are never used.
	var sv stackFile
	if st != nil {
		sv = *st
		e.pgDirect = st.PGDirect && !f.allowForeign
	}
	local := func(scheme string, port int) string { return fmt.Sprintf("%s://127.0.0.1:%d", scheme, port) }
	// The endpoint and token flags, in the order the guard checks them: each one's name, the value
	// given, the stack's value, the endpoint it sets, and whether -allow-foreign may leave it
	// empty.
	fields := []struct {
		flag, given, stack string
		dst                *string
		optional           bool
	}{
		{"-url", strings.TrimRight(f.url, "/"), local("http", sv.Ports.Dispatch), &e.dispatch, false},
		{"-listener", strings.TrimRight(f.listener, "/"), local("http", sv.Ports.Listener), &e.listener, false},
		{"-nats", f.nats, local("nats", sv.Ports.NATS), &e.nats, false},
		{"-nats-mon", strings.TrimRight(f.natsMon, "/"), local("http", sv.Ports.NATSMonitor), &e.natsMon, false},
		{"-pprof", strings.TrimRight(f.pprof, "/"), local("http", sv.Ports.Pprof), &e.pprof, false},
		{"-metrics", strings.TrimRight(f.metrics, "/"), local("http", sv.Ports.Metrics), &e.metrics, true},
		{"-pg", f.pg, fmt.Sprintf("postgres://postgres@127.0.0.1:%d/dispatch?sslmode=disable", sv.Ports.Postgres), &e.pg, false},
		{"-token", f.token, sv.DispatchToken, &e.token, false},
		{"-listener-token", f.listenerToken, sv.ListenerToken, &e.listenerToken, false},
	}
	var all []string
	for _, field := range fields {
		if !field.optional {
			all = append(all, field.flag)
		}
	}
	for _, field := range fields {
		switch {
		case f.allowForeign && field.given == "" && field.optional:
			// Left empty, it stays empty: the run goes without it.
		case f.allowForeign && field.given == "":
			return endpoints{}, &missingForeignFlagError{Flag: field.flag, All: all}
		case !f.allowForeign && field.given != "" && field.given != field.stack:
			return endpoints{}, &foreignFlagError{Flag: field.flag}
		case field.given != "":
			// A foreign URL that does not parse would reach an error that prints it whole,
			// credentials included; the refusal names the flag and the reason alone.
			if slices.Contains(urlFlags, field.flag) && !isEndpointURL(field.flag, field.given) {
				if field.flag == "-pg" {
					return endpoints{}, errors.New("-pg is not a postgres:// or postgresql:// URL that pgx accepts")
				}
				return endpoints{}, fmt.Errorf("%s is not a URL with a scheme and a host (for -nats, a comma-separated list of servers)", field.flag)
			}
			*field.dst = field.given
		default:
			*field.dst = field.stack
		}
	}
	if f.natsServerName != "" && !f.allowForeign {
		return endpoints{}, fmt.Errorf("-nats-server-name applies only with -allow-foreign; the stack's name is expected otherwise")
	}
	e.pids = map[string]int{}
	if f.allowForeign {
		e.natsServerName = f.natsServerName
		return e, nil
	}
	e.natsServerName = st.Name
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
	if err := checkNATSServerName(ctx, client, e.natsMon, e.natsServerName); err != nil {
		return err
	}
	status, body, err := guardGet(ctx, client, e.dispatch+"/api/v1/projects", e.token)
	if err != nil {
		return fmt.Errorf("guard: dispatch check failed: %w", err)
	}
	if status != http.StatusOK {
		return fmt.Errorf("guard: dispatch check failed: GET %s/api/v1/projects with the stack's agent token answered %d %s", redactURL(e.dispatch), status, trunc(body))
	}
	status, body, err = guardGet(ctx, client, e.listener+"/v1/sessions", e.listenerToken)
	if err != nil {
		return fmt.Errorf("guard: listener check failed: %w", err)
	}
	if status != http.StatusOK {
		return fmt.Errorf("guard: listener check failed: GET %s/v1/sessions with the stack's listener token answered %d %s", redactURL(e.listener), status, trunc(body))
	}
	other := make([]byte, 16)
	_, _ = rand.Read(other)
	status, _, err = guardGet(ctx, client, e.listener+"/v1/sessions", hex.EncodeToString(other))
	if err != nil {
		return fmt.Errorf("guard: listener check failed: %w", err)
	}
	if status/100 == 2 {
		return fmt.Errorf("guard: listener check failed: %s/v1/sessions answers %d to a token that is not the stack's, so it does not hold the stack's token", redactURL(e.listener), status)
	}
	return nil
}

// checkNATSServerName confirms that the NATS server monitored at monURL names itself name, so a
// command that publishes (a step, a backlog run) reaches the stack's own NATS and no other. An
// empty name skips the check.
func checkNATSServerName(ctx context.Context, client *http.Client, monURL, name string) error {
	if name == "" {
		return nil
	}
	var varz struct {
		ServerName string `json:"server_name"`
	}
	status, body, err := guardGet(ctx, client, monURL+"/varz", "")
	if err == nil && status != http.StatusOK {
		err = fmt.Errorf("GET %s/varz answered %d", redactURL(monURL), status)
	}
	if err == nil {
		err = json.Unmarshal(body, &varz)
	}
	if err != nil {
		return fmt.Errorf("guard: nats check failed: %w", err)
	}
	if varz.ServerName != name {
		return fmt.Errorf("guard: nats check failed: %s/varz server_name is %q, want %q", redactURL(monURL), varz.ServerName, name)
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
