package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"testing"
	"time"
)

func TestPollCountsEveryFailedReadAndLogsTheFirst(t *testing.T) {
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
	if logged := strings.Count(buf.String(), "probe: relation does not exist\n"); logged != min(failed, pollErrorsLogged) {
		t.Fatalf("%d failures logged of %d, want the first %d:\n%s", logged, failed, pollErrorsLogged, buf.String())
	}
	if total := fmt.Sprintf("probe: %d reads failed in all\n", failed); (failed > pollErrorsLogged) != strings.Contains(buf.String(), total) {
		t.Fatalf("the log should name the total only past %d failures:\n%s", pollErrorsLogged, buf.String())
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
