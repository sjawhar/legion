package retry

import (
	"testing"
	"time"
)

func TestDelayDoublesUpToTheMax(t *testing.T) {
	cases := []struct {
		attempts int
		want     time.Duration
	}{
		{1, time.Second},
		{2, 2 * time.Second},
		{3, 4 * time.Second},
		{10, MaxDelay},
		{100, MaxDelay},
	}
	for _, tc := range cases {
		if got := Delay(tc.attempts); got != tc.want {
			t.Errorf("Delay(%d) = %v, want %v", tc.attempts, got, tc.want)
		}
	}
}
