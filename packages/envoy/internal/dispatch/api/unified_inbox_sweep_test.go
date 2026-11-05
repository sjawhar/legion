package api

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/metrics"
)

func TestInboxShadowSweepExportsDurationAndFailures(t *testing.T) {
	for _, broken := range []bool{false, true} {
		name := "success"
		if broken {
			name = "query failure"
		}
		t.Run(name, func(t *testing.T) {
			_, database, _ := newTestServer(t, testServerOptions{})
			if broken {
				if _, err := database.Pool.Exec(context.Background(), `alter view graph_inbox_edges rename to missing_inbox_edges`); err != nil {
					t.Fatal(err)
				}
			}
			registry := metrics.New()
			runSweep(t, func(ctx context.Context) {
				RunInboxShadowSweep(ctx, database.Pool, sweepHorizon(database.Pool), registry)
			})
			scrape := func() string {
				response := httptest.NewRecorder()
				registry.Handler().ServeHTTP(response, httptest.NewRequest("GET", "/metrics", nil))
				return response.Body.String()
			}
			waitUntil(t, 10*time.Second, "inbox pass duration metric", func() bool {
				return strings.Contains(scrape(), "envoy_dispatch_inbox_shadow_duration_seconds_count 1\n")
			})
			text := scrape()
			failed := strings.Contains(text, "envoy_dispatch_inbox_shadow_failures_total 1\n")
			if failed != broken {
				t.Fatalf("query failure=%v, metrics:\n%s", broken, text)
			}
			if !broken {
				var checked int
				if err := database.Pool.QueryRow(context.Background(), `select count(*) from inbox_shadow_latest`).Scan(&checked); err != nil || checked != 2 {
					t.Fatalf("successful sweep did not compare the two people: checked=%d err=%v", checked, err)
				}
			} else {
				report, err := readInboxShadowReport(context.Background(), database.Pool, "")
				if err != nil || report.Errors != 2 || report.CheckedUsers != 2 || report.Ready {
					t.Fatalf("failed comparisons must persist errors and refuse cutover: %+v err=%v", report, err)
				}
			}
			t.Logf("sweep duration recorded; failure counter incremented=%v", failed)
		})
	}
}
