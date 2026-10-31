package delivery

// The intake's durable: the filter subject it carries, the flow control bind stamps on it, its
// cutover from an earlier release's durable, and how it keeps up with a burst and a backlog.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/duty"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/testnats"
)

// TestIntakeStampsItsFlowControlOnADurableAnEarlierReleaseLeft holds what bind does to the
// durable production already has. An earlier release left it with a filter-subject set that
// withholds every workflow run (TestWhichFilterShapeDeliversAWorkflowSubject), and with the
// server's own flow control, 1,000 messages outstanding against a 30-second ack wait, which
// redelivers faster than a handler doing its GitHub calls inline can acknowledge. Neither can be
// set once and forgotten, since the durable already exists: bind has to correct both without an
// operator noticing a log line, which is what this holds.
func TestIntakeStampsItsFlowControlOnADurableAnEarlierReleaseLeft(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	settings := seedDeliverySettings(t, ctx, pool)

	natsURL := testnats.URL(t)
	natsClient, err := bus.ConnectOwningStream([]string{natsURL})
	if err != nil {
		t.Fatalf("connect NATS: %v", err)
	}
	t.Cleanup(natsClient.Close)

	// The durable an earlier release left: its filter subjects, and the server's own flow control.
	legacy := natsgo.ConsumerConfig{
		Durable:        bus.DispatchDeliveryDurable,
		DeliverSubject: natsgo.NewInbox(),
		FilterSubjects: []string{
			"notifications.github.*.*.pr.*",
			contracts.GithubWorkflowSubject("acme", "widgets", pathBaseForTest(settings.DeployWorkflowPath), ">"),
			contracts.GithubWorkflowSubject("acme", "widgets", pathBaseForTest(settings.PRChecksWorkflowPath), ">"),
		},
		AckPolicy: natsgo.AckExplicitPolicy,
	}
	created, err := natsClient.JS().AddConsumer(bus.Stream, &legacy)
	if err != nil {
		t.Fatalf("pre-create the durable an earlier release left: %v", err)
	}
	if created.Config.MaxAckPending == intakeMaxAckPending || created.Config.AckWait == intakeAckWait() {
		t.Fatalf("the pre-created durable already carries this release's flow control (%s/%d), so the test proves nothing",
			created.Config.AckWait, created.Config.MaxAckPending)
	}

	intake := newIntake(natsClient, pool, deliveryTestLeases(t, pool), newFakeGitHub(t).newTestClient(), events.NewBroker())
	startIntake(t, intake)

	// bind replaces this durable (it carries a filter-subject set), deleting it and creating its
	// replacement, so a read can land between the two and find no durable at all; that is "not
	// yet", not a failure.
	var info *natsgo.ConsumerInfo
	waitFor(t, 20*time.Second, func() bool {
		read, err := natsClient.JS().ConsumerInfo(bus.Stream, bus.DispatchDeliveryDurable)
		if errors.Is(err, natsgo.ErrConsumerNotFound) {
			return false
		}
		if err != nil {
			t.Fatalf("read consumer info: %v", err)
		}
		info = read
		return info.Config.MaxAckPending == intakeMaxAckPending && info.Config.AckWait == intakeAckWait()
	}, func() string {
		if info == nil {
			return fmt.Sprintf("no durable named %s after %s", bus.DispatchDeliveryDurable, 20*time.Second)
		}
		return fmt.Sprintf("durable ack_wait/max_ack_pending = %s/%d after %s, want %s/%d (bind must correct an earlier release's flow control)",
			info.Config.AckWait, info.Config.MaxAckPending, 20*time.Second, intakeAckWait(), intakeMaxAckPending)
	})
	if info.Config.FilterSubject != githubIntakeSubject || len(info.Config.FilterSubjects) > 0 {
		t.Fatalf("durable filter = %q / %v, want %q", info.Config.FilterSubject, info.Config.FilterSubjects, githubIntakeSubject)
	}
	// The bound this release holds: what NATS may have outstanding has to be acknowledgeable
	// inside the ack wait, handled one at a time under intakeMessageTimeout.
	if worst := time.Duration(intakeMaxAckPending) * intakeMessageTimeout; worst >= intakeAckWait() {
		t.Fatalf("intakeMaxAckPending*intakeMessageTimeout = %s, want less than intakeAckWait (%s)", worst, intakeAckWait())
	}
}

// TestWhichFilterShapeDeliversAWorkflowSubject pins why the durable an earlier release left never
// recorded a workflow run. It stores the same workflow-run messages, then reads them through each
// filter shape in turn, and reports both what each shape claims (NumPending) and what it hands
// over. Every shape claims all of them. The old filter set -- the wildcard pull-request subject
// beside the workflow subject -- hands over none of the runs while handing over every pull
// request, and the comment on githubIntakeSubject rests on that.
func TestWhichFilterShapeDeliversAWorkflowSubject(t *testing.T) {
	natsClient := intakeTestClient(t)
	const stored = 12
	for i := range stored {
		publishWorkflowEnvelope(t, natsClient, "acme", "widgets", ".github/workflows/deploy.yml", int64(6000+i))
	}
	subject := contracts.GithubWorkflowSubject("acme", "widgets", "deploy.yml", "in_progress")
	oldSet := []string{
		"notifications.github.*.*.pr.*",
		contracts.GithubWorkflowSubject("acme", "widgets", "deploy.yml", ">"),
		contracts.GithubWorkflowSubject("acme", "widgets", "pr-checks.yml", ">"),
	}
	t.Logf("stored %d messages on %q", stored, subject)

	probe := func(name string, config natsgo.ConsumerConfig) uint64 {
		t.Helper()
		config.AckPolicy = natsgo.AckExplicitPolicy
		info, err := natsClient.JS().AddConsumer(bus.Stream, &config)
		if err != nil {
			t.Logf("%s: AddConsumer refused: %v", name, err)
			return 0
		}
		defer deleteProbeConsumer(t, natsClient, info.Name)
		t.Logf("%s: pending=%d", name, info.NumPending)
		return info.NumPending
	}

	singleWorkflow := probe("FilterSubject = the workflow subject with >", natsgo.ConsumerConfig{FilterSubject: oldSet[1]})
	probe("FilterSubject = the PR subject", natsgo.ConsumerConfig{FilterSubject: oldSet[0]})
	probe("FilterSubject = the PR-checks workflow subject with >", natsgo.ConsumerConfig{FilterSubject: oldSet[2]})
	pluralOne := probe("pull, FilterSubjects = [the workflow subject with >]", natsgo.ConsumerConfig{FilterSubjects: []string{oldSet[1]}})
	pluralAll := probe("pull, FilterSubjects = the whole old set", natsgo.ConsumerConfig{FilterSubjects: oldSet})

	// The same filters on a push consumer -- one carrying a DeliverSubject -- which is what this
	// package's durable is. This is the axis the probes above do not cover.
	pushSingle := probe("push, FilterSubject = the workflow subject with >",
		natsgo.ConsumerConfig{DeliverSubject: natsgo.NewInbox(), FilterSubject: oldSet[1]})
	pushPluralOne := probe("push, FilterSubjects = [the workflow subject with >]",
		natsgo.ConsumerConfig{DeliverSubject: natsgo.NewInbox(), FilterSubjects: []string{oldSet[1]}})
	pushPluralAll := probe("push, FilterSubjects = the whole old set",
		natsgo.ConsumerConfig{DeliverSubject: natsgo.NewInbox(), FilterSubjects: oldSet})
	pushWide := probe("push, FilterSubject = "+githubIntakeSubject,
		natsgo.ConsumerConfig{DeliverSubject: natsgo.NewInbox(), FilterSubject: githubIntakeSubject})

	// NumPending is what the server reports; what it hands over is a separate question. Fetch
	// from a fresh pull durable of each shape by name, through the jetstream package, which binds
	// no subject, and count what actually arrives.
	js, err := jetstream.New(natsClient.Conn)
	if err != nil {
		t.Fatalf("open the jetstream API: %v", err)
	}
	// deleteOnCleanup removes a probe durable when the test ends, so each probe this test makes is
	// gone with it whether or not a later step fails.
	deleteOnCleanup := func(durable string) {
		t.Cleanup(func() { deleteProbeConsumer(t, natsClient, durable) })
	}
	// fetch creates a fresh pull durable carrying filters -- as the one FilterSubject when plural
	// is false, as FilterSubjects when it is true -- fetches up to want messages from it by name,
	// and counts by kind what arrives.
	fetches := 0
	fetch := func(t *testing.T, name string, plural bool, want int, filters ...string) (runs, prs int) {
		t.Helper()
		fetches++
		config := jetstream.ConsumerConfig{Durable: fmt.Sprintf("probe-fetch-%d", fetches), AckPolicy: jetstream.AckExplicitPolicy}
		if plural {
			config.FilterSubjects = filters
		} else if len(filters) == 1 {
			config.FilterSubject = filters[0]
		} else {
			t.Fatalf("fetch %s: a single FilterSubject cannot hold %d filters", name, len(filters))
		}
		consumer, err := js.CreateConsumer(t.Context(), bus.Stream, config)
		if err != nil {
			t.Logf("fetch %s: create refused: %v", name, err)
			return 0, 0
		}
		deleteOnCleanup(config.Durable)
		batch, err := consumer.Fetch(want, jetstream.FetchMaxWait(3*time.Second))
		if err != nil {
			t.Logf("fetch %s: %v", name, err)
			return 0, 0
		}
		for msg := range batch.Messages() {
			if strings.Contains(msg.Subject(), ".pr.") {
				prs++
			} else {
				runs++
			}
			if err := msg.Ack(); err != nil {
				t.Logf("ack on %s: %v", name, err)
			}
		}
		t.Logf("fetch %s: %d of %d workflow runs and %d pull requests delivered (batch error %v)", name, runs, stored, prs, batch.Error())
		return runs, prs
	}
	// The table the PR body and githubIntakeSubject's comment give, row for row: how many of the
	// stored workflow runs each shape hands over.
	for _, row := range []struct {
		name    string
		plural  bool
		filters []string
		runs    int
	}{
		{"single, the workflow subject", false, []string{oldSet[1]}, stored},
		{"plural, the workflow subject alone", true, []string{oldSet[1]}, stored},
		{"plural, the workflow subject then the PR-checks workflow subject", true, []string{oldSet[1], oldSet[2]}, stored},
		{"plural, a concrete PR subject then the workflow subject", true, []string{"notifications.github.acme.widgets.pr.1", oldSet[1]}, stored},
		{"plural, the PR subject then the workflow subject", true, []string{oldSet[0], oldSet[1]}, 0},
		{"plural, the workflow subject then the PR subject", true, []string{oldSet[1], oldSet[0]}, 0},
		{"plural, the whole old set", true, oldSet, 0},
	} {
		t.Run(row.name, func(t *testing.T) {
			if runs, _ := fetch(t, row.name, row.plural, stored, row.filters...); runs != row.runs {
				t.Errorf("delivered %d of %d workflow runs, want %d; githubIntakeSubject's comment and the PR body give this table, so revisit both",
					runs, stored, row.runs)
			}
		})
	}

	// The whole config the stalled release built, field for field, under a name of its own: a
	// durable push consumer with the three filter subjects and this package's flow control. Kept
	// alive rather than probed, because the subscription bound to it below is the subject here.
	old, err := natsClient.JS().AddConsumer(bus.Stream, &natsgo.ConsumerConfig{
		Durable:        "probe-old-config",
		DeliverSubject: natsgo.NewInbox(),
		FilterSubjects: oldSet,
		AckPolicy:      natsgo.AckExplicitPolicy,
		AckWait:        intakeAckWait(),
		MaxAckPending:  intakeMaxAckPending,
	})
	if err != nil {
		t.Fatalf("create the old-config durable: %v", err)
	}
	deleteOnCleanup(old.Name)
	t.Logf("whole old config matched %d of %d stored messages", old.NumPending, stored)

	// Bound with an empty subject, the only one nats.go accepts against a plural-filter consumer.
	// Against the whole old set nothing arrives -- but the fetches above show the server hands that
	// set none of these messages however it is read. A plural set the server does deliver, bound
	// the same way, separates the empty subject from the filter set.
	pushed := func(name, consumer, subject string, want int) {
		t.Helper()
		got := drain(t, natsClient, consumer, subject)
		t.Logf("%s: %d of %d arrived", name, got, stored)
		if got != want {
			t.Errorf("%s: %d of %d arrived, want %d", name, got, stored, want)
		}
	}
	pushed("bound to the old config with an empty subject", "probe-old-config", "", 0)
	if _, err := natsClient.JS().AddConsumer(bus.Stream, &natsgo.ConsumerConfig{
		Durable:        "probe-plural-delivering",
		DeliverSubject: natsgo.NewInbox(),
		FilterSubjects: []string{oldSet[1], oldSet[2]},
		AckPolicy:      natsgo.AckExplicitPolicy,
	}); err != nil {
		t.Fatalf("create the delivering plural-filter durable: %v", err)
	}
	deleteOnCleanup("probe-plural-delivering")
	pushed("bound to [workflow, PR-checks workflow] with an empty subject", "probe-plural-delivering", "", stored)
	single, err := natsClient.JS().AddConsumer(bus.Stream, &natsgo.ConsumerConfig{
		Durable:        "probe-new-config",
		DeliverSubject: natsgo.NewInbox(),
		FilterSubject:  githubIntakeSubject,
		AckPolicy:      natsgo.AckExplicitPolicy,
		AckWait:        intakeAckWait(),
		MaxAckPending:  intakeMaxAckPending,
	})
	if err != nil {
		t.Fatalf("create the single-filter durable: %v", err)
	}
	deleteOnCleanup(single.Name)
	pushed("bound to the new config on its own filter subject", single.Name, githubIntakeSubject, stored)

	if pushWide != stored {
		t.Fatalf("the push consumer this release creates matched %d of %d stored messages", pushWide, stored)
	}
	if singleWorkflow != stored {
		t.Fatalf("the workflow subject matched %d of %d stored messages as a single pull FilterSubject", singleWorkflow, stored)
	}
	t.Logf("verdict: pull single=%d plural-one=%d plural-all=%d | push single=%d plural-one=%d plural-all=%d wide=%d (of %d stored)",
		singleWorkflow, pluralOne, pluralAll, pushSingle, pushPluralOne, pushPluralAll, pushWide, stored)

	// The old set does deliver pull requests. Add pull-request messages, which its first filter
	// names, and count by kind what a fresh durable of the whole old set hands over.
	const prs = 5
	for n := range prs {
		payload, err := json.Marshal(map[string]string{"kind": "pr", "repo": "acme/widgets", "number": strconv.Itoa(n + 1)})
		if err != nil {
			t.Fatalf("encode pull request payload: %v", err)
		}
		id := fmt.Sprintf("%s-pr-%d", t.Name(), n+1)
		if err := natsClient.Publish(contracts.Envelope{
			EventID: id, Source: "github", SourceEventID: id, DedupeKey: "github." + id,
			Topic:   fmt.Sprintf("notifications.github.acme.widgets.pr.%d", n+1),
			Payload: string(payload), TraceID: id, IssuedAt: time.Now().Unix(),
		}); err != nil {
			t.Fatalf("publish pull request envelope: %v", err)
		}
	}
	gotRuns, gotPRs := fetch(t, "plural, the whole old set, with pull requests stored too", true, stored+prs, oldSet...)
	if gotPRs != prs || gotRuns != 0 {
		t.Fatalf("the old filter set delivered %d of %d pull requests and %d of %d workflow runs; the intake's filter comment says it delivers every pull request and no workflow run, so revisit githubIntakeSubject's comment",
			gotPRs, prs, gotRuns, stored)
	}
}

// drain binds a push subscription to consumer on subject, the way this package's bind does, and
// reports how many messages reach its handler within a short window.
func drain(t *testing.T, natsClient *bus.Client, consumer, subject string) int {
	t.Helper()
	var mu sync.Mutex
	arrived := 0
	sub, err := natsClient.JS().Subscribe(subject, func(msg *natsgo.Msg) {
		mu.Lock()
		arrived++
		mu.Unlock()
		if err := msg.Ack(); err != nil {
			t.Logf("ack on %s: %v", consumer, err)
		}
	}, natsgo.Bind(bus.Stream, consumer), natsgo.ManualAck())
	if err != nil {
		t.Logf("subscribe to %s on subject %q: %v", consumer, subject, err)
		return 0
	}
	defer func() {
		if err := sub.Unsubscribe(); err != nil {
			t.Logf("unsubscribe from %s: %v", consumer, err)
		}
	}()
	time.Sleep(3 * time.Second)
	mu.Lock()
	defer mu.Unlock()
	return arrived
}

// TestBindReplacesAPluralFilterDurableAtItsOwnAckFloor holds the cutover from the durable
// production already has: one carrying the plural filter, part way through its stream, with some
// messages acknowledged and the rest not. bind replaces it at its own ack floor, so the
// replacement delivers exactly the messages the old durable had not acknowledged -- none of the
// acknowledged ones come back, and none of the unacknowledged ones are skipped.
func TestBindReplacesAPluralFilterDurableAtItsOwnAckFloor(t *testing.T) {
	shrinkIntakeFlowControl(t)
	pool, ctx := deliveryTestPool(t)
	settings := seedDeliverySettings(t, ctx, pool)
	natsClient := intakeTestClient(t)

	const acknowledged, unacknowledged = 5, 7
	for i := range acknowledged + unacknowledged {
		publishWorkflowEnvelope(t, natsClient, "acme", "widgets", settings.DeployWorkflowPath, int64(9000+i))
	}

	// A plural-filter durable part way through its stream, which is what bind replaces. Its filter
	// set is the two workflow subjects alone rather than the whole set an earlier release carried:
	// that set never hands a workflow run over (TestWhichFilterShapeDeliversAWorkflowSubject), so
	// it could not acknowledge one, and the replacement keys on the stream sequence of the ack
	// floor whichever plural set the durable carries. The first messages are acknowledged through
	// a pull fetch, which reaches them in stream order, so the ack floor sits at the last of them.
	if _, err := natsClient.JS().AddConsumer(bus.Stream, &natsgo.ConsumerConfig{
		Durable: bus.DispatchDeliveryDurable,
		FilterSubjects: []string{
			contracts.GithubWorkflowSubject("acme", "widgets", pathBaseForTest(settings.DeployWorkflowPath), ">"),
			contracts.GithubWorkflowSubject("acme", "widgets", pathBaseForTest(settings.PRChecksWorkflowPath), ">"),
		},
		AckPolicy: natsgo.AckExplicitPolicy,
	}); err != nil {
		t.Fatalf("create the plural-filter durable: %v", err)
	}
	js, err := jetstream.New(natsClient.Conn)
	if err != nil {
		t.Fatalf("open the jetstream API: %v", err)
	}
	legacy, err := js.Consumer(ctx, bus.Stream, bus.DispatchDeliveryDurable)
	if err != nil {
		t.Fatalf("look up the plural-filter durable: %v", err)
	}
	batch, err := legacy.Fetch(acknowledged, jetstream.FetchMaxWait(10*time.Second))
	if err != nil {
		t.Fatalf("fetch from the plural-filter durable: %v", err)
	}
	got := 0
	for msg := range batch.Messages() {
		if err := msg.DoubleAck(ctx); err != nil {
			t.Fatalf("acknowledge a message on the plural-filter durable: %v", err)
		}
		got++
	}
	if err := batch.Error(); err != nil || got != acknowledged {
		t.Fatalf("fetch the first %d messages: got %d, err %v", acknowledged, got, err)
	}
	before, err := natsClient.JS().ConsumerInfo(bus.Stream, bus.DispatchDeliveryDurable)
	if err != nil {
		t.Fatalf("read the plural-filter durable: %v", err)
	}
	if before.AckFloor.Consumer != acknowledged {
		t.Fatalf("plural-filter durable ack floor = %d, want %d before the cutover", before.AckFloor.Consumer, acknowledged)
	}

	var mu sync.Mutex
	fetched := map[int64]int{}
	fake := newFakeGitHub(t)
	fake.handle("GET /repos/acme/widgets/actions/runs/{run_id}", func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.ParseInt(r.PathValue("run_id"), 10, 64)
		if err != nil {
			t.Errorf("parse run id %q: %v", r.PathValue("run_id"), err)
		}
		mu.Lock()
		fetched[id]++
		mu.Unlock()
		mustEncode(t, w, smallRun(id, time.Now().UTC()))
	})
	intake := newIntake(natsClient, pool, deliveryTestLeases(t, pool), fake.newTestClient(), events.NewBroker())
	startIntake(t, intake)

	awaitRuns(t, ctx, pool, natsClient, unacknowledged, 30*time.Second)
	// Time for an acknowledged message the replacement wrongly replayed to arrive too.
	time.Sleep(2 * time.Second)
	t.Log(intakeStateReport(t, natsClient))

	after, err := natsClient.JS().ConsumerInfo(bus.Stream, bus.DispatchDeliveryDurable)
	if err != nil {
		t.Fatalf("read the replacement durable: %v", err)
	}
	if after.Config.FilterSubject != githubIntakeSubject || len(after.Config.FilterSubjects) > 0 {
		t.Fatalf("replacement filter = %q / %v, want %q", after.Config.FilterSubject, after.Config.FilterSubjects, githubIntakeSubject)
	}
	mu.Lock()
	defer mu.Unlock()
	for i := range acknowledged + unacknowledged {
		id, got := int64(9000+i), fetched[int64(9000+i)]
		if i < acknowledged && got != 0 {
			t.Errorf("run %d was acknowledged on the old durable and replayed %d times by the replacement", id, got)
		}
		if i >= acknowledged && got != 1 {
			t.Errorf("run %d was unacknowledged on the old durable and delivered %d times by the replacement, want 1", id, got)
		}
	}
}

// TestIntakeKeepsUpWithABurstOfDeliveryEvents publishes more events at once than the consumer may
// have outstanding and holds the whole path end to end: every one is stored, each costs exactly
// one GitHub fetch, nothing is redelivered and nothing is left outstanding. A burst larger than
// intakeMaxAckPending is the case that bound exists for, so a flow-control change that stalls the
// consumer -- an acknowledgement that never reaches the server, a credit never given back --
// fails here.
func TestIntakeKeepsUpWithABurstOfDeliveryEvents(t *testing.T) {
	shrinkIntakeFlowControl(t)
	pool, ctx := deliveryTestPool(t)
	settings := seedDeliverySettings(t, ctx, pool)

	const burst = 30
	var mu sync.Mutex
	fetches := map[int64]int{}
	fake := newFakeGitHub(t)
	fake.handle("GET /repos/acme/widgets/actions/runs/{run_id}", func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.ParseInt(r.PathValue("run_id"), 10, 64)
		if err != nil {
			t.Errorf("parse run id %q: %v", r.PathValue("run_id"), err)
		}
		mu.Lock()
		fetches[id]++
		mu.Unlock()
		time.Sleep(20 * time.Millisecond)
		mustEncode(t, w, smallRun(id, time.Now().UTC()))
	})

	natsClient := intakeTestClient(t)
	intake := newIntake(natsClient, pool, deliveryTestLeases(t, pool), fake.newTestClient(), events.NewBroker())
	startIntake(t, intake)
	awaitBoundDurable(t, natsClient)

	duplicates := 0
	for i := range burst {
		if publishWorkflowEnvelope(t, natsClient, "acme", "widgets", settings.DeployWorkflowPath, int64(4000+i)) {
			duplicates++
		}
	}
	if duplicates > 0 {
		t.Fatalf("%d of %d publishes were duplicates the stream already held, so the burst never reached the consumer", duplicates, burst)
	}
	awaitRuns(t, ctx, pool, natsClient, burst, 60*time.Second)
	t.Log(intakeStateReport(t, natsClient))

	// A redelivery, which flow control that did not fit the handler would cause, arrives within
	// the ack wait; give one time to land before reading the consumer's state.
	time.Sleep(2 * time.Second)
	info, err := natsClient.JS().ConsumerInfo(bus.Stream, bus.DispatchDeliveryDurable)
	if err != nil {
		t.Fatalf("read consumer info: %v", err)
	}
	if info.NumRedelivered != 0 || info.NumAckPending != 0 {
		t.Fatalf("consumer redelivered=%d ack_pending=%d, want 0/0 (every message acknowledged once, none redelivered)", info.NumRedelivered, info.NumAckPending)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(fetches) != burst {
		t.Fatalf("distinct runs fetched = %d, want %d", len(fetches), burst)
	}
	for id, count := range fetches {
		if count != 1 {
			t.Fatalf("run %d fetched %d times, want exactly 1 (a redelivery repeats its GitHub calls)", id, count)
		}
	}
}

// TestIntakeDrainsABacklogPublishedBeforeItBinds pins what the server does with events that
// arrive while no subscriber is attached: the window between the durable being created and the
// subscription attaching to it, and the whole time the process is down. The backlog is held
// undelivered rather than pushed at an inbox nobody is reading, so one larger than
// intakeMaxAckPending drains in full once the intake comes up, with nothing waiting on the ack
// wait to be redelivered.
func TestIntakeDrainsABacklogPublishedBeforeItBinds(t *testing.T) {
	shrinkIntakeFlowControl(t)
	pool, ctx := deliveryTestPool(t)
	settings := seedDeliverySettings(t, ctx, pool)

	backlog := 3 * intakeMaxAckPending
	fake := newFakeGitHub(t)
	fake.handle("GET /repos/acme/widgets/actions/runs/{run_id}", func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.ParseInt(r.PathValue("run_id"), 10, 64)
		if err != nil {
			t.Errorf("parse run id %q: %v", r.PathValue("run_id"), err)
		}
		mustEncode(t, w, smallRun(id, time.Now().UTC()))
	})

	natsClient := intakeTestClient(t)
	for i := range backlog {
		publishWorkflowEnvelope(t, natsClient, "acme", "widgets", settings.DeployWorkflowPath, int64(5000+i))
	}

	intake := newIntake(natsClient, pool, deliveryTestLeases(t, pool), fake.newTestClient(), events.NewBroker())
	startIntake(t, intake)

	awaitRuns(t, ctx, pool, natsClient, backlog, 60*time.Second)
	t.Log(intakeStateReport(t, natsClient))
}

// TestASecondProcessStandsByForADurableAnotherHolds: two envoy-dispatch processes serve one NATS
// and one Postgres, and one of them delivers from the durable: the one holding its hold. The
// second is a standby, not a failure: it logs nothing at ERROR, says which process holds the
// durable, never binds it while the first holds it, and takes it over once the first stops --
// which is how a stopped or dead process's delivery work is picked up.
func TestASecondProcessStandsByForADurableAnotherHolds(t *testing.T) {
	logs := captureLogs(t)
	pool, ctx := deliveryTestPool(t)
	seedDeliverySettings(t, ctx, pool)
	natsURL := testnats.URL(t)
	publisher := intakeTestClient(t)

	holder := startIntakeProcess(t, pool, natsURL, nil)
	held := awaitPushBound(t, publisher, "the first process did not bind the durable")
	startIntakeProcess(t, pool, natsURL, nil)
	standingBy := "dispatch delivery: NATS consumer held by another process; standing by"
	waitFor(t, 20*time.Second, func() bool {
		for _, record := range logs() {
			if record["msg"] == standingBy && record["name"] == bus.DispatchDeliveryDurable {
				return true
			}
		}
		return false
	}, func() string {
		return fmt.Sprintf("the standby said nothing about the durable it is standing by for: %v", logs())
	})
	for _, record := range logs() {
		if record["level"] == "ERROR" {
			t.Fatalf("standing by for a durable another process holds was logged as an error: %v", record)
		}
	}
	if info := awaitPushBound(t, publisher, "the first process lost the durable to the standby"); info.Config.DeliverSubject != held.Config.DeliverSubject || !info.Created.Equal(held.Created) {
		t.Fatalf("the standby changed the durable the first process holds: deliver subject %s created %s, was %s %s",
			info.Config.DeliverSubject, info.Created, held.Config.DeliverSubject, held.Created)
	}

	// The holder stops, and the standby takes the durable over on a deliver subject of its own.
	holder.stop()
	waitFor(t, 20*time.Second, func() bool {
		info, err := publisher.JS().ConsumerInfo(bus.Stream, bus.DispatchDeliveryDurable)
		return err == nil && info.PushBound && info.Config.DeliverSubject != held.Config.DeliverSubject
	}, func() string {
		return "the standby never took over the durable the first process let go: " + consumerState(publisher)
	})
}

// TestAConsumerAnotherProcessIsBoundToIsLeftAlone: the take-over deletes the durable and recreates
// it at its ack floor, which would take the push binding away from the process that holds it and
// leave that process delivering nothing. A durable bound to a subject no hold recorded -- a
// process of the build before holds, which binds without one -- is therefore left exactly as it
// is, filter set and all, however much this release would otherwise replace it.
func TestAConsumerAnotherProcessIsBoundToIsLeftAlone(t *testing.T) {
	logs := captureLogs(t)
	pool, ctx := deliveryTestPool(t)
	settings := seedDeliverySettings(t, ctx, pool)
	natsURL := testnats.URL(t)
	holder := intakeTestClient(t)

	// The durable an earlier release left: a filter-subject set, which this release replaces
	// (TestBindReplacesAPluralFilterDurableAtItsOwnAckFloor), as a push consumer so a process can
	// be bound to it.
	legacy := natsgo.ConsumerConfig{
		Durable:        bus.DispatchDeliveryDurable,
		DeliverSubject: natsgo.NewInbox(),
		FilterSubjects: []string{
			"notifications.github.*.*.pr.*",
			contracts.GithubWorkflowSubject("acme", "widgets", pathBaseForTest(settings.DeployWorkflowPath), ">"),
		},
		AckPolicy: natsgo.AckExplicitPolicy,
	}
	before, err := holder.JS().AddConsumer(bus.Stream, &legacy)
	if err != nil {
		t.Fatalf("pre-create the durable an earlier release left: %v", err)
	}
	held, err := holder.Subscribe(legacy.FilterSubjects[0], func(*natsgo.Msg) {}, natsgo.Bind(bus.Stream, bus.DispatchDeliveryDurable), natsgo.ManualAck())
	if err != nil {
		t.Fatalf("bind the other process to the durable: %v", err)
	}
	t.Cleanup(func() { _ = held.Unsubscribe() })
	// The intake starts only once the server reports the other process bound to the durable:
	// that binding reaches the server a moment after Subscribe returns, and the branch under test
	// is the one a bound durable takes.
	awaitPushBound(t, holder, "the durable is not bound to the other process")

	startIntakeProcess(t, pool, natsURL, nil)
	standingBy := "dispatch delivery: NATS consumer bound to a process that holds no hold; standing by until it lets go"
	waitFor(t, 20*time.Second, func() bool {
		for _, record := range logs() {
			if record["msg"] == standingBy && record["name"] == bus.DispatchDeliveryDurable {
				return true
			}
		}
		return false
	}, func() string {
		return fmt.Sprintf("the intake never said it was standing by for the bound durable: %v", logs())
	})
	time.Sleep(2 * durableBindInterval)
	after := awaitPushBound(t, holder, "the other process lost its binding to the durable")
	if !slices.Equal(after.Config.FilterSubjects, legacy.FilterSubjects) || after.Config.FilterSubject != "" {
		t.Fatalf("the durable's filter is %q / %v, want the set it carried: %v",
			after.Config.FilterSubject, after.Config.FilterSubjects, legacy.FilterSubjects)
	}
	if !after.Created.Equal(before.Created) {
		t.Fatalf("the durable was recreated (created %s, was %s) while another process was bound to it", after.Created, before.Created)
	}
	if !held.IsValid() {
		t.Fatal("the other process lost the subscription it had bound to the durable")
	}
	for _, record := range logs() {
		if record["level"] == "ERROR" {
			t.Fatalf("leaving a bound consumer alone was logged as an error: %v", record)
		}
	}
}

// TestABindWhoseLeaseIsHeldElsewhereChangesNothing: every change to the durable runs under the
// durable's own consumer lease (consumerLease), which the reconciliation of the build before holds
// takes too, so that two processes never change one durable at once. With that lease held by
// another caller, an intake that holds the hold touches the durable in no way -- no delete, no
// create, however far the durable is from what this release wants -- and delivers nothing; once
// the lease is free, it takes the durable over. The lease taken here is the one the take-over
// takes, bus.DispatchDeliveryDurable's: a test holding any other durable's lease would leave the
// take-over free and prove nothing.
func TestABindWhoseLeaseIsHeldElsewhereChangesNothing(t *testing.T) {
	logs := captureLogs(t)
	pool, ctx := deliveryTestPool(t)
	seedDeliverySettings(t, ctx, pool)
	leases := deliveryTestLeases(t, pool)
	natsClient := intakeTestClient(t)

	// A durable this release would recreate: the right filter subject, the server's own flow
	// control rather than this package's, and no subscriber.
	before, err := natsClient.JS().AddConsumer(bus.Stream, &natsgo.ConsumerConfig{
		Durable:        bus.DispatchDeliveryDurable,
		DeliverSubject: natsgo.NewInbox(),
		FilterSubject:  githubIntakeSubject,
		AckPolicy:      natsgo.AckExplicitPolicy,
	})
	if err != nil {
		t.Fatalf("pre-create the durable: %v", err)
	}

	// Another caller holds the lease for as long as its pass runs, as the other process's own
	// reconciliation does. It takes no period, as the take-over itself does not.
	taken, release := make(chan struct{}), make(chan struct{})
	ran := make(chan bool, 1)
	go func() {
		ran <- duty.Exclusive(ctx, leases, consumerLease(bus.DispatchDeliveryDurable), time.Time{}, 0, func(context.Context) {
			close(taken)
			<-release
		})
	}()
	releaseLease := sync.OnceFunc(func() { close(release) })
	t.Cleanup(releaseLease)
	select {
	case <-taken:
	case <-time.After(20 * time.Second):
		t.Fatalf("the other caller never took the %s lease", consumerLease(bus.DispatchDeliveryDurable))
	}

	startIntake(t, newIntake(natsClient, pool, leases, newFakeGitHub(t).newTestClient(), events.NewBroker()))
	skipped := func() bool {
		for _, record := range logs() {
			if record["msg"] == "dispatch duty: leased pass skipped" && record["lease"] == consumerLease(bus.DispatchDeliveryDurable) {
				return true
			}
		}
		return false
	}
	waitFor(t, 20*time.Second, skipped, func() string {
		return fmt.Sprintf("the intake never reported skipping the %s lease: %v", consumerLease(bus.DispatchDeliveryDurable), logs())
	})
	time.Sleep(2 * durableBindInterval)
	after, err := natsClient.JS().ConsumerInfo(bus.Stream, bus.DispatchDeliveryDurable)
	if err != nil {
		t.Fatalf("read the durable: %v", err)
	}
	if !after.Created.Equal(before.Created) || after.Config.DeliverSubject != before.Config.DeliverSubject || after.PushBound {
		t.Fatalf("the intake changed or bound a durable whose lease it did not hold: created %s deliver subject %s push-bound %t, was %s %s",
			after.Created, after.Config.DeliverSubject, after.PushBound, before.Created, before.Config.DeliverSubject)
	}

	releaseLease()
	if held := <-ran; !held {
		t.Fatalf("the other caller never held the %s lease, so the take-over was free", consumerLease(bus.DispatchDeliveryDurable))
	}
	takenOver := awaitPushBound(t, natsClient, "the intake did not take the durable over once the lease was free")
	if takenOver.Config.DeliverSubject == before.Config.DeliverSubject || takenOver.Config.MaxAckPending != intakeMaxAckPending || takenOver.Config.AckWait != intakeAckWait() {
		t.Fatalf("the take-over left deliver subject %s ack_wait %s max_ack_pending %d, want a fresh subject and %s/%d",
			takenOver.Config.DeliverSubject, takenOver.Config.AckWait, takenOver.Config.MaxAckPending, intakeAckWait(), intakeMaxAckPending)
	}
}

// awaitPushBound waits for the server to report the durable push-bound and answers with the info
// it read. A subscription's binding reaches the server a moment after Subscribe returns, so a
// single ConsumerInfo straight after a bind reads a consumer that is about to be bound and fails
// on it (CI run 39535772120); every other state this package asserts about a durable is waited
// for the same way (awaitBoundDurable).
func awaitPushBound(t *testing.T, client *bus.Client, failure string) *natsgo.ConsumerInfo {
	t.Helper()
	var info *natsgo.ConsumerInfo
	var err error
	waitFor(t, 20*time.Second, func() bool {
		info, err = client.JS().ConsumerInfo(bus.Stream, bus.DispatchDeliveryDurable)
		return err == nil && info.PushBound
	}, func() string {
		return fmt.Sprintf("%s: %+v, %v", failure, info, err)
	})
	return info
}

// consumerGenerationRow is one durable's config as a generation ships it.
type consumerGenerationRow struct {
	filterSubject string
	deliverPolicy natsgo.DeliverPolicy
	ackPolicy     natsgo.AckPolicy
	ackWait       time.Duration
	maxAckPending int
}

// consumerGenerations pins the config of each intake durable in every consumer generation a build
// has shipped. A row is never edited once a build carries it: a build that changes a durable's
// config ships a new generation (consumerConfigGeneration) and a new row, so a running older
// build, whose take-over keeps a durable of a higher generation as it is deployed, never reverts
// the change. Generation 1's filter subjects, ack waits and max ack pendings are the ones the
// build before generations compares and updates in place, so they stay until no process of that
// build runs.
var consumerGenerations = map[int]map[string]consumerGenerationRow{
	1: {
		bus.DispatchDeliveryDurable: {
			filterSubject: "notifications.github.>",
			deliverPolicy: natsgo.DeliverAllPolicy,
			ackPolicy:     natsgo.AckExplicitPolicy,
			ackWait:       5 * time.Minute,
			maxAckPending: 4,
		},
		bus.DispatchJobDurable: {
			filterSubject: "notifications.github.*.*.job.>",
			deliverPolicy: natsgo.DeliverNewPolicy,
			ackPolicy:     natsgo.AckExplicitPolicy,
			ackWait:       30 * time.Second,
			maxAckPending: 64,
		},
	},
}

// Each durable this build creates has exactly the config pinned for its generation, and carries
// that generation: a change to the config without a new generation and a new row fails here.
func TestTheConsumerConfigEqualsItsGenerationsPinnedRow(t *testing.T) {
	rows, ok := consumerGenerations[consumerConfigGeneration]
	if !ok {
		t.Fatalf("generation %d has no pinned rows; add them with the configs it ships", consumerConfigGeneration)
	}
	for _, config := range []natsgo.ConsumerConfig{deliveryConsumerConfig(), jobConsumerConfig()} {
		row, ok := rows[config.Durable]
		if !ok {
			t.Errorf("generation %d has no pinned row for %s", consumerConfigGeneration, config.Durable)
			continue
		}
		got := consumerGenerationRow{
			filterSubject: config.FilterSubject,
			deliverPolicy: config.DeliverPolicy,
			ackPolicy:     config.AckPolicy,
			ackWait:       config.AckWait,
			maxAckPending: config.MaxAckPending,
		}
		if got != row || len(config.FilterSubjects) > 0 {
			t.Errorf("generation %d ships %s as %+v (filter subjects %v), but its pinned row is %+v; a changed config takes a new generation",
				consumerConfigGeneration, config.Durable, got, config.FilterSubjects, row)
		}
		if stamped := config.Metadata[generationKey]; stamped != strconv.Itoa(consumerConfigGeneration) {
			t.Errorf("%s is created stamped with generation %q, want %d", config.Durable, stamped, consumerConfigGeneration)
		}
	}
}
