package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestHandoffWriteReadAndMessagesPersistInWorkspace(t *testing.T) {
	workspace := t.TempDir()
	var out, errb bytes.Buffer
	if code := run(context.Background(), []string{"legion", "handoff", "write", "--workspace", workspace, "--phase", "implement", "--data", `{"filesChanged":["x.go"]}`}, &out, &errb); code != 0 {
		t.Fatalf("handoff write = %d: %s", code, errb.String())
	}
	if _, err := os.Stat(filepath.Join(workspace, ".legion", "implement.json")); err != nil {
		t.Fatalf("handoff file: %v", err)
	}
	out.Reset()
	if code := run(context.Background(), []string{"legion", "handoff", "read", "--workspace", workspace, "--phase", "implement"}, &out, &errb); code != 0 || !strings.Contains(out.String(), `"phase": "implement"`) {
		t.Fatalf("handoff read = %d: stdout %s stderr %s", code, out.String(), errb.String())
	}
	out.Reset()
	if code := run(context.Background(), []string{"legion", "handoff", "message", "--workspace", workspace, "--from", "architect", "--to", "implement", "--body", "start"}, &out, &errb); code != 0 {
		t.Fatalf("handoff message = %d: %s", code, errb.String())
	}
	out.Reset()
	if code := run(context.Background(), []string{"legion", "handoff", "messages", "--workspace", workspace}, &out, &errb); code != 0 || !strings.Contains(out.String(), `"body": "start"`) {
		t.Fatalf("handoff messages = %d: stdout %s stderr %s", code, out.String(), errb.String())
	}
}

func TestHandoffCompleteRefusesUncommittedOrMissingPhaseFileBeforeHTTP(t *testing.T) {
	workspace := t.TempDir()
	t.Setenv("LEGION_ROLE", "tester")
	var out, errb bytes.Buffer
	code := run(context.Background(), []string{"legion", "handoff", "complete", "--workspace", workspace, "--summary", "tests passed", "--verdict", "pass"}, &out, &errb)
	if code != 1 || !strings.Contains(errb.String(), filepath.Join(".legion", "tester.json")) {
		t.Fatalf("handoff complete = %d, stderr %q; want a missing committed tester handoff refusal", code, errb.String())
	}
}
