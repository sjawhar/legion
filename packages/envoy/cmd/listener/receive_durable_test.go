package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os/exec"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/cmdtest"
	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/internal/session"
	"github.com/sjawhar/envoy/internal/store"
	"github.com/sjawhar/envoy/internal/testnats"
)

// A pod that cannot prepare its durable and has no other pod to give the lease to tries again: a
// sole lease holder gives the lease up at the alarm bound and takes it back once no other pod has,
// and a listener without a lease, the machine's one deliverer, keeps retrying and answers /healthz
// 503 unhealthy past the bound, so its runtime counts it unready, as the bounded subscribe that
// exited did.
func TestAPodThatCannotPrepareItsDurableAloneTriesAgain(t *testing.T) {
	granted, denied := consumerInfoDeniedNATS(t)
	timing := testReceiveTiming()
	timing.alarmAfter, timing.alarmEvery = 5*time.Second, time.Millisecond
	t.Run("sole holder", func(t *testing.T) {
		t.Parallel()
		handle, _ := recordingHandler()
		rig := newReceiverRigVia(t, denied, granted, "box-stuck-alone", "pod-a", handle, timing, testLeaseTiming())
		rig.lease.Start(context.Background())
		rig.receiver.Start()
		waitFor(t, 20*time.Second, "pod-a to give its lease up and take it again", func() bool {
			status := rig.lease.Status()
			return status.Term >= 2 && status.Holding
		})
		if alarms := durableAlarmRecords(rig.logs); len(alarms) != 1 || !strings.Contains(fmt.Sprint(alarms[0]["reason"]), "read the durable consumer") {
			t.Fatalf("pod-a's alarms: %v, want one at ERROR whose reason names the failed read", alarms)
		}
		if status := rig.lease.Status(); status.How != leaseTakenReleased {
			t.Fatalf("pod-a's second term: %+v, want taken from the record it released", status)
		}
	})
	t.Run("without a lease", func(t *testing.T) {
		t.Parallel()
		client, err := bus.ConnectOwningStream([]string{denied}, bus.WithReplicas(1), bus.WithName("receive-test without a lease"))
		if err != nil {
			t.Fatalf("connect: %v", err)
		}
		logs := &cmdtest.LockedBuffer{}
		handle, _ := recordingHandler()
		receiver := newDurableReceiver(durableReceiverOptions{
			client:   client,
			consumer: bus.ListenerDurable("box-stuck-unleased"),
			handle:   handle,
			refused:  func(err error) { t.Errorf("refused the durable: %v", err) },
			logger:   logging.NewWithWriter("box-stuck-unleased", logs),
			timing:   timing,
		})
		receiver.Start()
		t.Cleanup(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			receiver.Stop(ctx)
			client.Close()
			if t.Failed() {
				t.Logf("log:\n%s", logs.String())
			}
		})
		get := healthzFor(t, client, receiver)
		var code int
		var body map[string]any
		// A listener without a lease that does not receive yet is degraded from the start; this
		// waits for the answer past the alarm bound, which is the unready one.
		waitFor(t, 15*time.Second, "/healthz to report the failing durable", func() bool {
			code, body = get()
			return code != http.StatusOK
		})
		if code != http.StatusServiceUnavailable || body["status"] != "unhealthy" || body["receive"] != receivePreparing.String() ||
			!strings.Contains(fmt.Sprint(body["error"]), "read the durable consumer") {
			t.Fatalf("/healthz once the durable read has failed past the bound = %d %v, want 503 unhealthy, receive %q and an error naming the failed read",
				code, body, receivePreparing.String())
		}
		waitFor(t, time.Second, "the alarm", func() bool { return durableAlarms(logs) > 0 })
		if alarms := durableAlarmRecords(logs); len(alarms) != 1 || !strings.Contains(fmt.Sprint(alarms[0]["reason"]), "read the durable consumer") {
			t.Fatalf("alarms: %v, want one at ERROR whose reason names the failed read", alarms)
		}
		if state := receiver.report().State; state != receivePreparing {
			t.Fatalf("the receive loop is %s, want it still preparing the durable", state)
		}
	})
}

// Every durable an earlier listener created filters on its build's stream subjects. The holder
// corrects it to the consumer policy's filter, every subject, and it goes on delivering what is
// published afterwards on every subject the stream stores. nats-server 2.10 accepts an update that
// clears a push durable's filter instead, and then delivers nothing published after it.
func TestADurableFilteredOnTheStreamSubjectsDeliversEverySubjectOnceCorrected(t *testing.T) {
	uri := testnats.URL(t)
	const machineID = "box-filtered"
	consumer := bus.ListenerDurable(machineID)
	client, err := bus.ConnectOwningStream([]string{uri}, bus.WithReplicas(1))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(client.Close)
	if _, err := client.JS().AddConsumer(bus.Stream, &natsgo.ConsumerConfig{
		Durable:        consumer,
		DeliverSubject: natsgo.NewInbox(),
		FilterSubjects: bus.StreamSubjects(),
		AckPolicy:      natsgo.AckExplicitPolicy,
		AckWait:        60 * time.Second,
	}); err != nil {
		t.Fatalf("create the filtered durable: %v", err)
	}

	handle, received := recordingHandler()
	rig := startReceiverRig(t, uri, machineID, "pod-a", handle, testReceiveTiming(), testLeaseTiming())
	rig.awaitState(t, receiveActive, 10*time.Second)
	info, err := client.JS().ConsumerInfo(bus.Stream, consumer)
	if err != nil || info.Config.FilterSubject != consumerFilterAll || len(info.Config.FilterSubjects) != 0 {
		t.Fatalf("the corrected durable: %+v %v, want the filter %q alone", info, err, consumerFilterAll)
	}

	want := map[string]bool{}
	for _, pattern := range bus.StreamSubjects() {
		subject := strings.ReplaceAll(strings.ReplaceAll(pattern, ">", "regression"), "*", "x")
		if _, err := client.JS().Publish(subject, []byte(subject)); err != nil {
			t.Fatalf("publish on %s: %v", subject, err)
		}
		want[subject] = true
	}
	for range len(want) {
		message := awaitReceived(t, received, 5*time.Second, "the corrected durable")
		if !want[message.data] {
			t.Fatalf("the corrected durable delivered %q twice or unpublished", message.data)
		}
		delete(want, message.data)
	}
}

// A standby's self-health probe has no durable to check: it stays healthy, and the monitor keeps
// the standby up however long it waits, well past the three intervals after which a terminal
// failure would end it. /healthz answers it 200 healthy, as a standby naming the active pod.
func TestAStandbyOutlivesThreeSelfHealthIntervals(t *testing.T) {
	uri := testnats.URL(t)
	const machineID = "box-standby"
	handle, _ := recordingHandler()
	startReceiverRig(t, uri, machineID, "pod-a", handle, testReceiveTiming(), testLeaseTiming()).awaitState(t, receiveActive, 10*time.Second)
	standby := startReceiverRig(t, uri, machineID, "pod-b", handle, testReceiveTiming(), testLeaseTiming())
	waitFor(t, 5*time.Second, "the standby to read pod-a's lease", func() bool {
		return standby.lease.Status().ActiveInstance == "pod-a"
	})

	registry, err := store.Open(standby.client.Conn, store.WithReplicas(1))
	if err != nil {
		t.Fatalf("open interest registry: %v", err)
	}
	t.Cleanup(registry.StopWatch)
	sessions, err := session.OpenSessionRegistry(standby.client.Conn, session.WithSessionReplicas(1))
	if err != nil {
		t.Fatalf("open session registry: %v", err)
	}
	t.Cleanup(sessions.StopWatch)
	caches := listenerCaches(registry, sessions, nil)
	logs := &cmdtest.LockedBuffer{}
	ctx, cancel := context.WithCancel(context.Background())
	var terminated atomic.Bool
	done := make(chan struct{})
	const interval = 50 * time.Millisecond
	probe := func() error { return standby.receiver.probe(ctx) }
	go func() {
		defer close(done)
		runSelfHealthMonitor(ctx, logging.NewWithWriter("test", logs),
			func() error { return checkSelfHealth(caches, probe) },
			func(err error) bool { return isUnrecoverableSelfHealthFailure(err, standby.client, caches) },
			standby.client.Connected,
			func() error {
				return rebuildListenerDependencies(ctx, standby.client, caches, probe, standby.receiver.resubscribe)
			},
			func() { terminated.Store(true) },
			interval, 3)
	}()
	time.Sleep(15 * interval)
	cancel()
	<-done
	if terminated.Load() {
		t.Fatal("the self-health monitor terminated a standby")
	}
	if strings.Contains(logs.String(), "self-health probe failed") {
		t.Fatalf("a standby failed its self-health probe:\n%s", logs.String())
	}
	if state := standby.receiver.report().State; state != receiveStandby {
		t.Fatalf("the standby's receive loop is %s", state)
	}

	subscribeRoleLane(t, standby.client)
	var state atomic.Pointer[listenerDeps]
	state.Store(&listenerDeps{client: standby.client, registry: registry, sessions: sessions, caches: caches, receiver: standby.receiver})
	code, body := serveHealthz(&state)
	if code != http.StatusOK || body["status"] != "healthy" || body["role"] != "standby" || body["active_instance"] != "pod-a" {
		t.Fatalf("the standby's /healthz = %d %v, want 200 healthy, role standby, active_instance pod-a", code, body)
	}
	if _, ok := body["num_pending"]; ok {
		t.Fatalf("the standby's /healthz reports the durable's lag: %v", body)
	}
}

// The holder whose connection the bus replaces (nats.go gave up on it) subscribes again on the new
// connection, and keeps delivering.
func TestTheHolderReceivesOnAConnectionTheBusReplaced(t *testing.T) {
	uri := testnats.URL(t)
	handle, received := recordingHandler()
	rig := startReceiverRig(t, uri, "box-replaced", "pod-a", handle, testReceiveTiming(), testLeaseTiming())
	rig.awaitState(t, receiveActive, 10*time.Second)
	closed := rig.client.Conn
	closed.Close()
	waitFor(t, 30*time.Second, "the subscription to move onto the replacement connection", func() bool {
		report := rig.receiver.report()
		return rig.receiver.clientConn.Load() != closed && report.State == receiveActive && report.SubscriptionValid
	})
	publisher := testnats.Connect(t, uri)
	t.Cleanup(publisher.Close)
	js, err := publisher.JetStream()
	if err != nil {
		t.Fatalf("JetStream: %v", err)
	}
	if _, err := js.Publish(receiveTestSubject, []byte("after the replacement")); err != nil {
		t.Fatalf("publish: %v", err)
	}
	if message := awaitReceived(t, received, 10*time.Second, "the holder"); message.data != "after the replacement" {
		t.Fatalf("the holder received %q", message.data)
	}
}

// noLeaseConfig is a NATS server whose one nkey user may publish to anything but the listener
// state bucket: the grant of a hub that has not yet granted the bucket to its listeners.
func noLeaseConfig(user string) string {
	return bucketDeniedConfig(user, bus.ListenerStateBucket)
}

// A listener whose NATS user lacks the grant for the listener state bucket does not exit: it logs
// one ERROR naming the bucket and the grant, runs without a lease as the machine's one deliverer,
// delivers, and says in /healthz that the lease is unavailable.
func TestAListenerWithoutTheLeaseGrantDeliversWithoutALease(t *testing.T) {
	seed, public := testnats.User(t)
	_, uri := testnats.StartConfig(t, noLeaseConfig(public), []string{"-js"}, false)
	held := cmdtest.HoldPort(t)
	listener := &listenerProcess{port: held.Port}
	cmd := exec.Command(cmdtest.Build(t, "envoy-listener"))
	cmd.Env = []string{
		"PORT=" + strconv.Itoa(listener.port),
		"ENVOY_LISTEN_HOST=127.0.0.1",
		"ENVOY_MACHINE_ID=box-ungranted",
		"NATS_URLS=" + uri,
		"NATS_NKEY_SEED=" + seed,
		"ENVOY_API_TOKEN=" + listenerTestToken,
		"ENVOY_HOST_BRIDGE=127.0.0.1",
	}
	held.Pass(cmd, "ENVOY_LISTEN_FD")
	listener.Process = cmdtest.Start(t, "the listener box-ungranted", cmd)
	listener.waitHealthy(t)

	unavailable := func(record map[string]any) bool { return record["level"] == "ERROR" }
	if got := countRecords(listener, "listener lease unavailable: the NATS user lacks the grant for KV bucket envoy_listener; delivering without a lease", unavailable); got != 1 {
		t.Fatalf("the listener logged the missing grant %d times at ERROR, want once:\n%s", got, listener.Output.String())
	}
	status, answer := callListener(t, listener.port, http.MethodGet, "/healthz", "")
	var health map[string]any
	_ = json.Unmarshal([]byte(answer), &health)
	if status != http.StatusOK || health["role"] != "active" || health["lease"] != listenerLeaseUnavailable {
		t.Fatalf("/healthz without the grant = %d %v, want 200, role active, lease %q", status, health, listenerLeaseUnavailable)
	}

	topic := "notifications.github.acme.ungranted.pr.1"
	serving := startServingPort(t, "ses_ungranted", http.StatusNoContent)
	registerServingSession(t, listener, serving, "127.0.0.1", topic)
	publisher, err := bus.Connect([]string{uri}, bus.WithEnvironment(func(name string) (string, bool) {
		if name == "NATS_NKEY_SEED" {
			return seed, true
		}
		return "", false
	}))
	if err != nil {
		t.Fatalf("connect the publisher: %v", err)
	}
	t.Cleanup(publisher.Close)
	item := newEnvelope("github", topic, "pull request 1 was opened")
	publish(t, publisher, item)
	requirePromptCarries(t, serving.awaitPrompt(t, 10*time.Second), item)
	if listener.Cmd.ProcessState != nil {
		t.Fatalf("the listener exited:\n%s", listener.Output.String())
	}
}

// A holder whose renewals lapse stops receiving while its record still names it, so no other pod
// has taken the lease and nothing of the durable reaches a session. Its /healthz must not call
// that healthy: it answers 200 degraded, a standby naming itself, until it resumes or another pod
// takes the record. A degraded branch that asked only whether the record named a holder would
// read the lapsed pod's own record as an answer and call the pod delivering nothing healthy.
func TestALapsedHolderDoesNotAnswerHealthy(t *testing.T) {
	uri := testnats.URL(t)
	const machineID = "box-lapsed-health"
	handle, _ := recordingHandler()
	// The lease runs through a relay the test freezes; the bus connection answers throughout, so
	// nothing but the lease ends the term.
	leaseLink := startListenerRelay(t, uri)
	active := newReceiverRigVia(t, uri, leaseLink.relayed, machineID, "pod-a", handle, testReceiveTiming(), testLeaseTiming())
	active.lease.Start(context.Background())
	active.receiver.Start()
	active.awaitState(t, receiveActive, 10*time.Second)
	get := healthzFor(t, active.client, active.receiver)
	if code, body := get(); code != http.StatusOK || body["status"] != "healthy" || body["role"] != "active" {
		t.Fatalf("the holder's /healthz = %d %v, want 200 healthy, role active", code, body)
	}

	thaw := leaseLink.freeze()
	t.Cleanup(thaw)
	waitFor(t, 30*time.Second, "pod-a's lease to lapse, ending its term", func() bool {
		return strings.Contains(active.logs.String(), "listener lease lapsed")
	})
	active.awaitState(t, receiveStandby, 10*time.Second)
	code, body := get()
	if code != http.StatusOK || body["status"] != "degraded" || body["role"] != "standby" || body["active_instance"] != "pod-a" {
		t.Fatalf("a lapsed holder's /healthz = %d %v, want 200 degraded, role standby, active_instance pod-a", code, body)
	}
}
