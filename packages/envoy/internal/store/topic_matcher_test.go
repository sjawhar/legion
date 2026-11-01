package store

import (
	"slices"
	"testing"
)

// matcherKeys is the sorted keys, each once, that m matches topic with.
func matcherKeys(m *TopicMatcher, topic string) []string {
	keys := []string{}
	m.Match(topic, func(key string) {
		if !slices.Contains(keys, key) {
			keys = append(keys, key)
		}
	})
	slices.Sort(keys)
	return keys
}

// The matcher answers a literal topic, a "*" pattern and a ">" pattern alike, over keys that carry
// no machine at all: the known-session records are keyed by session id.
func TestTopicMatcherMatchesLiteralStarAndRestPatterns(t *testing.T) {
	m := NewTopicMatcher()
	m.Set("ses_literal", []string{"notifications.team.deploy-squad"})
	m.Set("ses_star", []string{"notifications.github.*.legion.pr"})
	m.Set("ses_rest", []string{"notifications.github.>"})
	m.Set("ses_none", nil)

	for _, tc := range []struct {
		topic string
		want  []string
	}{
		{"notifications.team.deploy-squad", []string{"ses_literal"}},
		{"notifications.team.deploy-squad.extra", []string{}},
		{"notifications.github.sjawhar.legion.pr", []string{"ses_rest", "ses_star"}},
		{"notifications.github.other.legion.issue", []string{"ses_rest"}},
		{"notifications.dispatch.issue.INFRA-626.created", []string{}},
	} {
		if got := matcherKeys(m, tc.topic); !slices.Equal(got, tc.want) {
			t.Errorf("Match(%q) = %v, want %v", tc.topic, got, tc.want)
		}
	}
}

// A key's second Set replaces its topics: the topics it no longer carries match it no more, and
// a key visited by two of its own patterns is still one key to a caller that drops repeats.
func TestTopicMatcherReplacesAKeysTopics(t *testing.T) {
	m := NewTopicMatcher()
	m.Set("ses_moving", []string{"notifications.team.alpha", "notifications.team.>"})
	if got := matcherKeys(m, "notifications.team.alpha"); !slices.Equal(got, []string{"ses_moving"}) {
		t.Fatalf("before the replacement Match = %v, want the one key", got)
	}
	m.Set("ses_moving", []string{"notifications.team.beta"})
	if got := matcherKeys(m, "notifications.team.alpha"); !slices.Equal(got, []string{}) {
		t.Errorf("the replaced topics still match: %v", got)
	}
	if got := matcherKeys(m, "notifications.team.beta"); !slices.Equal(got, []string{"ses_moving"}) {
		t.Errorf("Match of the new topic = %v, want the one key", got)
	}
}

// Remove drops the key, and removing one the matcher never held changes nothing.
func TestTopicMatcherRemovesAKey(t *testing.T) {
	m := NewTopicMatcher()
	m.Set("ses_going", []string{"notifications.team.alpha"})
	m.Set("ses_staying", []string{"notifications.team.alpha"})
	m.Remove("ses_going")
	m.Remove("ses_never_here")
	if got := matcherKeys(m, "notifications.team.alpha"); !slices.Equal(got, []string{"ses_staying"}) {
		t.Errorf("Match after the removal = %v, want the key that stayed", got)
	}
	m.Remove("ses_staying")
	if got := matcherKeys(m, "notifications.team.alpha"); !slices.Equal(got, []string{}) {
		t.Errorf("Match after every key left = %v, want none", got)
	}
}
