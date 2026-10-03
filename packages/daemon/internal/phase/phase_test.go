package phase

import (
	"slices"
	"testing"
)

// All is the wire contract's phase list, in the transition table's order.
func TestPhaseValuesMatchWorkflowWireContract(t *testing.T) {
	want := []string{
		"admitted", "planning", "implementing", "testing", "reviewing", "retro", "merging",
		"awaiting_merge", "production_check", "done", "held",
	}
	got := make([]string, len(All))
	for index, value := range All {
		got[index] = string(value)
	}
	if !slices.Equal(got, want) {
		t.Fatalf("All = %q, want %q", got, want)
	}
}
