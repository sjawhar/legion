package bus

import (
	"context"
	"net"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/testnats"
	tcnats "github.com/testcontainers/testcontainers-go/modules/nats"
)

type streamInfoJetStream struct {
	nats.JetStreamContext
	config                    nats.StreamConfig
	purgedSubjects            []string
	roleMessages              int
	injectRoleMessageOnUpdate bool
}

func (js *streamInfoJetStream) StreamInfo(_ string, _ ...nats.JSOpt) (*nats.StreamInfo, error) {
	return &nats.StreamInfo{Config: js.config}, nil
}

func (js *streamInfoJetStream) UpdateStream(cfg *nats.StreamConfig, _ ...nats.JSOpt) (*nats.StreamInfo, error) {
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
		conn, err := connectWithContext(ctx, "deadline-test", urls, nil, nil)
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
	ctr, err := tcnats.Run(baseCtx, "nats:2.10")
	if err != nil {
		t.Fatalf("start NATS: %v", err)
	}
	t.Cleanup(func() { _ = ctr.Terminate(baseCtx) })
	uri, err := ctr.ConnectionString(baseCtx)
	if err != nil {
		t.Fatalf("NATS connection string: %v", err)
	}

	connectCtx, cancel := context.WithTimeout(baseCtx, time.Second)
	defer cancel()
	reconnected := make(chan struct{}, 1)
	conn, err := connectWithContext(connectCtx, "reconnect-dialer-test", []string{uri}, func(*nats.Conn) {
		reconnected <- struct{}{}
	}, nil)
	if err != nil {
		t.Fatalf("connect with deadline: %v", err)
	}
	defer conn.Close()
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

func TestConnectPurgesLegacyRoleMessagesBeforeDurableConsumerRestart(t *testing.T) {
	ctx := context.Background()
	ctr, err := tcnats.Run(ctx, "nats:2.10")
	if err != nil {
		t.Fatalf("start NATS: %v", err)
	}
	t.Cleanup(func() { _ = ctr.Terminate(ctx) })
	uri, err := ctr.ConnectionString(ctx)
	if err != nil {
		t.Fatalf("NATS connection string: %v", err)
	}
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

	client, err := Connect([]string{uri})
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
		"notifications.github.>",
		"notifications.slack.>",
		"notifications.ghostwispr.>",
		"notifications.whatsapp.>",
		"notifications.envoy.exceptions.notifications.agent.>",
	}
	if !reflect.DeepEqual(streamCfg.Subjects, wantSubjects) {
		t.Fatalf("stream subjects = %v, want %v", streamCfg.Subjects, wantSubjects)
	}
	for _, roleSubject := range []string{
		"notifications.role.legion-controller",
		"notifications.envoy.exceptions.notifications.role.legion-controller",
	} {
		for _, streamSubject := range streamCfg.Subjects {
			if streamSubjectMatches(streamSubject, roleSubject) {
				t.Fatalf("stream subject %q captures role lane %q", streamSubject, roleSubject)
			}
		}
	}
}

func TestEnsureStreamWithConfigUpdatesSubjectsWhenExistingStreamDiffers(t *testing.T) {
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
