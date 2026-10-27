package classify

// PushPayload keeps absent payload fields distinct from explicit empty strings.
type PushPayload struct {
	ChangedPaths          *string `json:"changed_paths,omitempty"`
	ChangedPathsTruncated *string `json:"changed_paths_truncated,omitempty"`
}

// PushClassification records whether a push changed only the issue's own Legion handoff files.
type PushClassification struct {
	HandoffOnly bool   `json:"handoffOnly"`
	Unknown     string `json:"unknown,omitempty"`
}

// HandoffPath reports whether path is one of issue's own handoff files, under .legion/<issue>/,
// the directory each phase's role writes its handoff to and retro's last commit removes. Another
// issue's handoffs are not issue's: its reviewer never read them and its retro does not remove
// them, so READY refuses them as it refuses code (api's unreviewedChange), and a push changing them
// is a code push here too. No issue has no handoffs.
func HandoffPath(issue, path string) bool {
	prefix := ".legion/" + issue + "/"
	return issue != "" && len(path) >= len(prefix) && path[:len(prefix)] == prefix
}

// ClassifyPush determines whether every supplied path is one of issue's own handoff files
// (HandoffPath), issue being the key the pushed branch, legion/<issue>, names. The listener's
// truncation marker is authoritative because an empty changed-paths field is omitted upstream.
func ClassifyPush(p PushPayload, issue string) PushClassification {
	if p.ChangedPathsTruncated == nil {
		return PushClassification{
			Unknown: "changed_paths absent (listener predates LEGION-33)",
		}
	}
	switch *p.ChangedPathsTruncated {
	case "true":
		return PushClassification{Unknown: "changed_paths truncated"}
	case "false":
		if p.ChangedPaths == nil || *p.ChangedPaths == "" {
			return PushClassification{Unknown: "no commits listed"}
		}
		for _, path := range splitLines(*p.ChangedPaths) {
			if !HandoffPath(issue, path) {
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
