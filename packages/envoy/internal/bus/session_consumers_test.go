package bus

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/testnats"
)

func TestReduceFilterSubjects(t *testing.T) {
	for _, tc := range []struct {
		name     string
		subjects []string
		want     []string
	}{
		{name: "nothing", subjects: nil, want: nil},
		{name: "a duplicate is kept once", subjects: []string{"a.b", "a.b"}, want: []string{"a.b"}},
		{
			name:     "a subject and everything under it do not overlap",
			subjects: []string{"notifications.github.x.>", "notifications.github.x"},
			want:     []string{"notifications.github.x", "notifications.github.x.>"},
		},
		{
			name:     "an agent subject under every agent subject is dropped",
			subjects: []string{"notifications.agent.ses_a", "notifications.agent.>"},
			want:     []string{"notifications.agent.>"},
		},
		{
			name:     "a single-token wildcard contains its literal",
			subjects: []string{"a.*", "a.b"},
			want:     []string{"a.*"},
		},
		{
			name:     "partial overlap becomes the common prefix and >",
			subjects: []string{"a.*.c", "a.b.*"},
			want:     []string{"a.>"},
		},
		{
			name:     "first tokens that differ leave > alone",
			subjects: []string{"*.b", "a.*"},
			want:     []string{">"},
		},
		{
			name:     "a merge that contains another subject drops it",
			subjects: []string{"a.x", "a.*.c", "a.b.*"},
			want:     []string{"a.>"},
		},
		{
			name:     "merges repeat until no two overlap",
			subjects: []string{"a.b.*.d", "a.b.c.*", "a.*.x.y"},
			want:     []string{"a.>"},
		},
		{
			name:     "subjects that do not overlap stay, sorted",
			subjects: []string{"notifications.github.o.r.>", "notifications.agent.ses_a", "notifications.dispatch.issue.K-1.>"},
			want:     []string{"notifications.agent.ses_a", "notifications.dispatch.issue.K-1.>", "notifications.github.o.r.>"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := ReduceFilterSubjects(tc.subjects)
			if !slices.Equal(got, tc.want) {
				t.Fatalf("ReduceFilterSubjects(%q) = %q, want %q", tc.subjects, got, tc.want)
			}
			for i, a := range got {
				for _, b := range got[i+1:] {
					if subjectsOverlap(a, b) {
						t.Fatalf("ReduceFilterSubjects(%q) = %q, where %q and %q overlap", tc.subjects, got, a, b)
					}
				}
			}
		})
	}
}

func TestSessionConsumerName(t *testing.T) {
	if name, ok := SessionConsumerName("ses_abc-123"); !ok || name != "s-ses_abc-123" {
		t.Fatalf(`SessionConsumerName("ses_abc-123") = %q, %t, want "s-ses_abc-123", true`, name, ok)
	}
	for _, id := range []string{"", "a.b", "a*", "a>", "a b", "a\tb", "a\nb", "a/b", `a\b`} {
		if name, ok := SessionConsumerName(id); ok {
			t.Errorf("SessionConsumerName(%q) = %q, true; want no consumer", id, name)
		}
	}
}

// The relay subject travels over core NATS: no stream subject captures it, so a relay frame is
// never stored a second time.
func TestDeliverSubjectIsNotStored(t *testing.T) {
	subject := DeliverSubject("ses_abc")
	if subject != "notifications.envoy.deliver.ses_abc" {
		t.Fatalf("DeliverSubject = %q", subject)
	}
	if StreamCaptures(subject) {
		t.Fatalf("the stream captures %s", subject)
	}
}

// sessionConsumersOnStream connects a client that owns the stream, on a JetStream account of the
// test's own, and SessionConsumers beside it.
func sessionConsumersOnStream(t *testing.T) (*Client, *SessionConsumers) {
	t.Helper()
	uri := testnats.URL(t)
	owner, err := ConnectOwningStream([]string{uri})
	if err != nil {
		t.Fatalf("connect the stream's owner: %v", err)
	}
	t.Cleanup(owner.Close)
	consumers, err := ConnectSessionConsumers([]string{uri})
	if err != nil {
		t.Fatalf("connect session consumers: %v", err)
	}
	t.Cleanup(consumers.Close)
	return owner, consumers
}

// publishStored publishes body on subject through JetStream and returns its stream sequence.
func publishStored(t *testing.T, owner *Client, subject, body string) uint64 {
	t.Helper()
	ack, err := owner.JS().Publish(subject, []byte(body))
	if err != nil {
		t.Fatalf("publish %s: %v", subject, err)
	}
	return ack.Sequence
}

// pullSession starts a pull of sessionID's consumer, stopped when the test ends.
func pullSession(t *testing.T, consumers *SessionConsumers, sessionID string) *SessionPull {
	t.Helper()
	pull, err := consumers.Pull(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("pull %s: %v", sessionID, err)
	}
	t.Cleanup(pull.Stop)
	return pull
}

// next returns the next message pull holds within 5 s.
func next(t *testing.T, pull *SessionPull) SessionMessage {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	msg, err := pull.Next(ctx)
	if err != nil {
		t.Fatalf("next message: %v", err)
	}
	return msg
}

// requireNoMessage requires pull to hold no message for window.
func requireNoMessage(t *testing.T, pull *SessionPull, window time.Duration) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), window)
	defer cancel()
	if msg, err := pull.Next(ctx); err == nil {
		t.Fatalf("got message %d on %s (delivery %d), want none", msg.StreamSeq, msg.Subject, msg.Delivered)
	}
}

// A consumer as Ensure creates it carries the session consumer settings, its reduced filters, and
// replicas left to the stream's.
func TestEnsureCreatesTheConsumerWithTheSessionSettings(t *testing.T) {
	_, consumers := sessionConsumersOnStream(t)
	ctx := context.Background()
	start, err := consumers.Ensure(ctx, "ses_settings", []string{
		contracts.AgentSubject("ses_settings"), "notifications.github.o.r.>", "notifications.github.o.r.pull.*",
	}, 0)
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if start != (SessionConsumerStart{Created: true, StartSeq: 1}) {
		t.Fatalf("Ensure of a new consumer on an empty stream = %+v, want created at 1 with no gap", start)
	}
	info, err := consumers.Info(ctx, "ses_settings")
	if err != nil {
		t.Fatalf("info: %v", err)
	}
	config := info.Config
	if config.Durable != "s-ses_settings" {
		t.Errorf("durable = %q, want s-ses_settings", config.Durable)
	}
	if config.AckPolicy != jetstream.AckExplicitPolicy {
		t.Errorf("ack policy = %v, want explicit", config.AckPolicy)
	}
	// What DeliverNew delivers, from the sequence after the stream's last, with a start sequence the
	// ack floor reads from (ackFloor).
	if config.DeliverPolicy != jetstream.DeliverByStartSequencePolicy || config.OptStartSeq != 1 {
		t.Errorf("deliver policy = %v from %d, want by start sequence from 1", config.DeliverPolicy, config.OptStartSeq)
	}
	if config.AckWait != 60*time.Second {
		t.Errorf("ack wait = %s, want 60s", config.AckWait)
	}
	if config.MaxAckPending != 1024 {
		t.Errorf("max ack pending = %d, want 1024", config.MaxAckPending)
	}
	if config.MaxDeliver != -1 {
		t.Errorf("max deliver = %d, want -1 (unlimited)", config.MaxDeliver)
	}
	if config.InactiveThreshold != 72*time.Hour {
		t.Errorf("inactive threshold = %s, want 72h", config.InactiveThreshold)
	}
	if config.Replicas != 0 {
		t.Errorf("replicas = %d, want 0 (the stream's)", config.Replicas)
	}
	if want := []string{contracts.AgentSubject("ses_settings"), "notifications.github.o.r.>"}; !slices.Equal(config.FilterSubjects, want) {
		t.Errorf("filters = %q, want %q", config.FilterSubjects, want)
	}

	// The same settings again change nothing and create nothing.
	again, err := consumers.Ensure(ctx, "ses_settings", []string{contracts.AgentSubject("ses_settings"), "notifications.github.o.r.>"}, 0)
	if err != nil || again != (SessionConsumerStart{}) {
		t.Fatalf("Ensure of a consumer as it stands = %+v, %v; want nothing done", again, err)
	}
}

// A new consumer delivers what the stream stores from its creation on, and a restart at its floor
// delivers nothing the stream held before it, never the stream from sequence 1: not when it has
// delivered nothing, not when its first delivery is still unacked, though nats-server v2.10.29
// reports such a consumer's ack floor as 0, and not once everything it delivered is acked.
func TestRestartAtFloorNeverDeliversWhatTheStreamHeldBeforeTheConsumer(t *testing.T) {
	owner, consumers := sessionConsumersOnStream(t)
	ctx := context.Background()
	const session = "ses_fresh"
	agent := contracts.AgentSubject(session)
	filters := []string{agent, "notifications.dispatch.issue.K-1.>"}
	publishStored(t, owner, agent, "before 1")
	publishStored(t, owner, "notifications.github.o.r.push", "unfiltered")
	last := publishStored(t, owner, agent, "before 2")

	if start, err := consumers.Ensure(ctx, session, filters, 0); err != nil || start.StartSeq != last+1 {
		t.Fatalf("Ensure of a new consumer = %+v, %v; want a start at %d", start, err, last+1)
	}
	info, err := consumers.Info(ctx, session)
	if err != nil {
		t.Fatalf("info: %v", err)
	}
	t.Logf("the server reports a new consumer's ack floor as %d, the stream's last sequence being %d", info.AckFloor.Stream, last)
	start, err := consumers.RestartAtFloor(ctx, session, filters)
	if err != nil {
		t.Fatalf("restart at floor: %v", err)
	}
	if start != (SessionConsumerStart{Created: true, StartSeq: last + 1}) {
		t.Fatalf("RestartAtFloor of a consumer that delivered nothing = %+v, want created at %d with no gap", start, last+1)
	}

	held := publishStored(t, owner, agent, "held")
	pull := pullSession(t, consumers, session)
	first := next(t, pull)
	if first.StreamSeq != held {
		t.Fatalf("first message = %d %q, want %d \"held\"", first.StreamSeq, first.Data, held)
	}
	pull.Stop()
	if info, err = consumers.Info(ctx, session); err != nil {
		t.Fatalf("info: %v", err)
	}
	t.Logf("with its first delivery unacked the server reports the ack floor as %d", info.AckFloor.Stream)
	if start, err = consumers.RestartAtFloor(ctx, session, filters); err != nil {
		t.Fatalf("restart at floor: %v", err)
	}
	if start.StartSeq != held {
		t.Fatalf("RestartAtFloor with the first delivery unacked = %+v, want a start at %d", start, held)
	}
	again := pullSession(t, consumers, session)
	redelivered := next(t, again)
	if redelivered.StreamSeq != held {
		t.Fatalf("after the restart got %d %q, want %d \"held\"", redelivered.StreamSeq, redelivered.Data, held)
	}
	requireNoMessage(t, again, 300*time.Millisecond)
	if err := redelivered.Ack(); err != nil {
		t.Fatalf("ack: %v", err)
	}
	again.Stop()
	waitForAckFloor(t, consumers, session, held)
	if start, err = consumers.RestartAtFloor(ctx, session, filters); err != nil || start.StartSeq != held+1 {
		t.Fatalf("RestartAtFloor with everything acked = %+v, %v; want a start at %d", start, err, held+1)
	}
}

// After some later messages were acked and earlier ones were not, a restart at the floor delivers
// again, in stream order, everything from the first unacked message on, the acked ones after it
// included, and each message's metadata names its stored subject, sequence, delivery and time.
func TestRestartAtFloorRedeliversTheUnackedInStreamOrder(t *testing.T) {
	owner, consumers := sessionConsumersOnStream(t)
	ctx := context.Background()
	const session = "ses_restart"
	agent := contracts.AgentSubject(session)
	filters := []string{agent, "notifications.github.o.r.>"}
	if _, err := consumers.Ensure(ctx, session, filters, 0); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	var seqs []uint64
	for _, subject := range []string{agent, "notifications.github.o.r.push", agent, "notifications.slack.c.message", agent} {
		seqs = append(seqs, publishStored(t, owner, subject, subject))
	}
	// The slack message is outside the filters, so the consumer delivers four.
	delivered := []uint64{seqs[0], seqs[1], seqs[2], seqs[4]}
	pull := pullSession(t, consumers, session)
	var first []SessionMessage
	for range delivered {
		first = append(first, next(t, pull))
	}
	for i, msg := range first {
		if msg.StreamSeq != delivered[i] || msg.Delivered != 1 {
			t.Fatalf("delivery %d = seq %d delivery %d, want seq %d delivery 1", i, msg.StreamSeq, msg.Delivered, delivered[i])
		}
		if msg.Subject != string(msg.Data) {
			t.Fatalf("delivery %d names subject %q, stored on %q", i, msg.Subject, msg.Data)
		}
		if time.Since(msg.StoredAt) > time.Minute || msg.StoredAt.After(time.Now()) {
			t.Fatalf("delivery %d stored at %s", i, msg.StoredAt)
		}
	}
	// The first and third are acked; the second and fourth are not.
	for _, index := range []int{0, 2} {
		if err := first[index].Ack(); err != nil {
			t.Fatalf("ack: %v", err)
		}
	}
	pull.Stop()
	waitForAckFloor(t, consumers, session, seqs[0])

	start, err := consumers.RestartAtFloor(ctx, session, filters)
	if err != nil {
		t.Fatalf("restart at floor: %v", err)
	}
	if start.StartSeq != seqs[0]+1 || start.Gap {
		t.Fatalf("RestartAtFloor = %+v, want a start at %d and no gap", start, seqs[0]+1)
	}
	again := pullSession(t, consumers, session)
	for _, want := range delivered[1:] {
		if msg := next(t, again); msg.StreamSeq != want {
			t.Fatalf("after the restart got seq %d, want %d", msg.StreamSeq, want)
		}
	}
	requireNoMessage(t, again, 300*time.Millisecond)
}

// waitForAckFloor waits for the server to have taken the acks that bring sessionID's ack floor to
// floor: an ack is a publish the server answers nothing to.
func waitForAckFloor(t *testing.T, consumers *SessionConsumers, sessionID string, floor uint64) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		info, err := consumers.Info(context.Background(), sessionID)
		if err != nil {
			t.Fatalf("info: %v", err)
		}
		if info.AckFloor.Stream == floor {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the ack floor stayed %d, want %d", info.AckFloor.Stream, floor)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// A restart whose floor lies below the stream's first sequence, the messages after the floor gone
// from the stream as aged-out ones are, and a consumer seeded at a sequence the stream no longer
// holds, are both a gap; one seeded at the sequence the stream stores next is not.
func TestAStartBelowTheStreamsFirstSequenceIsAGap(t *testing.T) {
	owner, consumers := sessionConsumersOnStream(t)
	ctx := context.Background()
	const session = "ses_gap"
	agent := contracts.AgentSubject(session)
	if _, err := consumers.Ensure(ctx, session, []string{agent}, 0); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	gone := []uint64{publishStored(t, owner, agent, "one"), publishStored(t, owner, agent, "two")}
	last := publishStored(t, owner, agent, "three")
	for _, seq := range gone {
		if err := owner.JS().DeleteMsg(Stream, seq); err != nil {
			t.Fatalf("delete %d: %v", seq, err)
		}
	}
	start, err := consumers.RestartAtFloor(ctx, session, []string{agent})
	if err != nil {
		t.Fatalf("restart at floor: %v", err)
	}
	if start != (SessionConsumerStart{Created: true, StartSeq: gone[0], Gap: true}) {
		t.Fatalf("RestartAtFloor below the stream's first sequence = %+v, want created at %d with a gap", start, gone[0])
	}

	seeded, err := consumers.Ensure(ctx, "ses_seeded", []string{contracts.AgentSubject("ses_seeded")}, gone[0])
	if err != nil {
		t.Fatalf("ensure seeded: %v", err)
	}
	if seeded != (SessionConsumerStart{Created: true, StartSeq: gone[0], Gap: true}) {
		t.Fatalf("Ensure seeded below the stream's first sequence = %+v, want created at %d with a gap", seeded, gone[0])
	}
	held, err := consumers.Ensure(ctx, "ses_held", []string{contracts.AgentSubject("ses_held")}, last+1)
	if err != nil {
		t.Fatalf("ensure held: %v", err)
	}
	if held.Gap {
		t.Fatalf("Ensure seeded at the stream's next sequence = %+v, want no gap", held)
	}
}

// JetStream refuses one consumer two filters that overlap and takes two that do not, which is why
// Ensure reduces them first (ReduceFilterSubjects); the overlapping set reduced is taken.
func TestConsumerFiltersMustNotOverlap(t *testing.T) {
	_, consumers := sessionConsumersOnStream(t)
	ctx := context.Background()
	disjoint := []string{contracts.AgentSubject("ses_filters"), "notifications.github.o.r.>"}
	if _, err := consumers.create(ctx, "s-ses_disjoint", disjoint, 0); err != nil {
		t.Fatalf("create with filters that do not overlap: %v", err)
	}
	overlapping := []string{"notifications.github.o.r.>", "notifications.github.*.r.pull"}
	_, err := consumers.create(ctx, "s-ses_overlapping", overlapping, 0)
	if apiErrorCode(err) != jetstream.JSErrCodeOverlappingFilterSubjects {
		t.Fatalf("create with overlapping filters = %v, want the server's overlapping filters error", err)
	}
	if _, err := consumers.Ensure(ctx, "ses_overlapping", overlapping, 0); err != nil {
		t.Fatalf("Ensure with the overlapping filters: %v", err)
	}
	info, err := consumers.Info(ctx, "ses_overlapping")
	if err != nil {
		t.Fatalf("info: %v", err)
	}
	if want := []string{"notifications.github.>"}; !slices.Equal(info.Config.FilterSubjects, want) {
		t.Fatalf("filters = %q, want %q", info.Config.FilterSubjects, want)
	}
}

// A filter Ensure adds starts at the consumer's position, which an update keeps: a message stored on
// the new subject before a message the consumer has since delivered is not delivered, one stored
// after the update is.
func TestEnsureAddsAFilterAtTheConsumersPosition(t *testing.T) {
	owner, consumers := sessionConsumersOnStream(t)
	ctx := context.Background()
	const session = "ses_follow"
	agent := contracts.AgentSubject(session)
	if _, err := consumers.Ensure(ctx, session, []string{agent}, 0); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	publishStored(t, owner, "notifications.github.o.r.push", "before the follow")
	own := publishStored(t, owner, agent, "own")
	pull := pullSession(t, consumers, session)
	if msg := next(t, pull); msg.StreamSeq != own {
		t.Fatalf("first message = %d %q, want %d \"own\"", msg.StreamSeq, msg.Data, own)
	}
	start, err := consumers.Ensure(ctx, session, []string{agent, "notifications.github.o.r.>"}, 0)
	if err != nil || start.Created {
		t.Fatalf("Ensure adding a filter = %+v, %v; want an update", start, err)
	}
	after := publishStored(t, owner, "notifications.github.o.r.push", "after the follow")
	if msg := next(t, pull); msg.StreamSeq != after {
		t.Fatalf("next message = %d %q, want %d (stored after the follow)", msg.StreamSeq, msg.Data, after)
	}
	if err := consumers.UpdateFilter(ctx, session, []string{agent}); err != nil {
		t.Fatalf("update filter: %v", err)
	}
	info, err := consumers.Info(ctx, session)
	if err != nil {
		t.Fatalf("info: %v", err)
	}
	if !slices.Equal(info.Config.FilterSubjects, []string{agent}) {
		t.Fatalf("filters after UpdateFilter = %q, want only %s", info.Config.FilterSubjects, agent)
	}
	if err := consumers.UpdateFilter(ctx, "ses_missing", []string{agent}); !errors.Is(err, ErrSessionConsumerNotFound) {
		t.Fatalf("UpdateFilter of a missing consumer = %v, want ErrSessionConsumerNotFound", err)
	}
}

// InProgress restarts a message's ack wait: a message kept in progress past several ack waits is
// not delivered again, and once InProgress stops it is, a wait later.
func TestInProgressExtendsTheAckWait(t *testing.T) {
	owner, consumers := sessionConsumersOnStream(t)
	consumers.ackWait = 2 * time.Second
	ctx := context.Background()
	const session = "ses_progress"
	agent := contracts.AgentSubject(session)
	if _, err := consumers.Ensure(ctx, session, []string{agent}, 0); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	seq := publishStored(t, owner, agent, "held")
	pull := pullSession(t, consumers, session)
	msg := next(t, pull)
	if msg.StreamSeq != seq || msg.Delivered != 1 {
		t.Fatalf("got seq %d delivery %d, want %d delivery 1", msg.StreamSeq, msg.Delivered, seq)
	}
	for range 6 {
		if err := msg.InProgress(); err != nil {
			t.Fatalf("in progress: %v", err)
		}
		requireNoMessage(t, pull, 500*time.Millisecond)
	}
	again := next(t, pull)
	if again.StreamSeq != seq || again.Delivered != 2 {
		t.Fatalf("after InProgress stopped got seq %d delivery %d, want %d delivery 2", again.StreamSeq, again.Delivered, seq)
	}
}

// A NAK with a delay has the message delivered again once the delay has passed.
func TestNakWithDelayRedelivers(t *testing.T) {
	owner, consumers := sessionConsumersOnStream(t)
	ctx := context.Background()
	const session = "ses_nak"
	agent := contracts.AgentSubject(session)
	if _, err := consumers.Ensure(ctx, session, []string{agent}, 0); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	seq := publishStored(t, owner, agent, "refused")
	pull := pullSession(t, consumers, session)
	if err := next(t, pull).NakWithDelay(200 * time.Millisecond); err != nil {
		t.Fatalf("nak: %v", err)
	}
	if again := next(t, pull); again.StreamSeq != seq || again.Delivered != 2 {
		t.Fatalf("after the NAK got seq %d delivery %d, want %d delivery 2", again.StreamSeq, again.Delivered, seq)
	}
}

// Delete removes the consumer, and a second delete of it is no error; a session id that names no
// consumer is refused before anything is sent.
func TestDeleteAndSessionsWithoutAConsumer(t *testing.T) {
	_, consumers := sessionConsumersOnStream(t)
	ctx := context.Background()
	if _, err := consumers.Ensure(ctx, "ses_delete", []string{contracts.AgentSubject("ses_delete")}, 0); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	for range 2 {
		if err := consumers.Delete(ctx, "ses_delete"); err != nil {
			t.Fatalf("delete: %v", err)
		}
	}
	if _, err := consumers.Info(ctx, "ses_delete"); !errors.Is(err, ErrSessionConsumerNotFound) {
		t.Fatalf("info after delete = %v, want ErrSessionConsumerNotFound", err)
	}
	if _, err := consumers.RestartAtFloor(ctx, "ses_delete", []string{contracts.AgentSubject("ses_delete")}); !errors.Is(err, ErrSessionConsumerNotFound) {
		t.Fatalf("RestartAtFloor of a missing consumer = %v, want ErrSessionConsumerNotFound", err)
	}
	if _, err := consumers.Pull(ctx, "ses_delete"); !errors.Is(err, ErrSessionConsumerNotFound) {
		t.Fatalf("Pull of a missing consumer = %v, want ErrSessionConsumerNotFound", err)
	}
	if _, err := consumers.Ensure(ctx, "ses.dotted", []string{"notifications.agent.ses.dotted"}, 0); !errors.Is(err, ErrNoSessionConsumerName) {
		t.Fatalf("Ensure of a dotted session id = %v, want ErrNoSessionConsumerName", err)
	}
}

// StreamID reads the identity the stream's owner gave it.
func TestSessionConsumersStreamID(t *testing.T) {
	owner, consumers := sessionConsumersOnStream(t)
	info, err := owner.JS().StreamInfo(Stream)
	if err != nil {
		t.Fatalf("stream info: %v", err)
	}
	id, err := consumers.StreamID(context.Background())
	if err != nil {
		t.Fatalf("stream id: %v", err)
	}
	if want := info.Config.Metadata[StreamIDMetadataKey]; id == "" || id != want {
		t.Fatalf("StreamID = %q, want %q", id, want)
	}
}

// A NATS user whose grant predates session consumers may read a consumer's info but not create
// one: Ensure returns ErrConsumerNotGranted, at once rather than after a request timeout, so the
// listener can answer the registration with core delivery.
func TestEnsureWithoutTheCreateGrantIsErrConsumerNotGranted(t *testing.T) {
	seed, public := testnats.User(t)
	uri := testnats.StartNkeyGranted(t, public, "$JS.API.INFO", "$JS.API.STREAM.>", "$JS.API.CONSUMER.INFO.>", "_INBOX.>").URL
	t.Setenv("NATS_NKEY_SEED", seed)
	owner, err := ConnectOwningStream([]string{uri})
	if err != nil {
		t.Fatalf("connect the stream's owner: %v", err)
	}
	t.Cleanup(owner.Close)
	consumers, err := ConnectSessionConsumers([]string{uri})
	if err != nil {
		t.Fatalf("connect session consumers: %v", err)
	}
	t.Cleanup(consumers.Close)

	started := time.Now()
	_, err = consumers.Ensure(context.Background(), "ses_ungranted", []string{contracts.AgentSubject("ses_ungranted")}, 0)
	if !errors.Is(err, ErrConsumerNotGranted) || !errors.Is(err, ErrPublishDenied) {
		t.Fatalf("Ensure without the create grant = %v, want ErrConsumerNotGranted", err)
	}
	if elapsed := time.Since(started); elapsed > 3*time.Second {
		t.Fatalf("Ensure took %s to report the refusal", elapsed)
	}
}
