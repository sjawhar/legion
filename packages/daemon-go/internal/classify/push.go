package classify

import "time"

// HeadClock is the ordering clock for a PR-head observation.
type HeadClock struct {
	UpdatedAt time.Time `json:"updatedAt"`
	Source    string    `json:"source"`
}

// PushPayload keeps absent payload fields distinct from explicit empty strings.
type PushPayload struct {
	ChangedPaths          *string `json:"changed_paths,omitempty"`
	ChangedPathsTruncated *string `json:"changed_paths_truncated,omitempty"`
}

// PushClassification records whether a push changed only Legion handoff files.
type PushClassification struct {
	HandoffOnly bool   `json:"handoffOnly"`
	Unknown     string `json:"unknown,omitempty"`
}

// SupersededBy fences a stale GitHub observation. A complete GitHub read wins a same-clock tie
// over a webhook; two observations from the same source still apply in arrival order.
func SupersededBy(incoming, applied HeadClock) bool {
	if incoming.UpdatedAt.IsZero() || applied.UpdatedAt.IsZero() {
		return false
	}
	if incoming.UpdatedAt.Before(applied.UpdatedAt) {
		return true
	}
	if incoming.UpdatedAt.After(applied.UpdatedAt) {
		return false
	}
	return applied.Source == "resync" && incoming.Source == "webhook"
}

// ClassifyPush determines whether every supplied path is a Legion handoff file. The listener's
// truncation marker is authoritative because an empty changed-paths field is omitted upstream.
func ClassifyPush(p PushPayload) PushClassification {
	if p.ChangedPathsTruncated == nil {
		return PushClassification{
			Unknown: "changed_paths absent (listener predates LEGION-33)",
		}
	}
	switch *p.ChangedPathsTruncated {
	case "true":
		return PushClassification{Unknown: "changed_paths truncated at 100"}
	case "false":
		if p.ChangedPaths == nil || *p.ChangedPaths == "" {
			return PushClassification{Unknown: "no commits listed"}
		}
		for _, path := range splitLines(*p.ChangedPaths) {
			if len(path) < len(".legion/") || path[:len(".legion/")] != ".legion/" {
				return PushClassification{}
			}
		}
		return PushClassification{HandoffOnly: true}
	default:
		return PushClassification{Unknown: "changed_paths_truncated=" + *p.ChangedPathsTruncated + " unrecognised"}
	}
}

func splitLines(value string) []string {
	start := 0
	paths := make([]string, 0, 1)
	for index := range len(value) {
		if value[index] != '\n' {
			continue
		}
		paths = append(paths, value[start:index])
		start = index + 1
	}
	return append(paths, value[start:])
}
