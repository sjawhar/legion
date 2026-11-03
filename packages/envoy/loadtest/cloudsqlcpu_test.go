package main

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	cloudsqlTestProject  = "theorem-agent-infra"
	cloudsqlTestInstance = "dispatch-60a4923"
	cloudsqlTestToken    = "ya29.test-token"
)

// cloudsqlTokenServer is the metadata server: it answers the default service account's token to
// a read that carries Metadata-Flavor: Google, as the real one does.
func cloudsqlTokenServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Metadata-Flavor") != "Google" {
			http.Error(w, "missing Metadata-Flavor", http.StatusForbidden)
			return
		}
		fmt.Fprintf(w, `{"access_token":%q,"expires_in":3599,"token_type":"Bearer"}`, cloudsqlTestToken)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// cloudsqlAnswer is Cloud Monitoring's answer to projects.timeSeries.list in its recorded shape:
// one cloudsql_database series, its points newest first, with the alignment's unit.
func cloudsqlAnswer(nextPageToken string, points ...cloudsqlCPUPoint) string {
	var b strings.Builder
	fmt.Fprintf(&b, `{"timeSeries":[{"metric":{"type":%q,"labels":{}},`, cloudsqlCPUMetric)
	fmt.Fprintf(&b, `"resource":{"type":"cloudsql_database","labels":{"database_id":%q,"project_id":%q,"region":"us-central1"}},`,
		cloudsqlDatabaseID(cloudsqlTestProject, cloudsqlTestInstance), cloudsqlTestProject)
	b.WriteString(`"metricKind":"GAUGE","valueType":"DOUBLE","points":[`)
	for i, p := range points {
		if i > 0 {
			b.WriteString(",")
		}
		fmt.Fprintf(&b, `{"interval":{"startTime":%q,"endTime":%q},"value":{"doubleValue":%g}}`,
			p.At.Add(-cloudsqlAlignment).Format(time.RFC3339), p.At.Format(time.RFC3339), p.Utilization)
	}
	b.WriteString(`]}],"unit":"10^2.%"`)
	if nextPageToken != "" {
		fmt.Fprintf(&b, `,"nextPageToken":%q`, nextPageToken)
	}
	b.WriteString("}")
	return b.String()
}

// cloudsqlStepDir is a step directory whose meta.json holds the test window (cgroup_test.go).
func cloudsqlStepDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	meta, err := json.Marshal(metaFile{Step: "cloudsql", WindowStart: cgWinStart, WindowEnd: cgWinEnd})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "meta.json"), meta, 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

// runCloudSQLCPUAgainst runs the command against the two test servers and reads back what it
// wrote.
func runCloudSQLCPUAgainst(t *testing.T, api string, extra ...string) (string, cloudsqlCPUFile, error) {
	t.Helper()
	dir := cloudsqlStepDir(t)
	args := append([]string{
		"-step", dir, "-project", cloudsqlTestProject, "-instance", cloudsqlTestInstance,
		"-api", api, "-token-url", cloudsqlTokenServer(t).URL,
	}, extra...)
	err := runCloudSQLCPU(args)
	out := filepath.Join(dir, "cloudsql-cpu.json")
	var f cloudsqlCPUFile
	if b, readErr := os.ReadFile(out); readErr == nil {
		if err := json.Unmarshal(b, &f); err != nil {
			t.Fatalf("cloudsql-cpu.json is not JSON: %v: %s", err, b)
		}
	}
	return out, f, err
}

// TestCloudSQLCPUReadsTheWindow: against a recorded-shape answer, the request carries the
// filter, the interval and the 60 s alignment the step's window asks for, and the file holds the
// mean, the peak and every sample, as fractions and as cores of the stated vCPU count.
func TestCloudSQLCPUReadsTheWindow(t *testing.T) {
	points := []cloudsqlCPUPoint{
		{At: cgWinStart.Add(3 * time.Minute), Utilization: 0.30},
		{At: cgWinStart.Add(2 * time.Minute), Utilization: 0.50},
		{At: cgWinStart.Add(1 * time.Minute), Utilization: 0.25},
	}
	var got *http.Request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r
		fmt.Fprint(w, cloudsqlAnswer("", points...))
	}))
	t.Cleanup(srv.Close)

	out, f, err := runCloudSQLCPUAgainst(t, srv.URL)
	if err != nil {
		t.Fatalf("cloudsql-cpu: %v", err)
	}
	if got == nil {
		t.Fatal("the command made no request to Cloud Monitoring")
	}
	if got.Method != http.MethodGet {
		t.Errorf("the read is a %s, want the GET of projects.timeSeries.list", got.Method)
	}
	if want := "/v3/projects/" + cloudsqlTestProject + "/timeSeries"; got.URL.Path != want {
		t.Errorf("the read asked for %s, want %s", got.URL.Path, want)
	}
	if auth := got.Header.Get("Authorization"); auth != "Bearer "+cloudsqlTestToken {
		t.Errorf("the read carries %q, want the metadata server's token", auth)
	}
	q := got.URL.Query()
	for key, want := range map[string]string{
		"filter": fmt.Sprintf("metric.type=%q AND resource.labels.database_id=%q", cloudsqlCPUMetric,
			cloudsqlDatabaseID(cloudsqlTestProject, cloudsqlTestInstance)),
		"interval.startTime":           cgWinStart.Format(time.RFC3339),
		"interval.endTime":             cgWinEnd.Format(time.RFC3339),
		"aggregation.alignmentPeriod":  "60s",
		"aggregation.perSeriesAligner": "ALIGN_MEAN",
	} {
		if q.Get(key) != want {
			t.Errorf("the read's %s is %q, want %q", key, q.Get(key), want)
		}
	}

	if f.Samples != 3 {
		t.Fatalf("%s holds %d samples, want 3", out, f.Samples)
	}
	for _, c := range []struct {
		what      string
		got, want float64
	}{
		{"utilization_mean", f.UtilizationMean, 0.35},
		{"utilization_max", f.UtilizationMax, 0.50},
		{"cores_mean", f.CoresMean, 5.6},
		{"cores_peak_60s", f.CoresPeak60s, 8.0},
		{"limit_cores", f.LimitCores, 16},
		{"window_s", f.WindowS, cgWinEnd.Sub(cgWinStart).Seconds()},
		{"alignment_period_s", f.AlignmentPeriodS, 60},
	} {
		if math.Abs(c.got-c.want) > 1e-9 {
			t.Errorf("%s is %g, want %g", c.what, c.got, c.want)
		}
	}
	if f.DatabaseID != cloudsqlDatabaseID(cloudsqlTestProject, cloudsqlTestInstance) || f.Metric != cloudsqlCPUMetric {
		t.Errorf("the file names %s of %s, want the instance's CPU utilization", f.Metric, f.DatabaseID)
	}
	// The answer's points are newest first; the file's are the window in order, each with the
	// cores its fraction works out to.
	for i, want := range []cloudsqlCPUPoint{points[2], points[1], points[0]} {
		p := f.Points[i]
		if !p.At.Equal(want.At) || p.Utilization != want.Utilization || math.Abs(p.Cores-want.Utilization*16) > 1e-9 {
			t.Errorf("point %d is %+v, want %s at %g (%g cores)", i, p, want.At.Format(time.RFC3339), want.Utilization, want.Utilization*16)
		}
	}
}

// TestCloudSQLCPUFollowsThePages: a window the API answers in two pages gives every point once.
func TestCloudSQLCPUFollowsThePages(t *testing.T) {
	pages := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pages++
		if token := r.URL.Query().Get("pageToken"); token == "" {
			fmt.Fprint(w, cloudsqlAnswer("page-2", cloudsqlCPUPoint{At: cgWinStart.Add(2 * time.Minute), Utilization: 0.50}))
			return
		} else if token != "page-2" {
			t.Errorf("the second read carries the page token %q", token)
		}
		fmt.Fprint(w, cloudsqlAnswer("", cloudsqlCPUPoint{At: cgWinStart.Add(time.Minute), Utilization: 0.25}))
	}))
	t.Cleanup(srv.Close)
	_, f, err := runCloudSQLCPUAgainst(t, srv.URL)
	if err != nil {
		t.Fatalf("cloudsql-cpu: %v", err)
	}
	if pages != 2 || f.Samples != 2 || math.Abs(f.UtilizationMean-0.375) > 1e-9 {
		t.Fatalf("read %d pages and %d samples, mean %g; want both pages' points once", pages, f.Samples, f.UtilizationMean)
	}
}

// TestCloudSQLCPURefusesWhatItCannotMeasure: a 403, a 404, a window with no sample and a body
// that does not decode are each refused by name, and none of them writes a file: Cloud SQL at 0
// cores would pass the rule that an unread instance must not pass.
func TestCloudSQLCPURefusesWhatItCannotMeasure(t *testing.T) {
	for _, tc := range []struct {
		name, body, want string
		status           int
	}{
		{name: "403", status: http.StatusForbidden, body: `{"error":{"message":"Permission monitoring.timeSeries.list denied"}}`, want: "answered 403"},
		{name: "404", status: http.StatusNotFound, body: `{"error":{"message":"Project not found"}}`, want: "answered 404"},
		{name: "empty window", status: http.StatusOK, body: `{"unit":"10^2.%"}`, want: "holds no sample"},
		{name: "undecodable body", status: http.StatusOK, body: "<html>no JSON here</html>", want: "did not decode"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				fmt.Fprint(w, tc.body)
			}))
			t.Cleanup(srv.Close)
			out, _, err := runCloudSQLCPUAgainst(t, srv.URL)
			if err == nil {
				t.Fatalf("a %s was not refused", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("the refusal %q does not say %q", err, tc.want)
			}
			if !strings.Contains(err.Error(), cloudsqlDatabaseID(cloudsqlTestProject, cloudsqlTestInstance)) {
				t.Errorf("the refusal %q names no instance", err)
			}
			if _, statErr := os.Stat(out); statErr == nil {
				t.Errorf("a refused read wrote %s", out)
			}
		})
	}
}

// TestCloudSQLCPURefusesAMetadataServerWithNoToken: a pod whose identity gives no access token
// says so, rather than reading Cloud Monitoring without one.
func TestCloudSQLCPURefusesAMetadataServerWithNoToken(t *testing.T) {
	metadata := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "no service account", http.StatusNotFound)
	}))
	t.Cleanup(metadata.Close)
	err := runCloudSQLCPU([]string{
		"-step", cloudsqlStepDir(t), "-project", cloudsqlTestProject, "-instance", cloudsqlTestInstance,
		"-api", "http://127.0.0.1:1", "-token-url", metadata.URL,
	})
	if err == nil || !strings.Contains(err.Error(), "no access token") {
		t.Fatalf("want a refusal naming the missing access token, got %v", err)
	}
}

// TestCloudSQLCPURefusesBadFlags: the flags are checked before any connection.
func TestCloudSQLCPURefusesBadFlags(t *testing.T) {
	dir := cloudsqlStepDir(t)
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"-project", cloudsqlTestProject, "-instance", cloudsqlTestInstance}, "-step is required"},
		{[]string{"-step", dir, "-instance", cloudsqlTestInstance}, "-project is required"},
		{[]string{"-step", dir, "-project", cloudsqlTestProject}, "-instance is required"},
		{[]string{"-step", dir, "-project", cloudsqlTestProject, "-instance", cloudsqlTestInstance, "-vcpus", "0"}, "-vcpus must be above 0"},
	} {
		if err := runCloudSQLCPU(tc.args); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("runCloudSQLCPU(%v): want an error naming %q, got %v", tc.args, tc.want, err)
		}
	}
}
