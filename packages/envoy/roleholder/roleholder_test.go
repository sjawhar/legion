package roleholder

import "testing"

// The listener's no-holder reasons are exactly the two its 404 and its refusal exception name,
// and nothing else is one: the delivery path's reasons in particular are failures after a holder
// was resolved, which the daemon handles otherwise.
func TestIsNoHolderReasonAcceptsTheListenersTwoRefusalsAlone(t *testing.T) {
	for reason, want := range map[string]bool{
		"unclaimed":       true,
		"holder_lapsed":   true,
		"no_holder":       false,
		"delivery_failed": false,
		"receipt_timeout": false,
		"live":            false,
		"":                false,
		"Unclaimed":       false,
	} {
		if got := IsNoHolderReason(reason); got != want {
			t.Errorf("IsNoHolderReason(%q) = %t, want %t", reason, got, want)
		}
	}
}
