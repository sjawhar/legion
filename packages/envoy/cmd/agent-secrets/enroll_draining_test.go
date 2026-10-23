package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/broker/helper"
)

// TestEnrollHelperRetriesADrainingHelper: `enroll --helper` against a helper that answers
// DRAINING (an install is about to restart it) retries, as register does, and enrolls with the
// restarted helper rather than exiting 1.
func TestEnrollHelperRetriesADrainingHelper(t *testing.T) {
	sock, calls := drainingHelper(t, 2)
	dir := filepath.Join(t.TempDir(), "keys")
	t.Setenv("AGENT_SECRETS_HELPER_SOCK", sock)
	t.Setenv("AGENT_SECRETS_KEY_DIR", dir)
	var stdout, stderr bytes.Buffer
	if code := cmdEnrollHelper("box:1", "tp", "", &stdout, &stderr); code != 0 || calls.Load() != 3 {
		t.Fatalf("enroll against a helper draining twice = exit %d after %d calls (stderr %q); want 0 on the third", code, calls.Load(), stderr.String())
	}
	if got, err := os.ReadFile(filepath.Join(dir, enrollmentFile)); err != nil || strings.TrimSpace(string(got)) != "e" {
		t.Fatalf("enrollment file %q, %v; want the restarted helper's enrollment", got, err)
	}
}

// TestEnrollHelperStopsRetryingDrainingAtItsWait: a helper still draining once the wait budget
// has passed is reported with its DRAINING code, exit 1.
func TestEnrollHelperStopsRetryingDrainingAtItsWait(t *testing.T) {
	sock, _ := drainingHelper(t, 1<<30)
	t.Setenv("AGENT_SECRETS_HELPER_SOCK", sock)
	t.Setenv("AGENT_SECRETS_KEY_DIR", filepath.Join(t.TempDir(), "keys"))
	old := enrollDrainWait
	enrollDrainWait = time.Second
	t.Cleanup(func() { enrollDrainWait = old })
	var stdout, stderr bytes.Buffer
	start := time.Now()
	code := cmdEnrollHelper("box:1", "tp", "", &stdout, &stderr)
	elapsed := time.Since(start)
	if code != 1 || !strings.Contains(stderr.String(), helper.CodeDraining) || elapsed < time.Second || elapsed > 30*time.Second {
		t.Fatalf("enroll against a helper that stays draining = exit %d after %s (stderr %q); want 1 naming DRAINING once its 1 s wait passed", code, elapsed, stderr.String())
	}
}
