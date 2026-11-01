package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
// has passed (none here) is reported with its DRAINING code, exit 1, after that one call.
func TestEnrollHelperStopsRetryingDrainingAtItsWait(t *testing.T) {
	sock, calls := drainingHelper(t, 1<<30)
	t.Setenv("AGENT_SECRETS_HELPER_SOCK", sock)
	t.Setenv("AGENT_SECRETS_KEY_DIR", filepath.Join(t.TempDir(), "keys"))
	old := enrollDrainWait
	enrollDrainWait = 0
	t.Cleanup(func() { enrollDrainWait = old })
	var stdout, stderr bytes.Buffer
	code := cmdEnrollHelper("box:1", "tp", "", &stdout, &stderr)
	if code != 1 || !strings.Contains(stderr.String(), helper.CodeDraining) || calls.Load() != 1 {
		t.Fatalf("enroll against a helper that stays draining = exit %d after %d calls (stderr %q); want 1 naming DRAINING after one call", code, calls.Load(), stderr.String())
	}
}
