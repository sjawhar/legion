package delivery

// Two envoy-dispatch processes over one Postgres and one NATS, as a rolling release runs them: each
// intake here is a process of its own, with its own NATS connection, its own database pool and its
// own GitHub, and the tests hold which of them handles each message when the one delivering stops,
// dies, or is cut off and comes back.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	natsgo "github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/dispatch/duty"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/testnats"
)

// errCutOff is every statement of a process the test has cut off from Postgres.
var errCutOff = errors.New("the process is cut off from the database")

// cuttableLeases is one process's lease pool, whose every statement fails once the test cuts it, as
// a process the network cut off from Postgres or the kernel killed sends nothing: the hold it
// renews on this pool then lapses.
type cuttableLeases struct {
	pool *pgxpool.Pool
	cut  atomic.Bool
}

func (l *cuttableLeases) Acquire(ctx context.Context) (*pgxpool.Conn, error) {
	if l.cut.Load() {
		return nil, errCutOff
	}
	return l.pool.Acquire(ctx)
}

func (l *cuttableLeases) Exec(ctx context.Context, sql string, arguments ...any) (pgconn.CommandTag, error) {
	if l.cut.Load() {
		return pgconn.CommandTag{}, errCutOff
	}
	return l.pool.Exec(ctx, sql, arguments...)
}

func (l *cuttableLeases) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	if l.cut.Load() {
		return cutOffRow{}
	}
	return l.pool.QueryRow(ctx, sql, args...)
}

// cutOffRow is the row a cut-off process reads.
type cutOffRow struct{}

func (cutOffRow) Scan(...any) error { return errCutOff }

// natsCut is a TCP relay to the test's NATS server that one process's NATS connection goes
// through. Cut closes every connection through it and closes each new one at once, as a network
// partition leaves the process: its client reconnects and fails until Restore. The relay keeps its
// port for the whole test, so the client's reconnects reach it again once restored. latency holds
// back each read from the client by that long before the server is handed it, in order, as a slow
// link to the server does.
type natsCut struct {
	listener net.Listener
	target   string
	latency  time.Duration

	mu    sync.Mutex
	cut   bool
	conns []net.Conn
}

// startNATSCut relays to natsURL's server, holding back what the client sends by latency, until
// the test ends, and returns the relay and the URL a client connects to it by, with natsURL's
// credentials.
func startNATSCut(t *testing.T, natsURL string, latency time.Duration) (*natsCut, string) {
	t.Helper()
	parsed, err := url.Parse(natsURL)
	if err != nil {
		t.Fatalf("parse the NATS URL: %v", err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen for the NATS relay: %v", err)
	}
	relay := &natsCut{listener: listener, target: parsed.Host, latency: latency}
	t.Cleanup(func() {
		_ = listener.Close()
		relay.Cut()
	})
	go relay.serve()
	parsed.Host = listener.Addr().String()
	return relay, parsed.String()
}

func (r *natsCut) serve() {
	for {
		client, err := r.listener.Accept()
		if err != nil {
			return
		}
		r.mu.Lock()
		if r.cut {
			r.mu.Unlock()
			_ = client.Close()
			continue
		}
		server, err := net.Dial("tcp", r.target)
		if err != nil {
			r.mu.Unlock()
			_ = client.Close()
			continue
		}
		r.conns = append(r.conns, client, server)
		r.mu.Unlock()
		go func() { r.toServer(server, client); _ = server.Close() }()
		go func() { _, _ = io.Copy(client, server); _ = client.Close() }()
	}
}

// toServer copies what the client sends to the server, each read held back by latency.
func (r *natsCut) toServer(server, client net.Conn) {
	if r.latency == 0 {
		_, _ = io.Copy(server, client)
		return
	}
	buffer := make([]byte, 32*1024)
	for {
		n, err := client.Read(buffer)
		if n > 0 {
			time.Sleep(r.latency)
			if _, err := server.Write(buffer[:n]); err != nil {
				return
			}
		}
		if err != nil {
			return
		}
	}
}

// Cut closes every connection through the relay and every new one until Restore.
func (r *natsCut) Cut() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cut = true
	for _, conn := range r.conns {
		_ = conn.Close()
	}
	r.conns = nil
}

// Restore relays new connections again.
func (r *natsCut) Restore() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cut = false
}

// intakeProcess is one envoy-dispatch process's delivery intake: its NATS client, its lease pool,
// and the GitHub it fetches each merged pull request from, which counts the fetches by number.
type intakeProcess struct {
	client *bus.Client
	leases *cuttableLeases
	stop   func()
	// release lets every fetch its GitHub holds answer.
	release func()

	mu      sync.Mutex
	fetched map[int]int
}

// fetches is how many times this process fetched pull request number.
func (p *intakeProcess) fetches(number int) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.fetched[number]
}

// startIntakeProcess runs a delivery intake as a process of its own on pool's database (a pool of
// its own, storetest.SecondProcess) and natsURL's server, until the test ends or its stop is
// called. Its GitHub answers every pull request 404 after counting it, which the intake logs and
// acknowledges, and holds the fetch of each number block reports true until the fetch's own
// deadline or the test's end.
func startIntakeProcess(t *testing.T, pool *store.Pool, natsURL string, block func(number int) bool) *intakeProcess {
	t.Helper()
	second := storetest.SecondProcess(t, &store.Store{Pool: pool})
	leasePool, err := second.Pool.Leases()
	if err != nil {
		t.Fatalf("open the process's lease pool: %v", err)
	}
	client, err := bus.Connect([]string{natsURL}, bus.WithName(intakeConnectionName))
	if err != nil {
		t.Fatalf("connect the process's intake: %v", err)
	}
	t.Cleanup(client.Close)
	process := &intakeProcess{client: client, leases: &cuttableLeases{pool: leasePool}, fetched: map[int]int{}}

	held := make(chan struct{})
	fake := newFakeGitHub(t)
	fake.handle("GET /repos/{owner}/{repo}/pulls/{number}", func(w http.ResponseWriter, r *http.Request) {
		number, err := strconv.Atoi(r.PathValue("number"))
		if err != nil {
			t.Errorf("parse pull request number %q: %v", r.PathValue("number"), err)
		}
		process.mu.Lock()
		process.fetched[number]++
		process.mu.Unlock()
		if block != nil && block(number) {
			select {
			case <-held:
			case <-r.Context().Done():
			}
		}
		w.WriteHeader(http.StatusNotFound)
	})
	github := fake.newTestClient()
	// Registered after the fake's server, so it runs first: the server's Close waits for the
	// handler it holds.
	process.release = sync.OnceFunc(func() { close(held) })
	t.Cleanup(process.release)

	intake := newIntake(client, second.Pool, process.leases, github, events.NewBroker())
	runCtx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		intake.Run(runCtx)
	}()
	process.stop = sync.OnceFunc(func() {
		cancel()
		<-done
	})
	t.Cleanup(process.stop)
	return process
}

// publishMergedPullRequest publishes the merge of acme/widgets pull request number by a population
// author, which the intake fetches from GitHub.
func publishMergedPullRequest(t *testing.T, publisher *bus.Client, number int) {
	t.Helper()
	publishPullRequestEnvelope(t, publisher, "acme", "widgets", number, map[string]string{
		"kind": "pr", "action": "closed", "merged": "true", "repo": "acme/widgets",
		"number": strconv.Itoa(number), "author": "octocat",
	})
}

// consumerState is the delivery durable's state, one line, for a failure message.
func consumerState(publisher *bus.Client) string {
	info, err := publisher.JS().ConsumerInfo(bus.Stream, bus.DispatchDeliveryDurable)
	if err != nil {
		return err.Error()
	}
	return fmt.Sprintf("push_bound=%t deliver_subject=%s delivered=%d ack_floor=%d ack_pending=%d pending=%d redelivered=%d",
		info.PushBound, info.Config.DeliverSubject, info.Delivered.Stream, info.AckFloor.Stream, info.NumAckPending, info.NumPending, info.NumRedelivered)
}

// awaitAckPending waits for the delivery durable to have want messages outstanding with its
// subscriber: delivered to it, and not yet acknowledged.
func awaitAckPending(t *testing.T, publisher *bus.Client, want int) {
	t.Helper()
	waitFor(t, 20*time.Second, func() bool {
		info, err := publisher.JS().ConsumerInfo(bus.Stream, bus.DispatchDeliveryDurable)
		return err == nil && info.NumAckPending == want
	}, func() string {
		return fmt.Sprintf("the durable does not have %d messages outstanding: %s", want, consumerState(publisher))
	})
}

// A holder stopped gracefully, as a rolling release stops the old process, hands the messages it
// had been delivered and not yet handled back at once, and lets the standby take over at once: the
// standby handles every one of them within seconds, not after the ack wait.
func TestAGracefulStopHandsTheBufferedMessagesToTheStandby(t *testing.T) {
	shrinkIntakeFlowControl(t)
	// One message in the holder's handler and four behind it in its client's buffer.
	maxAckPending := intakeMaxAckPending
	intakeMaxAckPending = 5
	t.Cleanup(func() { intakeMaxAckPending = maxAckPending })
	pool, ctx := deliveryTestPool(t)
	seedDeliverySettings(t, ctx, pool)
	natsURL := testnats.URL(t)
	publisher := intakeTestClient(t)

	const inFlight = 100
	holder := startIntakeProcess(t, pool, natsURL, func(number int) bool { return number == inFlight })
	awaitBoundDurable(t, publisher)
	standby := startIntakeProcess(t, pool, natsURL, nil)

	publishMergedPullRequest(t, publisher, inFlight)
	waitFor(t, 20*time.Second, func() bool { return holder.fetches(inFlight) == 1 }, func() string {
		return "the holder never started on the first message: " + consumerState(publisher)
	})
	buffered := []int{1, 2, 3, 4}
	for _, number := range buffered {
		publishMergedPullRequest(t, publisher, number)
	}
	awaitAckPending(t, publisher, 1+len(buffered))

	stopped := time.Now()
	go holder.stop()
	// The message in the holder's handler finishes as the stop begins.
	holder.release()

	waitFor(t, 5*time.Second, func() bool {
		for _, number := range buffered {
			if standby.fetches(number) == 0 {
				return false
			}
		}
		return true
	}, func() string {
		return fmt.Sprintf("the standby has not handled the stopped holder's buffered messages %v within 5s: standby %v, holder %v; %s",
			buffered, fetchesOf(standby, buffered), fetchesOf(holder, buffered), consumerState(publisher))
	})
	t.Logf("the standby handled the buffered messages %s after the holder's stop", time.Since(stopped))
	for _, number := range buffered {
		if got := holder.fetches(number); got != 0 {
			t.Errorf("the stopping holder handled buffered message %d (%d fetches); it should have handed it back", number, got)
		}
	}
}

// A holder abandoned without a stop, as a SIGKILL leaves it -- its NATS connection gone and its hold
// no longer renewed -- strands the messages it had been delivered. The standby takes the durable
// over once the hold lapses and is redelivered every one of them at once: within the hold's 15 s
// TTL, a 2 s bind interval and a margin, not after the 85 s ack wait.
func TestAnAbandonedHoldersMessagesReachTheStandbyWithinTheHoldsLapse(t *testing.T) {
	shrinkIntakeFlowControl(t)
	pool, ctx := deliveryTestPool(t)
	seedDeliverySettings(t, ctx, pool)
	natsURL := testnats.URL(t)
	publisher := intakeTestClient(t)

	holder := startIntakeProcess(t, pool, natsURL, func(int) bool { return true })
	awaitBoundDurable(t, publisher)
	standby := startIntakeProcess(t, pool, natsURL, nil)

	stranded := []int{1, 2, 3, 4}
	for _, number := range stranded {
		publishMergedPullRequest(t, publisher, number)
	}
	awaitAckPending(t, publisher, len(stranded))

	abandoned := time.Now()
	holder.leases.cut.Store(true)
	holder.client.Close()

	waitFor(t, 25*time.Second, func() bool {
		for _, number := range stranded {
			if standby.fetches(number) == 0 {
				return false
			}
		}
		return true
	}, func() string {
		return fmt.Sprintf("the standby has not handled the abandoned holder's messages %v within 25s: standby %v; %s",
			stranded, fetchesOf(standby, stranded), consumerState(publisher))
	})
	t.Logf("the standby handled the stranded messages %s after the holder was abandoned", time.Since(abandoned))
}

// A holder cut off from NATS and Postgres loses its hold, and the standby takes the durable over.
// When the holder's connection comes back, its client sends its subscription again on the deliver
// subject it had; the standby's take-over moved the durable to another, so the holder is handed
// nothing, and each later message is handled by exactly one process.
func TestAHolderCutOffAndRestoredHandlesNothingTheStandbyTookOver(t *testing.T) {
	shrinkIntakeFlowControl(t)
	pool, ctx := deliveryTestPool(t)
	seedDeliverySettings(t, ctx, pool)
	natsURL := testnats.URL(t)
	publisher := intakeTestClient(t)

	relay, relayedURL := startNATSCut(t, natsURL, 0)
	holder := startIntakeProcess(t, pool, relayedURL, nil)
	awaitBoundDurable(t, publisher)
	standby := startIntakeProcess(t, pool, natsURL, nil)

	relay.Cut()
	holder.leases.cut.Store(true)
	cut := time.Now()
	waitFor(t, 30*time.Second, func() bool {
		info, err := publisher.JS().ConsumerInfo(bus.Stream, bus.DispatchDeliveryDurable)
		return err == nil && info.PushBound
	}, func() string {
		return "the standby never took the durable over from the cut-off holder: " + consumerState(publisher)
	})
	t.Logf("the standby took the durable over %s after the holder was cut off", time.Since(cut))

	relay.Restore()
	holder.leases.cut.Store(false)
	waitFor(t, 20*time.Second, holder.client.Connected, func() string {
		return "the holder's NATS connection did not come back"
	})
	// Time for the holder's client to send its subscriptions again.
	time.Sleep(2 * time.Second)

	later := make([]int, 20)
	for i := range later {
		later[i] = 200 + i
		publishMergedPullRequest(t, publisher, later[i])
	}
	waitFor(t, 30*time.Second, func() bool {
		for _, number := range later {
			if standby.fetches(number)+holder.fetches(number) == 0 {
				return false
			}
		}
		return true
	}, func() string {
		return fmt.Sprintf("not every later message was handled: standby %v, holder %v; %s",
			fetchesOf(standby, later), fetchesOf(holder, later), consumerState(publisher))
	})
	// Time for a second delivery of any of them to land.
	time.Sleep(2 * time.Second)
	for _, number := range later {
		if got := standby.fetches(number) + holder.fetches(number); got != 1 {
			t.Errorf("message %d was handled %d times (standby %d, holder %d), want exactly once",
				number, got, standby.fetches(number), holder.fetches(number))
		}
	}
}

// A process of the build before holds binds the durable by looking it up and binding it when the
// lookup reports nobody bound to it. A take-over that created the durable before it subscribed to
// the durable's deliver subject would leave a moment in which such a lookup finds the durable
// unbound, and both processes would then be handed every message on one deliver subject. The
// take-over subscribes first, so the durable is push-bound from the moment it exists: a lookup the
// moment the server announces its creation finds it bound, even with the taking process's link to
// the server slow.
func TestATakeOverCreatesTheDurableAlreadyBound(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	seedDeliverySettings(t, ctx, pool)
	natsURL := testnats.URL(t)
	intakeTestClient(t)

	// The previous build's standby, as fast as it can be: it looks the durable up the moment the
	// server announces that the durable was created.
	type lookup struct {
		bound bool
		err   error
	}
	lookups := make(chan lookup, 8)
	observer := testnats.Connect(t, natsURL)
	t.Cleanup(observer.Close)
	observerJS, err := observer.JetStream()
	if err != nil {
		t.Fatalf("open the observer's JetStream context: %v", err)
	}
	created := "$JS.EVENT.ADVISORY.CONSUMER.CREATED." + bus.Stream + "." + bus.DispatchDeliveryDurable
	if _, err := observer.Subscribe(created, func(*natsgo.Msg) {
		info, err := observerJS.ConsumerInfo(bus.Stream, bus.DispatchDeliveryDurable)
		select {
		case lookups <- lookup{bound: err == nil && info.PushBound, err: err}:
		default:
		}
	}); err != nil {
		t.Fatalf("subscribe to the durable's creation: %v", err)
	}
	if err := observer.Flush(); err != nil {
		t.Fatalf("flush the observer's subscription: %v", err)
	}

	_, slowURL := startNATSCut(t, natsURL, 500*time.Millisecond)
	startIntakeProcess(t, pool, slowURL, nil)
	select {
	case first := <-lookups:
		if !first.bound {
			t.Fatalf("a lookup the moment the take-over created the durable found it unbound (error %v): a process that binds by lookup would have bound it too", first.err)
		}
	case <-time.After(60 * time.Second):
		t.Fatal("the take-over never created the durable")
	}
}

// generationKey is the consumer metadata key that holds the generation of the durable's settings,
// written out here rather than taken from the code under test, so a build that renamed it would not
// see the generation another build stamped.
const generationKey = "dispatch.consumer-generation"

// intakeHold is the name of the hold a process delivers from the delivery durable under, written
// out for the same reason.
const intakeHold = "delivery-intake-" + bus.DispatchDeliveryDurable

// intakeConsumerLease is the name of the lease every change to the delivery durable runs under, of
// this build and of the build before holds, written out for the same reason.
const intakeConsumerLease = "delivery-consumer-" + bus.DispatchDeliveryDurable

// A consumer a newer build stamped with a higher generation keeps that build's settings: a standby
// of this build changes nothing about it across two settings polls while the newer build holds it,
// and once that build lets go, the standby's take-over recreates the durable with the newer build's
// settings, not its own.
func TestAStandbyOfAnOlderGenerationLeavesANewerConsumerAlone(t *testing.T) {
	shrinkIntakeFlowControl(t)
	pool, ctx := deliveryTestPool(t)
	seedDeliverySettings(t, ctx, pool)
	natsURL := testnats.URL(t)
	publisher := intakeTestClient(t)

	// The newer build's durable: generation 2, with flow control this build does not set.
	newer := natsgo.ConsumerConfig{
		Durable:        bus.DispatchDeliveryDurable,
		DeliverSubject: natsgo.NewInbox(),
		FilterSubject:  githubIntakeSubject,
		AckPolicy:      natsgo.AckExplicitPolicy,
		AckWait:        7 * time.Minute,
		MaxAckPending:  9,
		Metadata:       map[string]string{generationKey: "2"},
	}
	before, err := publisher.JS().AddConsumer(bus.Stream, &newer)
	if err != nil {
		t.Fatalf("create the newer build's durable: %v", err)
	}
	// The newer build's holder: its hold, recording its deliver subject, and its subscription.
	newerBuild, err := bus.Connect([]string{natsURL})
	if err != nil {
		t.Fatalf("connect the newer build's process: %v", err)
	}
	t.Cleanup(newerBuild.Close)
	newerLeases, err := storetest.SecondProcess(t, &store.Store{Pool: pool}).Pool.Leases()
	if err != nil {
		t.Fatalf("open the newer build's lease pool: %v", err)
	}
	hold, err := duty.TryHold(ctx, newerLeases, intakeHold, uuid.New(),
		duty.HoldOptions{TTL: 15 * time.Second, Renew: 5 * time.Second, Margin: 2 * time.Second, Token: newer.DeliverSubject})
	if err != nil || hold == nil {
		t.Fatalf("take the newer build's hold: hold %v, error %v", hold, err)
	}
	t.Cleanup(func() { _ = hold.Release(context.Background()) })
	held, err := newerBuild.Subscribe(githubIntakeSubject, func(msg *natsgo.Msg) { _ = msg.Ack() },
		natsgo.Bind(bus.Stream, bus.DispatchDeliveryDurable), natsgo.ManualAck())
	if err != nil {
		t.Fatalf("bind the newer build's process: %v", err)
	}
	awaitPushBound(t, publisher, "the newer build's process is not bound")

	standby := startIntakeProcess(t, pool, natsURL, nil)
	time.Sleep(2*settingsPollInterval + durableBindInterval)
	after, err := publisher.JS().ConsumerInfo(bus.Stream, bus.DispatchDeliveryDurable)
	if err != nil {
		t.Fatalf("read the durable: %v", err)
	}
	if after.Config.AckWait != newer.AckWait || after.Config.MaxAckPending != newer.MaxAckPending ||
		after.Config.Metadata[generationKey] != "2" || after.Config.DeliverSubject != newer.DeliverSubject ||
		!after.Created.Equal(before.Created) {
		t.Fatalf("the standby changed the newer build's durable: ack_wait %s max_ack_pending %d generation %q deliver_subject %s created %s, want %s %d %q %s %s",
			after.Config.AckWait, after.Config.MaxAckPending, after.Config.Metadata[generationKey], after.Config.DeliverSubject, after.Created,
			newer.AckWait, newer.MaxAckPending, "2", newer.DeliverSubject, before.Created)
	}

	// The newer build stops: it unsubscribes and releases its hold.
	if err := held.Unsubscribe(); err != nil {
		t.Fatalf("unsubscribe the newer build's process: %v", err)
	}
	if err := hold.Release(ctx); err != nil {
		t.Fatalf("release the newer build's hold: %v", err)
	}
	var taken *natsgo.ConsumerInfo
	waitFor(t, 20*time.Second, func() bool {
		taken, err = publisher.JS().ConsumerInfo(bus.Stream, bus.DispatchDeliveryDurable)
		return err == nil && taken.PushBound && taken.Config.DeliverSubject != newer.DeliverSubject
	}, func() string {
		return "the standby never took the released durable over: " + consumerState(publisher)
	})
	if taken.Config.AckWait != newer.AckWait || taken.Config.MaxAckPending != newer.MaxAckPending || taken.Config.Metadata[generationKey] != "2" {
		t.Fatalf("the take-over recreated the durable with ack_wait %s max_ack_pending %d generation %q, want the newer build's %s %d %q",
			taken.Config.AckWait, taken.Config.MaxAckPending, taken.Config.Metadata[generationKey], newer.AckWait, newer.MaxAckPending, "2")
	}
	publishMergedPullRequest(t, publisher, 5)
	waitFor(t, 20*time.Second, func() bool { return standby.fetches(5) == 1 }, func() string {
		return "the standby that took the durable over did not handle a message: " + consumerState(publisher)
	})
}

// A consumer bound by a process of the build before holds (it binds without taking a hold, so no
// hold recorded its deliver subject) is never taken from it: this build's standby waits, handles
// nothing, and changes nothing, until that process lets go. Then it takes the durable over on a
// deliver subject of its own.
func TestANewBuildStandbyWaitsForABindingNoHoldRecorded(t *testing.T) {
	shrinkIntakeFlowControl(t)
	pool, ctx := deliveryTestPool(t)
	seedDeliverySettings(t, ctx, pool)
	natsURL := testnats.URL(t)
	publisher := intakeTestClient(t)

	// The previous build's durable and its process bound to it.
	previous := natsgo.ConsumerConfig{
		Durable:        bus.DispatchDeliveryDurable,
		DeliverSubject: natsgo.NewInbox(),
		FilterSubject:  githubIntakeSubject,
		AckPolicy:      natsgo.AckExplicitPolicy,
		AckWait:        intakeAckWait(),
		MaxAckPending:  intakeMaxAckPending,
	}
	before, err := publisher.JS().AddConsumer(bus.Stream, &previous)
	if err != nil {
		t.Fatalf("create the previous build's durable: %v", err)
	}
	previousBuild, err := bus.Connect([]string{natsURL})
	if err != nil {
		t.Fatalf("connect the previous build's process: %v", err)
	}
	t.Cleanup(previousBuild.Close)
	var received atomic.Int32
	held, err := previousBuild.Subscribe(githubIntakeSubject, func(msg *natsgo.Msg) {
		received.Add(1)
		_ = msg.Ack()
	}, natsgo.Bind(bus.Stream, bus.DispatchDeliveryDurable), natsgo.ManualAck())
	if err != nil {
		t.Fatalf("bind the previous build's process: %v", err)
	}
	awaitPushBound(t, publisher, "the previous build's process is not bound")

	standby := startIntakeProcess(t, pool, natsURL, nil)
	time.Sleep(3 * durableBindInterval)
	publishMergedPullRequest(t, publisher, 1)
	waitFor(t, 20*time.Second, func() bool { return received.Load() == 1 }, func() string {
		return "the previous build's process stopped receiving while the standby stood by: " + consumerState(publisher)
	})
	after, err := publisher.JS().ConsumerInfo(bus.Stream, bus.DispatchDeliveryDurable)
	if err != nil {
		t.Fatalf("read the durable: %v", err)
	}
	if !after.Created.Equal(before.Created) || after.Config.DeliverSubject != previous.DeliverSubject || !held.IsValid() {
		t.Fatalf("the standby took the previous build's binding: created %s deliver_subject %s (want %s %s), subscription valid %t",
			after.Created, after.Config.DeliverSubject, before.Created, previous.DeliverSubject, held.IsValid())
	}
	if got := standby.fetches(1); got != 0 {
		t.Fatalf("the standby handled a message the previous build's process holds (%d fetches)", got)
	}

	if err := held.Unsubscribe(); err != nil {
		t.Fatalf("unsubscribe the previous build's process: %v", err)
	}
	waitFor(t, 20*time.Second, func() bool {
		info, err := publisher.JS().ConsumerInfo(bus.Stream, bus.DispatchDeliveryDurable)
		return err == nil && info.PushBound && info.Config.DeliverSubject != previous.DeliverSubject
	}, func() string {
		return "the standby never took the durable over on a deliver subject of its own once the previous build let go: " + consumerState(publisher)
	})
	publishMergedPullRequest(t, publisher, 2)
	waitFor(t, 20*time.Second, func() bool { return standby.fetches(2) == 1 }, func() string {
		return "the standby that took the durable over did not handle a message: " + consumerState(publisher)
	})
	if got := received.Load(); got != 1 {
		t.Fatalf("the previous build's process received %d messages, want only the one before it let go", got)
	}
}

// fetchesOf is each of numbers' fetch count at process, for a failure message.
func fetchesOf(process *intakeProcess, numbers []int) map[int]int {
	counts := make(map[int]int, len(numbers))
	for _, number := range numbers {
		counts[number] = process.fetches(number)
	}
	return counts
}

// failCreatesWhile makes the take-over's create of the durable fail while failing reports true,
// as a JetStream API that times out, or a crash between the delete and the create, leaves the
// durable deleted and not created. It counts every create attempt, and restores the real create
// when the test ends, after every intake the test starts has stopped.
func failCreatesWhile(t *testing.T, failing func() bool) *atomic.Int32 {
	t.Helper()
	attempts := &atomic.Int32{}
	create := createDurable
	createDurable = func(js natsgo.JetStreamContext, config *natsgo.ConsumerConfig, opts ...natsgo.JSOpt) (*natsgo.ConsumerInfo, error) {
		attempts.Add(1)
		if failing() {
			return nil, errors.New("the test fails the create: JetStream API timed out")
		}
		return create(js, config, opts...)
	}
	t.Cleanup(func() { createDurable = create })
	return attempts
}

// durablePartWayThrough publishes merged pull requests 1 to acknowledged+unacknowledged and leaves
// the delivery durable as a holder that stopped part way through them leaves it: unbound, with the
// first acknowledged of them acknowledged and the rest not. They are acknowledged through a pull
// fetch, which reaches them in stream order, so the ack floor sits at the last of them.
func durablePartWayThrough(t *testing.T, ctx context.Context, publisher *bus.Client, acknowledged, unacknowledged int) {
	t.Helper()
	for number := 1; number <= acknowledged+unacknowledged; number++ {
		publishMergedPullRequest(t, publisher, number)
	}
	if _, err := publisher.JS().AddConsumer(bus.Stream, &natsgo.ConsumerConfig{
		Durable:       bus.DispatchDeliveryDurable,
		FilterSubject: githubIntakeSubject,
		AckPolicy:     natsgo.AckExplicitPolicy,
	}); err != nil {
		t.Fatalf("create the durable: %v", err)
	}
	js, err := jetstream.New(publisher.Conn)
	if err != nil {
		t.Fatalf("open the jetstream API: %v", err)
	}
	consumer, err := js.Consumer(ctx, bus.Stream, bus.DispatchDeliveryDurable)
	if err != nil {
		t.Fatalf("look up the durable: %v", err)
	}
	batch, err := consumer.Fetch(acknowledged, jetstream.FetchMaxWait(10*time.Second))
	if err != nil {
		t.Fatalf("fetch from the durable: %v", err)
	}
	got := 0
	for msg := range batch.Messages() {
		if err := msg.DoubleAck(ctx); err != nil {
			t.Fatalf("acknowledge a message: %v", err)
		}
		got++
	}
	if err := batch.Error(); err != nil || got != acknowledged {
		t.Fatalf("fetch the first %d messages: got %d, err %v", acknowledged, got, err)
	}
	if info, err := publisher.JS().ConsumerInfo(bus.Stream, bus.DispatchDeliveryDurable); err != nil || info.AckFloor.Consumer != uint64(acknowledged) {
		t.Fatalf("the durable's ack floor = %s, want %d", consumerState(publisher), acknowledged)
	}
}

// requireDeliveredFromTheFloor waits for process to handle each of the unacknowledged messages
// once, then gives an acknowledged message the take-over wrongly replayed time to arrive too, and
// fails the test for any of them.
func requireDeliveredFromTheFloor(t *testing.T, publisher *bus.Client, process *intakeProcess, acknowledged, unacknowledged int) {
	t.Helper()
	var acked, unacked []int
	for number := 1; number <= acknowledged+unacknowledged; number++ {
		if number <= acknowledged {
			acked = append(acked, number)
		} else {
			unacked = append(unacked, number)
		}
	}
	waitFor(t, 30*time.Second, func() bool {
		for _, number := range unacked {
			if process.fetches(number) == 0 {
				return false
			}
		}
		return true
	}, func() string {
		return fmt.Sprintf("the intake did not handle the unacknowledged messages %v: %v; %s", unacked, fetchesOf(process, unacked), consumerState(publisher))
	})
	time.Sleep(2 * time.Second)
	for _, number := range acked {
		if got := process.fetches(number); got != 0 {
			t.Errorf("message %d was acknowledged before the take-over and was handled %d times after it", number, got)
		}
	}
	for _, number := range unacked {
		if got := process.fetches(number); got != 1 {
			t.Errorf("message %d was not acknowledged before the take-over and was handled %d times after it, want 1", number, got)
		}
	}
}

// A take-over deletes the durable and creates it again at its ack floor. When every try of the
// create fails after the delete, the take-over gives the consumer lease up with the durable absent,
// and the next take-over of the same intake finds no durable and creates it at that same floor:
// none of the acknowledged messages come back, and none of the others are skipped.
func TestATakeOverWhoseCreateFailsCreatesTheDurableAtItsFloorOnTheNextAttempt(t *testing.T) {
	shrinkIntakeFlowControl(t)
	pool, ctx := deliveryTestPool(t)
	seedDeliverySettings(t, ctx, pool)
	natsURL := testnats.URL(t)
	publisher := intakeTestClient(t)
	const acknowledged, unacknowledged = 5, 4
	durablePartWayThrough(t, ctx, publisher, acknowledged, unacknowledged)

	// Every try of the first take-over's create starts within takeOverFinishWithin of its first
	// try, so failing each create until then fails every try of that take-over, and the create
	// that succeeds is a later take-over's.
	var first atomic.Int64
	var failed atomic.Int32
	attempts := failCreatesWhile(t, func() bool {
		first.CompareAndSwap(0, time.Now().UnixNano())
		if time.Since(time.Unix(0, first.Load())) < takeOverFinishWithin {
			failed.Add(1)
			return true
		}
		return false
	})
	process := startIntakeProcess(t, pool, natsURL, nil)
	requireDeliveredFromTheFloor(t, publisher, process, acknowledged, unacknowledged)
	if got := failed.Load(); got < 2 {
		t.Fatalf("the take-over tried the create %d times before it gave up, want it tried again", got)
	}
	if got, failedTries := attempts.Load(), failed.Load(); got <= failedTries {
		t.Fatalf("the intake tried the create %d times, want the %d failed ones and one after them", got, failedTries)
	}
}

// A take-over whose create fails after the delete leaves its start to the next holder: a process
// stopped gracefully before any create succeeded hands the hold on, and a fresh intake, as a
// restarted process runs one, creates the durable at the old floor: none of the acknowledged
// messages come back, and none of the others are skipped.
func TestATakeOverWhoseCreateFailsLeavesTheFloorToTheNextHolder(t *testing.T) {
	shrinkIntakeFlowControl(t)
	pool, ctx := deliveryTestPool(t)
	seedDeliverySettings(t, ctx, pool)
	natsURL := testnats.URL(t)
	publisher := intakeTestClient(t)
	const acknowledged, unacknowledged = 5, 4
	durablePartWayThrough(t, ctx, publisher, acknowledged, unacknowledged)

	var failing atomic.Bool
	failing.Store(true)
	attempts := failCreatesWhile(t, failing.Load)
	first := startIntakeProcess(t, pool, natsURL, nil)
	waitFor(t, 20*time.Second, func() bool {
		_, err := publisher.JS().ConsumerInfo(bus.Stream, bus.DispatchDeliveryDurable)
		return attempts.Load() > 0 && errors.Is(err, natsgo.ErrConsumerNotFound)
	}, func() string {
		return fmt.Sprintf("the first intake did not delete the durable and fail its create (%d creates): %s", attempts.Load(), consumerState(publisher))
	})
	first.stop()
	failing.Store(false)

	next := startIntakeProcess(t, pool, natsURL, nil)
	requireDeliveredFromTheFloor(t, publisher, next, acknowledged, unacknowledged)
}

// previousBuildReconciler runs the configuration reconciliation of the build before holds
// (legion-envoy-v19.0.0's reconcileDurableConsumer, cut to its first case) against the delivery
// durable every interval until the test ends, as a process of that build does on its own pool:
// under the durable's consumer lease, it looks the durable up, and when there is none it creates
// the durable with that build's config, which names no start and so delivers from the stream's
// first message. It reads no hold. created counts the durables it created, and present the passes
// that found the durable there.
func previousBuildReconciler(t *testing.T, pool *store.Pool, natsURL string, interval time.Duration) (created, present *atomic.Int32) {
	t.Helper()
	leases, err := storetest.SecondProcess(t, &store.Store{Pool: pool}).Pool.Leases()
	if err != nil {
		t.Fatalf("open the previous build's lease pool: %v", err)
	}
	client, err := bus.Connect([]string{natsURL})
	if err != nil {
		t.Fatalf("connect the previous build's process: %v", err)
	}
	t.Cleanup(client.Close)
	created, present = &atomic.Int32{}, &atomic.Int32{}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			duty.Exclusive(ctx, leases, intakeConsumerLease, time.Time{}, 0, func(context.Context) {
				_, err := client.JS().ConsumerInfo(bus.Stream, bus.DispatchDeliveryDurable)
				switch {
				case errors.Is(err, natsgo.ErrConsumerNotFound):
					previous := natsgo.ConsumerConfig{
						Durable:        bus.DispatchDeliveryDurable,
						DeliverSubject: natsgo.NewInbox(),
						FilterSubject:  githubIntakeSubject,
						AckPolicy:      natsgo.AckExplicitPolicy,
						AckWait:        intakeAckWait(),
						MaxAckPending:  intakeMaxAckPending,
					}
					if _, err := client.JS().AddConsumer(bus.Stream, &previous); err == nil {
						created.Add(1)
					}
				case err == nil:
					present.Add(1)
				}
			})
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	return created, present
}

// A take-over whose create fails once after the delete creates the durable itself, at its ack
// floor, while a process of the build before holds reconciles the same durable under the same
// consumer lease: that process never finds the durable missing, so it never creates its own,
// which would replay every message the durable had acknowledged. None of the acknowledged
// messages come back, none of the others are skipped, and that process finds the durable there.
func TestATakeOverWhoseCreateFailsCreatesTheDurableBeforeAPreviousBuildReconciles(t *testing.T) {
	shrinkIntakeFlowControl(t)
	pool, ctx := deliveryTestPool(t)
	seedDeliverySettings(t, ctx, pool)
	natsURL := testnats.URL(t)
	publisher := intakeTestClient(t)
	const acknowledged, unacknowledged = 5, 4
	durablePartWayThrough(t, ctx, publisher, acknowledged, unacknowledged)
	floor, err := publisher.JS().ConsumerInfo(bus.Stream, bus.DispatchDeliveryDurable)
	if err != nil {
		t.Fatalf("read the durable: %v", err)
	}
	start := floor.AckFloor.Stream + 1

	created, present := previousBuildReconciler(t, pool, natsURL, 100*time.Millisecond)
	var failed atomic.Bool
	attempts := failCreatesWhile(t, func() bool { return !failed.Swap(true) })
	process := startIntakeProcess(t, pool, natsURL, nil)
	requireDeliveredFromTheFloor(t, publisher, process, acknowledged, unacknowledged)
	if got := attempts.Load(); got < 2 {
		t.Fatalf("the intake tried the create %d times, want the failed one and one after it", got)
	}
	if got := created.Load(); got != 0 {
		t.Fatalf("the previous build's process found the durable missing and created it %d times", got)
	}
	taken, err := publisher.JS().ConsumerInfo(bus.Stream, bus.DispatchDeliveryDurable)
	if err != nil {
		t.Fatalf("read the durable: %v", err)
	}
	if taken.Config.DeliverPolicy != natsgo.DeliverByStartSequencePolicy || taken.Config.OptStartSeq != start ||
		taken.Config.Metadata[generationKey] != strconv.Itoa(consumerConfigGeneration) {
		t.Fatalf("the durable has deliver policy %d, start %d, generation %q; want this build's durable by start sequence (%d) at %d, generation %d",
			taken.Config.DeliverPolicy, taken.Config.OptStartSeq, taken.Config.Metadata[generationKey],
			natsgo.DeliverByStartSequencePolicy, start, consumerConfigGeneration)
	}
	before := present.Load()
	waitFor(t, 10*time.Second, func() bool { return present.Load() > before }, func() string {
		return "the previous build's process did not find the durable present after the take-over: " + consumerState(publisher)
	})
}
