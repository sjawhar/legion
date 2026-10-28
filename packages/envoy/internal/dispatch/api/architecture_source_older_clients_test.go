package api

// The singular architecture-source routes kept for dispatch clients older than the per-repository
// sources: their contract and their warnings. The GitHub fake and the source helpers are shared
// with architecture_source_test.go.

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// The WARN lines the singular routes log, one per call, so an operator can find the callers that
// still need an upgrade before the routes are removed.
const (
	singularReadWarning = "dispatch: GET /api/v1/projects/{key}/architecture-source called by a dispatch client older than this release; upgrade it, the route will be removed"
	singularSyncWarning = "dispatch: POST /api/v1/projects/{key}/architecture-source/sync called by a dispatch client older than this release; upgrade it, the route will be removed"
)

// captureSourceWarnings sends the default logger to a log until the test ends. The function it
// answers takes the WARN records about an architecture-source route logged since its last call,
// each as "<message> project=<project>", in order. The server's background goroutines (a
// document settlement among them) write that log while the test reads it, so it is a locked log
// read from an offset, never a buffer the test resets.
func captureSourceWarnings(t *testing.T) func() []string {
	t.Helper()
	previous := slog.Default()
	logs := &storetest.LockedLog{}
	slog.SetDefault(slog.New(slog.NewJSONHandler(logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	read := 0
	return func() []string {
		t.Helper()
		logged := logs.String()
		since := logged[read:]
		read = len(logged)
		warnings := []string{}
		decoder := json.NewDecoder(strings.NewReader(since))
		for decoder.More() {
			var record struct {
				Level   string `json:"level"`
				Msg     string `json:"msg"`
				Project string `json:"project"`
			}
			if err := decoder.Decode(&record); err != nil {
				t.Fatalf("decode log record: %v\n%s", err, since)
			}
			if record.Level == slog.LevelWarn.String() && strings.Contains(record.Msg, "architecture-source") {
				warnings = append(warnings, record.Msg+" project="+record.Project)
			}
		}
		return warnings
	}
}

// A dispatch client older than this release reads the singular route on every issue create and
// takes null as "no model": it is kept with that contract, answering the project's first source,
// so a client and server that ship separately do not turn every create into "Could not check".
// Each call logs one WARN naming the route and the project.
func TestArchitectureSourceSingularReadForOlderClients(t *testing.T) {
	warnings := captureSourceWarnings(t)
	handler, _ := newArchitectureSourceServer(t, twoSourceFake())
	createTestProject(t, handler, "CORE")
	read := func(project string) *httptest.ResponseRecorder {
		return agentRequest(t, handler, http.MethodGet, "/api/v1/projects/"+project+"/architecture-source", nil, "agent-token")
	}

	if none := read("CORE"); none.Code != http.StatusOK || strings.TrimSpace(none.Body.String()) != "null" {
		t.Fatalf("singular read without a source: status=%d body=%s, want 200 null", none.Code, none.Body.String())
	}
	if absent := read("NOPE"); absent.Code != http.StatusNotFound || decodeBody[map[string]string](t, absent)["code"] != "NOT_FOUND" {
		t.Fatalf("singular read of a project that does not exist: status=%d body=%s, want 404 NOT_FOUND", absent.Code, absent.Body.String())
	}
	if got, want := warnings(), []string{singularReadWarning + " project=CORE", singularReadWarning + " project=NOPE"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("WARNs for the reads without a source = %q, want one per call: %q", got, want)
	}

	putSource(t, handler, "CORE", "legion/arch", "main")
	putSource(t, handler, "CORE", "acme/infra", "release")
	warnings()
	first := read("CORE")
	if first.Code != http.StatusOK {
		t.Fatalf("singular read with two sources: status=%d body=%s", first.Code, first.Body.String())
	}
	if got, want := decodeStrict[architectureSourceResponse](t, first).String(), "CORE legion/arch@main enabled=true commit=- error=- components=0"; got != want {
		t.Fatalf("singular read with two sources = %s, want the first added, %s", got, want)
	}
	if got, want := warnings(), []string{singularReadWarning + " project=CORE"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("WARNs for the read with two sources = %q, want %q", got, want)
	}
}

// `dispatch architecture-sync` from a client older than this release posts the singular sync and
// prints the one row it answers. It is kept: every source is attempted, and the row answered is
// the first that failed, or the first when none did, so a failure on any source reaches that
// client. An access failure is recorded on its row rather than refused with a 409. Each call logs
// one WARN naming the route and the project.
func TestArchitectureSourceSingularSyncForOlderClients(t *testing.T) {
	sync := func(t *testing.T, handler http.Handler, project string) *httptest.ResponseRecorder {
		t.Helper()
		return agentRequest(t, handler, http.MethodPost, "/api/v1/projects/"+project+"/architecture-source/sync", nil, "agent-token")
	}
	warnings := captureSourceWarnings(t)

	handler, _ := newArchitectureSourceServer(t, twoSourceFake())
	createTestProject(t, handler, "CORE")
	for _, project := range []string{"CORE", "NOPE"} {
		if missing := sync(t, handler, project); missing.Code != http.StatusNotFound || decodeBody[map[string]string](t, missing)["code"] != "SOURCE_NOT_FOUND" {
			t.Fatalf("singular sync of %s without a source: status=%d body=%s, want 404 SOURCE_NOT_FOUND", project, missing.Code, missing.Body.String())
		}
	}
	if got, want := warnings(), []string{singularSyncWarning + " project=CORE", singularSyncWarning + " project=NOPE"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("WARNs for the syncs without a source = %q, want one per call: %q", got, want)
	}

	for _, tc := range []struct {
		name    string
		failing string
		access  bool
		want    string
	}{
		{"both sources healthy", "", false,
			"CORE legion/arch@main enabled=true commit=" + shaArch + " error=- components=2"},
		{"model error on the second source", "acme/infra", false,
			`CORE acme/infra@main enabled=true commit=` + shaInfra + ` error=broken.md: unknown parent "nowhere" components=1`},
		{"access failure on the first source", "legion/arch", true,
			"CORE legion/arch@main enabled=true commit=" + shaArch + " error=the branch does not exist: main on legion/arch components=2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			warnings := captureSourceWarnings(t)
			fake := twoSourceFake()
			handler, _ := newArchitectureSourceServer(t, fake)
			createTestProject(t, handler, "CORE")
			putSource(t, handler, "CORE", "legion/arch", "main")
			putSource(t, handler, "CORE", "acme/infra", "main")
			syncSources(t, handler, "CORE")

			if failing := fake.repos[tc.failing]; failing != nil && tc.access {
				failing.branchMissing = true
			} else if failing != nil {
				failing.commit = shaBroken
				failing.files = map[string]string{"broken.md": "---\ntitle: Broken\nparent: nowhere\n---\nBroken.\n"}
			}
			fake.headReads = nil
			warnings()
			synced := sync(t, handler, "CORE")
			if synced.Code != http.StatusOK {
				t.Fatalf("singular sync: status=%d body=%s", synced.Code, synced.Body.String())
			}
			if want := []string{"legion/arch@main", "acme/infra@main"}; !reflect.DeepEqual(fake.headReads, want) {
				t.Fatalf("head reads = %q, want both sources attempted: %q", fake.headReads, want)
			}
			if got := decodeStrict[architectureSourceResponse](t, synced).String(); got != tc.want {
				t.Fatalf("singular sync = %s, want %s", got, tc.want)
			}
			if got, want := warnings(), []string{singularSyncWarning + " project=CORE"}; !reflect.DeepEqual(got, want) {
				t.Fatalf("WARNs for the singular sync = %q, want %q", got, want)
			}
		})
	}
}
