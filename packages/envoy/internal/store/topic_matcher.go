package store

// TopicMatcher indexes a key's subscribed topics and reports every key whose topics match a
// published topic, on any machine. It is the interest rows' own subject index (subjectIndex),
// exported for the known-session records (internal/session), which are keyed by session id and
// have no machine: a publish must count up to ~100,000 of them per request, which a linear scan
// over the records cannot answer, while the index follows the published topic's tokens and the
// patterns that share them. Its wildcard rules are routing.Match's, as subjectIndex's are.
//
// sjawhar/legion#2603 deletes matchAll, the all-machines walk this type calls, and narrows the
// interest registry to a machine's own tree. Whichever of the two lands second restores that walk
// here, in this file, rather than in Registry: the known-session records are what need it, and
// Registry has no caller for it once #2603 lands.
//
// A TopicMatcher is not safe for concurrent use: its one caller, session.KnownSessions, keeps it
// in step with its record map under that store's own lock.
type TopicMatcher struct {
	index *subjectIndex
}

// NewTopicMatcher returns an empty matcher.
func NewTopicMatcher() *TopicMatcher {
	return &TopicMatcher{index: newSubjectIndex()}
}

// Set indexes key under topics, replacing whatever topics key was indexed under. An empty or nil
// topics list leaves key matching nothing, as a record with no topic does.
func (m *TopicMatcher) Set(key string, topics []string) {
	m.index.set(key, Interest{Topics: topics})
}

// Remove drops key from the index, and every node it alone kept.
func (m *TopicMatcher) Remove(key string) {
	m.index.remove(key)
}

// Match calls visit with each key whose topics match topic; a key with several matching topics is
// visited once per matching topic, so a caller that counts keys drops the repeats (Registry's
// matchLocked does the same).
func (m *TopicMatcher) Match(topic string, visit func(key string)) {
	m.index.matchAll(topic, visit)
}
