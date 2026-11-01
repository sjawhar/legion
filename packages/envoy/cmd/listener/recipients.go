package main

// recipientsFor counts the sessions, other than sender, that a message published on topic is for:
// total is every session that follows the topic, and connected how many of those hold a session
// entry now. known is false on a listener with no known-session store (listenerDeps.known, the
// degraded listener whose NATS grant does not yet open the bucket), whose count would be the live
// sessions alone; the publish route leaves both fields out of its answer then, rather than send a
// number that means something narrower than the one it names.
//
// total is the union of two stores, deduped by session id: the known-session records whose topics
// match the topic (session.KnownSessions.Match), one per session that registered in the last 30
// days, and the interest rows whose topics match it on any machine (store.Registry.MatchAll). A
// session that registered through a listener of a release older than those records has a row and
// no record, and it is a recipient like any other, which is why the rows are in the union too.
//
// connected is how many of those sessions the session registry still holds an entry for
// (session.SessionRegistry.Get, whose entries carry a 5-minute TTL), and it is the number a reader
// acts on: against 30 days of records a team topic almost never counts zero, so total alone would
// never again tell a sender that nobody is there to read its message.
//
// Every read here is of memory: both matchers answer from their caches' subject indexes and the
// session registry from its cache, so counting makes no JetStream API call and the publish it
// precedes waits on no round trip of its own.
//
// sjawhar/legion#2603 deletes store.Registry.MatchAll; whichever of the two lands second
// reconciles this one call site, since the all-machines walk itself lives in store.TopicMatcher.
func recipientsFor(d *listenerDeps, topic, sender string) (total, connected int, known bool) {
	if d.known == nil {
		return 0, 0, false
	}
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
			connected++
		}
	}
	for _, record := range d.known.Match(topic) {
		count(record.SessionID)
	}
	for _, interest := range d.registry.MatchAll(topic) {
		count(interest.SessionID)
	}
	return total, connected, true
}

// recipientCounts is recipientsFor's answer as publishResponse carries it: a pointer each, so a
// count of zero is sent as the number it is, and both nil on a listener with no known-session
// store, whose answer then holds neither field.
func recipientCounts(d *listenerDeps, topic, sender string) (recipients, connected *int) {
	total, live, known := recipientsFor(d, topic, sender)
	if !known {
		return nil, nil
	}
	return &total, &live
}
