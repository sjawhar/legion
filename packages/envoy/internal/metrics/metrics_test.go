package metrics

import (
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// scrape returns the registry's exposition text.
func scrape(t *testing.T, r *Registry) string {
	t.Helper()
	recorder := httptest.NewRecorder()
	r.Handler().ServeHTTP(recorder, httptest.NewRequest("GET", "/metrics", nil))
	return recorder.Body.String()
}

// An optional gauge whose function reports no value still names itself (HELP and TYPE), so the
// series is known to the scraper, but writes no sample line: the reader sees the value as missing,
// never as a number the function could not stand behind. A value it does report, zero included,
// is written as a sample.
func TestAnOptionalGaugeWritesNoSampleWhenItsFunctionReportsNone(t *testing.T) {
	var value atomic.Int64
	var known atomic.Bool
	r := New()
	r.NewOptionalGaugeFunc("envoy_test_listed", "Rows the test lists", func() (int64, bool) {
		return value.Load(), known.Load()
	})

	body := scrape(t, r)
	for _, line := range []string{"# HELP envoy_test_listed Rows the test lists\n", "# TYPE envoy_test_listed gauge\n"} {
		if !strings.Contains(body, line) {
			t.Fatalf("scrape with no value lacks %q:\n%s", line, body)
		}
	}
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, "envoy_test_listed") {
			t.Fatalf("scrape with no value has the sample %q:\n%s", line, body)
		}
	}

	known.Store(true)
	if body := scrape(t, r); !strings.Contains(body, "\nenvoy_test_listed 0\n") {
		t.Fatalf("scrape of a known zero lacks the sample 0:\n%s", body)
	}
	value.Store(3)
	if body := scrape(t, r); !strings.Contains(body, "\nenvoy_test_listed 3\n") {
		t.Fatalf("scrape of a known 3 lacks the sample 3:\n%s", body)
	}
}
