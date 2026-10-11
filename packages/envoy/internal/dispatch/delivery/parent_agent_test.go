package delivery

import "testing"

func TestParentAgentsValue(t *testing.T) {
	planners := SessionGroup{ID: "11111111-1111-1111-1111-111111111111", Name: "planner", Sessions: []string{"s1", "s2"}}
	agents := NewParentAgents([]SessionGroup{planners}, map[string]string{
		"s1": "Planner",
		"s3": "Planner",
		"s4": "Reviewer",
		"s5": "group:" + planners.ID,
	})
	for session, want := range map[string]string{
		// A member is its group, whatever its own title.
		"s1": "group:" + planners.ID,
		"s2": "group:" + planners.ID,
		// A session outside every group shows by its title, or its id when it has none.
		"s4": "Reviewer",
		"s9": "s9",
		// A session outside the group whose title is the group's name carries its id beside it.
		"s3": "Planner (s3)",
		// Only a group's name earns the id; a title that spells a group's value does not.
		"s5": "group:" + planners.ID,
	} {
		if got := agents.Value(session); got != want {
			t.Errorf("Value(%q) = %q, want %q", session, got, want)
		}
	}
}
