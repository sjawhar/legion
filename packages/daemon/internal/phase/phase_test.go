package phase

import "testing"

func TestPhaseValuesMatchWorkflowWireContract(t *testing.T) {
	want := map[Phase]string{
		Admitted:        "admitted",
		Planning:        "planning",
		Implementing:    "implementing",
		Testing:         "testing",
		Reviewing:       "reviewing",
		Retro:           "retro",
		Merging:         "merging",
		AwaitingMerge:   "awaiting_merge",
		ProductionCheck: "production_check",
		Done:            "done",
		Held:            "held",
	}
	for value, text := range want {
		if string(value) != text {
			t.Fatalf("phase %q = %q, want %q", value, value, text)
		}
	}
}
