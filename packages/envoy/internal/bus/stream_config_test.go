package bus

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"maps"
	"net"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/testnats"
)

type streamInfoJetStream struct {
	nats.JetStreamContext
	config                    nats.StreamConfig
	purgedSubjects            []string
	roleMessages              int
	injectRoleMessageOnUpdate bool
	updateErr                 error
}

func (js *streamInfoJetStream) StreamInfo(_ string, _ ...nats.JSOpt) (*nats.StreamInfo, error) {
	return &nats.StreamInfo{Config: js.config}, nil
}

func (js *streamInfoJetStream) UpdateStream(cfg *nats.StreamConfig, _ ...nats.JSOpt) (*nats.StreamInfo, error) {
	if js.updateErr != nil {
		return nil, js.updateErr
	}
	if js.injectRoleMessageOnUpdate {
		js.roleMessages++
		js.injectRoleMessageOnUpdate = false
	}
	js.config = *cfg
	return &nats.StreamInfo{Config: js.config}, nil
}

func (js *streamInfoJetStream) PurgeStream(_ string, opts ...nats.JSOpt) error {
	for _, opt := range opts {
		request, ok := opt.(*nats.StreamPurgeRequest)
		if !ok || request.Subject == "" {
			continue
		}
		js.purgedSubjects = append(js.purgedSubjects, request.Subject)
		if request.Subject == "notifications.role.>" || request.Subject == "notifications.envoy.exceptions.notifications.role.>" {
			js.roleMessages = 0
		}
	}
	return nil
}

func (js *streamInfoJetStream) ConsumerNames(_ string, _ ...nats.JSOpt) <-chan string {
	names := make(chan string)
	close(names)
	return names
}

func TestConnectWithContextBoundsMultipleUnresponsiveServers(t *testing.T) {
	accepted := make(chan net.Conn, 32)
	urls := make([]string, 0, 2)
	for range 2 {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("listen: %v", err)
		}
		t.Cleanup(func() { _ = listener.Close() })
		go func() {
			for {
				conn, err := listener.Accept()
				if err != nil {
					return
				}
				accepted <- conn
			}
		}()
		urls = append(urls, "nats://"+listener.Addr().String())
	}
	t.Cleanup(func() {
		for {
			select {
			case conn := <-accepted:
				_ = conn.Close()
			default:
				return
			}
		}
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result := make(chan error, 1)
	started := time.Now()
	go func() {
		conn, err := connectWithContext(ctx, "deadline-test", urls, nil, nil, nil, nil, nil, nil)
		if conn != nil {
			conn.Close()
		}
		result <- err
	}()
	select {
	case err := <-result:
		if err == nil {
			t.Fatal("connect unexpectedly succeeded against unresponsive servers")
		}
		if elapsed := time.Since(started); elapsed > 6*time.Second {
			t.Fatalf("connect returned after %s, want the outer five-second deadline", elapsed)
		}
	case <-time.After(6 * time.Second):
		t.Fatal("connect exceeded the outer deadline while walking NATS URLs")
	}
}

func TestConnectWithContextDoesNotPoisonAutomaticReconnect(t *testing.T) {
	baseCtx := context.Background()
	uri := testnats.URL(t)

	// The property under test is that a connect context that is done by the time the server
	// drops the connection does not poison automatic reconnect; cancel it explicitly instead of
	// racing a short dial deadline against the freshly started server.
	connectCtx, cancel := context.WithTimeout(baseCtx, 10*time.Second)
	defer cancel()
	reconnected := make(chan struct{}, 1)
	conn, err := connectWithContext(connectCtx, "reconnect-dialer-test", []string{uri}, nil, nil, func(*nats.Conn) {
		reconnected <- struct{}{}
	}, nil, nil, nil)
	if err != nil {
		t.Fatalf("connect with deadline: %v", err)
	}
	defer conn.Close()
	cancel()
	<-connectCtx.Done()

	if err := conn.ForceReconnect(); err != nil {
		t.Fatalf("force reconnect: %v", err)
	}
	select {
	case <-reconnected:
	case <-time.After(10 * time.Second):
		t.Fatal("connection did not automatically reconnect after its initial context expired")
	}
}

func TestEnsureStreamWithConfig_updatesMaxAgeWhenExistingStreamDiffers(t *testing.T) {
	// Given
	oldConfig := *streamCfg
	oldConfig.MaxAge = time.Minute
	js := &streamInfoJetStream{config: oldConfig}
	desiredConfig := oldConfig
	desiredConfig.MaxAge = 72 * time.Hour

	// When
	if err := ensureStreamWithConfig(js, &desiredConfig); err != nil {
		t.Fatalf("ensure stream: %v", err)
	}

	// Then
	if js.config.MaxAge != desiredConfig.MaxAge {
		t.Fatalf("stream MaxAge = %s, want %s", js.config.MaxAge, desiredConfig.MaxAge)
	}
}

func TestConnectOwningStreamMigratesExistingStreamDuplicatesWindow(t *testing.T) {
	uri := testnats.URL(t)
	legacyConn := testnats.Connect(t, uri)
	t.Cleanup(legacyConn.Close)
	legacyJS, err := legacyConn.JetStream()
	if err != nil {
		t.Fatalf("open legacy JetStream: %v", err)
	}
	legacyConfig := *streamCfg
	legacyConfig.Duplicates = 0
	if _, err := legacyJS.AddStream(&legacyConfig); err != nil {
		t.Fatalf("create legacy stream: %v", err)
	}

	client, err := ConnectOwningStream([]string{uri})
	if err != nil {
		t.Fatalf("connect and migrate stream: %v", err)
	}
	t.Cleanup(client.Close)
	info, err := client.JS().StreamInfo(Stream)
	if err != nil {
		t.Fatalf("read migrated stream: %v", err)
	}
	if info.Config.Duplicates != 72*time.Hour {
		t.Fatalf("stream duplicate window = %s, want 72h", info.Config.Duplicates)
	}
}

func TestEnsureStreamWithConfigPurgesLegacyRoleMessages(t *testing.T) {
	oldConfig := *streamCfg
	oldConfig.Subjects = []string{"notifications.>"}
	js := &streamInfoJetStream{config: oldConfig}

	if err := ensureStreamWithConfig(js, streamCfg); err != nil {
		t.Fatalf("ensure stream: %v", err)
	}

	wantPurgedSubjects := []string{
		"notifications.role.>",
		"notifications.envoy.exceptions.notifications.role.>",
		"notifications.role.>",
		"notifications.envoy.exceptions.notifications.role.>",
	}
	if !reflect.DeepEqual(js.purgedSubjects, wantPurgedSubjects) {
		t.Fatalf("purged subjects = %v, want %v", js.purgedSubjects, wantPurgedSubjects)
	}
}

func TestEnsureStreamWithConfigPurgesRoleMessagesPublishedDuringMigration(t *testing.T) {
	oldConfig := *streamCfg
	oldConfig.Subjects = []string{"notifications.>"}
	js := &streamInfoJetStream{
		config:                    oldConfig,
		injectRoleMessageOnUpdate: true,
	}

	if err := ensureStreamWithConfig(js, streamCfg); err != nil {
		t.Fatalf("ensure stream: %v", err)
	}
	if js.roleMessages != 0 {
		t.Fatalf("legacy role messages published during migration remained: %d", js.roleMessages)
	}
	wantPurges := []string{
		"notifications.role.>",
		"notifications.envoy.exceptions.notifications.role.>",
		"notifications.role.>",
		"notifications.envoy.exceptions.notifications.role.>",
	}
	if !reflect.DeepEqual(js.purgedSubjects, wantPurges) {
		t.Fatalf("purged subjects = %v, want %v", js.purgedSubjects, wantPurges)
	}
}

func TestConnectOwningStreamPurgesLegacyRoleMessagesBeforeDurableConsumerRestart(t *testing.T) {
	uri := testnats.URL(t)
	legacyConn := testnats.Connect(t, uri)
	legacyJS, err := legacyConn.JetStream()
	if err != nil {
		t.Fatalf("open legacy JetStream: %v", err)
	}
	if _, err := legacyJS.AddStream(&nats.StreamConfig{
		Name:      Stream,
		Subjects:  []string{"notifications.>"},
		Retention: nats.LimitsPolicy,
		MaxAge:    72 * time.Hour,
		Storage:   nats.FileStorage,
		Replicas:  1,
	}); err != nil {
		t.Fatalf("create legacy stream: %v", err)
	}

	var legacyDelivery atomic.Int32
	legacySub, err := legacyJS.Subscribe("notifications.>", func(*nats.Msg) {
		legacyDelivery.Add(1)
	}, nats.Durable("legacy-role-replay"), nats.DeliverAll(), nats.AckExplicit(), nats.ManualAck())
	if err != nil {
		t.Fatalf("start legacy durable consumer: %v", err)
	}
	if err := legacyConn.Flush(); err != nil {
		t.Fatalf("flush legacy durable consumer: %v", err)
	}
	if _, err := legacyJS.Publish("notifications.role.legion-replay", []byte("legacy role message")); err != nil {
		t.Fatalf("publish legacy role message: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for legacyDelivery.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if legacyDelivery.Load() != 1 {
		t.Fatal("legacy durable consumer did not receive the role message")
	}
	_ = legacySub
	legacyConn.Close()

	client, err := ConnectOwningStream([]string{uri})
	if err != nil {
		t.Fatalf("migrate stream: %v", err)
	}
	t.Cleanup(client.Close)
	info, err := client.JS().StreamInfo(Stream)
	if err != nil {
		t.Fatalf("read migrated stream: %v", err)
	}
	if info.State.Msgs != 0 {
		t.Fatalf("legacy role messages remained in stream after migration: %d", info.State.Msgs)
	}

	redelivered := make(chan struct{}, 1)
	restartedSub, err := client.Subscribe("notifications.>", func(*nats.Msg) {
		redelivered <- struct{}{}
	}, nats.Durable("legacy-role-replay"), nats.AckExplicit(), nats.ManualAck())
	if err != nil {
		t.Fatalf("restart durable consumer: %v", err)
	}
	t.Cleanup(func() { _ = restartedSub.Unsubscribe() })

	select {
	case <-redelivered:
		t.Fatal("legacy role message was redelivered after the durable consumer restart")
	case <-time.After(500 * time.Millisecond):
	}
}

func TestStreamConfigExcludesRoleLanes(t *testing.T) {
	wantSubjects := []string{
		"notifications.agent.>",
		"notifications.dispatch.>",
		"notifications.github.>",
		"notifications.slack.>",
		"notifications.legion.>",
		"notifications.ghostwispr.>",
		"notifications.whatsapp.>",
		"notifications.envoy.exceptions.notifications.agent.>",
	}
	if !reflect.DeepEqual(streamCfg.Subjects, wantSubjects) {
		t.Fatalf("stream subjects = %v, want %v", streamCfg.Subjects, wantSubjects)
	}
	for _, streamSubject := range streamCfg.Subjects {
		if subjectCapturesRoleLanes(streamSubject) {
			t.Fatalf("stream subject %q captures a role lane", streamSubject)
		}
	}
}

// Every subject of the stream's configuration is classified for /v1/replay as shared or private,
// and in one list only, so a subject added to the stream without a decision on whether a session
// may replay it through a registered interest fails here; a list naming a subject the stream does
// not carry is a stale classification and fails too.
func TestEveryStreamSubjectIsClassifiedForReplay(t *testing.T) {
	for _, subject := range streamCfg.Subjects {
		shared, private := slices.Contains(sharedStreamSubjects, subject), slices.Contains(privateStreamSubjects, subject)
		switch {
		case !shared && !private:
			t.Errorf("stream subject %q is neither shared nor private for replay: add it to sharedStreamSubjects or privateStreamSubjects with the reason its publisher gives", subject)
		case shared && private:
			t.Errorf("stream subject %q is both shared and private for replay", subject)
		}
	}
	for _, subject := range slices.Concat(sharedStreamSubjects, privateStreamSubjects) {
		if !slices.Contains(streamCfg.Subjects, subject) {
			t.Errorf("replay classifies %q, which is no subject of the stream", subject)
		}
	}
}

// A topic is shared only under one shared stream subject: an issue, document, GitHub, Slack, Ghost
// Wispr or WhatsApp topic, a wildcard inside one of them included, and nothing under an agent
// subject, an exception lane or the Legion controller's notices, nor a wildcard wider than one
// shared subject.
func TestSharedStreamTopicHoldsOnlyTopicsUnderASharedStreamSubject(t *testing.T) {
	for topic, want := range map[string]bool{
		"notifications.dispatch.issue.KEY-1.>":                        true,
		"notifications.dispatch.issue.KEY-1.ask.answered":             true,
		"notifications.dispatch.document.ACME.notes-md.>":             true,
		"notifications.dispatch.>":                                    true,
		"notifications.github.acme.widgets.pr.7":                      true,
		"notifications.github.acme.widgets.pr.7.>":                    true,
		"notifications.slack.T1.C1.message":                           true,
		"notifications.ghostwispr.20260326041405.>":                   true,
		"notifications.whatsapp.15551234567.>":                        true,
		"notifications.agent.ses_a":                                   false,
		"notifications.agent.>":                                       false,
		"notifications.envoy.exceptions.notifications.agent.ses_a":    false,
		"notifications.envoy.exceptions.>":                            false,
		"notifications.envoy.exceptions.notifications.dispatch.issue": false,
		"notifications.legion.ACME.controller":                        false,
		"notifications.role.reviewer":                                 false,
		"notifications.team.x.y":                                      false,
		"notifications.dispatch":                                      false,
		"notifications.*.acme.widgets.pr.7":                           false,
		"notifications.*":                                             false,
		"notifications.>":                                             false,
		">":                                                           false,
	} {
		if got := SharedStreamTopic(topic); got != want {
			t.Errorf("SharedStreamTopic(%q) = %v, want %v", topic, got, want)
		}
	}
}

// The legacy notifications.> catch-all captured the role lanes, so it is the one deployed subject
// start-up replaces outright with the binary's own list.
func TestEnsureStreamWithConfigReplacesTheLegacyRoleLaneCatchAll(t *testing.T) {
	oldConfig := *streamCfg
	oldConfig.Subjects = []string{"notifications.>"}
	js := &streamInfoJetStream{config: oldConfig}
	desiredConfig := oldConfig
	desiredConfig.Subjects = []string{"notifications.github.>"}

	if err := ensureStreamWithConfig(js, &desiredConfig); err != nil {
		t.Fatalf("ensure stream: %v", err)
	}
	if !reflect.DeepEqual(js.config.Subjects, desiredConfig.Subjects) {
		t.Fatalf("stream subjects = %v, want %v", js.config.Subjects, desiredConfig.Subjects)
	}
}

// Several deployments ensure one stream: the listener and Dispatch run from one image but deploy
// separately, and a rollback or a restart during a rollout starts a binary compiled with a
// different subject list. Whichever starts must leave every subject the other still needs.
func TestConnectOwningStreamKeepsTheSubjectsAnotherDeploymentOfTheStreamNeeds(t *testing.T) {
	uri := testnats.URL(t)
	// The other deployment was compiled before notifications.legion.> existed and carries a
	// subject this binary does not know.
	other := *streamCfg
	other.Subjects = []string{"notifications.retired.>"}
	for _, subject := range streamCfg.Subjects {
		if subject != "notifications.legion.>" {
			other.Subjects = append(other.Subjects, subject)
		}
	}
	otherConn := testnats.Connect(t, uri)
	t.Cleanup(otherConn.Close)
	otherJS, err := otherConn.JetStream()
	if err != nil {
		t.Fatalf("open the other deployment's JetStream: %v", err)
	}
	if err := ensureStreamWithConfig(otherJS, &other); err != nil {
		t.Fatalf("the other deployment creates the stream: %v", err)
	}

	client, err := ConnectOwningStream([]string{uri})
	if err != nil {
		t.Fatalf("this binary connects: %v", err)
	}
	t.Cleanup(client.Close)
	assertStreamSubjectsInclude(t, client.JS(), append([]string{"notifications.retired.>"}, streamCfg.Subjects...))

	// The other deployment restarts while this binary is still live.
	if err := ensureStreamWithConfig(otherJS, &other); err != nil {
		t.Fatalf("the other deployment restarts: %v", err)
	}
	assertStreamSubjectsInclude(t, client.JS(), append([]string{"notifications.retired.>"}, streamCfg.Subjects...))
	notice := contracts.Envelope{
		EventID:        "notice-skew",
		Source:         "agent",
		SourceEventID:  "notice-skew",
		Topic:          "notifications.legion.omp.LEGION-208",
		DedupeKey:      "legion-outbox:skew",
		IssuedAt:       contracts.NowMillis(),
		PayloadSummary: "notice published after the other deployment restarted",
		TraceID:        "notice-skew",
	}
	if err := client.Publish(notice); err != nil {
		t.Fatalf("publish a Legion notice after the other deployment restarted: %v", err)
	}
}

func assertStreamSubjectsInclude(t *testing.T, js nats.JetStreamContext, want []string) {
	t.Helper()
	info, err := js.StreamInfo(Stream)
	if err != nil {
		t.Fatalf("read stream: %v", err)
	}
	for _, subject := range want {
		if !slices.Contains(info.Config.Subjects, subject) {
			t.Fatalf("stream subjects = %v, missing %q", info.Config.Subjects, subject)
		}
	}
}

// JetStream refuses two overlapping subjects in one stream. When one deployment widens, narrows or
// splits a subject another deployment still holds, each start (and a rollback's) must still
// succeed, with the starting binary's shape of that subject in the stream.
//
// Each subtest takes its own testnats.URL, inside the subtest, so each runs in an account of its
// own. Its reset deletes Stream: in a fresh account there is nothing to delete, but in one account
// shared by both, the second subtest's reset would delete the stream the first left, emptying the
// account, and its recreate of that name would race the background removal of the account's
// streams directory that nats-server schedules on every stream delete and that succeeds once that
// directory is empty. The deleted stream's own directory is not what a recreate races:
// nats-server renames it aside and removes the renamed copy, which a recreate never meets.
func TestEnsureStreamWithConfigStartsWhenADeployedSubjectOverlapsItsOwn(t *testing.T) {
	shaped := func(legion string) nats.StreamConfig {
		cfg := *streamCfg
		cfg.Subjects = nil
		for _, subject := range streamCfg.Subjects {
			if subject == "notifications.legion.>" {
				subject = legion
			}
			cfg.Subjects = append(cfg.Subjects, subject)
		}
		return cfg
	}
	wide, narrow := shaped("notifications.legion.>"), shaped("notifications.legion.*.*")

	for _, tc := range []struct {
		name            string
		deployed, start nats.StreamConfig
	}{
		{name: "widen", deployed: narrow, start: wide},
		{name: "narrow", deployed: wide, start: narrow},
	} {
		t.Run(tc.name, func(t *testing.T) {
			uri := testnats.URL(t)
			conn := testnats.Connect(t, uri)
			t.Cleanup(conn.Close)
			js, err := conn.JetStream()
			if err != nil {
				t.Fatalf("open JetStream: %v", err)
			}
			if err := js.DeleteStream(Stream); err != nil && !errors.Is(err, nats.ErrStreamNotFound) {
				t.Fatalf("reset stream: %v", err)
			}
			if err := ensureStreamWithConfig(js, &tc.deployed); err != nil {
				t.Fatalf("the deployed binary creates the stream: %v", err)
			}
			for _, step := range []struct {
				name string
				cfg  nats.StreamConfig
			}{
				{name: "the new binary starts", cfg: tc.start},
				{name: "the rollback starts", cfg: tc.deployed},
				{name: "the new binary starts again", cfg: tc.start},
			} {
				if err := ensureStreamWithConfig(js, &step.cfg); err != nil {
					t.Fatalf("%s: %v", step.name, err)
				}
				info, err := js.StreamInfo(Stream)
				if err != nil {
					t.Fatalf("%s: read stream: %v", step.name, err)
				}
				got, want := slices.Clone(info.Config.Subjects), slices.Clone(step.cfg.Subjects)
				slices.Sort(got)
				slices.Sort(want)
				if !slices.Equal(got, want) {
					t.Fatalf("%s: stream subjects = %v, want the starting binary's %v", step.name, info.Config.Subjects, step.cfg.Subjects)
				}
			}
		})
	}
}

// capturedLogs collects the default slog logger's JSON records for one test. Its writes and reads
// share a lock because a bus client an earlier test closed can still be logging: nats.go's close
// queues the disconnected and closed callbacks on the connection's async dispatcher and returns
// without waiting for them (nats.go v1.50.0, nats.go:5892-5910), the bus's closed callback logs
// through the default logger, and every test of the package shares one binary.
type capturedLogs struct {
	mu     sync.Mutex
	buffer bytes.Buffer
}

func (l *capturedLogs) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buffer.Write(p)
}

// String is a snapshot of the records logged so far; nothing reads the buffer another way.
func (l *capturedLogs) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buffer.String()
}

// records decodes every JSON record logged so far.
func (l *capturedLogs) records(t *testing.T) []map[string]any {
	t.Helper()
	var records []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(l.String()), "\n") {
		if line == "" {
			continue
		}
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("decode log line %q: %v", line, err)
		}
		records = append(records, record)
	}
	return records
}

// recordsWithMsg decodes every JSON record logged so far whose msg is message.
func (l *capturedLogs) recordsWithMsg(t *testing.T, message string) []map[string]any {
	t.Helper()
	var records []map[string]any
	for _, record := range l.records(t) {
		if record["msg"] == message {
			records = append(records, record)
		}
	}
	return records
}

// captureLogs points the default slog logger at a JSON handler writing into the returned logs
// until the test ends. A test that calls it must not run in parallel: the default is the process's.
func captureLogs(t *testing.T) *capturedLogs {
	t.Helper()
	logs := &capturedLogs{}
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return logs
}

// Every subject that can capture a role lane leaves the stream, whichever role it names: the role
// lanes travel over core NATS and a retained copy would be delivered twice.
func TestEnsureStreamWithConfigDropsEveryRoleLaneSubject(t *testing.T) {
	for _, roleSubject := range []string{
		"notifications.role.sre",
		"notifications.role.*",
		"notifications.envoy.exceptions.notifications.role.sre",
	} {
		t.Run(roleSubject, func(t *testing.T) {
			deployed := *streamCfg
			deployed.Subjects = append(slices.Clone(streamCfg.Subjects), roleSubject)
			js := &streamInfoJetStream{config: deployed}
			if err := ensureStreamWithConfig(js, streamCfg); err != nil {
				t.Fatalf("ensure stream: %v", err)
			}
			if slices.Contains(js.config.Subjects, roleSubject) {
				t.Fatalf("stream subjects = %v, still capturing the role lane %q", js.config.Subjects, roleSubject)
			}
			if !slices.Contains(js.purgedSubjects, "notifications.role.>") {
				t.Fatalf("purged subjects = %v, want the role lanes purged", js.purgedSubjects)
			}
		})
	}
}

// A deployed subject split into several of the binary's own names every one it was replaced by.
func TestEnsureStreamWithConfigNamesEverySubjectThatReplacedAnOverlappingOne(t *testing.T) {
	logs := captureLogs(t)
	deployed := *streamCfg
	deployed.Subjects = []string{"notifications.agent.>", "notifications.legion.>"}
	own := *streamCfg
	own.Subjects = []string{"notifications.agent.>", "notifications.legion.*", "notifications.legion.*.*"}
	js := &streamInfoJetStream{config: deployed}
	if err := ensureStreamWithConfig(js, &own); err != nil {
		t.Fatalf("ensure stream: %v", err)
	}
	records := logs.recordsWithMsg(t, "envoy nats stream subject replaced by an overlapping one")
	if len(records) != 1 {
		t.Fatalf("replacement records = %v, want one for notifications.legion.>", records)
	}
	if records[0]["dropped"] != "notifications.legion.>" {
		t.Fatalf("dropped = %v, want notifications.legion.>", records[0]["dropped"])
	}
	kept, _ := records[0]["kept"].([]any)
	if !reflect.DeepEqual(kept, []any{"notifications.legion.*", "notifications.legion.*.*"}) {
		t.Fatalf("kept = %v, want both subjects that replaced it", records[0]["kept"])
	}
}

// A start that keeps deployed subjects it does not compile names them, so the retire step can find
// them; it names them on every start that keeps them, whether or not the stream changed.
func TestEnsureStreamWithConfigNamesTheDeployedSubjectsItKeepsWithoutCompilingThem(t *testing.T) {
	logs := captureLogs(t)
	deployed := *streamCfg
	deployed.Subjects = append(slices.Clone(streamCfg.Subjects), "notifications.retired.>")
	js := &streamInfoJetStream{config: deployed}
	for _, start := range []string{"the stream is already reconciled", "a second start"} {
		if err := ensureStreamWithConfig(js, streamCfg); err != nil {
			t.Fatalf("%s: ensure stream: %v", start, err)
		}
	}
	records := logs.recordsWithMsg(t, "envoy nats stream keeps subjects this binary does not compile")
	if len(records) != 2 {
		t.Fatalf("records = %v, want one per start", records)
	}
	for _, record := range records {
		if subjects, _ := record["subjects"].([]any); !reflect.DeepEqual(subjects, []any{"notifications.retired.>"}) {
			t.Fatalf("subjects = %v, want [notifications.retired.>]", record["subjects"])
		}
	}
}

// Nothing is reported as replaced or kept when the update that would install it fails.
func TestEnsureStreamWithConfigLogsNoReconciliationWhenTheUpdateFails(t *testing.T) {
	logs := captureLogs(t)
	deployed := *streamCfg
	deployed.Subjects = []string{"notifications.retired.>", "notifications.legion.>"}
	own := *streamCfg
	own.Subjects = []string{"notifications.legion.*"}
	js := &streamInfoJetStream{config: deployed, updateErr: errors.New("update refused")}
	if err := ensureStreamWithConfig(js, &own); err == nil {
		t.Fatal("ensure stream succeeded, want the update's error")
	}
	for _, message := range []string{
		"envoy nats stream subject replaced by an overlapping one",
		"envoy nats stream keeps subjects this binary does not compile",
	} {
		if records := logs.recordsWithMsg(t, message); len(records) != 0 {
			t.Fatalf("%q logged %v although the update failed", message, records)
		}
	}
}

// streamTestJetStream is a JetStream context in an account of the test's own, with no stream yet.
func streamTestJetStream(t *testing.T) nats.JetStreamContext {
	t.Helper()
	conn := testnats.Connect(t, testnats.URL(t))
	t.Cleanup(conn.Close)
	js, err := conn.JetStream()
	if err != nil {
		t.Fatalf("open JetStream: %v", err)
	}
	return js
}

func deployedStream(t *testing.T, js nats.JetStreamContext) *nats.StreamInfo {
	t.Helper()
	info, err := js.StreamInfo(Stream)
	if err != nil {
		t.Fatalf("read stream: %v", err)
	}
	return info
}

// A stream a start creates carries an id of its own: two streams, each in an account of its own,
// get different ones.
func TestEnsureStreamWithConfigCreatesTheStreamWithAFreshID(t *testing.T) {
	var ids []string
	for _, name := range []string{"first stream", "second stream"} {
		t.Run(name, func(t *testing.T) {
			js := streamTestJetStream(t)
			if err := ensureStreamWithConfig(js, streamCfg); err != nil {
				t.Fatalf("create stream: %v", err)
			}
			id := deployedStream(t, js).Config.Metadata[StreamIDMetadataKey]
			if len(id) != 32 || strings.Trim(id, "0123456789abcdef") != "" {
				t.Fatalf("stream id = %q, want 32 hex digits", id)
			}
			ids = append(ids, id)
		})
	}
	if len(ids) == 2 && ids[0] == ids[1] {
		t.Fatalf("both streams got id %q", ids[0])
	}
}

// A deployed stream without an id gets one on the next start, even when nothing else about it
// differs from the binary's configuration, and keeps the rest of its configuration and metadata:
// settings the binary's configuration does not choose (here the message limits and a description;
// in a cluster, the stream's placement) stay as deployed, and so do the ones it would only keep (a
// byte limit, with no limit of its own set) or already matches (DiscardOld, the replicas).
func TestEnsureStreamWithConfigGivesADeployedStreamWithoutAnIDOne(t *testing.T) {
	js := streamTestJetStream(t)
	deployed := *streamCfg
	deployed.Description = "deployed by operations"
	deployed.MaxMsgs = 100_000
	deployed.MaxBytes = 64 << 20
	deployed.MaxMsgsPerSubject = 1_000
	deployed.Metadata = map[string]string{"owner": "operations"}
	if _, err := js.AddStream(&deployed); err != nil {
		t.Fatalf("create the stream without an id: %v", err)
	}
	before := deployedStream(t, js).Config

	if err := ensureStreamWithConfig(js, streamCfg); err != nil {
		t.Fatalf("ensure stream: %v", err)
	}

	info := deployedStream(t, js)
	id := info.Config.Metadata[StreamIDMetadataKey]
	if id == "" {
		t.Fatalf("stream metadata = %v, want an id", info.Config.Metadata)
	}
	want := before
	want.Metadata = maps.Clone(before.Metadata)
	want.Metadata[StreamIDMetadataKey] = id
	if !reflect.DeepEqual(info.Config, want) {
		t.Fatalf("stream config = %+v, want the deployed one with only the id added: %+v", info.Config, want)
	}
	if err := ensureStreamWithConfig(js, streamCfg); err != nil {
		t.Fatalf("second start: %v", err)
	}
	if again := deployedStream(t, js).Config.Metadata[StreamIDMetadataKey]; again != id {
		t.Fatalf("stream id = %q after a second start, want %q", again, id)
	}
}

// A start that changes the deployed stream's subjects and retention writes the stream's whole
// configuration, and keeps its id and the rest of its metadata in it.
func TestEnsureStreamWithConfigKeepsTheIDAndMetadataWhenItReconcilesTheStream(t *testing.T) {
	js := streamTestJetStream(t)
	deployed := *streamCfg
	deployed.MaxAge = time.Hour
	deployed.Duplicates = time.Hour
	deployed.Subjects = slices.DeleteFunc(slices.Clone(streamCfg.Subjects), func(subject string) bool {
		return subject == "notifications.legion.>"
	})
	deployed.Metadata = map[string]string{StreamIDMetadataKey: "deployed-id", "owner": "operations"}
	if _, err := js.AddStream(&deployed); err != nil {
		t.Fatalf("create the deployed stream: %v", err)
	}

	if err := ensureStreamWithConfig(js, streamCfg); err != nil {
		t.Fatalf("ensure stream: %v", err)
	}

	info := deployedStream(t, js)
	if info.Config.MaxAge != streamCfg.MaxAge || !slices.Contains(info.Config.Subjects, "notifications.legion.>") {
		t.Fatalf("stream config = %+v, want the binary's retention and subjects", info.Config)
	}
	if info.Config.Metadata[StreamIDMetadataKey] != "deployed-id" || info.Config.Metadata["owner"] != "operations" {
		t.Fatalf("stream metadata = %v, want the id deployed-id and owner kept", info.Config.Metadata)
	}
}

// The id a start gives the stream is never replaced: not by the same binary's next start, not by
// another deployment's start that changes the stream's subjects, and not by a start that found no
// stream and created it after another start already had.
func TestEnsureStreamWithConfigNeverReplacesTheStreamsID(t *testing.T) {
	uri := testnats.URL(t)
	client, err := ConnectOwningStream([]string{uri})
	if err != nil {
		t.Fatalf("this binary connects: %v", err)
	}
	t.Cleanup(client.Close)
	id := deployedStream(t, client.JS()).Config.Metadata[StreamIDMetadataKey]
	if id == "" {
		t.Fatal("the created stream has no id")
	}

	otherConn := testnats.Connect(t, uri)
	t.Cleanup(otherConn.Close)
	otherJS, err := otherConn.JetStream()
	if err != nil {
		t.Fatalf("open the other deployment's JetStream: %v", err)
	}
	other := *streamCfg
	other.Subjects = append(slices.Clone(streamCfg.Subjects), "notifications.retired.>")
	if err := ensureStreamWithConfig(otherJS, &other); err != nil {
		t.Fatalf("the other deployment starts: %v", err)
	}
	if got := deployedStream(t, otherJS).Config.Metadata[StreamIDMetadataKey]; got != id {
		t.Fatalf("stream id = %q after the other deployment's start, want %q", got, id)
	}

	restarted, err := ConnectOwningStream([]string{uri})
	if err != nil {
		t.Fatalf("this binary restarts: %v", err)
	}
	t.Cleanup(restarted.Close)
	if got := deployedStream(t, restarted.JS()).Config.Metadata[StreamIDMetadataKey]; got != id {
		t.Fatalf("stream id = %q after a restart, want %q", got, id)
	}

	if err := ensureStreamWithConfig(&createdMeanwhileJetStream{JetStreamContext: otherJS}, streamCfg); err != nil {
		t.Fatalf("a start that found no stream before another created it: %v", err)
	}
	if got := deployedStream(t, otherJS).Config.Metadata[StreamIDMetadataKey]; got != id {
		t.Fatalf("stream id = %q after a start that found no stream, want %q", got, id)
	}
}

// createdMeanwhileJetStream answers its first StreamInfo with no stream, as a start sees it when
// another start creates the stream between this one's read and its create.
type createdMeanwhileJetStream struct {
	nats.JetStreamContext
	answered bool
}

func (js *createdMeanwhileJetStream) StreamInfo(stream string, opts ...nats.JSOpt) (*nats.StreamInfo, error) {
	if !js.answered {
		js.answered = true
		return nil, nats.ErrStreamNotFound
	}
	return js.JetStreamContext.StreamInfo(stream, opts...)
}

// staleStreamInfoJetStream answers every StreamInfo with the stream as it stood before any update,
// as two starts see it when both read the stream before either writes.
type staleStreamInfoJetStream struct {
	nats.JetStreamContext
	read    nats.StreamInfo
	updates []nats.StreamConfig
}

func (js *staleStreamInfoJetStream) StreamInfo(_ string, _ ...nats.JSOpt) (*nats.StreamInfo, error) {
	info := js.read
	return &info, nil
}

func (js *staleStreamInfoJetStream) UpdateStream(cfg *nats.StreamConfig, _ ...nats.JSOpt) (*nats.StreamInfo, error) {
	js.updates = append(js.updates, *cfg)
	return &nats.StreamInfo{Config: *cfg}, nil
}

// Two starts that both find the deployed stream without an id, before either writes one, write the
// same id, so the later write does not replace the earlier one.
func TestEnsureStreamWithConfigRacingStartsWriteOneID(t *testing.T) {
	js := &staleStreamInfoJetStream{read: nats.StreamInfo{
		Config:  *streamCfg,
		Created: time.Date(2026, 10, 27, 9, 30, 0, 123456789, time.UTC),
	}}
	for _, start := range []string{"the listener", "Dispatch's server"} {
		if err := ensureStreamWithConfig(js, streamCfg); err != nil {
			t.Fatalf("%s starts: %v", start, err)
		}
	}
	if len(js.updates) != 2 {
		t.Fatalf("updates = %d, want one per start", len(js.updates))
	}
	first, second := js.updates[0].Metadata[StreamIDMetadataKey], js.updates[1].Metadata[StreamIDMetadataKey]
	if first == "" || first != second {
		t.Fatalf("racing starts wrote ids %q and %q, want one id", first, second)
	}
}
