package api

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/sjawhar/envoy/oidc"
	"github.com/sjawhar/envoy/oidc/oidctest"

	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/settings"
)

// settingsFile is the smallest configuration the settings route's tests change.
const settingsRouteFile = `project: DEMO
state_dir: /var/lib/legion
postgres_dsn: postgres://legion@127.0.0.1:5432/legion
admission_cap: 4
instructions: /etc/legion/instructions.md
`

// settingsRouteServer is a server whose settings route reaches a real settings holder over a
// legion.yaml of the test's own: the route is held to what the daemon's own holder answers —
// the class of each path, its holder and the values as a log line shows them — and not to a
// stand-in that could agree with the route and with nothing else. It keeps no store: what the
// store does with a change is internal/settings' own test.
func settingsRouteServer(t *testing.T, auth SettingsRouteAuth) (http.Handler, *config.Live) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "legion.yaml")
	if err := os.WriteFile(path, []byte(settingsRouteFile), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	live, err := config.NewLive(path, func(string) string { return "" }, nil)
	if err != nil {
		t.Fatalf("resolve %s: %v", path, err)
	}
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	keeper := settings.New(live, nil, quiet)
	server := NewServer("127.0.0.1", 8439, Options{
		Project: testProject, Log: quiet, ApplySettings: keeper.Route,
		Settings: NewLiveSettings(HeldSettings{OperatorToken: testOperatorToken, SettingsRoute: auth}),
	})
	return server.Handler, live
}

// settingsCall is one request to the settings route with the given bearer ("" sends none).
func settingsCall(t *testing.T, handler http.Handler, bearer string, body any) *httptest.ResponseRecorder {
	t.Helper()
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("encode the request: %v", err)
	}
	request := httptest.NewRequest(http.MethodPut, "/legion/v1/operator/settings", bytes.NewReader(encoded))
	if bearer != "" {
		request.Header.Set("Authorization", "Bearer "+bearer)
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

// settingsAnswer is the route's answer decoded.
func settingsAnswer(t *testing.T, recorder *httptest.ResponseRecorder) map[string]SettingResult {
	t.Helper()
	var answer SettingsChangeResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &answer); err != nil {
		t.Fatalf("decode %s: %v", recorder.Body.String(), err)
	}
	return answer.Settings
}

// The route answers, per key, whether the change applied at once, by a rebuild, or at the next
// launch, with what the daemon holds for it — and names the values as the daemon's own log line
// does, a credential withheld (acceptance A5, "the route's answer names, per key").
func TestTheSettingsRouteNamesWhatEachKeyDid(t *testing.T) {
	handler, live := settingsRouteServer(t, SettingsRouteAuth{})

	recorder := settingsCall(t, handler, testOperatorToken, SettingsChangeRequest{
		By: "ana@example.com",
		Changes: map[string]json.RawMessage{
			"admission_cap":              json.RawMessage(`9`),
			"worker_rpc_timeout_seconds": json.RawMessage(`11`),
			"instructions":               json.RawMessage(`"/etc/legion/instructions-2.md"`),
			"daemon_url":                 json.RawMessage(`"http://legion-demo-daemon.legion.svc:13370"`),
		},
	})

	if recorder.Code != http.StatusOK {
		t.Fatalf("the settings route = %d; body %s", recorder.Code, recorder.Body)
	}
	for path, want := range map[string]SettingResult{
		"admission_cap":              {Applied: string(config.OutcomeAtOnce), Old: "4", New: "9"},
		"worker_rpc_timeout_seconds": {Applied: string(config.OutcomeRebuilt), Holder: config.HolderWorkerStream, Old: "5s", New: "11s"},
		"instructions":               {Applied: string(config.OutcomeNextLaunch), Holder: config.HolderLaunchSpecs, Old: "/etc/legion/instructions.md", New: "/etc/legion/instructions-2.md"},
		// A credential, and an address whose userinfo may carry one, reads "(withheld)" on both
		// sides whatever moved: a rotation is seen without being shown.
		"daemon_url": {Applied: string(config.OutcomeNextLaunch), Holder: config.HolderRuntime, Old: "(withheld)", New: "(withheld)"},
	} {
		if got := settingsAnswer(t, recorder)[path]; got != want {
			t.Errorf("the answer for %s = %+v, want %+v", path, got, want)
		}
	}
	if cap := live.Snapshot().AdmissionCap; cap != 9 {
		t.Errorf("the holder reads admission_cap %d, want the applied 9", cap)
	}
}

// A key the daemon is already running on is the route's from now on and moved nothing, which the
// answer says rather than reporting a change that did not happen.
func TestTheSettingsRouteSaysWhenAKeyWasAlreadyWhatWasAsked(t *testing.T) {
	handler, _ := settingsRouteServer(t, SettingsRouteAuth{})

	recorder := settingsCall(t, handler, testOperatorToken, SettingsChangeRequest{
		Changes: map[string]json.RawMessage{"admission_cap": json.RawMessage(`4`)},
	})

	want := SettingResult{Applied: string(config.OutcomeAtOnce), Unchanged: true}
	if got := settingsAnswer(t, recorder)["admission_cap"]; got != want {
		t.Fatalf("the answer = %+v, want %+v", got, want)
	}
}

// A key fixed for the process is refused by name with the reason its row gives, and a key the
// class table does not hold is refused as an unknown setting. Neither stops the keys beside them
// (acceptance A5, "a key fixed for the process is refused by name").
func TestTheSettingsRouteRefusesTheFixedAndTheUnknownByName(t *testing.T) {
	handler, live := settingsRouteServer(t, SettingsRouteAuth{})

	recorder := settingsCall(t, handler, testOperatorToken, SettingsChangeRequest{
		Changes: map[string]json.RawMessage{
			"bind":          json.RawMessage(`"0.0.0.0"`),
			"port":          json.RawMessage(`13371`),
			"wishful_key":   json.RawMessage(`1`),
			"admission_cap": json.RawMessage(`6`),
		},
	})

	if recorder.Code != http.StatusOK {
		t.Fatalf("the settings route = %d; body %s", recorder.Code, recorder.Body)
	}
	answer := settingsAnswer(t, recorder)
	for _, path := range []string{"bind", "port"} {
		result := answer[path]
		if result.Applied != "" || result.Refused == "" {
			t.Errorf("the answer for %s = %+v, want a refusal and nothing applied", path, result)
		}
		if row, _ := config.ClassOf(path); result.Refused != row.Why {
			t.Errorf("the refusal of %s = %q, want its row's reason %q", path, result.Refused, row.Why)
		}
	}
	if got := answer["wishful_key"].Refused; got != "unknown setting" {
		t.Errorf("the refusal of an unknown key = %q, want \"unknown setting\"", got)
	}
	if got := answer["admission_cap"]; got.Applied != string(config.OutcomeAtOnce) || got.New != "6" {
		t.Errorf("the answer for admission_cap = %+v, want it applied beside the refusals", got)
	}
	if cap := live.Snapshot().AdmissionCap; cap != 6 {
		t.Errorf("the holder reads admission_cap %d, want the applied 6", cap)
	}
	if bind := live.Snapshot().Bind; bind != "127.0.0.1" {
		t.Errorf("bind = %q, want the file's own: a fixed key is refused, never applied", bind)
	}
}

// A value the loader will not take changes nothing: the daemon keeps the settings it has and the
// caller is told why.
func TestTheSettingsRouteRefusesAValueTheLoaderWillNotTake(t *testing.T) {
	handler, live := settingsRouteServer(t, SettingsRouteAuth{})

	recorder := settingsCall(t, handler, testOperatorToken, SettingsChangeRequest{
		Changes: map[string]json.RawMessage{"admission_cap": json.RawMessage(`0`)},
	})

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("the settings route = %d; body %s", recorder.Code, recorder.Body)
	}
	if cap := live.Snapshot().AdmissionCap; cap != 4 {
		t.Fatalf("admission_cap = %d, want the file's own 4: nothing was applied", cap)
	}
}

// A call with no token, and one with a bearer that is neither the operator's nor a token this
// deployment admits, are refused (acceptance A5, "a call with no token").
func TestTheSettingsRouteRefusesACallerItDoesNotKnow(t *testing.T) {
	handler, live := settingsRouteServer(t, SettingsRouteAuth{})
	body := SettingsChangeRequest{Changes: map[string]json.RawMessage{"admission_cap": json.RawMessage(`9`)}}

	for name, bearer := range map[string]string{"no token": "", "another bearer": "not-the-operators"} {
		t.Run(name, func(t *testing.T) {
			recorder := settingsCall(t, handler, bearer, body)
			if recorder.Code != http.StatusForbidden {
				t.Fatalf("the settings route with %s = %d; body %s", name, recorder.Code, recorder.Body)
			}
		})
	}
	if cap := live.Snapshot().AdmissionCap; cap != 4 {
		t.Fatalf("admission_cap = %d, want the file's own 4: no refused call applied anything", cap)
	}
}

// The projected ServiceAccount token arm: a token this deployment's cluster issuer signed, for
// the audience it names and naming a ServiceAccount it names, opens the route, and the change is
// recorded against the person the caller named rather than the account. A token for another
// audience, and one of another ServiceAccount, are refused (acceptance A5).
func TestTheSettingsRouteTakesTheServiceAccountTokensItNames(t *testing.T) {
	issuer := oidctest.New(t)
	key := issuer.PublishKey(t, "cluster-key")
	const audience = "legion-daemon"
	const dispatch = "system:serviceaccount:dispatch:dispatch"
	handler, live := settingsRouteServer(t, SettingsRouteAuth{
		Verifier:        oidc.NewLazyVerifier(issuer.URL(), audience),
		ServiceAccounts: []string{dispatch},
	})
	body := func(cap string) SettingsChangeRequest {
		return SettingsChangeRequest{
			By:      "ana@example.com",
			Changes: map[string]json.RawMessage{"admission_cap": json.RawMessage(cap)},
		}
	}

	recorder := settingsCall(t, handler, issuer.Mint(t, key, issuer.Claims(dispatch, audience)), body("8"))
	if recorder.Code != http.StatusOK {
		t.Fatalf("the settings route with Dispatch's projected token = %d; body %s", recorder.Code, recorder.Body)
	}
	if got := settingsAnswer(t, recorder)["admission_cap"]; got.New != "8" {
		t.Fatalf("the answer = %+v, want admission_cap 8", got)
	}

	for name, token := range map[string]string{
		"another audience":        issuer.Mint(t, key, issuer.Claims(dispatch, "some-other-audience")),
		"another ServiceAccount":  issuer.Mint(t, key, issuer.Claims("system:serviceaccount:default:curious", audience)),
		"a token of its own make": issuer.Mint(t, oidctest.NewKey(t, "cluster-key"), issuer.Claims(dispatch, audience)),
	} {
		t.Run(name, func(t *testing.T) {
			recorder := settingsCall(t, handler, token, body("12"))
			if recorder.Code != http.StatusForbidden {
				t.Fatalf("the settings route with %s = %d; body %s", name, recorder.Code, recorder.Body)
			}
		})
	}
	if cap := live.Snapshot().AdmissionCap; cap != 8 {
		t.Fatalf("admission_cap = %d, want the 8 the admitted token applied and nothing the refused ones asked for", cap)
	}
}

// A deployment that names no ServiceAccount takes the operator bearer alone: a token of a
// cluster issuer is nobody here, however sound it is.
func TestTheSettingsRouteTakesNoProjectedTokenWhenTheDeploymentNamesNone(t *testing.T) {
	issuer := oidctest.New(t)
	key := issuer.PublishKey(t, "cluster-key")
	handler, _ := settingsRouteServer(t, SettingsRouteAuth{})

	recorder := settingsCall(t, handler, issuer.Mint(t, key, issuer.Claims("system:serviceaccount:dispatch:dispatch", "legion-daemon")),
		SettingsChangeRequest{Changes: map[string]json.RawMessage{"admission_cap": json.RawMessage(`9`)}})

	if recorder.Code != http.StatusForbidden {
		t.Fatalf("the settings route = %d; body %s", recorder.Code, recorder.Body)
	}
}

// The settings route is a PUT and nothing else: the daemon's catch-all answers "no route" for
// every other method, as it does for every path it does not serve.
func TestTheSettingsPathAnswersNoRouteForAnotherMethod(t *testing.T) {
	handler, _ := settingsRouteServer(t, SettingsRouteAuth{})

	for _, method := range []string{http.MethodGet, http.MethodPost, http.MethodDelete} {
		request := httptest.NewRequest(method, "/legion/v1/operator/settings", nil)
		request.Header.Set("Authorization", "Bearer "+testOperatorToken)
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusNotFound {
			t.Errorf("%s /legion/v1/operator/settings = %d; want 404", method, recorder.Code)
		}
		if got := recorder.Body.String(); !jsonError(t, got, "no route") {
			t.Errorf("%s /legion/v1/operator/settings answered %s, want \"no route\"", method, got)
		}
	}
}

// jsonError is whether body is an error document saying want.
func jsonError(t *testing.T, body, want string) bool {
	t.Helper()
	var answer map[string]string
	if err := json.Unmarshal([]byte(body), &answer); err != nil {
		return false
	}
	return answer["error"] == want
}
