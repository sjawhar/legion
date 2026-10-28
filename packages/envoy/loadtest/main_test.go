package main

import "testing"

func TestSeriesSessions(t *testing.T) {
	for factor, want := range map[float64]int{0.25: 100, 0.5: 200, 1: 400, 2: 467, 5: 667, 10: 1000} {
		if got := seriesSessions(factor); got != want {
			t.Errorf("seriesSessions(%g) = %d, want %d", factor, got, want)
		}
	}
}
