package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"strconv"
	"strings"
	"testing"
	"time"
)

// unreachablePG is a Postgres URL that refuses every connect: each reader of the run's database is
// tested against it to show that it reports the failure instead of an empty result that reads as
// measured.
const unreachablePG = "postgres://postgres@127.0.0.1:1/dispatch?sslmode=disable"

// TestReadErrorsLogsTheFirstAndATotalPastThem: every failure counts; run.log gets one line for each
// of the first pollErrorsLogged, then one line naming the probe and the total, only when more
// failed.
func TestReadErrorsLogsTheFirstAndATotalPastThem(t *testing.T) {
	for _, failures := range []int{1, pollErrorsLogged, pollErrorsLogged + 2} {
		var buf bytes.Buffer
		errs := readErrors{name: "probe", lg: log.New(&buf, "", 0)}
		for i := range failures {
			errs.add(fmt.Errorf("failure %d", i+1))
		}
		if got := errs.done(); got != failures {
			t.Fatalf("done() = %d after %d failures", got, failures)
		}
		lines := strings.Split(strings.TrimSuffix(buf.String(), "\n"), "\n")
		logged := 0
		for i := range failures {
			for _, l := range lines {
				if strings.Contains(l, fmt.Sprintf("failure %d", i+1)) {
					logged++
					if i >= pollErrorsLogged {
						t.Errorf("%d failures: failure %d is logged, past the first %d", failures, i+1, pollErrorsLogged)
					}
				}
			}
		}
		if logged != min(failures, pollErrorsLogged) {
			t.Errorf("%d failures: %d logged, want %d", failures, logged, min(failures, pollErrorsLogged))
		}
		totals := len(lines) - logged
		if failures <= pollErrorsLogged {
			if totals != 0 {
				t.Errorf("%d failures: %d lines besides the failures, want no total:\n%s", failures, totals, buf.String())
			}
			continue
		}
		last := lines[len(lines)-1]
		if totals != 1 || !strings.Contains(last, "probe") || !strings.Contains(last, strconv.Itoa(failures)) {
			t.Errorf("%d failures: want one last line that names the probe and the count:\n%s", failures, buf.String())
		}
	}
}

func TestPollCountsEveryFailedRead(t *testing.T) {
	var buf bytes.Buffer
	lg := log.New(&buf, "", 0)
	calls := 0
	from := time.Now()
	failed := poll(context.Background(), from, from.Add(30*time.Millisecond), time.Millisecond, "probe", lg, func(context.Context) error {
		calls++
		if calls%2 == 0 {
			return nil
		}
		return errors.New("relation does not exist")
	})
	if calls == 0 || failed != (calls+1)/2 {
		t.Fatalf("%d failed of %d reads, want every odd read counted", failed, calls)
	}
	if logged := strings.Count(buf.String(), "relation does not exist"); logged != min(failed, pollErrorsLogged) {
		t.Fatalf("%d failures logged of %d, want the first %d:\n%s", logged, failed, pollErrorsLogged, buf.String())
	}
}

func TestPollStopsUncountedWhenTheContextEnds(t *testing.T) {
	var buf bytes.Buffer
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	calls := 0
	from := time.Now()
	failed := poll(ctx, from, from.Add(time.Hour), time.Millisecond, "probe", log.New(&buf, "", 0), func(context.Context) error {
		calls++
		cancel()
		return context.Canceled
	})
	if failed != 0 || calls != 1 || buf.Len() != 0 {
		t.Fatalf("%d failed of %d reads, log %q: want one read, not counted, nothing logged", failed, calls, buf.String())
	}
}

func TestPollSkipsTheSlotsASlowReadOverran(t *testing.T) {
	calls := 0
	from := time.Now()
	failed := poll(context.Background(), from, from.Add(60*time.Millisecond), 10*time.Millisecond, "probe", log.New(&bytes.Buffer{}, "", 0), func(context.Context) error {
		calls++
		time.Sleep(25 * time.Millisecond)
		return nil
	})
	// 7 slots (0, 10, ..., 60 ms); each 25 ms read overruns the next slot, which is skipped.
	if failed != 0 || calls == 0 || calls >= 7 {
		t.Fatalf("%d reads (%d failed), want fewer than the 7 slots and none failed", calls, failed)
	}
}
