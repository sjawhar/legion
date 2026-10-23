package main

import (
	"bufio"
	"encoding/json"
	"net"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/broker/helper"
)

// drainingHelper listens at a fresh socket and answers register DRAINING for its first drains
// calls, then enrolled; it counts the calls.
func drainingHelper(t *testing.T, drains int32) (string, *atomic.Int32) {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "h.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	var calls atomic.Int32
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			_, _ = bufio.NewReader(conn).ReadBytes('\n')
			resp := helper.Response{Code: helper.CodeDraining, Error: "the helper is restarting; retry"}
			if calls.Add(1) > drains {
				resp = helper.Response{OK: true, State: "enrolled", RuntimeID: "rt", EnrollmentID: "e"}
			}
			data, _ := json.Marshal(resp)
			_, _ = conn.Write(append(data, '\n'))
			_ = conn.Close()
		}
	}()
	return sock, &calls
}

// TestRegisterRetriesADrainingHelper: a helper that answers DRAINING is about to be restarted, so
// register retries it and registers with the restarted helper rather than giving up at once.
func TestRegisterRetriesADrainingHelper(t *testing.T) {
	sock, calls := drainingHelper(t, 2)
	resp, err := registerWithPatience(sock, 5, 0)
	if err != nil || !resp.OK || resp.State != "enrolled" || calls.Load() != 3 {
		t.Fatalf("register against a helper draining twice = %+v, %v after %d calls; want enrolled on the third", resp, err, calls.Load())
	}
}

// TestRegisterStopsRetryingDrainingAtItsWait: a helper still draining when the --wait budget runs
// out is reported, as before, with its DRAINING code.
func TestRegisterStopsRetryingDrainingAtItsWait(t *testing.T) {
	sock, _ := drainingHelper(t, 1<<30)
	start := time.Now()
	_, err := registerWithPatience(sock, 1, 0)
	if err == nil || time.Since(start) < time.Second || time.Since(start) > 3*time.Second {
		t.Fatalf("register against a helper that stays draining = %v after %s; want DRAINING after about 1 s", err, time.Since(start))
	}
}
