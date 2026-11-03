package daemon

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/notify"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// settingsListener is the Envoy listener the daemon's notices go to (noticeListener), serving
// for the test's life, and its URL as `envoy_url` names it.
func settingsListener(t *testing.T) (*noticeListener, string) {
	t.Helper()
	listener := newNoticeListener()
	server := httptest.NewServer(listener)
	t.Cleanup(server.Close)
	return listener, server.URL
}

// noticePayload is one published notice's payload, which the listener carries as a JSON document
// in a string.
func noticePayload(t *testing.T, published listenerPublish) map[string]any {
	t.Helper()
	payload := map[string]any{}
	if err := json.Unmarshal([]byte(published.Payload), &payload); err != nil {
		t.Fatalf("decode the notice payload %s: %v", published.Payload, err)
	}
	return payload
}

// settingsRequest is one call of the running daemon's settings route, with bearer as its
// Authorization ("" sends none), and the answer it gave.
func settingsRequest(t *testing.T, d *daemon, bearer string, body any) (int, map[string]api.SettingResult) {
	t.Helper()
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("encode the request: %v", err)
	}
	request, err := http.NewRequest(http.MethodPut, d.base+"/legion/v1/operator/settings", strings.NewReader(string(encoded)))
	if err != nil {
		t.Fatalf("build the request: %v", err)
	}
	if bearer != "" {
		request.Header.Set("Authorization", "Bearer "+bearer)
	}
	response, err := d.client.Do(request)
	if err != nil {
		t.Fatalf("PUT the settings route: %v", err)
	}
	defer response.Body.Close()
	answered, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read the answer: %v", err)
	}
	var answer api.SettingsChangeResponse
	if response.StatusCode == http.StatusOK {
		if err := json.Unmarshal(answered, &answer); err != nil {
			t.Fatalf("decode %s: %v", answered, err)
		}
	}
	return response.StatusCode, answer.Settings
}

// A change through the route reaches the running daemon whole: the route answers what it did per
// key, the daemon's state reports the new value with the route as its source, the person who
// asked and when, one log line records the change (contract 5), and the project's controller is
// told on its own topic (acceptance A5).
func TestARouteChangeIsLoggedToldToTheControllerAndShownInTheState(t *testing.T) {
	listener, envoyURL := settingsListener(t)
	live := liveSettings(t, "envoy_url: "+envoyURL)
	logs := &syncBuffer{}
	d := startDaemonOn(t, live, fakeRuntime(fake.NewRuntime(), &built{}), slog.New(slog.NewJSONHandler(logs, nil)), 10*time.Second)
	before := d.state()
	if before.Settings.AdmissionCap != 4 || before.SettingSources["admission_cap"].Source != config.SourceFile {
		t.Fatalf("admission_cap before the change = %d from %+v, want the file's 4",
			before.Settings.AdmissionCap, before.SettingSources["admission_cap"])
	}

	status, answer := settingsRequest(t, d, testOperatorToken, api.SettingsChangeRequest{
		By:      "ana@example.com",
		Changes: map[string]json.RawMessage{"admission_cap": json.RawMessage(`7`)},
	})

	if status != http.StatusOK {
		t.Fatalf("the settings route = %d", status)
	}
	want := api.SettingResult{Applied: string(config.OutcomeAtOnce), Old: "4", New: "7"}
	if got := answer["admission_cap"]; got != want {
		t.Fatalf("the answer for admission_cap = %+v, want %+v", got, want)
	}
	state := d.state()
	if state.Settings.AdmissionCap != 7 {
		t.Errorf("the state reports admission_cap %d, want the applied 7", state.Settings.AdmissionCap)
	}
	source := state.SettingSources["admission_cap"]
	if source.Source != config.SourceRoute || source.By != "ana@example.com" || time.Since(source.At) > time.Minute {
		t.Errorf("settingSources[admission_cap] = %+v, want the route, ana and the moment of the change", source)
	}
	for _, phrase := range []string{
		`"msg":"legion settings: a setting changed"`, `"path":"admission_cap"`,
		`"old":"4"`, `"new":"7"`, `"source":"route"`, `"by":"ana@example.com"`,
	} {
		if !strings.Contains(logs.String(), phrase) {
			t.Errorf("the log holds no %s; it holds:\n%s", phrase, logs)
		}
	}
	topic := notify.ControllerTopic(projectTokenOf(t, live))
	testwait.Eventually(t, "the controller to be told of the settings change", func() bool {
		return len(listener.acceptedOn(topic)) == 1
	})
	payload := noticePayload(t, listener.acceptedOn(topic)[0])
	if payload["path"] != "admission_cap" || payload["new"] != "7" ||
		payload["source"] != config.SourceRoute || payload["by"] != "ana@example.com" {
		t.Errorf("the controller was told %+v, want the key, its new value, the source and the person", payload)
	}
}

// The file arm and the route arm are one path: a change written to legion.yaml reaches the
// running daemon through the same holder, the same log line, the same controller topic and the
// same state, with the file as its source, and takes effect with nothing restarted (acceptance
// A5, "a file change and a route change take the same path").
func TestAFileChangeAndARouteChangeTakeTheSamePath(t *testing.T) {
	listener, envoyURL := settingsListener(t)
	live := liveSettings(t, "envoy_url: "+envoyURL)
	logs := &syncBuffer{}
	d := startDaemonOn(t, live, fakeRuntime(fake.NewRuntime(), &built{}), slog.New(slog.NewJSONHandler(logs, nil)), 10*time.Second)
	topic := notify.ControllerTopic(projectTokenOf(t, live))

	if status, _ := settingsRequest(t, d, testOperatorToken, api.SettingsChangeRequest{
		By:      "ana@example.com",
		Changes: map[string]json.RawMessage{"max_fix_attempts": json.RawMessage(`6`)},
	}); status != http.StatusOK {
		t.Fatalf("the settings route = %d", status)
	}
	setInConfig(t, live, "review_round_cap", "5")

	testwait.Eventually(t, "the daemon to settle the file it is watching", func() bool {
		return d.state().Settings.ReviewRoundCap == 5
	})
	state := d.state()
	if state.Settings.MaxFixAttempts != 6 {
		t.Errorf("max_fix_attempts = %d, want the route's 6: the file changed no such key", state.Settings.MaxFixAttempts)
	}
	if got := state.SettingSources["review_round_cap"]; got.Source != config.SourceFile || got.By != "" {
		t.Errorf("settingSources[review_round_cap] = %+v, want the file and nobody", got)
	}
	if got := state.SettingSources["max_fix_attempts"]; got.Source != config.SourceRoute || got.By != "ana@example.com" {
		t.Errorf("settingSources[max_fix_attempts] = %+v, want the route and ana", got)
	}
	for _, phrase := range []string{`"path":"review_round_cap"`, `"new":"5"`, `"source":"file"`} {
		if !strings.Contains(logs.String(), phrase) {
			t.Errorf("the log holds no %s for the file's own change; it holds:\n%s", phrase, logs)
		}
	}
	testwait.Eventually(t, "the controller to be told of both changes", func() bool {
		return len(listener.acceptedOn(topic)) == 2
	})
}

// A route change survives the daemon's restart, and the file takes its key back the moment the
// file itself changes that key (acceptance A5, "a route change survives a restart until the file
// changes that key"). The restarted daemon reads what the route changed from its own store,
// before it builds anything from the configuration, and its state still names the route, the
// person and the moment.
func TestARouteChangeSurvivesARestartUntilTheFileChangesThatKey(t *testing.T) {
	live := liveSettings(t)
	d := startDaemonOn(t, live, fakeRuntime(fake.NewRuntime(), &built{}), quietLogger(), 10*time.Second)
	if status, _ := settingsRequest(t, d, testOperatorToken, api.SettingsChangeRequest{
		By:      "ana@example.com",
		Changes: map[string]json.RawMessage{"admission_cap": json.RawMessage(`7`)},
	}); status != http.StatusOK {
		t.Fatalf("the settings route = %d", status)
	}
	changed := d.state().SettingSources["admission_cap"]
	d.stop()

	d = restartOn(t, live)

	state := d.state()
	if state.Settings.AdmissionCap != 7 {
		t.Fatalf("admission_cap after the restart = %d, want the route's 7", state.Settings.AdmissionCap)
	}
	// The moment is the one the store kept, which the row was written with: the state's own
	// stamp of the same change is a different read of the clock, microseconds away, and Postgres
	// keeps a timestamp to the microsecond. What a restart must keep is the change's own
	// instant, not one clock read of it.
	if got := state.SettingSources["admission_cap"]; got.Source != config.SourceRoute ||
		got.By != "ana@example.com" || got.At.Sub(changed.At).Abs() > time.Second {
		t.Fatalf("settingSources[admission_cap] after the restart = %+v, want the change as it was made, %+v", got, changed)
	}

	// The file changes that key: the daemon it comes up as runs on the file's own value, and its
	// state names the file.
	d.stop()
	setInConfig(t, live, "admission_cap", "9")
	d = restartOn(t, live)

	state = d.state()
	if state.Settings.AdmissionCap != 9 {
		t.Fatalf("admission_cap = %d, want the file's own 9: the file changed that key", state.Settings.AdmissionCap)
	}
	if got := state.SettingSources["admission_cap"]; got.Source != config.SourceFile {
		t.Fatalf("settingSources[admission_cap] = %+v, want the file", got)
	}
}

// A key fixed for the process is refused by name through the running daemon's own route, and the
// daemon goes on serving on the socket it bound (acceptance A5).
func TestTheRunningDaemonRefusesAFixedKeyByName(t *testing.T) {
	live := liveSettings(t)
	d := startDaemonOn(t, live, fakeRuntime(fake.NewRuntime(), &built{}), quietLogger(), 10*time.Second)

	status, answer := settingsRequest(t, d, testOperatorToken, api.SettingsChangeRequest{
		By: "ana@example.com",
		Changes: map[string]json.RawMessage{
			"bind": json.RawMessage(`"0.0.0.0"`),
			"port": json.RawMessage(`13999`),
		},
	})

	if status != http.StatusOK {
		t.Fatalf("the settings route = %d", status)
	}
	for _, path := range []string{"bind", "port"} {
		row, _ := config.ClassOf(path)
		if got := answer[path]; got.Applied != "" || got.Refused != row.Why {
			t.Errorf("the answer for %s = %+v, want its row's refusal %q", path, got, row.Why)
		}
	}
	if got := d.state().Settings.Bind; got != "127.0.0.1" {
		t.Errorf("bind = %q, want the socket the daemon bound at boot", got)
	}
}

// A call with no bearer at all is refused, and nothing of the daemon moves (acceptance A5).
func TestTheRunningDaemonRefusesASettingsCallWithNoToken(t *testing.T) {
	live := liveSettings(t)
	d := startDaemonOn(t, live, fakeRuntime(fake.NewRuntime(), &built{}), quietLogger(), 10*time.Second)

	status, _ := settingsRequest(t, d, "", api.SettingsChangeRequest{
		Changes: map[string]json.RawMessage{"admission_cap": json.RawMessage(`7`)},
	})

	if status != http.StatusForbidden {
		t.Fatalf("the settings route with no token = %d, want 403", status)
	}
	if got := d.state().Settings.AdmissionCap; got != 4 {
		t.Fatalf("admission_cap = %d, want the file's own 4", got)
	}
}

// restartOn starts the daemon again over the same configuration file and the same database, as a
// pod restarted onto the same ConfigMap does: a settings holder of its own, reading that file
// afresh. The API port moves to one this test holds, since the port the stopped daemon bound is
// nothing this harness can hand out twice (heldListen).
func restartOn(t *testing.T, live *config.Live) *daemon {
	t.Helper()
	setInConfig(t, live, "port", strconv.Itoa(holdPortOn(t, "127.0.0.1")))
	next, err := config.NewLive(live.Path(), func(string) string { return "" }, nil)
	if err != nil {
		t.Fatalf("resolve %s: %v", live.Path(), err)
	}
	return startDaemonOn(t, next, fakeRuntime(fake.NewRuntime(), &built{}), quietLogger(), 10*time.Second)
}

// setInConfig writes `<key>: <value>` into the configuration file the holder reads, replacing
// the line the file already holds for that key: a file carrying one key twice is a file no
// operator writes, and no two readers of it agree.
func setInConfig(t *testing.T, live *config.Live, key, value string) {
	t.Helper()
	path := live.Path()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	lines := strings.Split(strings.TrimRight(string(body), "\n"), "\n")
	written := key + ": " + value
	replaced := false
	for i, line := range lines {
		if strings.HasPrefix(line, key+":") {
			lines[i], replaced = written, true
			break
		}
	}
	if !replaced {
		lines = append(lines, written)
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// projectTokenOf is the claim project token of the holder's project, which the controller's
// topic is keyed by (notify.ControllerTopic).
func projectTokenOf(t *testing.T, live *config.Live) string {
	t.Helper()
	token, err := claim.ProjectToken(live.Snapshot().Project)
	if err != nil {
		t.Fatalf("the project token of %s: %v", live.Snapshot().Project, err)
	}
	return token
}
