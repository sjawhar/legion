package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/modelroute"
)

// The model probe on the real Oh My Pi, against a stand-in for the gateway: only the real binary
// shows what the profile's retries, and a key command that fails, do to a turn, which is what the
// probe judges. LEGION_TEST_OMP names the pinned binary, as for internal/modelroute's route test;
// a run without one skips, except on GitHub Actions (GITHUB_ACTIONS, not CI: agent harnesses on
// the devbox export CI=true), whose daemon-go job installs it.
func TestTheModelProbeOnTheRealOhMyPi(t *testing.T) {
	omp := os.Getenv("LEGION_TEST_OMP")
	switch {
	case omp == "" && os.Getenv("GITHUB_ACTIONS") == "true":
		t.Fatal("LEGION_TEST_OMP is unset on GitHub Actions: name the pinned Oh My Pi binary (the daemon-go job installs it)")
	case omp == "":
		t.Skip("LEGION_TEST_OMP names no Oh My Pi binary")
	}
	home := t.TempDir()
	_, defaultModel, _ := strings.Cut(modelroute.DefaultModel, "/")
	for _, testCase := range []struct {
		name string
		// status is what the gateway answers the default alias with (0: a reply); every other
		// alias gets a reply, so a turn that fell back would pass where it must not.
		status      int
		unreachable bool
		// noToken leaves the token file absent, as in a pod without the gateway token volume.
		noToken     bool
		unavailable bool
		want, anyOf []string
	}{
		{name: "the gateway answers"},
		// Oh My Pi's client retries a 529 on its own backoff, past maxRetries and printing nothing
		// until it gives up, so the bound usually ends the turn first: either names the gateway.
		{name: "overloaded", status: 529, unavailable: true, want: []string{"routed to http://127.0.0.1:"}, anyOf: []string{"529", "timed out after 30s"}},
		{name: "the model not found", status: 404, want: []string{"the gateway refused it: 404"}},
		{name: "the key refused", status: 401, want: []string{"the gateway refused it: 401"}},
		// The key command failing is the image's fault, never the gateway's moment: Oh My Pi starts
		// on the pinned alias and exits naming the provider it has no key for.
		{name: "the key command fails", noToken: true, want: []string{"found no usable model", "No API key found for anthropic"}},
		{name: "unreachable", unreachable: true, unavailable: true, want: []string{"routed to http://127.0.0.1:"}, anyOf: []string{"Connection error", "timed out after 30s"}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			gw := &standIn{status: testCase.status, alias: defaultModel}
			server := httptest.NewServer(gw)
			t.Cleanup(server.Close)
			url := server.URL
			if testCase.unreachable {
				listener, err := net.Listen("tcp", "127.0.0.1:0")
				if err != nil {
					t.Fatal(err)
				}
				url = "http://" + listener.Addr().String()
				listener.Close()
			}
			profile := strings.ReplaceAll(testCase.name, " ", "-")
			token := filepath.Join(t.TempDir(), "token")
			if !testCase.noToken {
				if err := os.WriteFile(token, []byte("gateway-token\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			base := map[string]string{"HOME": home, "OMP_PROFILE": profile, modelroute.EnvURL: url, "PATH": "/usr/local/bin:/usr/bin:/bin"}
			installed, err := modelroute.InstallKeyedBy(func(name string) (string, bool) { value, ok := base[name]; return value, ok }, token)
			if err != nil {
				t.Fatal(err)
			}
			env := map[string]string{}
			for _, pair := range installed.Environ([]string{"HOME=" + home, "OMP_PROFILE=" + profile, "PATH=/usr/local/bin:/usr/bin:/bin"}) {
				name, value, _ := strings.Cut(pair, "=")
				env[name] = value
			}
			// The turn's bound is the gate's, when under modelTurnTimeout: 30 s keeps the suite short,
			// and an overloaded or unreachable gateway still retries past it.
			gate := pluginGate{
				env: env, workDir: t.TempDir(), invocation: omp, timeout: 30 * time.Second,
				model: modelroute.DefaultModel, route: installed.Route, log: slog.New(slog.NewTextHandler(io.Discard, nil)),
			}

			started := time.Now()
			err = gate.verifyModelRoute(context.Background())
			elapsed := time.Since(started)

			var unavailable *ModelRouteUnavailable
			switch {
			case testCase.want == nil && err != nil:
				t.Fatalf("verifyModelRoute = %v, want a pass", err)
			case testCase.want != nil && (err == nil || errors.As(err, &unavailable) != testCase.unavailable):
				t.Fatalf("verifyModelRoute = %v, want unavailable %t", err, testCase.unavailable)
			}
			for _, want := range testCase.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("verifyModelRoute = %v, want it to say %q", err, want)
				}
			}
			if len(testCase.anyOf) > 0 && !slices.ContainsFunc(testCase.anyOf, func(want string) bool { return strings.Contains(err.Error(), want) }) {
				t.Errorf("verifyModelRoute = %v, want it to say one of %q", err, testCase.anyOf)
			}
			if strings.Contains(fmt.Sprint(err), "past the gateway") {
				t.Errorf("a gateway failure reads as a route past the gateway: %v", err)
			}
			if other := gw.others(); other != "" {
				t.Errorf("the turn fell back to %s", other)
			}
			if elapsed > gate.timeout+5*time.Second {
				t.Errorf("the round trip took %s, past its bound %s", elapsed, gate.timeout)
			}
			t.Logf("%s: %v in %s", testCase.name, err, elapsed.Round(time.Millisecond))
		})
	}
}

// standIn is a stand-in for the gateway's /anthropic route: it answers the default alias with
// status (a streamed reply when 0) and any other alias with a reply, recording which other alias
// a turn asked for.
type standIn struct {
	status int
	alias  string

	mu    sync.Mutex
	other string
}

func (s *standIn) others() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.other
}

func (s *standIn) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Model string `json:"model"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	if r.URL.Path != "/anthropic/v1/messages" {
		http.NotFound(w, r)
		return
	}
	if body.Model != s.alias {
		s.mu.Lock()
		s.other = body.Model
		s.mu.Unlock()
	} else if s.status != 0 {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(s.status)
		fmt.Fprintf(w, `{"type":"error","error":{"type":"stand_in_error","message":"stand-in %d"}}`, s.status)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	for _, event := range []string{
		`{"type":"message_start","message":{"id":"msg_stand_in","type":"message","role":"assistant","model":"` + body.Model + `","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":5,"output_tokens":1}}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"ok"}}`,
		`{"type":"content_block_stop","index":0}`,
		`{"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":1}}`,
		`{"type":"message_stop"}`,
	} {
		var typed struct {
			Type string `json:"type"`
		}
		_ = json.Unmarshal([]byte(event), &typed)
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", typed.Type, event)
	}
}
