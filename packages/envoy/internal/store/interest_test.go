package store

import "testing"

func TestMergeTopics(t *testing.T) {
	item := Interest{Topics: []string{"notifications.github.>", "notifications.slack.*.*.mention"}}
	next := Merge(item, []string{"notifications.slack.*.*.mention", "notifications.agent.s1"})
	if len(next.Topics) != 3 {
		t.Fatalf("expected 3 topics, got %d", len(next.Topics))
	}
}

func TestRemoveTopics(t *testing.T) {
	item := Interest{Topics: []string{"a", "b", "c"}}
	next := Remove(item, []string{"b"})
	if len(next.Topics) != 2 {
		t.Fatalf("expected 2 topics, got %d", len(next.Topics))
	}
	if next.Topics[0] != "a" || next.Topics[1] != "c" {
		t.Fatalf("unexpected topics: %#v", next.Topics)
	}
	clear := Remove(item, nil)
	if len(clear.Topics) != 0 {
		t.Fatalf("expected full removal, got %#v", clear.Topics)
	}
}
