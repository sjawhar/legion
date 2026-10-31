package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/processes"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// getDashboardBuildsAs is GET /api/v1/dashboard/builds as a person ("cookie") or the shared agent
// token ("bearer"), answering the status, the decoded body and the raw live array.
func getDashboardBuildsAs(t *testing.T, handler http.Handler, credential string) (int, dashboardBuilds, string) {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, "/api/v1/dashboard/builds", nil)
	switch credential {
	case "cookie":
		request.Header.Set("X-Dispatch-User", "alice")
	case "bearer":
		request.Header.Set("Authorization", "Bearer agent-token")
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	var body dashboardBuilds
	var raw struct {
		Live json.RawMessage `json:"live"`
	}
	if response.Code == http.StatusOK {
		if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode %s: %v", response.Body.String(), err)
		}
		if err := json.Unmarshal(response.Body.Bytes(), &raw); err != nil {
			t.Fatalf("decode %s: %v", response.Body.String(), err)
		}
	}
	return response.Code, body, string(raw.Live)
}

// registerProcess writes a registry row for a process serving build whose last heartbeat was
// ageSeconds ago by the database's clock.
func registerProcess(t *testing.T, database *store.Store, build string, ageSeconds float64) string {
	t.Helper()
	var instance string
	if err := database.Pool.QueryRow(context.Background(), `
		insert into server_processes (instance, dashboard_build, signing_keys, heartbeat_at)
		values (gen_random_uuid(), $1, '{fingerprint}', now() - make_interval(secs => $2))
		returning instance::text`, build, ageSeconds).Scan(&instance); err != nil {
		t.Fatalf("register a process on build %q: %v", build, err)
	}
	return instance
}

// publishBuild writes the dashboard_builds row a process writes at boot for build, naming entry.
func publishBuild(t *testing.T, database *store.Store, build, entry string) {
	t.Helper()
	if _, err := database.Pool.Exec(context.Background(), `insert into dashboard_builds (build, entry) values ($1, $2)`, build, entry); err != nil {
		t.Fatalf("publish build %q: %v", build, err)
	}
}

// The route answers this process's entry chunk and the entry chunks of the builds the live
// processes serve, each once and sorted, for a person and an agent alike. A process whose
// heartbeat has left the live window, whose build no process published, whose page names no entry
// or that serves no dashboard adds nothing; once a process ages out, its build leaves the list.
func TestDashboardBuildsNamesTheEntryChunksOfTheLiveProcesses(t *testing.T) {
	handler, database, _ := newTestServer(t, testServerOptions{dashboardEntry: "/assets/index-a.js"})
	publishBuild(t, database, "build-a", "/assets/index-a.js")
	publishBuild(t, database, "build-b", "/assets/index-b.js")
	publishBuild(t, database, "build-old", "/assets/index-old.js")
	publishBuild(t, database, "build-no-entry", "")
	b := registerProcess(t, database, "build-b", 0)
	registerProcess(t, database, "build-a", 0)
	registerProcess(t, database, "build-a", 1)
	registerProcess(t, database, "build-old", processes.LiveWindow.Seconds()+60)
	registerProcess(t, database, "build-never-published", 0)
	registerProcess(t, database, "build-no-entry", 0)
	registerProcess(t, database, "", 0)

	for _, credential := range []string{"cookie", "bearer"} {
		code, body, _ := getDashboardBuildsAs(t, handler, credential)
		if want := []string{"/assets/index-a.js", "/assets/index-b.js"}; code != http.StatusOK || body.Serving != "/assets/index-a.js" || !slices.Equal(body.Live, want) {
			t.Fatalf("as %s: %d %+v, want 200 serving /assets/index-a.js and live %q", credential, code, body, want)
		}
	}

	// B's process stops beating: once its heartbeat is older than the live window, only A is live.
	if _, err := database.Pool.Exec(context.Background(), `
		update server_processes set heartbeat_at = now() - make_interval(secs => $2) where instance = $1::uuid`,
		b, processes.LiveWindow.Seconds()+1); err != nil {
		t.Fatal(err)
	}
	if code, body, _ := getDashboardBuildsAs(t, handler, "cookie"); code != http.StatusOK || !slices.Equal(body.Live, []string{"/assets/index-a.js"}) {
		t.Fatalf("after B aged out: %d %+v, want live [/assets/index-a.js]", code, body)
	}
}

// With no live process and no dashboard of its own, the route answers an empty serving and an
// empty live array, never null, and refuses a caller with no credential.
func TestDashboardBuildsWithNothingLive(t *testing.T) {
	handler, database, _ := newTestServer(t, testServerOptions{})
	publishBuild(t, database, "build-a", "/assets/index-a.js")
	if code, body, live := getDashboardBuildsAs(t, handler, "cookie"); code != http.StatusOK || body.Serving != "" || live != "[]" {
		t.Fatalf("nothing live: %d %+v (live %s), want 200 with serving \"\" and live []", code, body, live)
	}
	if code, _, _ := getDashboardBuildsAs(t, handler, "anonymous"); code != http.StatusUnauthorized {
		t.Fatalf("anonymous: %d, want 401", code)
	}
}
