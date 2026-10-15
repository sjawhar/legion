package bus_test

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	tcnats "github.com/testcontainers/testcontainers-go/modules/nats"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/testnats"
)

// TestWithNameNamesEveryConnectionTheClientDials: the server lists a client's connection under the
// name WithName gives it, and the connection the client dials to replace a closed one under the
// same name, while a client given no name stays "envoy". An operator reading the server's
// connections can then tell Dispatch's intakes from its publisher and from every listener. The
// client's lifecycle log lines carry the same name, so a line such as `envoy nats connection
// closed` says which of a process's clients it came from.
func TestWithNameNamesEveryConnectionTheClientDials(t *testing.T) {
	logs := captureBusLogs(t)
	ctx := context.Background()
	container, err := tcnats.Run(ctx, testnats.Image, testcontainers.WithCmd("-js", "-m", "8222"))
	testcontainers.CleanupContainer(t, container)
	if err != nil {
		t.Fatalf("start NATS: %v", err)
	}
	uri, err := container.ConnectionString(ctx)
	if err != nil {
		t.Fatalf("NATS connection string: %v", err)
	}
	connections := serverConnections(t, ctx, container)

	named, err := bus.Connect([]string{uri}, bus.WithName("dispatch-intake"))
	if err != nil {
		t.Fatalf("connect a named client: %v", err)
	}
	t.Cleanup(named.Close)
	unnamed, err := bus.Connect([]string{uri})
	if err != nil {
		t.Fatalf("connect an unnamed client: %v", err)
	}
	t.Cleanup(unnamed.Close)

	listed := connections()
	first := listed["dispatch-intake"]
	if len(first) != 1 || len(listed["envoy"]) != 1 {
		t.Fatalf("the server lists %v, want one connection named dispatch-intake and one named envoy", listed)
	}

	// Closed underneath it, the client dials a replacement (recover).
	named.Conn.Close()
	deadline := time.Now().Add(30 * time.Second)
	for {
		listed = connections()
		if replaced := listed["dispatch-intake"]; len(replaced) == 1 && replaced[0] != first[0] {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("after the client replaced connection %d the server lists %v, want a new connection named dispatch-intake", first[0], listed)
		}
		time.Sleep(100 * time.Millisecond)
	}

	// One line from a connection callback, one from the recovery.
	for _, message := range []string{"envoy nats connection closed", "envoy nats recovery successful"} {
		for !loggedWithName(logs, message, "dispatch-intake") {
			if time.Now().After(deadline) {
				t.Fatalf("no %q record with name dispatch-intake among:\n%s", message, logs.String())
			}
			time.Sleep(100 * time.Millisecond)
		}
	}
}

// loggedWithName reports whether logs holds a record with message msg whose name is name.
func loggedWithName(logs *busLogs, msg, name string) bool {
	for _, line := range logs.lines() {
		var record struct{ Msg, Name string }
		if json.Unmarshal([]byte(line), &record) == nil && record.Msg == msg && record.Name == name {
			return true
		}
	}
	return false
}

// serverConnections reads the open connections of the server container runs from its monitoring
// endpoint, which it serves on 8222 when started with -m 8222, as each name's connection ids.
func serverConnections(t *testing.T, ctx context.Context, container *tcnats.NATSContainer) func() map[string][]uint64 {
	t.Helper()
	host, err := container.Host(ctx)
	if err != nil {
		t.Fatalf("NATS host: %v", err)
	}
	port, err := container.MappedPort(ctx, "8222/tcp")
	if err != nil {
		t.Fatalf("NATS monitoring port: %v", err)
	}
	endpoint := "http://" + net.JoinHostPort(host, port.Port()) + "/connz"
	return func() map[string][]uint64 {
		t.Helper()
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
		if err != nil {
			t.Fatalf("build the connections request: %v", err)
		}
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatalf("read %s: %v", endpoint, err)
		}
		defer response.Body.Close()
		var connz struct {
			Connections []struct {
				CID  uint64 `json:"cid"`
				Name string `json:"name"`
			} `json:"connections"`
		}
		if err := json.NewDecoder(response.Body).Decode(&connz); err != nil {
			t.Fatalf("decode %s: %v", endpoint, err)
		}
		byName := map[string][]uint64{}
		for _, connection := range connz.Connections {
			byName[connection.Name] = append(byName[connection.Name], connection.CID)
		}
		for name := range byName {
			slices.Sort(byName[name])
		}
		return byName
	}
}
