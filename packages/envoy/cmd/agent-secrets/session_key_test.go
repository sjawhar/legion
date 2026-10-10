package main

import (
	"bufio"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/broker/helper"
)

// fakeHelperByOp answers each connection with the response its op names, or, for an op it has no
// answer for, the line every helper answers an op it does not know, and returns every request it
// received.
func fakeHelperByOp(t *testing.T, answers map[string]helper.Response) (string, <-chan helper.Request) {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "h.sock")
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: sock, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	reqs := make(chan helper.Request, 8)
	go func() {
		for {
			conn, err := ln.AcceptUnix()
			if err != nil {
				return
			}
			line, _ := bufio.NewReader(conn).ReadBytes('\n')
			var req helper.Request
			_ = json.Unmarshal(line, &req)
			reqs <- req
			resp, ok := answers[req.Op]
			if !ok {
				resp = helper.Response{Code: helper.CodeBadRequest, Error: "unknown op " + req.Op}
			}
			data, _ := json.Marshal(resp)
			_, _ = conn.Write(append(data, '\n'))
			conn.Close()
		}
	}()
	return sock, reqs
}

func drainOps(reqs <-chan helper.Request) []string {
	var ops []string
	for {
		select {
		case r := <-reqs:
			ops = append(ops, r.Op)
		default:
			return ops
		}
	}
}

// TestEnrollHelperAsksForASessionKey: enroll --helper takes no --kind and asks the helper's
// kind-free enroll-session-key; unenroll --helper asks unenroll-session-key.
func TestEnrollHelperAsksForASessionKey(t *testing.T) {
	binary := buildAgentSecrets(t)
	lease := time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano)
	sock, reqs := fakeHelperByOp(t, map[string]helper.Response{
		"enroll-session-key":   {OK: true, EnrollmentID: "enr-key-1", LeaseExpires: lease},
		"unenroll-session-key": {OK: true},
	})
	keyDir := t.TempDir()
	env := []string{"AGENT_SECRETS_HELPER_SOCK=" + sock}
	stdout, stderr, exit := runAgentSecrets(t, binary, "http://unused", keyDir, env,
		"enroll", "--helper", "--runtime-id", "container-1", "--thumbprint", "tp-1")
	if exit != 0 || stdout != "enr-key-1\n" {
		t.Fatalf("enroll --helper = %d %q (stderr %q), want exit 0 and the bare enrollment id", exit, stdout, stderr)
	}
	if data, err := os.ReadFile(filepath.Join(keyDir, "enrollment")); err != nil || strings.TrimSpace(string(data)) != "enr-key-1" {
		t.Fatalf("enrollment file: err=%v content=%q", err, data)
	}
	if _, stderr, exit := runAgentSecrets(t, binary, "http://unused", keyDir, env,
		"unenroll", "--helper", "--enrollment", "enr-key-1"); exit != 0 {
		t.Fatalf("unenroll --helper = %d (stderr %q)", exit, stderr)
	}
	if ops := drainOps(reqs); len(ops) != 2 || ops[0] != "enroll-session-key" || ops[1] != "unenroll-session-key" {
		t.Fatalf("helper ops = %v, want enroll-session-key then unenroll-session-key", ops)
	}
	if _, stderr, exit := runAgentSecrets(t, binary, "http://unused", keyDir, env,
		"enroll", "--helper", "--kind", "box", "--runtime-id", "container-1", "--thumbprint", "tp-1"); exit != exitUsageError || !strings.Contains(stderr, "kind") {
		t.Fatalf("enroll --helper --kind box = %d (stderr %q), want a usage error naming the flag", exit, stderr)
	}
}

// TestEnrollHelperFallsBackToAHelperBeforeTheSessionKey: a helper from before enroll-session-key
// (every release before it, running until it restarts) answers it as an unknown op, and the CLI
// asks it enroll-box and unenroll-box instead, saying so on stderr, so a session a launcher
// enrolls keeps the broker while the machine's helper is older than its CLI.
func TestEnrollHelperFallsBackToAHelperBeforeTheSessionKey(t *testing.T) {
	binary := buildAgentSecrets(t)
	lease := time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano)
	sock, reqs := fakeHelperByOp(t, map[string]helper.Response{
		"enroll-box":   {OK: true, EnrollmentID: "enr-box-1", LeaseExpires: lease},
		"unenroll-box": {OK: true},
	})
	keyDir := t.TempDir()
	env := []string{"AGENT_SECRETS_HELPER_SOCK=" + sock}
	stdout, stderr, exit := runAgentSecrets(t, binary, "http://unused", keyDir, env,
		"enroll", "--helper", "--runtime-id", "container-1", "--thumbprint", "tp-1")
	if exit != 0 || stdout != "enr-box-1\n" || !strings.Contains(stderr, "older") {
		t.Fatalf("enroll --helper against an older helper = %d %q (stderr %q), want the enrollment and a note naming the older helper", exit, stdout, stderr)
	}
	if _, stderr, exit := runAgentSecrets(t, binary, "http://unused", keyDir, env,
		"unenroll", "--helper", "--enrollment", "enr-box-1"); exit != 0 {
		t.Fatalf("unenroll --helper against an older helper = %d (stderr %q)", exit, stderr)
	}
	want := []string{"enroll-session-key", "enroll-box", "unenroll-session-key", "unenroll-box"}
	if ops := drainOps(reqs); strings.Join(ops, " ") != strings.Join(want, " ") {
		t.Fatalf("helper ops = %v, want %v", ops, want)
	}
}
