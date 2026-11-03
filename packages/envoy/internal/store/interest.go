package store

import (
	"slices"

	"github.com/sjawhar/envoy/internal/bus"
)

type Interest struct {
	SessionID string   `json:"session_id"`
	MachineID string   `json:"machine_id"`
	Dir       string   `json:"dir"`
	Topics    []string `json:"topics"`
	UpdatedAt int64    `json:"updated_at"`
}

func Merge(item Interest, topics []string) Interest {
	seen := map[string]bool{}
	out := make([]string, 0, len(item.Topics)+len(topics))
	for _, topic := range item.Topics {
		if seen[topic] {
			continue
		}
		seen[topic] = true
		out = append(out, topic)
	}
	for _, topic := range topics {
		if seen[topic] {
			continue
		}
		seen[topic] = true
		out = append(out, topic)
	}
	item.Topics = out
	return item
}

func Remove(item Interest, topics []string) Interest {
	if len(topics) == 0 {
		item.Topics = nil
		return item
	}
	cut := map[string]bool{}
	for _, topic := range topics {
		cut[topic] = true
	}
	out := make([]string, 0, len(item.Topics))
	for _, topic := range item.Topics {
		if cut[topic] {
			continue
		}
		out = append(out, topic)
	}
	item.Topics = out
	return item
}

// dropPrivateStreamTopics removes from topics every one reaching a private stream subject other
// than sessionID's own agent subject (bus.ReachesPrivateStreamSubject): another session's inbox, a
// failed-delivery lane, the Legion controller lane, or a wildcard overlapping one. Registry.Match
// fans a stored topic out to its serve-port session, so such a topic would hand that session
// messages meant for another session or for the controller's role holder. The listener refuses
// one at /v1/interests/subscribe; this drops one a row stored before that guard holds, so every
// write of a row (Upsert, Remove) cleans it. sessionID is the row's key, not the session_id its
// stored value names. It filters topics in place, so callers pass a slice they own (Merge's and
// Remove's results are).
func dropPrivateStreamTopics(sessionID string, topics []string) []string {
	return slices.DeleteFunc(topics, func(topic string) bool {
		return bus.ReachesPrivateStreamSubject(sessionID, topic)
	})
}
