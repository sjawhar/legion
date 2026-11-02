package main

import (
	"github.com/sjawhar/envoy/internal/session"
	"github.com/sjawhar/envoy/internal/store"
)

// recipientCounts is how many sessions, other than sender, a message published on topic is for,
// as publishResponse carries the two numbers: recipients is every session that follows the topic
// and connected how many of those hold a session entry now, a pointer each so that a count of
// zero is sent as the number it is. Both are nil on a listener with no known-session store
// (listenerDeps.known, the degraded listener whose NATS grant does not yet open the bucket),
// whose count would be the live sessions alone; the publish route leaves both fields out of its
// answer then, rather than send a number that means something narrower than the one it names.
//
// recipients is the union of two stores, deduped by session id: the known-session records whose
// topics match the topic (session.KnownSessions.MatchEach), one per session that registered in
// the last 30 days, and the interest rows whose topics match it on any machine, which rows
// answers with. A session that registered through a listener of a release older than those
// records has a row and no record, and it is a recipient like any other, which is why the rows
// are in the union too.
//
// rows is a function because that walk is the expensive read here - every machine's subject tree,
// deduped and sorted - and because who makes it differs: an ad-hoc topic send walks the index
// once for the recipients it waits on and hands those same rows over (sendToTopic), while a
// publish on a topic the stream captures forwards to no one and has the walk made for the count
// alone. A listener with no known-session store calls it not at all, since it has no count to
// make.
//
// connected is how many of those sessions the session registry still holds an entry for
// (session.SessionRegistry.Get, whose entries carry a 5-minute TTL), and it is the number a reader
// acts on: against 30 days of records a team topic almost never counts zero, so recipients alone
// would never again tell a sender that nobody is there to read its message.
//
// Every read here is of memory: both matchers answer from their caches' subject indexes and the
// session registry from its cache, so counting makes no JetStream API call and the publish it
// precedes waits on no round trip of its own.
//
// sjawhar/legion#2603 deletes store.Registry.MatchAll, which both callers' rows walk; whichever
// of the two lands second reconciles them, since the all-machines walk itself lives in
// store.TopicMatcher.
func recipientCounts(d *listenerDeps, topic, sender string, rows func() []store.Interest) (recipients, connected *int) {
	if d.known == nil {
		return nil, nil
	}
	total, live := 0, 0
	counted := map[string]struct{}{}
	count := func(sessionID string) {
		if sessionID == sender {
			return
		}
		if _, dup := counted[sessionID]; dup {
			return
		}
		counted[sessionID] = struct{}{}
		total++
		if _, err := d.sessions.Get(sessionID); err == nil {
			live++
		}
	}
	d.known.MatchEach(topic, func(record session.KnownSession) { count(record.SessionID) })
	for _, interest := range rows() {
		count(interest.SessionID)
	}
	return &total, &live
}
