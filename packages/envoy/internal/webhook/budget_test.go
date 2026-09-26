package webhook

import (
	"context"
	"errors"
	"testing"
)

// A request that cannot get room holds nothing the budget has not charged: its next buffer is
// allocated only once the charge succeeds, so requests waiting for room add nothing to memory.
func TestABodyReadWaitingForRoomAllocatesNothing(t *testing.T) {
	bodies := newBodyBudget(bodyReadStep)
	longest := bodies.begin()
	defer longest.release()
	if _, err := longest.grow(context.Background(), nil, bodyReadStep); err != nil {
		t.Fatalf("the longest holder's first buffer: %v", err)
	}
	waiting := bodies.begin()
	defer waiting.release()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	var err error
	allocations := testing.AllocsPerRun(10, func() {
		_, err = waiting.grow(ctx, nil, githubMaxBody)
	})

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("grow with a full budget and an ended context = %v, want context.Canceled", err)
	}
	if allocations != 0 {
		t.Fatalf("a read that could not get room allocated %.0f times, want none", allocations)
	}
}
