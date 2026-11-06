package docs

import (
	"context"
	"errors"
	"fmt"
	"runtime/debug"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// errLiveWriteMeasured ends the measurement's joined transaction without committing it, so the
// write's peak is the fork's own - its build, the trees the operation reads and the rendering it
// leaves - and not the publish's apply into the room or the room load a cold write's publish
// would make.
var errLiveWriteMeasured = errors.New("the live write was measured")

// liveWriteMeasurer is one shape measured by TestALiveWritesPeakHeapIsWithinItsSlot: the service
// and document a write is made on, with the state its store holds.
type liveWriteMeasurer struct {
	service    *Service
	artifactID string
	state      []byte
}

// measureLiveWrite runs one live write on the document and returns what it cost: the write is
// made inside a joined transaction that is then discarded, so what the measurement holds at its
// peak is the fork, the tree before the operation, the tree it leaves, the rendering of both and
// the update and version it appends - the whole life of the fork a grant of the copy-heap bound
// covers (liveWriteWeight) - and nothing of the publish.
//
// warm says the room must be resident before the write, so the fork is the replica cloned
// (Service.forkPoint); otherwise the room is closed and the fork is the stored state decoded
// (coldFork). find and with are the one replacement the write makes, a few bytes in one heading,
// so what is measured is the fork, the trees and the render rather than the change.
func (m *liveWriteMeasurer) measureLiveWrite(t *testing.T, warm bool, find, with string) loadCost {
	t.Helper()
	closeResidentRoom(t, m.service, m.artifactID)
	storeHistory(t, m.service, m.artifactID, m.state)
	if warm {
		if err := loadRoomOf(m.service, m.artifactID); err != nil {
			t.Fatalf("load the room: %v", err)
		}
		if m.service.srv.GetDoc(m.artifactID) == nil {
			t.Fatal("the room did not stay resident for the warm write")
		}
	} else if m.service.srv.GetDoc(m.artifactID) != nil {
		t.Fatal("the room is resident, so the write would fork warm")
	}
	var cost loadCost
	err := joinedWrite(m.service, func(joined context.Context) error {
		cost = func() loadCost {
			defer debug.SetGCPercent(debug.SetGCPercent(copyMeasurementGCPercent))
			return measureLoad(t, func() any {
				outcome, err := m.service.ApplyOps(joined, m.artifactID,
					[]model.EditOp{{Op: "replace", Find: find, With: with}},
					model.Actor{Kind: "user", ID: "alice"}, nil)
				if err != nil {
					t.Fatalf("the measured write: %v", err)
				}
				return outcome
			})
		}()
		return errLiveWriteMeasured
	})
	if !errors.Is(err, errLiveWriteMeasured) {
		t.Fatalf("the measured write's transaction: %v", err)
	}
	return cost
}

// What a live write holds at its peak is what its grant of the copy-heap bound is weighed for
// (liveWriteHeapPerByte): the fork, the tree the operation starts from, the tree it leaves, the
// renderings of both and the update and version it appends, all of which the write holds at once
// while it weighs what it changed (applyLive). It measures both ways a fork is built - the
// replica cloned for a write to a resident room (Service.forkPoint) and the stored state decoded
// for a write to a document no room holds (coldFork) - at GOMAXPROCS=1 with the collector at
// copyMeasurementGCPercent, so the sampled peak is what is live (measureLoad).
//
// It runs in a process of its own (measuredAlone), as the room's heap prices do: a whole run of
// this package holds a gigabyte or more of other rooms, settlements and their garbage by the
// time this runs, and what they hold and free while the sampler reads the heap every
// millisecond is counted into the peak - 154,327,768 bytes for the cloned mebibyte inside the
// package's run on 2026-11-05 against 67,103,336 in a process of its own.
func TestALiveWritesPeakHeapIsWithinItsSlot(t *testing.T) {
	skipUnderRace(t)
	if !measuredAlone(t) {
		return
	}
	for _, size := range []int{256 << 10, 1 << 20} {
		t.Run(fmt.Sprintf("%d bytes of markdown", size), func(t *testing.T) {
			state, _ := residentRoomState(t, size)
			service, artifactID := newTestService(t)
			service.settle = time.Hour
			measurer := &liveWriteMeasurer{service: service, artifactID: artifactID, state: state}
			for _, path := range []struct {
				name string
				warm bool
			}{{"the replica cloned", true}, {"the stored state decoded", false}} {
				cost := measurer.measureLiveWrite(t, path.warm, "Section 0", "Section Zero")
				kept := max(cost.held, 0)
				t.Logf("%d-byte state, a live write whose fork is %s: %s, %d bytes allocated, %d peak (%.1f a byte of state), %d kept (%.1f a byte), against a %d-byte slot",
					len(state), path.name, cost.took.Round(time.Millisecond), cost.allocated,
					cost.peak, float64(cost.peak)/float64(len(state)), kept, float64(kept)/float64(len(state)), loadSlotBytes)
				if int64(cost.peak) > loadSlotBytes {
					t.Errorf("a live write whose fork is %s holds %d bytes at its peak, %.1f a byte of its %d-byte state, past the %d bytes one load slot allows",
						path.name, cost.peak, float64(cost.peak)/float64(len(state)), len(state), loadSlotBytes)
				}
			}
		})
	}
}
