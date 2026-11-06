package main

import (
	"bytes"
	"context"
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestCopyMetaRefusesAFileThatIsNotAJSONObject: loadgen run refuses, before any connection, a
// -copy-meta file that is not one JSON object, naming the file.
func TestCopyMetaRefusesAFileThatIsNotAJSONObject(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("LOADTEST_ROOT", dir)
	for _, content := range []string{`[{"namespace":"loadtest"}]`, `null`, `"loadtest"`, `{"namespace":`, ``, `{"a":1} {"b":2}`} {
		path := filepath.Join(dir, "copy.json")
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := readCopyMeta(path); err == nil || !strings.Contains(err.Error(), path) || !strings.Contains(err.Error(), "not a JSON object") {
			t.Errorf("-copy-meta holding %q: error %v, want a refusal naming %s", content, err, path)
		}
		// The same refusal from loadgen run, whose endpoints are all given, so it reaches the flag
		// without stack.json and opens no connection.
		out := filepath.Join(dir, "step")
		err := runLoad([]string{"-allow-foreign", "-url", "http://127.0.0.1:1", "-listener", "http://127.0.0.1:1", "-nats", "nats://127.0.0.1:1",
			"-nats-mon", "http://127.0.0.1:1", "-pprof", "http://127.0.0.1:1", "-pg", "postgres://postgres@127.0.0.1:1/dispatch",
			"-token", "t", "-listener-token", "l", "-events", "0", "-sessions", "1", "-requests", "0", "-out", out, "-copy-meta", path})
		if err == nil || !strings.Contains(err.Error(), path) {
			t.Errorf("loadgen run with -copy-meta holding %q: error %v, want a refusal naming %s", content, err, path)
		}
		if _, statErr := os.Stat(out); statErr == nil {
			t.Errorf("loadgen run made the step directory before it refused -copy-meta")
		}
	}
	if _, err := readCopyMeta(filepath.Join(dir, "absent.json")); err == nil || !strings.Contains(err.Error(), "absent.json") {
		t.Errorf("an absent -copy-meta file: error %v, want one naming it", err)
	}
}

// TestCopyMetaIsRecordedVerbatimUnderCopy: meta.json records the object under copy, and without
// -copy-meta it has no copy key.
func TestCopyMetaIsRecordedVerbatimUnderCopy(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("LOADTEST_ROOT", dir)
	path := filepath.Join(dir, "copy.json")
	if err := os.WriteFile(path, []byte(`{"namespace": "loadtest", "images": {"dispatch": "ghcr.io/x@sha256:ab"}, "nodes": [1, 2]}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	copyMeta, err := readCopyMeta(path)
	if err != nil {
		t.Fatal(err)
	}
	pg := "postgres://postgres@127.0.0.1:1/dispatch?sslmode=disable"
	var logged bytes.Buffer
	for _, c := range []struct {
		copyMeta json.RawMessage
		want     string
	}{{copyMeta, `"copy":{"namespace":"loadtest","images":{"dispatch":"ghcr.io/x@sha256:ab"},"nodes":[1,2]}`}, {nil, ""}} {
		meta := collectMeta(context.Background(), runConfig{boxBusyBefore: 0, copyMeta: c.copyMeta, ep: endpoints{pg: pg, pids: map[string]int{}}}, log.New(&logged, "", 0))
		b, err := json.Marshal(meta)
		if err != nil {
			t.Fatal(err)
		}
		if c.want == "" {
			if strings.Contains(string(b), `"copy"`) {
				t.Errorf("meta.json without -copy-meta has a copy key:\n%s", b)
			}
			continue
		}
		if !strings.Contains(string(b), c.want) {
			t.Errorf("meta.json lacks %s:\n%s", c.want, b)
		}
	}
}
