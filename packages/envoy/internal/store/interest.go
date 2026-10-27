package store

import "encoding/json"

type Interest struct {
	SessionID string   `json:"session_id"`
	MachineID string   `json:"machine_id"`
	Dir       string   `json:"dir"`
	Topics    []string `json:"topics"`
	UpdatedAt int64    `json:"updated_at"`

	// unknown holds the JSON of every key a stored interest carries that Interest does not name: a
	// field a newer build writes, which encodeInterest writes back so a heartbeat through this build
	// keeps it. It is never modified once decoded, so copies of an interest may share it.
	unknown map[string]json.RawMessage
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
