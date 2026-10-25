package bus

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"net/url"
	"strings"
	"testing"

	"github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/testnats"
)

// captureDefaultSlog points slog's default at a JSON handler writing into the returned buffer
// until the test ends, as TestAConsumerNotActiveReportIsAWarning does. A test that calls it must
// not run in parallel: the default is the process's.
func captureDefaultSlog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var records bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&records, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return &records
}

// decodeRecords decodes each line slog's JSON handler wrote into records.
func decodeRecords(t *testing.T, records *bytes.Buffer) []map[string]any {
	t.Helper()
	var decoded []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(records.String()), "\n") {
		if line == "" {
			continue
		}
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("decode %q: %v", line, err)
		}
		decoded = append(decoded, record)
	}
	return decoded
}

// nats.go authenticates with a server URL's userinfo, a user name alone as a token and
// user:password as a password, so neither part may reach a log line or a logged error.
// url.URL.Redacted is not enough: it keeps the user name, which is the whole of a token.
func TestNATSURLWithoutUserinfo(t *testing.T) {
	for _, tc := range []struct {
		raw, want string
	}{
		{"nats://user:secret@nats.example.test:4222", "nats://nats.example.test:4222"},
		{"nats://token@127.0.0.1:4223", "nats://127.0.0.1:4223"},
		{"u:secret@127.0.0.1:4224", "nats://127.0.0.1:4224"},
		{" nats://127.0.0.1:4222 ", "nats://127.0.0.1:4222"},
		{"127.0.0.1:4222", "127.0.0.1:4222"},
		{"nats://user:secret@host:4222/%zz", "host:4222/%zz"},
		{"nats://user:secret@host:4222/a@b", "b"},
	} {
		if got := natsURLWithoutUserinfo(tc.raw); got != tc.want {
			t.Errorf("natsURLWithoutUserinfo(%q) = %q, want %q", tc.raw, got, tc.want)
		}
	}
}

// The failed-reconnect WARN names the servers the client was configured with, and the refusal of
// a remote server names that server; both are logged, so neither carries a URL's userinfo.
func TestLoggedNATSURLsCarryNoUserinfo(t *testing.T) {
	records := captureDefaultSlog(t)
	urls := []string{
		"nats://user-marker-0:pw-marker-1@127.0.0.1:4222",
		"nats://tok-marker-2@127.0.0.1:4223",
		"u-marker-5:pw-marker-3@127.0.0.1:4224",
	}
	secrets := []string{"user-marker-0", "pw-marker-1", "tok-marker-2", "u-marker-5", "pw-marker-3"}

	options("log-url-test", urls, nil, nil, nil).ReconnectErrCB(nil, errors.New("dial refused"))

	// Only this client's records: a client an earlier test left reconnecting logs through the same
	// default.
	var mine []map[string]any
	for _, record := range decodeRecords(t, records) {
		if record["name"] == "log-url-test" {
			mine = append(mine, record)
		}
	}
	if len(mine) != 1 {
		t.Fatalf("%d records logged, want 1: %s", len(mine), records)
	}
	record := mine[0]
	if record["level"] != "WARN" || record["msg"] != "envoy nats reconnect attempt failed" {
		t.Fatalf("record = %v, want the failed reconnect WARN", record)
	}
	servers, _ := record["servers"].(string)
	for _, host := range []string{"127.0.0.1:4222", "127.0.0.1:4223", "127.0.0.1:4224"} {
		if !strings.Contains(servers, host) {
			t.Errorf("servers %q does not name %s", servers, host)
		}
	}
	for _, secret := range secrets {
		if strings.Contains(records.String(), secret) {
			t.Errorf("the failed reconnect WARN holds %q: %s", secret, records)
		}
	}

	unset := func(string) (string, bool) { return "", false }
	err := refuseRemoteNATS([]string{"nats://user-marker-6:pw-marker-4@nats.example.test:4222"}, unset)
	if !errors.Is(err, ErrRemoteNATS) {
		t.Fatalf("refuseRemoteNATS = %v, want a %v", err, ErrRemoteNATS)
	}
	if !strings.Contains(err.Error(), "nats.example.test") {
		t.Errorf("refusal %q does not name nats.example.test", err)
	}
	for _, secret := range []string{"user-marker-6", "pw-marker-4"} {
		if strings.Contains(err.Error(), secret) {
			t.Errorf("refusal %q holds %q", err, secret)
		}
	}
}

// The reconnected INFO names the server the connection reached, which nats.go renders with the
// URL's userinfo (ConnectedUrl); testnats.URL is a user:password URL, so the line must drop it.
func TestTheReconnectedLineCarriesNoUserinfo(t *testing.T) {
	uri := testnats.URL(t)
	configured, err := url.Parse(uri)
	if err != nil {
		t.Fatalf("parse %s: %v", uri, err)
	}
	if configured.User == nil {
		t.Fatalf("testnats.URL %s carries no userinfo, so this test proves nothing", uri)
	}
	nc, err := nats.Connect(uri)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(nc.Close)
	records := captureDefaultSlog(t)

	options("reconnected-url-test", nil, nil, nil, nil).ReconnectedCB(nc)

	var logged string
	for _, record := range decodeRecords(t, records) {
		if record["name"] == "reconnected-url-test" && record["msg"] == "envoy nats reconnected" {
			logged, _ = record["url"].(string)
		}
	}
	if logged == "" {
		t.Fatalf("no reconnected record with a url: %s", records)
	}
	parsed, err := url.Parse(logged)
	if err != nil {
		t.Fatalf("parse the logged url %q: %v", logged, err)
	}
	if parsed.User != nil {
		t.Errorf("the logged url %q carries userinfo", logged)
	}
	if parsed.Host != configured.Host {
		t.Errorf("the logged url %q names host %q, want %q", logged, parsed.Host, configured.Host)
	}
}
