package pgmigrate

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sjawhar/envoy/internal/testpg"
)

// What the watch reports after a sequence of readings. One empty reading of a wait that named its
// holders is the cancellation race between pg_locks and pg_blocking_pids and is held back; two in a
// row mean nobody holding the lock can be named - the holder left, or only a prepared transaction,
// which pg_blocking_pids reports as pid 0, still blocks - and a departed holder must not be named.
// Each row is the sequence that tells one term of the rule from its absence.
func TestObserveNamesOnlyAHolderTheLatestReadingsSupport(t *testing.T) {
	named := func(pid uint32) *LockWait {
		return &LockWait{LockType: "relation", Mode: "AccessExclusiveLock", Object: "messages", Holders: []Session{{PID: pid}}}
	}
	empty := func() *LockWait {
		return &LockWait{LockType: "relation", Mode: "AccessExclusiveLock", Object: "messages"}
	}
	otherLock := &LockWait{LockType: "relation", Mode: "AccessExclusiveLock", Object: "asks"}
	otherMode := &LockWait{LockType: "relation", Mode: "ShareLock", Object: "messages"}
	for _, tc := range []struct {
		name     string
		readings []*LockWait
		want     *LockWait
	}{
		{"one empty reading as the cancellation lands keeps the holder", []*LockWait{named(97), empty()}, named(97)},
		{"two empty readings in a row name nobody", []*LockWait{named(97), empty(), empty()}, empty()},
		{"a holder named after a held-back reading replaces it, and the next empty reading is held back again", []*LockWait{named(97), empty(), named(98), empty()}, named(98)},
		{"a holder that takes over replaces the one before it", []*LockWait{named(97), named(98)}, named(98)},
		{"an empty reading of another lock replaces at once", []*LockWait{named(97), otherLock}, otherLock},
		{"an empty reading of another mode on the same table replaces at once", []*LockWait{named(97), otherMode}, otherMode},
		{"a reading of not waiting ends the run, so the next empty one is held back", []*LockWait{named(97), empty(), nil, empty()}, named(97)},
		{"a first reading that names nobody is recorded, so the refusal names the lock", []*LockWait{empty()}, empty()},
		{"a reading of not waiting after a named one keeps it, and the next empty one is held back", []*LockWait{named(97), nil, empty()}, named(97)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var w watch
			for _, reading := range tc.readings {
				w.observe(reading)
			}
			if !reflect.DeepEqual(w.wait, tc.want) {
				t.Errorf("after %d readings the watch reports %+v, want %+v", len(tc.readings), w.wait, tc.want)
			}
		})
	}
}

// rollbackMarker is the rollback that ends a diagnosis's transaction as it crosses the wire: pgx
// sends a statement with no arguments through the simple protocol, whose Query message holds the
// text null-terminated, so the terminator tells that rollback from the "rollback to savepoint …"
// of the nested transaction each holder's diagnosis reads in (holderDiagnosis).
var rollbackMarker = []byte("rollback\x00")

// forwardUntilRollback listens on a loopback port of its own and passes bytes between one client
// and the Postgres server dsn names, until it reads the client's rollback: from then on it passes
// nothing in either direction and holds both connections open, so that rollback neither completes
// nor fails. That is the transport failure a diagnosis's cleanup must not wait on without a
// bound. It returns the address to dial, and every goroutine it starts has ended when the test
// returns.
func forwardUntilRollback(t *testing.T, dsn string) *net.TCPAddr {
	t.Helper()
	config, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse the test database's URL: %v", err)
	}
	network, address := pgconn.NetworkAddress(config.Host, config.Port)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		conns   []net.Conn
		closing bool
		stopped bool
	)
	// track holds every connection the forwarder opens: a pump blocked reading one ends only
	// when that connection closes, which the cleanup does for all of them.
	track := func(conn net.Conn) bool {
		mu.Lock()
		defer mu.Unlock()
		if closing {
			conn.Close()
			return false
		}
		conns = append(conns, conn)
		return true
	}
	t.Cleanup(func() {
		listener.Close()
		mu.Lock()
		closing = true
		for _, conn := range conns {
			conn.Close()
		}
		mu.Unlock()
		wg.Wait()
	})
	// pump passes what it reads from src on to dst until either end stops or the rollback has
	// been read. watch, set on the client's side alone, stops the forwarder on the chunk holding
	// the rollback and passes that chunk on to nobody; a rollback split across two reads is
	// caught by scanning the tail of the one before.
	pump := func(dst, src net.Conn, watch bool) {
		defer wg.Done()
		buf := make([]byte, 16*1024)
		var tail []byte
		for {
			n, err := src.Read(buf)
			if n > 0 {
				scan := append(append([]byte(nil), tail...), buf[:n]...)
				mu.Lock()
				if watch && !stopped && bytes.Contains(scan, rollbackMarker) {
					stopped = true
				}
				halted := stopped
				mu.Unlock()
				if halted {
					return
				}
				if keep := len(rollbackMarker) - 1; len(scan) > keep {
					scan = scan[len(scan)-keep:]
				}
				tail = scan
				if _, err := dst.Write(buf[:n]); err != nil {
					return
				}
			}
			if err != nil {
				return
			}
		}
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			client, err := listener.Accept()
			if err != nil {
				return
			}
			if !track(client) {
				return
			}
			server, err := net.Dial(network, address)
			if err != nil {
				t.Errorf("dial the test database: %v", err)
				client.Close()
				return
			}
			if !track(server) {
				return
			}
			wg.Add(2)
			go pump(server, client, true)
			go pump(client, server, false)
		}
	}()
	return listener.Addr().(*net.TCPAddr)
}

// A diagnosis whose cleanup cannot finish returns anyway, with what it read. diagnoseHolders rolls
// its read-only transaction back and closes its connection on a context of their own, bounded by
// diagnosisCleanupTimeout, so a transport that takes the rollback and answers it never - neither
// completing nor failing - holds the runner up a second at most, not for as long as that
// transport lives, and the holder the diagnosis read before the stop is still named.
func TestDiagnoseHoldersReturnsWhenTheCleanupCannotFinish(t *testing.T) {
	ctx := context.Background()
	dsn := testpg.URL(t)
	// The session the diagnosis reads. It stays connected, so pg_stat_activity has a row for it.
	holder, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect the holder: %v", err)
	}
	t.Cleanup(func() { holder.Close(context.Background()) })
	var pid uint32
	if err := holder.QueryRow(ctx, "select pg_backend_pid()").Scan(&pid); err != nil {
		t.Fatalf("read the holder's pid: %v", err)
	}

	addr := forwardUntilRollback(t, dsn)
	config, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse the test database's URL: %v", err)
	}
	config.Host, config.Port = addr.IP.String(), uint16(addr.Port)
	// The forwarder has to read the rollback in the clear, and a fallback would reach the server
	// behind its back.
	config.TLSConfig, config.Fallbacks = nil, nil
	dial := func(ctx context.Context) (*pgx.Conn, error) { return pgx.ConnectConfig(ctx, config) }

	bound := LockTimeout + 2*time.Second
	start := time.Now()
	done := make(chan []string, 1)
	go func() { done <- diagnoseHolders(ctx, dial, []Session{{PID: pid}}) }()
	select {
	case diagnoses := <-done:
		want := fmt.Sprintf("diagnosis of pid %d, read when refused: ", pid)
		if len(diagnoses) != 1 || !strings.HasPrefix(diagnoses[0], want) {
			t.Fatalf("diagnoseHolders returned %q after %v, want one diagnosis starting %q", diagnoses, time.Since(start), want)
		}
	case <-time.After(bound):
		t.Fatalf("diagnoseHolders has not returned in %v: its cleanup is waiting on a transport that passes nothing", bound)
	}
}
