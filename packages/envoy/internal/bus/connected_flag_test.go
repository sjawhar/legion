package bus

import (
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/testnats"
)

// The connected flag follows the installed connection's last event, whatever the order of that
// event and the install. A connection's events reach the client on nats.go's callback goroutine
// while the install runs on another, so an event can land before the install, or after it has
// set the flag, and the flag must end as the last event said. Rows that let events land after the
// install use the install's test hook (afterInstall), the moment an install that set the flag and
// then asked the connection for its state, apart from the flag, could lose the event: a reconnect
// landing there left the flag reading down on a live connection, and nothing set it again until
// the connection's next event. The connection itself is down throughout, so a flag that asked it
// would read down; only the events say up.
func TestTheConnectedFlagFollowsTheInstalledConnectionsLastEventInAnyOrder(t *testing.T) {
	for _, row := range []struct {
		name string
		// dialedUp is how the dial returned the connection: connected for a dial that waited for
		// the connect (the constructor's foreground dial, a recovery's replacement), not for the
		// background dial nats.go connects later.
		dialedUp bool
		before   []bool // events of the connection before the install
		after    []bool // events of the connection after the install has set the flag
		want     bool
	}{
		{"a disconnect and a reconnect land after a foreground install", true, nil, []bool{false, true}, true},
		{"a background install's first connect lands after it", false, nil, []bool{true}, true},
		{"a background install's first connect lands before it", false, []bool{true}, nil, true},
		{"a drop lands before a foreground install", true, []bool{false}, nil, false},
		{"a drop lands after a foreground install", true, nil, []bool{false}, false},
		{"a background install with no event yet", false, nil, nil, false},
		{"a foreground install with no event yet", true, nil, nil, true},
	} {
		t.Run(row.name, func(t *testing.T) {
			nc := downConnection(t)
			c := &Client{name: t.Name()}
			for _, up := range row.before {
				c.noteConnection(nc, up)
			}
			c.afterInstall = func(installed *nats.Conn) {
				for _, up := range row.after {
					c.noteConnection(installed, up)
				}
			}
			if err := c.install(nc, nil, row.dialedUp); err != nil {
				t.Fatalf("install: %v", err)
			}
			if got := c.ConnectedConn() == nc; got != row.want || c.Connected() != row.want {
				t.Fatalf("after events %v, the install (dialed up %v) and events %v, the client reads connected %v, want %v",
					row.before, row.dialedUp, row.after, got, row.want)
			}
		})
	}
}

// A late event of a connection the client has replaced moves nothing: the flag follows the
// connection installed after it.
func TestALateEventOfAReplacedConnectionLeavesTheFlagToItsReplacement(t *testing.T) {
	old, replacement := downConnection(t), downConnection(t)
	c := &Client{name: t.Name()}
	if err := c.install(old, nil, true); err != nil {
		t.Fatalf("install the first connection: %v", err)
	}
	if err := c.install(replacement, nil, true); err != nil {
		t.Fatalf("install the replacement: %v", err)
	}
	c.noteConnection(old, false)
	if c.ConnectedConn() != replacement {
		t.Fatal("a late disconnect of the replaced connection cleared the flag its replacement set")
	}
	c.noteConnection(old, true)
	c.noteConnection(replacement, false)
	if c.Connected() {
		t.Fatal("a late connect of the replaced connection set the flag after its replacement dropped")
	}
}

// downConnection returns a connection to a server that has stopped, which nats.go is trying to
// reconnect and does not report connected. It carries none of the client's callbacks, so only the
// test's own calls report its events.
func downConnection(t *testing.T) *nats.Conn {
	t.Helper()
	server, uri := testnats.Start(t)
	nc, err := nats.Connect(uri, nats.MaxReconnects(-1), nats.ReconnectWait(time.Hour))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(nc.Close)
	server.Stop(t)
	deadline := time.Now().Add(10 * time.Second)
	for nc.IsConnected() {
		if time.Now().After(deadline) {
			t.Fatal("the connection still reads connected 10s after its server stopped")
		}
		time.Sleep(10 * time.Millisecond)
	}
	return nc
}
