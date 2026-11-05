package ghrename

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/logging"
)

// recordingBus is a Bus that records every envelope it was handed, in the order it was handed
// them, and fails the nth publish when failOn is n: the order of the two publishes and which of
// them failed are what every test here reads.
type recordingBus struct {
	published []contracts.Envelope
	calls     int
	// failOn is the 1-based call that fails with failWith; 0 fails none.
	failOn   int
	failWith error
}

func (b *recordingBus) publish(item contracts.Envelope) error {
	b.calls++
	if b.failOn == b.calls {
		return b.failWith
	}
	b.published = append(b.published, item)
	return nil
}

func (b *recordingBus) Publish(item contracts.Envelope) error { return b.publish(item) }

func (b *recordingBus) PublishWithin(_ context.Context, item contracts.Envelope) error {
	return b.publish(item)
}

// topics are the topics the bus was handed, in order, which is what a fan-out case states.
func (b *recordingBus) topics() []string {
	var topics []string
	for _, item := range b.published {
		topics = append(topics, item.Topic)
	}
	return topics
}

// publishMethods are the two one-method interfaces a *Publisher satisfies, so every case runs
// through both: the webhook handlers hold PublishWithin and the CI summary loop holds Publish, and
// the two must fan out alike.
var publishMethods = []struct {
	name    string
	publish func(*Publisher, contracts.Envelope) error
}{
	{"Publish", func(p *Publisher, item contracts.Envelope) error { return p.Publish(item) }},
	{"PublishWithin", func(p *Publisher, item contracts.Envelope) error {
		return p.PublishWithin(context.Background(), item)
	}},
}

// testEnvelope is a GitHub envelope on topic with every field of contracts.Envelope set, so a test
// comparing an envelope against its copy compares fields that carry something.
func testEnvelope(topic string) contracts.Envelope {
	expires, kept := int64(1700000600000), int64(1700000500000)
	return contracts.Envelope{
		EventID:        "evt_01HQ8ZZZZZZZZZZZZZZZZZZZZZ",
		Source:         "github",
		SourceEventID:  "d34db33f-0000-4000-8000-000000000001",
		SourceSession:  "ses_source",
		Topic:          topic,
		DedupeKey:      "github.d34db33f-0000-4000-8000-000000000001",
		IssuedAt:       1700000400000,
		ExpiresAt:      &expires,
		KeptAt:         &kept,
		PayloadSummary: "sjawhar commented on pr 7706",
		PayloadRef:     "payloads/7706",
		Payload:        `{"action":"created"}`,
		TraceID:        "trace-7706",
		Sender:         &contracts.EnvelopeSender{SessionID: "ses_sender", Machine: "mach", Roles: []string{"reviewer"}},
		InReplyTo:      "evt_01HQ8YYYYYYYYYYYYYYYYYYYYY",
		Supersedes:     "evt_01HQ8XXXXXXXXXXXXXXXXXXXXX",
		Urgency:        "normal",
		ExpectsReply:   "no",
	}
}

// mustParse is renames from value, or a failed test: a case's value is a constant of the test.
func mustParse(t *testing.T, value string) Renames {
	t.Helper()
	renames, err := Parse(value)
	if err != nil {
		t.Fatalf("Parse(%q) refused the value: %v, want the pairs it holds", value, err)
	}
	return renames
}

// TestPublisherFansOutToTheFormerName covers which topics a publish puts on the bus and in which
// order: the envelope's own topic always first, its copy under the former name after it, and
// nothing at all beyond the one publish when no pair names the repository the topic names.
func TestPublisherFansOutToTheFormerName(t *testing.T) {
	const pair = "sjawhar/legion=cybermonklabs/legion"

	cases := []struct {
		name    string
		renames string
		topic   string
		want    []string
	}{
		{
			name:    "a pair names the repository",
			renames: pair,
			topic:   "notifications.github.cybermonklabs.legion.pr.7706",
			want: []string{
				"notifications.github.cybermonklabs.legion.pr.7706",
				"notifications.github.sjawhar.legion.pr.7706",
			},
		},
		{
			name:    "a mention copy of the same repository",
			renames: pair,
			topic:   "notifications.github.cybermonklabs.legion.pr.7706.mention",
			want: []string{
				"notifications.github.cybermonklabs.legion.pr.7706.mention",
				"notifications.github.sjawhar.legion.pr.7706.mention",
			},
		},
		{
			name:    "no pair configured",
			renames: "",
			topic:   "notifications.github.cybermonklabs.legion.pr.7706",
			want:    []string{"notifications.github.cybermonklabs.legion.pr.7706"},
		},
		{
			name:    "a topic no pair names",
			renames: pair,
			topic:   "notifications.github.acme.site.pr.1",
			want:    []string{"notifications.github.acme.site.pr.1"},
		},
		{
			name:    "the former name itself, which is never rewritten again",
			renames: pair,
			topic:   "notifications.github.sjawhar.legion.pr.7706",
			want:    []string{"notifications.github.sjawhar.legion.pr.7706"},
		},
		{
			name:    "a topic of another source",
			renames: pair,
			topic:   "notifications.slack.c0123456789.thread",
			want:    []string{"notifications.slack.c0123456789.thread"},
		},
	}

	for _, tc := range cases {
		for _, method := range publishMethods {
			t.Run(tc.name+"/"+method.name, func(t *testing.T) {
				bus := &recordingBus{}
				publisher := Publishing(bus, mustParse(t, tc.renames), logging.NewWithWriter("test", &bytes.Buffer{}))

				if err := method.publish(publisher, testEnvelope(tc.topic)); err != nil {
					t.Fatalf("%s of %q: %v, want the publish to succeed", method.name, tc.topic, err)
				}

				got := bus.topics()
				if len(got) != len(tc.want) || bus.calls != len(tc.want) {
					t.Fatalf("%s of %q put %v on the bus in %d calls, want %v in %d",
						method.name, tc.topic, got, bus.calls, tc.want, len(tc.want))
				}
				for i, want := range tc.want {
					if got[i] != want {
						t.Errorf("%s of %q: publish %d went to %q, want %q (the envelope's own topic goes first, its copy second)",
							method.name, tc.topic, i+1, got[i], want)
					}
				}
			})
		}
	}
}

// TestPublisherCopyDiffersOnlyInTopic covers the fact the whole change rests on: the copy carries
// the envelope's own EventID, DedupeKey, SourceEventID, IssuedAt and payload, so a client dedupes
// the two pushes by event_id and the Legion daemon folds them into one fact. The comparison is of
// the two structs with the topic equalised, so a field contracts.Envelope grows later is compared
// too — and a field it grows that cannot be compared with == fails to build here, which is the
// same signal.
func TestPublisherCopyDiffersOnlyInTopic(t *testing.T) {
	for _, method := range publishMethods {
		t.Run(method.name, func(t *testing.T) {
			bus := &recordingBus{}
			publisher := Publishing(bus, mustParse(t, "sjawhar/legion=cybermonklabs/legion"), logging.NewWithWriter("test", io.Discard))
			item := testEnvelope("notifications.github.cybermonklabs.legion.pr.7706.mention")

			if err := method.publish(publisher, item); err != nil {
				t.Fatalf("%s: %v, want the publish to succeed", method.name, err)
			}

			if len(bus.published) != 2 {
				t.Fatalf("%s put %v on the bus, want the envelope and its copy", method.name, bus.topics())
			}
			original, copied := bus.published[0], bus.published[1]
			if original != item {
				t.Errorf("%s published %+v as the original, want the envelope it was given, %+v", method.name, original, item)
			}
			if copied.Topic != "notifications.github.sjawhar.legion.pr.7706.mention" {
				t.Errorf("%s published the copy on %q, want the same subject under the former name", method.name, copied.Topic)
			}
			equalised := copied
			equalised.Topic = original.Topic
			if equalised != original {
				t.Errorf("%s published a copy differing from its envelope in more than Topic:\ncopy     %+v\nenvelope %+v",
					method.name, copied, original)
			}
		})
	}
}

// TestPublisherFailures covers which publish's error a caller is given, and what reached the bus
// before it: a failed original costs its copy the attempt, and a failed copy fails the publish
// rather than being swallowed, since a swallowed one leaves a subscriber of the former name
// waiting for an event that never arrives.
func TestPublisherFailures(t *testing.T) {
	errOriginal := errors.New("the original was refused")
	errCopy := errors.New("the copy was refused")

	cases := []struct {
		name       string
		failOn     int
		failWith   error
		wantErr    error
		wantCalls  int
		wantTopics []string
	}{
		{
			name:       "the original fails",
			failOn:     1,
			failWith:   errOriginal,
			wantErr:    errOriginal,
			wantCalls:  1,
			wantTopics: nil,
		},
		{
			name:       "the copy fails",
			failOn:     2,
			failWith:   errCopy,
			wantErr:    errCopy,
			wantCalls:  2,
			wantTopics: []string{"notifications.github.cybermonklabs.legion.pr.7706"},
		},
	}

	for _, tc := range cases {
		for _, method := range publishMethods {
			t.Run(tc.name+"/"+method.name, func(t *testing.T) {
				bus := &recordingBus{failOn: tc.failOn, failWith: tc.failWith}
				publisher := Publishing(bus, mustParse(t, "sjawhar/legion=cybermonklabs/legion"), logging.NewWithWriter("test", io.Discard))

				err := method.publish(publisher, testEnvelope("notifications.github.cybermonklabs.legion.pr.7706"))

				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("%s with publish %d failing returned %v, want %v", method.name, tc.failOn, err, tc.wantErr)
				}
				if bus.calls != tc.wantCalls {
					t.Errorf("%s with publish %d failing made %d calls, want %d", method.name, tc.failOn, bus.calls, tc.wantCalls)
				}
				got := bus.topics()
				if len(got) != len(tc.wantTopics) {
					t.Fatalf("%s with publish %d failing left %v on the bus, want %v", method.name, tc.failOn, got, tc.wantTopics)
				}
				for i, want := range tc.wantTopics {
					if got[i] != want {
						t.Errorf("%s with publish %d failing: publish %d went to %q, want %q", method.name, tc.failOn, i+1, got[i], want)
					}
				}
			})
		}
	}
}

// TestPublisherDeclinedRewriteIsLoggedOnce covers the subject limit a copy is bounded by, at the
// byte either side of it: a rewrite of exactly contracts.SubjectMaxBytes is published as every
// other copy is and logs nothing, and a former name long enough to push the rewrite one byte past
// it is declined -- the envelope goes out under its own topic alone, and one warning names the
// pair. Declining is what keeps a copy the bus would refuse for ever out of the way of the
// publish it copies, and the line is the one skip an operator is owed.
func TestPublisherDeclinedRewriteIsLoggedOnce(t *testing.T) {
	const renames = "acme/a-much-longer-former-name=acme/new"
	const current = "notifications.github.acme.new"
	const former = "notifications.github.acme.a-much-longer-former-name"

	cases := []struct {
		name string
		// length is the length of the rewritten subject the trailing segment is built for.
		length   int
		declined bool
	}{
		{name: "a rewrite of exactly SubjectMaxBytes", length: contracts.SubjectMaxBytes},
		{name: "a rewrite one byte past SubjectMaxBytes", length: contracts.SubjectMaxBytes + 1, declined: true},
	}

	for _, tc := range cases {
		for _, method := range publishMethods {
			t.Run(tc.name+"/"+method.name, func(t *testing.T) {
				tail := strings.Repeat("a", tc.length-len(former)-1)
				topic := current + "." + tail
				want := []string{topic, former + "." + tail}
				if tc.declined {
					want = want[:1]
				}

				var logged bytes.Buffer
				bus := &recordingBus{}
				publisher := Publishing(bus, mustParse(t, renames), logging.NewWithWriter("test", &logged))

				if err := method.publish(publisher, testEnvelope(topic)); err != nil {
					t.Fatalf("%s of a subject whose rewrite is %d bytes: %v, want the envelope's own publish to succeed",
						method.name, tc.length, err)
				}

				got := bus.topics()
				if len(got) != len(want) || bus.calls != len(want) {
					t.Fatalf("%s of a subject whose rewrite is %d bytes put %v on the bus in %d calls, want %d of them",
						method.name, tc.length, got, bus.calls, len(want))
				}
				for i, subject := range want {
					if got[i] != subject {
						t.Errorf("%s of a subject whose rewrite is %d bytes: publish %d went to %.60q… (%d bytes), want %.60q… (%d bytes)",
							method.name, tc.length, i+1, got[i], len(got[i]), subject, len(subject))
					}
				}

				if !tc.declined {
					if logged.Len() != 0 {
						t.Errorf("%s of a rewrite of exactly %d bytes logged %q, want nothing: the copy went out",
							method.name, contracts.SubjectMaxBytes, logged.String())
					}
					return
				}
				lines := strings.Split(strings.TrimSpace(logged.String()), "\n")
				if logged.Len() == 0 || len(lines) != 1 {
					t.Fatalf("%s of a subject whose rewrite is too long logged %d lines (%q), want one naming the pair",
						method.name, len(lines), logged.String())
				}
				for _, want := range []string{"acme/a-much-longer-former-name", "acme/new"} {
					if !strings.Contains(lines[0], want) {
						t.Errorf("%s logged %q, want a line naming %q, the pair whose copy was skipped", method.name, lines[0], want)
					}
				}
			})
		}
	}
}

// TestPublisherLogsNothingForAnUnnamedRepository covers the silence every other publish keeps: a
// repository no pair names is most of what a deployment publishes, and a line per publish would
// bury the one skip that matters.
func TestPublisherLogsNothingForAnUnnamedRepository(t *testing.T) {
	for _, method := range publishMethods {
		t.Run(method.name, func(t *testing.T) {
			var logged bytes.Buffer
			bus := &recordingBus{}
			publisher := Publishing(bus, mustParse(t, "sjawhar/legion=cybermonklabs/legion"), logging.NewWithWriter("test", &logged))

			if err := method.publish(publisher, testEnvelope("notifications.github.acme.site.pr.1")); err != nil {
				t.Fatalf("%s of a repository no pair names: %v, want the publish to succeed", method.name, err)
			}

			if logged.Len() != 0 {
				t.Errorf("%s of a repository no pair names logged %q, want nothing", method.name, logged.String())
			}
		})
	}
}
