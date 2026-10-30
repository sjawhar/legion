package main

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"strconv"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// What the document rooms hold - how many rooms are resident, how many browsers are connected,
// what their appends cost - is invisible from outside the process unless it is exported. The
// loopback profiler's /debug/vars serves it as the expvar variable dispatch_rooms, which an
// operator reads through `kubectl port-forward` while people edit: after two editors join two
// documents, it names two resident rooms and two connections.
func TestDispatchRoomsReportsTheRoomsHeld(t *testing.T) {
	database := storetest.Open(t)
	pprofPort := freeLoopbackPort(t)
	process := startDispatchProcessWith(t, database.Pool.Config().ConnString(), dispatchOptions{
		Env: map[string]string{"DISPATCH_PPROF_PORT": strconv.Itoa(pprofPort)},
	})
	process.waitHealthy(t)
	process.call(t, http.MethodPost, "/api/v1/projects", `{"key":"ROOM","name":"Rooms"}`, http.StatusCreated)
	for _, spec := range []string{"first", "second"} {
		var issue struct {
			PrimaryArtifactID string `json:"primary_artifact_id"`
		}
		created := process.call(t, http.MethodPost, "/api/v1/issues", `{"project":"ROOM","title":"Room `+spec+`","spec":"`+spec+`"}`, http.StatusCreated)
		if err := json.Unmarshal(created, &issue); err != nil || issue.PrimaryArtifactID == "" {
			t.Fatalf("create issue: %v: %s", err, created)
		}
		process.openEditor(t, issue.PrimaryArtifactID, spec+"\n")
	}

	response, err := http.Get("http://127.0.0.1:" + strconv.Itoa(pprofPort) + "/debug/vars")
	if err != nil {
		t.Fatalf("GET /debug/vars on the profiler: %v", err)
	}
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	var vars map[string]json.RawMessage
	if response.StatusCode != http.StatusOK || json.Unmarshal(body, &vars) != nil {
		t.Fatalf("GET /debug/vars on the profiler = %d %.200q, want the process's expvar variables", response.StatusCode, body)
	}
	exported, ok := vars["dispatch_rooms"]
	if !ok {
		t.Fatalf("/debug/vars serves no dispatch_rooms variable")
	}
	var rooms struct {
		ResidentRooms *int `json:"resident_rooms"`
		Connections   *int `json:"connections"`
	}
	if err := json.Unmarshal(exported, &rooms); err != nil {
		t.Fatalf("decode dispatch_rooms %s: %v", exported, err)
	}
	if rooms.ResidentRooms == nil || *rooms.ResidentRooms != 2 || rooms.Connections == nil || *rooms.Connections != 2 {
		t.Fatalf("dispatch_rooms = %s, want resident_rooms 2 and connections 2 with two editors in two documents", exported)
	}
}

// freeLoopbackPort is a loopback port the kernel chose and nothing held when it returned. The
// profiler takes its port by number (DISPATCH_PPROF_PORT, never 0), not as an inherited socket, so
// the port is free only until another program binds it, which on a test machine none does.
func freeLoopbackPort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()
	return port
}
