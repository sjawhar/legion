package main

// A GitHub rename moves no subscription. GitHub's deliveries name the repository it holds now, so
// the listener publishes on notifications.github.cybermonklabs.legion.… while every session that
// registered its interests before the move still waits on notifications.github.sjawhar.legion.…
// and hears nothing. ENVOY_GITHUB_REPOSITORY_RENAMES is what closes that: the listener publishes
// each GitHub envelope a second time under the configured former name, the envelope's own topic
// first and the copy straight after (internal/ghrename).
//
// This file drives the whole of that through the built binary: one envoy-listener over a real
// nats-server, both of its GitHub producers — the /webhook/github route and the CI summary loop —
// and four core-NATS subscribers shaped exactly as a client's subscriptions are shaped. Everything
// it asserts is read off the stream in stream order or off those subscribers, because the question
// the feature answers is what a subscriber receives and in which order.

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/cmdtest"
	"github.com/sjawhar/envoy/internal/contracts"
)

// The rename this file configures, and the one pull request every event of it names. The number
// and the delivery ids are this test's alone, so nothing it reads off the stream belongs to
// another test's fixture.
const (
	renamePair     = "sjawhar/legion=cybermonklabs/legion"
	renamePR       = "8821"
	renameSecret   = "rename-fanout-secret"
	renameSHA      = "e8e8e8e8e8e8e8e8e8e8e8e8e8e8e8e8e8e8e8e8"
	renameDelivPR  = "d-rename-fanout-pr-8821"
	renameDelivRev = "d-rename-fanout-review-8821"
	renameDelivCmt = "d-rename-fanout-comment-8821"
)

// The subjects the four events are published on, under the repository's current name and under
// the former name each copy is addressed to.
var (
	renameCurPR      = contracts.GithubSubject("cybermonklabs", "legion", "pr."+renamePR)
	renameCurChecks  = contracts.GithubSubject("cybermonklabs", "legion", "pr."+renamePR+".checks")
	renameCurReview  = contracts.GithubSubject("cybermonklabs", "legion", "pr."+renamePR+".review")
	renameCurMention = contracts.GithubSubject("cybermonklabs", "legion", "pr."+renamePR+".mention")
	renameCurRepoMen = contracts.GithubSubject("cybermonklabs", "legion", "mention")
	renameCurComment = contracts.GithubSubject("cybermonklabs", "legion", "pr."+renamePR+".comment")

	renameOldPR      = contracts.GithubSubject("sjawhar", "legion", "pr."+renamePR)
	renameOldChecks  = contracts.GithubSubject("sjawhar", "legion", "pr."+renamePR+".checks")
	renameOldReview  = contracts.GithubSubject("sjawhar", "legion", "pr."+renamePR+".review")
	renameOldMention = contracts.GithubSubject("sjawhar", "legion", "pr."+renamePR+".mention")
	renameOldRepoMen = contracts.GithubSubject("sjawhar", "legion", "mention")
	renameOldComment = contracts.GithubSubject("sjawhar", "legion", "pr."+renamePR+".comment")
)

// renameWait bounds every wait this file makes: a subscriber's next frame, the stream's next
// message, and the CI settlement, which the summary loop emits a few one-second ticks after the
// listener turns healthy (TestAListenerWithAGitHubRouteSettlesTheCIBucket measures the same wait).
const renameWait = 30 * time.Second

// renameQuiet is how long the negative control gives a copy that must never come. It runs after
// every event has already reached the stream and the current name's subscriber, so a copy would
// have been published before the wait begins; the wait covers the publish and delivery of one
// message, not the events themselves.
const renameQuiet = 3 * time.Second

// The listener publishes a renamed repository's GitHub envelopes a second time under the former
// name, the envelope's own topic first and the copy straight after, and a subscriber of either
// name receives its own. Both of the listener's GitHub producers do it: the webhook route and the
// CI summary loop.
func TestRenameFansOutUnderTheFormerName(t *testing.T) {
	rig := startRenameCase(t, "rename-on", renamePair)
	frames := rig.drive(t, true)

	// D5: each event's own topic is on the stream BEFORE its copy, read in stream order. For the
	// mention delivery all six are here in their exact order: contracts.GithubEnvelopes gives the
	// most specific mention first, then the repository-wide mention, then the comment's own topic,
	// and the publisher puts each copy straight after the envelope it copies.
	assertRenameSubjects(t, rig.listener, "the stream", renameStoredSubjects(frames), []string{
		renameCurChecks, renameOldChecks,
		renameCurPR, renameOldPR,
		renameCurReview, renameOldReview,
		renameCurMention, renameOldMention,
		renameCurRepoMen, renameOldRepoMen,
		renameCurComment, renameOldComment,
	})

	// k-copy-identity. The copy differs from the envelope it copies in its Topic and in nothing
	// else, for a webhook delivery and for a CI settlement alike. That identity is the whole of
	// the "a session subscribed to both names sees it once" guarantee: the clients drop the repeat
	// by event_id, so a copy carrying an id of its own would show that session the event twice. It
	// is also the whole of the Legion daemon's safety, because factID leaves the topic out of a
	// fact's identity, so the two copies fold into one fact only while every other field matches.
	// Every field of contracts.Envelope is compared, through reflection, so a field added to the
	// envelope later is compared here without this test being touched.
	assertRenameCopy(t, rig.listener, "the CI settlement", frames[0], frames[1])
	assertRenameCopy(t, rig.listener, "the pull_request delivery", frames[2], frames[3])

	// S1 subscribed to the former name, as a session that registered before the rename did: it
	// receives every event of the repository, each as the copy published under that name.
	assertRenameSubjects(t, rig.listener, "S1 (the former name)", renameMsgSubjects(rig.s1.take(t, rig.listener, 5)), []string{
		renameOldChecks, renameOldPR, renameOldReview, renameOldMention, renameOldComment,
	})

	// S2 holds the one shape five live subscriptions hold, pr.<n>.review alone, and receives that
	// event's copy and nothing else.
	assertRenameSubjects(t, rig.listener, "S2 (the former name's reviews)", renameMsgSubjects(rig.s2.take(t, rig.listener, 1)), []string{
		renameOldReview,
	})

	// S3 subscribed to the current name and receives every event under it, unchanged by the
	// rename: the copy is published beside the original, never in its place.
	assertRenameSubjects(t, rig.listener, "S3 (the current name)", renameMsgSubjects(rig.s3.take(t, rig.listener, 5)), []string{
		renameCurChecks, renameCurPR, renameCurReview, renameCurMention, renameCurComment,
	})

	// S4 subscribed to both names, so it receives each event twice. It is a raw NATS subscription
	// with no client-side dedupe, so the two frames are what a real client is handed; what makes
	// that client show one card and not two is that both frames carry one event_id.
	both := rig.s4.take(t, rig.listener, 10)
	assertRenameSubjects(t, rig.listener, "S4 (both names)", renameMsgSubjects(both), []string{
		renameCurChecks, renameOldChecks,
		renameCurPR, renameOldPR,
		renameCurReview, renameOldReview,
		renameCurMention, renameOldMention,
		renameCurComment, renameOldComment,
	})
	for i := 0; i < len(both); i += 2 {
		own := renameDecode(t, rig.listener, both[i].Subject, both[i].Data)
		copied := renameDecode(t, rig.listener, both[i+1].Subject, both[i+1].Data)
		if own.EventID != copied.EventID {
			t.Fatalf("S4 (both names): the frames on %s and %s carry event_id %q and %q, want one id: a client subscribed to both names dedupes by event_id and would show this event twice\n%s",
				both[i].Subject, both[i+1].Subject, own.EventID, copied.EventID, rig.listener.Output.String())
		}
	}
}

// The negative control, on a nats-server and a listener of its own: with
// ENVOY_GITHUB_REPOSITORY_RENAMES unset, the same rig publishes every event under the current name
// alone. Nothing reaches the former name's subscribers, which is the defect the setting removes
// and the state every deployment that configures no rename stays in.
func TestRenameUnsetPublishesTheCurrentNameAlone(t *testing.T) {
	rig := startRenameCase(t, "rename-off", "")
	frames := rig.drive(t, false)

	assertRenameSubjects(t, rig.listener, "the stream", renameStoredSubjects(frames), []string{
		renameCurChecks, renameCurPR, renameCurReview, renameCurMention, renameCurRepoMen, renameCurComment,
	})

	// S3 receives every event: the rig drove all four, so a silent S1 and S2 below is the absent
	// copy and not an absent event.
	assertRenameSubjects(t, rig.listener, "S3 (the current name)", renameMsgSubjects(rig.s3.take(t, rig.listener, 5)), []string{
		renameCurChecks, renameCurPR, renameCurReview, renameCurMention, renameCurComment,
	})
	assertRenameSubjects(t, rig.listener, "S4 (both names)", renameMsgSubjects(rig.s4.take(t, rig.listener, 5)), []string{
		renameCurChecks, renameCurPR, renameCurReview, renameCurMention, renameCurComment,
	})

	// One wait, not two: every event reached the stream and S3 before this line, so a copy would
	// have been published before the wait began, and after S1's three seconds of silence nothing
	// is on its way to S2 either.
	rig.s1.quiet(t, rig.listener, renameQuiet)
	rig.s2.empty(t, rig.listener)
}

// renameCase is one run of the rig: its own nats-server, its own listener process, and the four
// subscribers, which are subscribed before the listener starts so no event can be published past
// them.
type renameCase struct {
	client   *bus.Client
	listener *listenerProcess
	s1       *renameSub
	s2       *renameSub
	s3       *renameSub
	s4       *renameSub
}

// startRenameCase starts a nats-server of this test's own (setupTestNATS), seeds the CI record the
// summary loop settles, subscribes the four subscribers, and starts a built envoy-listener over
// that server with the GitHub webhook route mounted and ENVOY_GITHUB_REPOSITORY_RENAMES set to
// renames, which an empty renames leaves unset. It returns once the listener is healthy.
func startRenameCase(t *testing.T, machineID, renames string) *renameCase {
	t.Helper()
	client := setupTestNATS(t)
	t.Cleanup(client.Close)
	seedDueCIRecord(t, client, "cybermonklabs", "legion", renamePR, renameSHA)

	// Each subscriber is written as a client writes it: a client subscribing to topic X subscribes
	// X and X.>, because NATS's > needs at least one trailing token and so matches none of the
	// bare pr.<n> a pull_request delivery is published on.
	rig := &renameCase{
		client: client,
		s1:     subscribeRename(t, client, "S1 (the former name)", renameOldPR, renameOldPR+".>"),
		s2:     subscribeRename(t, client, "S2 (the former name's reviews)", renameOldReview),
		s3:     subscribeRename(t, client, "S3 (the current name)", renameCurPR, renameCurPR+".>"),
		s4:     subscribeRename(t, client, "S4 (both names)", renameCurPR, renameCurPR+".>", renameOldPR, renameOldPR+".>"),
	}

	env := []string{
		"ENVOY_WEBHOOKS=github",
		"ENVOY_GITHUB_WEBHOOK_SECRET=" + renameSecret,
		"ENVOY_REVIEWER_APP_ID=1",
	}
	if renames != "" {
		env = append(env, "ENVOY_GITHUB_REPOSITORY_RENAMES="+renames)
	}
	rig.listener = startListenerProcess(t, cmdtest.Build(t, "envoy-listener"), client.Conn.ConnectedUrl(), machineID, env...)
	rig.listener.waitHealthy(t)
	return rig
}

// drive produces the four events of cybermonklabs/legion#8821, waiting for each to have reached
// the stream whole before it produces the next, so the stream's order is the publish order of one
// event at a time and no event's frames interleave with another's. copies says whether a rename is
// configured, which is what decides how many messages each event leaves. It returns every message
// the stream holds on notifications.github.>, in stream order.
//
// One pull_request delivery is published on the bare pr.<n> and on nothing a pr.<n>.> subscriber
// matches, so all four events are needed to cover the subscriber shapes: the review is the event a
// subscriber of notifications.github.sjawhar.legion.pr.<n>.> hears, the CI settlement is the only
// proof of the second GitHub producer, and the mention is the delivery that fans out to three
// topics at once.
func (c *renameCase) drive(t *testing.T, copies bool) []bus.StoredMessage {
	t.Helper()
	per := 1
	if copies {
		per = 2
	}

	// Event 3, first on the wire because the summary loop settles the seeded record on its own
	// schedule: waiting for it here is what keeps it out of the middle of a webhook's fan-out.
	// The record was seeded before the listener started, so this is the loop's first pass over it.
	want := per
	c.awaitStream(t, want)

	// Event 1: a pull_request delivery, published on the bare pr.<n>.
	postRenameWebhook(t, c.listener, "pull_request", renameDelivPR,
		`{"action":"opened","number":`+renamePR+`,`+
			`"pull_request":{"number":`+renamePR+`,"title":"rename fan-out","html_url":"https://github.com/cybermonklabs/legion/pull/`+renamePR+`","user":{"login":"rename-author"},"body":"a pull request body with no trigger in it"},`+
			`"repository":{"name":"legion","owner":{"login":"cybermonklabs"},"full_name":"cybermonklabs/legion"},`+
			`"sender":{"login":"rename-author","type":"User"}}`)
	want += per
	c.awaitStream(t, want)

	// Event 2: a pull_request_review, published on pr.<n>.review, which is what a subscriber of
	// pr.<n>.> hears. Its body carries no trigger, so it fans out to no mention topic.
	postRenameWebhook(t, c.listener, "pull_request_review", renameDelivRev,
		`{"action":"submitted",`+
			`"review":{"state":"approved","body":"reads fine to me","user":{"login":"rename-reviewer"},"html_url":"https://github.com/cybermonklabs/legion/pull/`+renamePR+`#pullrequestreview-1"},`+
			`"pull_request":{"number":`+renamePR+`,"title":"rename fan-out","html_url":"https://github.com/cybermonklabs/legion/pull/`+renamePR+`"},`+
			`"repository":{"name":"legion","owner":{"login":"cybermonklabs"},"full_name":"cybermonklabs/legion"},`+
			`"sender":{"login":"rename-reviewer","type":"User"}}`)
	want += per
	c.awaitStream(t, want)

	// Event 4: an issue_comment on a pull request whose body carries the mention trigger, which is
	// three topics and so three copies: six messages for one delivery.
	postRenameWebhook(t, c.listener, "issue_comment", renameDelivCmt,
		`{"action":"created",`+
			`"issue":{"number":`+renamePR+`,"title":"rename fan-out","html_url":"https://github.com/cybermonklabs/legion/pull/`+renamePR+`","pull_request":{"url":"https://api.github.com/repos/cybermonklabs/legion/pulls/`+renamePR+`"}},`+
			`"comment":{"body":"@legion please read the rename fan-out","user":{"login":"rename-author"},"html_url":"https://github.com/cybermonklabs/legion/pull/`+renamePR+`#issuecomment-1"},`+
			`"repository":{"name":"legion","owner":{"login":"cybermonklabs"},"full_name":"cybermonklabs/legion"},`+
			`"sender":{"login":"rename-author","type":"User"}}`)
	want += 3 * per
	return c.awaitStream(t, want)
}

// awaitStream waits, on a bounded poll, for the stream to hold count messages on
// notifications.github.> and returns them in stream order.
func (c *renameCase) awaitStream(t *testing.T, count int) []bus.StoredMessage {
	t.Helper()
	deadline := time.Now().Add(renameWait)
	for {
		frames := renameStreamFrames(t, c.client)
		if len(frames) >= count {
			return frames
		}
		if time.Now().After(deadline) {
			t.Fatalf("the stream holds %d messages on notifications.github.> after %s, want %d; it holds %v\n%s",
				len(frames), renameWait, count, renameStoredSubjects(frames), c.listener.Output.String())
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// renameStreamFrames reads every message the notification stream holds on notifications.github.>,
// in stream order, by walking it from its first sequence with next_by_subj. Stream order is the
// publish order, which is the whole of what D5 asks; GetLastMsg cannot show it.
func renameStreamFrames(t *testing.T, client *bus.Client) []bus.StoredMessage {
	t.Helper()
	var frames []bus.StoredMessage
	for seq := uint64(1); ; {
		ctx, cancel := context.WithTimeout(context.Background(), renameWait)
		message, err := client.NextStoredMessage(ctx, "notifications.github.>", seq)
		cancel()
		if errors.Is(err, natsgo.ErrMsgNotFound) {
			return frames
		}
		if err != nil {
			t.Fatalf("read the stream's first message on notifications.github.> at or after sequence %d: %v", seq, err)
		}
		frames = append(frames, message)
		seq = message.Sequence + 1
	}
}

// renameSub is one subscriber of the rig: the raw core-NATS subscriptions a client holding one
// topic holds, over the bus client's own connection, and the frames they delivered in arrival
// order. One channel takes every subscription of the subscriber, so the order it reads is the
// order the connection received them in.
type renameSub struct {
	name string
	msgs chan *natsgo.Msg
}

// subscribeRename subscribes name to every subject and flushes, so the server holds the
// subscriptions before the caller produces anything.
func subscribeRename(t *testing.T, client *bus.Client, name string, subjects ...string) *renameSub {
	t.Helper()
	sub := &renameSub{name: name, msgs: make(chan *natsgo.Msg, 64)}
	for _, subject := range subjects {
		subscription, err := client.Conn.ChanSubscribe(subject, sub.msgs)
		if err != nil {
			t.Fatalf("%s: subscribe %s: %v", name, subject, err)
		}
		t.Cleanup(func() { _ = subscription.Unsubscribe() })
	}
	if err := client.Conn.Flush(); err != nil {
		t.Fatalf("%s: flush its subscriptions: %v", name, err)
	}
	return sub
}

// take waits for the subscriber's next count frames, each within renameWait, and returns them in
// arrival order.
func (s *renameSub) take(t *testing.T, listener *listenerProcess, count int) []*natsgo.Msg {
	t.Helper()
	got := make([]*natsgo.Msg, 0, count)
	for len(got) < count {
		select {
		case message := <-s.msgs:
			got = append(got, message)
		case <-time.After(renameWait):
			t.Fatalf("%s received %d of %d frames within %s; it has %v and is still waiting\n%s",
				s.name, len(got), count, renameWait, renameMsgSubjects(got), listener.Output.String())
		}
	}
	return got
}

// quiet requires that nothing reaches the subscriber within grace.
func (s *renameSub) quiet(t *testing.T, listener *listenerProcess, grace time.Duration) {
	t.Helper()
	select {
	case message := <-s.msgs:
		t.Fatalf("%s received %q within %s with ENVOY_GITHUB_REPOSITORY_RENAMES unset, want nothing: the listener published under the former name with no rename configured\n%s",
			s.name, message.Subject, grace, listener.Output.String())
	case <-time.After(grace):
	}
}

// empty requires that nothing is already waiting for the subscriber, without waiting itself. It
// is the second half of a negative control whose first half waited: a copy of any of the events
// would have been published before quiet began, so a quiet S1 leaves nothing on its way to S2.
func (s *renameSub) empty(t *testing.T, listener *listenerProcess) {
	t.Helper()
	select {
	case message := <-s.msgs:
		t.Fatalf("%s received %q with ENVOY_GITHUB_REPOSITORY_RENAMES unset, want nothing: the listener published under the former name with no rename configured\n%s",
			s.name, message.Subject, listener.Output.String())
	default:
	}
}

// postRenameWebhook posts one signed GitHub delivery to the listener's /webhook/github route and
// requires the 200 that says every envelope of it was published.
func postRenameWebhook(t *testing.T, listener *listenerProcess, event, delivery, body string) {
	t.Helper()
	mac := hmac.New(sha256.New, []byte(renameSecret))
	mac.Write([]byte(body))
	request, err := http.NewRequest(http.MethodPost, "http://127.0.0.1:"+strconv.Itoa(listener.port)+"/webhook/github", strings.NewReader(body))
	if err != nil {
		t.Fatalf("build the %s delivery %s: %v", event, delivery, err)
	}
	request.Header.Set("X-GitHub-Delivery", delivery)
	request.Header.Set("X-GitHub-Event", event)
	request.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	status, answer := do(t, request)
	if status != http.StatusOK {
		t.Fatalf("the %s delivery %s: status %d %q, want 200\n%s", event, delivery, status, answer, listener.Output.String())
	}
}

// assertRenameCopy requires that the message stored on copied is the one stored on own with
// nothing but its topic changed. Every field of contracts.Envelope is compared, by name, so a
// field added to the envelope and not carried over by the copy fails here.
func assertRenameCopy(t *testing.T, listener *listenerProcess, what string, own, copied bus.StoredMessage) {
	t.Helper()
	original := renameDecode(t, listener, own.Subject, own.Data)
	second := renameDecode(t, listener, copied.Subject, copied.Data)
	if original.Topic != own.Subject {
		t.Fatalf("%s: the message stored on %s carries topic %q, want its own subject\n%s", what, own.Subject, original.Topic, listener.Output.String())
	}
	if second.Topic != copied.Subject {
		t.Fatalf("%s: the copy stored on %s carries topic %q, want its own subject\n%s", what, copied.Subject, second.Topic, listener.Output.String())
	}
	equalised := second
	equalised.Topic = original.Topic
	left, right := reflect.ValueOf(original), reflect.ValueOf(equalised)
	for i := range left.NumField() {
		field := left.Type().Field(i).Name
		if !reflect.DeepEqual(left.Field(i).Interface(), right.Field(i).Interface()) {
			t.Fatalf("%s: the copy on %s differs from the envelope on %s in %s: the copy has %#v, want the original's %#v\n%s",
				what, copied.Subject, own.Subject, field, right.Field(i).Interface(), left.Field(i).Interface(), listener.Output.String())
		}
	}
}

// assertRenameSubjects requires that who received exactly want, in that order.
func assertRenameSubjects(t *testing.T, listener *listenerProcess, who string, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s holds %d subjects, want %d:\n got  %v\n want %v\n%s", who, len(got), len(want), got, want, listener.Output.String())
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("%s holds %q at position %d, want %q:\n got  %v\n want %v\n%s", who, got[i], i, want[i], got, want, listener.Output.String())
		}
	}
}

// renameDecode decodes one stored or delivered envelope, naming the subject it came from.
func renameDecode(t *testing.T, listener *listenerProcess, subject string, data []byte) contracts.Envelope {
	t.Helper()
	var envelope contracts.Envelope
	if err := json.Unmarshal(data, &envelope); err != nil {
		t.Fatalf("decode the message on %s: %v: %s\n%s", subject, err, data, listener.Output.String())
	}
	return envelope
}

// renameStoredSubjects are the subjects of stored messages, in their order.
func renameStoredSubjects(frames []bus.StoredMessage) []string {
	subjects := make([]string, 0, len(frames))
	for _, frame := range frames {
		subjects = append(subjects, frame.Subject)
	}
	return subjects
}

// renameMsgSubjects are the subjects of delivered frames, in their order.
func renameMsgSubjects(frames []*natsgo.Msg) []string {
	subjects := make([]string, 0, len(frames))
	for _, frame := range frames {
		subjects = append(subjects, frame.Subject)
	}
	return subjects
}
