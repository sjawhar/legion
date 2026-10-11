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
// kind-free enroll-session-key; unenroll --helper asks unenroll-session-key. The `--kind box`
// dotfiles' agentbox passes until it moves onto this release still enrolls the same way, and any
// other --kind is a usage error naming the flag, asking the helper nothing.
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
	for _, kind := range [][]string{{"--kind", "box"}, {"--kind=box"}} {
		args := append(append([]string{"enroll", "--helper"}, kind...), "--runtime-id", "container-1", "--thumbprint", "tp-1")
		if stdout, stderr, exit := runAgentSecrets(t, binary, "http://unused", keyDir, env, args...); exit != 0 || stdout != "enr-key-1\n" {
			t.Fatalf("enroll %v = %d %q (stderr %q), want agentbox's call to enroll as before", kind, exit, stdout, stderr)
		}
		if ops := drainOps(reqs); len(ops) != 1 || ops[0] != "enroll-session-key" {
			t.Fatalf("enroll %v helper ops = %v, want enroll-session-key", kind, ops)
		}
	}
	for _, kind := range [][]string{{"--kind", "pod"}, {"--kind=host"}, {"--kind", "session"}, {"-kind", "box2"}} {
		args := append(append([]string{"enroll", "--helper"}, kind...), "--runtime-id", "container-1", "--thumbprint", "tp-1")
		if _, stderr, exit := runAgentSecrets(t, binary, "http://unused", keyDir, env, args...); exit != exitUsageError || !strings.Contains(stderr, "--kind") {
			t.Fatalf("enroll %v = %d (stderr %q), want a usage error naming the flag", kind, exit, stderr)
		}
	}
	if ops := drainOps(reqs); len(ops) != 0 {
		t.Fatalf("a refused --kind asked the helper %v, want nothing", ops)
	}
}

// TestEnrollWithoutHelperNamesHelperWhateverKind: enroll without --helper is refused for that
// first, whatever --kind it carries, as every release before the session kind refused it, so a
// caller that pins that refusal (stage 4a's self-enroll negative, which runs `enroll --kind pod`
// in a pod against whatever worker image it is given) reads the same answer from either release.
func TestEnrollWithoutHelperNamesHelperWhateverKind(t *testing.T) {
	binary := buildAgentSecrets(t)
	sock, reqs := fakeHelperByOp(t, map[string]helper.Response{})
	env := []string{"AGENT_SECRETS_HELPER_SOCK=" + sock}
	for _, kind := range [][]string{{"--kind", "pod"}, {"--kind=box"}, {}} {
		args := append(append([]string{"enroll"}, kind...), "--runtime-id", "pod-1", "--thumbprint", "tp-1")
		if _, stderr, exit := runAgentSecrets(t, binary, "http://unused", t.TempDir(), env, args...); exit != exitUsageError || !strings.Contains(stderr, "--helper is required") {
			t.Fatalf("enroll %v without --helper = %d (stderr %q), want exit %d naming --helper", kind, exit, stderr, exitUsageError)
		}
	}
	if ops := drainOps(reqs); len(ops) != 0 {
		t.Fatalf("an enroll without --helper asked the helper %v, want nothing", ops)
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
