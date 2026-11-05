package ghrename

import (
	"context"
	"log/slog"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/logging"
)

// Bus is what the publisher publishes through. *bus.Client satisfies it. It is declared here, and
// not imported from internal/bus, so that this package stays free of the bus and of everything the
// bus pulls in: a rename is a fact about subjects, and the one thing it needs of a bus is that it
// takes an envelope. The two methods are the two one-method publisher interfaces this package's
// callers hold — webhook.Publisher (PublishWithin, the webhook handlers) and cistore.Publisher
// (Publish, the CI summary loop) — so one *Publisher satisfies both.
type Bus interface {
	Publish(contracts.Envelope) error
	PublishWithin(context.Context, contracts.Envelope) error
}

// Publisher is a Bus that publishes each GitHub envelope a second time under the former name of the
// repository its topic names. Publishing builds one.
type Publisher struct {
	bus     Bus
	renames Renames
	logger  *logging.Logger
}

// Publishing wraps bus so every GitHub envelope is published a second time under the former name of
// the repository its topic names, which is what keeps a session that subscribed before the rename
// hearing about the repository (see this package's doc comment).
//
// The copy differs from the envelope it copies in its Topic and in nothing else: the same EventID,
// DedupeKey, SourceEventID, IssuedAt and payload. A client dedupes the two pushes by event_id and
// the Legion daemon folds them into one fact by the same id, so a session subscribed to both names
// sees one event; a copy carrying an id of its own would show that session the event twice.
//
// The envelope is published as it was given first, and its copy after it. The envelope's own topic
// is the one a reader of the current name waits on, so it never waits behind a copy, and the order
// the handler hands envelopes over in is kept: contracts.GithubEnvelopes gives the most specific
// copy of a mention first, this publisher puts each copy straight after the envelope it copies, and
// so a mention's three topics go out as six in a defined order, every name still most specific
// first.
//
// renames may be the zero value, which a caller that configures no rename passes; it publishes no
// copy.
func Publishing(bus Bus, renames Renames, logger *logging.Logger) *Publisher {
	return &Publisher{bus: bus, renames: renames, logger: logger}
}

// Publish publishes item, then its copy under the former name. It is cistore.Publisher.
//
// A copy that fails to publish fails the publish that carried it, and its error is the one
// returned, rather than being logged and dropped. On the webhook path the handler then answers
// non-2xx, GitHub redelivers, and the original is refused as a duplicate of a message id the
// stream already holds, so the redelivery costs one publish and adds only the copy. On the CI
// settlement path the record's claim is released and the next pass settles it again under a fresh
// EventID, so a subscriber of the current name can see one settlement card a second time. A
// repeated card is cosmetic; swallowing the copy's error would leave the subscriber of the former
// name waiting for a settlement that never arrives, which is the defect this package exists to
// remove.
func (p *Publisher) Publish(item contracts.Envelope) error {
	if err := p.bus.Publish(item); err != nil {
		return err
	}
	former, ok := p.formerTopic(item.Topic)
	if !ok {
		return nil
	}
	item.Topic = former
	return p.bus.Publish(item)
}

// PublishWithin publishes item, then its copy under the former name, each within ctx. It is
// webhook.Publisher.
//
// A copy that fails to publish fails the publish, and its error is the one returned: see Publish
// for why that failure is not swallowed.
func (p *Publisher) PublishWithin(ctx context.Context, item contracts.Envelope) error {
	if err := p.bus.PublishWithin(ctx, item); err != nil {
		return err
	}
	former, ok := p.formerTopic(item.Topic)
	if !ok {
		return nil
	}
	item.Topic = former
	return p.bus.PublishWithin(ctx, item)
}

// formerTopic is topic under the former name of the repository it names, and whether there is such
// a copy to publish at all. A deployment that configures no rename, and a topic no pair names,
// answer false having allocated nothing, so the publisher is the bus's own behaviour for them: one
// call, the same error. One walk of the topic and one map read decide it, which is what every
// GitHub publish pays. The caller publishes the copy by giving the envelope it already holds by
// value this topic, so no envelope is copied for a repository nothing renames.
//
// A rewrite the former name makes too long for the bus is the one false worth a line in the log,
// and the same lookup has already found the pair to name in it, so it is logged here.
func (p *Publisher) formerTopic(topic string) (string, bool) {
	former, pair, named := p.renames.rewrite(topic)
	if !named {
		return "", false
	}
	if len(former) > contracts.SubjectMaxBytes {
		p.warnDeclined(topic, former, pair)
		return "", false
	}
	return former, true
}

// warnDeclined logs the one line a skipped copy is worth: the rewrite of subject under pair's
// former name is past contracts.SubjectMaxBytes, so the envelope went out under its own topic alone
// and a subscriber of the former name did not hear it. Every other copy a publisher does not make
// is "no pair names this repository", which is every publish of every other repository and is
// silent.
func (p *Publisher) warnDeclined(subject, former string, pair Rename) {
	p.logger.Warn("github rename copy skipped: the subject under the former name is past the subject limit",
		slog.String("former", pair.Former),
		slog.String("current", pair.Current),
		slog.String("topic", subject),
		slog.Int("subject_bytes", len(subject)),
		slog.Int("copy_bytes", len(former)),
		slog.Int("subject_max_bytes", contracts.SubjectMaxBytes),
	)
}
