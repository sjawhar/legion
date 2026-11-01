package bus

import (
	"slices"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/testnats"
)

// generationKey is the stream metadata key that holds the generation of the stream's settings,
// spelled out rather than read from streamGenerationKey: every build reads the one key, so a build
// that renamed it would not see the generation another build stamped.
const generationKey = "dispatch.stream-generation"

// rawJetStream is a JetStream context on a plain connection to url, which ensures nothing: the
// test plays another build's process with it.
func rawJetStream(t *testing.T, url string) nats.JetStreamContext {
	t.Helper()
	conn, err := nats.Connect(url, nats.Timeout(5*time.Second))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(conn.Close)
	js, err := conn.JetStream(nats.MaxWait(10 * time.Second))
	if err != nil {
		t.Fatalf("open JetStream: %v", err)
	}
	return js
}

// A stream whose settings a newer build stamped keeps every one of them when a client of this
// build starts, whether it names a replica count (the listener) or not (Dispatch): the older build
// does not revert what the newer one set.
func TestAnOwningClientLeavesAStreamANewerGenerationStampedUntouched(t *testing.T) {
	urls := startJetStreamCluster(t)
	js := rawJetStream(t, urls[0])
	newer := nats.StreamConfig{
		Name:       Stream,
		Subjects:   []string{"notifications.github.>", "notifications.newer.>"},
		Retention:  nats.LimitsPolicy,
		MaxAge:     time.Hour,
		Duplicates: time.Minute,
		Storage:    nats.FileStorage,
		Replicas:   clusterSize,
		Metadata:   map[string]string{generationKey: "2"},
	}
	if _, err := js.AddStream(&newer); err != nil {
		t.Fatalf("the newer build creates the stream: %v", err)
	}
	for _, tc := range []struct {
		name    string
		options []ConnectOption
	}{
		{"a client that names its replicas", []ConnectOption{WithReplicas(1)}},
		{"a client that names none", []ConnectOption{WithName("dispatch")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client, err := ConnectOwningStream(urls, tc.options...)
			if err != nil {
				t.Fatalf("connect: %v", err)
			}
			t.Cleanup(client.Close)
			got := deployedStream(t, js).Config
			if got.MaxAge != newer.MaxAge || got.Duplicates != newer.Duplicates ||
				!slices.Equal(got.Subjects, newer.Subjects) || got.Replicas != newer.Replicas {
				t.Fatalf("stream MaxAge=%s Duplicates=%s Subjects=%v Replicas=%d, want the newer build's MaxAge=%s Duplicates=%s Subjects=%v Replicas=%d",
					got.MaxAge, got.Duplicates, got.Subjects, got.Replicas, newer.MaxAge, newer.Duplicates, newer.Subjects, newer.Replicas)
			}
			if got.Metadata[generationKey] != "2" {
				t.Fatalf("stream generation = %q, want the newer build's 2", got.Metadata[generationKey])
			}
		})
	}
}

// A stream without a generation (a build before generations) or with an older one is reconciled
// and stamped with this build's generation.
func TestAnOwningClientStampsItsGenerationOnAnUnstampedStream(t *testing.T) {
	uri := testnats.URL(t)
	js := rawJetStream(t, uri)
	unstamped := *streamCfg
	unstamped.Replicas = 1
	if _, err := js.AddStream(&unstamped); err != nil {
		t.Fatalf("the previous build creates the stream: %v", err)
	}
	client, err := ConnectOwningStream([]string{uri})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(client.Close)
	if got := deployedStream(t, js).Config.Metadata[generationKey]; got != "1" {
		t.Fatalf("stream generation = %q, want 1", got)
	}
}

// A stream deleted while an owning client runs is created again when the client reconnects, so a
// stream lost while a process runs does not stay lost until the process restarts.
func TestAStreamDeletedWhileTheClientRunsIsRecreatedOnItsReconnect(t *testing.T) {
	srv, uri := testnats.Start(t)
	client, err := ConnectOwningStream([]string{uri})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(client.Close)
	admin := testnats.Connect(t, uri)
	adminJS, err := admin.JetStream()
	if err != nil {
		t.Fatalf("open JetStream: %v", err)
	}
	if err := adminJS.DeleteStream(Stream); err != nil {
		t.Fatalf("delete the stream: %v", err)
	}
	admin.Close()

	srv.Stop(t)
	srv.Start(t)

	checker := testnats.Connect(t, uri)
	t.Cleanup(checker.Close)
	checkerJS, err := checker.JetStream()
	if err != nil {
		t.Fatalf("open JetStream: %v", err)
	}
	deadline := time.Now().Add(20 * time.Second)
	for {
		info, err := checkerJS.StreamInfo(Stream)
		if err == nil {
			if !slices.Equal(info.Config.Subjects, streamSubjects) {
				t.Fatalf("recreated stream subjects = %v, want %v", info.Config.Subjects, streamSubjects)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the stream was not recreated after the client reconnected: %v", err)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// A topic only a newer build's subjects capture (notifications.x.>, which this build does not
// compile) is published through JetStream with its MsgId once the deployed stream captures it, by
// a client that owns the stream and by one that does not: the stream keeps one copy and reports
// the repeat a duplicate.
func TestATopicOnlyTheDeployedStreamCapturesIsPublishedThroughJetStream(t *testing.T) {
	const topic = "notifications.x.y"
	if StreamCaptures(topic) {
		t.Fatalf("this build compiles a subject capturing %s; the test needs one it does not", topic)
	}
	for _, tc := range []struct {
		name    string
		connect func(urls []string, options ...ConnectOption) (*Client, error)
	}{
		{"an owning client", ConnectOwningStream},
		{"a client that owns no stream", Connect},
	} {
		t.Run(tc.name, func(t *testing.T) {
			uri := testnats.URL(t)
			js := rawJetStream(t, uri)
			newer := *streamCfg
			newer.Replicas = 1
			newer.Subjects = append(slices.Clone(streamSubjects), "notifications.x.>")
			newer.Metadata = map[string]string{generationKey: "1"}
			if _, err := js.AddStream(&newer); err != nil {
				t.Fatalf("the newer build creates the stream: %v", err)
			}
			client, err := tc.connect([]string{uri})
			if err != nil {
				t.Fatalf("connect: %v", err)
			}
			t.Cleanup(client.Close)
			envelope := contracts.Envelope{
				EventID:        "evt-newer-subject",
				Source:         "dispatch",
				Topic:          topic,
				DedupeKey:      "dispatch.newer-subject.1",
				IssuedAt:       contracts.NowMillis(),
				PayloadSummary: "a topic only the newer build's subjects capture",
				TraceID:        "trace-newer-subject",
			}
			for attempt, want := range []bool{false, true} {
				duplicate, err := client.PublishReportingDuplicate(envelope)
				if err != nil {
					t.Fatalf("publish %d: %v", attempt+1, err)
				}
				if duplicate != want {
					t.Fatalf("publish %d reported duplicate=%t, want %t", attempt+1, duplicate, want)
				}
			}
			msg, err := js.GetLastMsg(Stream, topic)
			if err != nil {
				t.Fatalf("the stream holds nothing on %s: %v", topic, err)
			}
			if got, want := msg.Header.Get(nats.MsgIdHdr), envelope.DedupeKey+":"+topic; got != want {
				t.Fatalf("stored MsgId = %q, want %q", got, want)
			}
			info, err := js.StreamInfo(Stream, &nats.StreamInfoRequest{SubjectsFilter: topic})
			if err != nil {
				t.Fatalf("read the stream: %v", err)
			}
			if got := info.State.Subjects[topic]; got != 1 {
				t.Fatalf("the stream holds %d messages on %s, want 1", got, topic)
			}
		})
	}
}

// A client follows a subject a newer build adds while the client runs: it reads the deployed
// subjects again every streamSubjectsRefresh, whether it owns the stream or not, and then publishes
// a topic under the new subject through JetStream with its MsgId.
func TestAClientFollowsASubjectANewerBuildAddsWhileItRuns(t *testing.T) {
	const topic = "notifications.x.y"
	for _, tc := range []struct {
		name    string
		connect func(urls []string, options ...ConnectOption) (*Client, error)
	}{
		{"an owning client", ConnectOwningStream},
		{"a client that owns no stream", Connect},
	} {
		t.Run(tc.name, func(t *testing.T) {
			uri := testnats.URL(t)
			js := rawJetStream(t, uri)
			current := *streamCfg
			current.Replicas = 1
			current.Metadata = map[string]string{generationKey: "1"}
			if _, err := js.AddStream(&current); err != nil {
				t.Fatalf("create the stream: %v", err)
			}
			client, err := tc.connect([]string{uri}, withStreamSubjectsRefresh(50*time.Millisecond))
			if err != nil {
				t.Fatalf("connect: %v", err)
			}
			t.Cleanup(client.Close)
			if !client.UsesCoreTransport(topic) {
				t.Fatalf("%s takes JetStream before any stream captures it", topic)
			}

			newer := current
			newer.Subjects = append(slices.Clone(streamSubjects), "notifications.x.>")
			newer.Metadata = map[string]string{generationKey: "2"}
			if _, err := js.UpdateStream(&newer); err != nil {
				t.Fatalf("the newer build adds its subject: %v", err)
			}
			deadline := time.Now().Add(10 * time.Second)
			for client.UsesCoreTransport(topic) {
				if time.Now().After(deadline) {
					t.Fatalf("the client still sends %s over core NATS 10 s after the stream captured it", topic)
				}
				time.Sleep(20 * time.Millisecond)
			}
			envelope := contracts.Envelope{
				EventID:        "evt-followed-subject",
				Source:         "dispatch",
				Topic:          topic,
				DedupeKey:      "dispatch.followed-subject.1",
				IssuedAt:       contracts.NowMillis(),
				PayloadSummary: "a topic under a subject added while the client ran",
				TraceID:        "trace-followed-subject",
			}
			for attempt, want := range []bool{false, true} {
				duplicate, err := client.PublishReportingDuplicate(envelope)
				if err != nil {
					t.Fatalf("publish %d: %v", attempt+1, err)
				}
				if duplicate != want {
					t.Fatalf("publish %d reported duplicate=%t, want %t", attempt+1, duplicate, want)
				}
			}
		})
	}
}

// A client that connects in the background (WithConnectInBackground) while its server is down
// reads the deployed stream's subjects at its first connection, as a client that waits reads them
// at connect: an owning client through its ensure of the stream, any other through its read. A
// topic under a subject only a newer build added then takes JetStream at once, without waiting a
// streamSubjectsRefresh.
func TestABackgroundClientReadsTheDeployedSubjectsAtItsFirstConnection(t *testing.T) {
	const topic = "notifications.x.y"
	for _, tc := range []struct {
		name    string
		connect func(urls []string, options ...ConnectOption) (*Client, error)
	}{
		{"an owning client", ConnectOwningStream},
		{"a client that owns no stream", Connect},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, uri := testnats.Start(t)
			js := rawJetStream(t, uri)
			newer := *streamCfg
			newer.Replicas = 1
			newer.Subjects = append(slices.Clone(streamSubjects), "notifications.x.>")
			newer.Metadata = map[string]string{generationKey: "2"}
			if _, err := js.AddStream(&newer); err != nil {
				t.Fatalf("the newer build creates the stream: %v", err)
			}
			srv.Stop(t)

			client, err := tc.connect([]string{uri}, WithConnectInBackground(), withStreamSubjectsRefresh(time.Hour))
			if err != nil {
				t.Fatalf("connect while the server is down: %v", err)
			}
			t.Cleanup(client.Close)
			if !client.UsesCoreTransport(topic) {
				t.Fatalf("%s takes JetStream before the client has read any stream", topic)
			}

			srv.Start(t)
			deadline := time.Now().Add(15 * time.Second)
			for client.UsesCoreTransport(topic) {
				if time.Now().After(deadline) {
					t.Fatalf("the client still sends %s over core NATS 15 s after the server came up", topic)
				}
				time.Sleep(20 * time.Millisecond)
			}
		})
	}
}
