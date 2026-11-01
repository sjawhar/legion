package intake

import (
	"encoding/json"
	"fmt"

	"github.com/sjawhar/legion/daemon/internal/dispatch"
)

// LabelSetter is the kind of the Dispatch actor (`user`, `session` or `system`) that added
// dispatch.LegionLabel to an issue, read from its Dispatch event log as dispatch.Client.EventsAfter
// serves it from sequence 0, oldest first. The setter is the actor of the newest issue.created or
// issue.updated event whose labels carry the label (dispatch.CarriesLegionLabel, in any case) while
// the previous such event's did not; an issue.created that carries it counts as adding it. Only
// those two event types move the label state: every other event (a comment, a claim, a review,
// an issue.closed) is passed over and counts for nothing, so a person's comment between a
// session's label and a person's later unrelated edit leaves the session the setter, and a label
// removed and set again names whoever set it last. It is "" when no event added the label. An
// event that does not decode as a Dispatch event, or whose issue payload does not decode, is an
// error naming its position, since a log the daemon cannot read names no setter it can act on.
//
// The daemon reads the log for this because an unrecorded issue has no earlier state in the
// daemon: the event that reaches it says the label is there, not who put it there (DispatchIssue.
// LabelSetter).
func LabelSetter(events []json.RawMessage) (string, error) {
	setter, labelled := "", false
	for i, raw := range events {
		var event dispatchEvent
		if err := json.Unmarshal(raw, &event); err != nil {
			return "", fmt.Errorf("decode Dispatch event %d of %d: %w", i+1, len(events), err)
		}
		if event.Type != "issue.created" && event.Type != "issue.updated" {
			continue
		}
		var issue struct {
			Labels []string `json:"labels"`
		}
		if len(event.Payload) > 0 {
			if err := json.Unmarshal(event.Payload, &issue); err != nil {
				return "", fmt.Errorf("decode Dispatch issue payload of event %d of %d: %w", i+1, len(events), err)
			}
		}
		now := dispatch.CarriesLegionLabel(issue.Labels)
		if now && !labelled {
			setter = event.Actor.Kind
		}
		labelled = now
	}
	return setter, nil
}
