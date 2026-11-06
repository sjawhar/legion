package docs

import (
	"context"
	"fmt"
	"math"
	"testing"
	"time"
)

// productionGoMemoryLimitBytes is the Go memory limit of Dispatch's production pod: 90 per cent of
// the 32Gi its container is given (theorem-labs/agent-infra dispatch/Pulumi.prod.yaml's appMemory,
// setGoMemoryLimit's ratio), 30,923,764,531 bytes, eight times the calibration and three bytes
// more.
const productionGoMemoryLimitBytes = 30_923_764_531

// The bounds at the calibration are the constants this package calibrated at 4 GiB, each exactly,
// so every figure measured and every drive written against them still stands: a pod of that size,
// and a test or a process that names no limit, runs under the same numbers as before the bounds
// were derived at all.
func TestTheMemoryBoundsAtTheCalibrationAreTodaysConstants(t *testing.T) {
	want := MemoryBounds{
		Limit:             3_865_470_566,
		RoomBudget:        roomBudgetBytes,
		DocumentReadCache: documentReadBudget,
		LoadSlots:         1,
		PeerWriteQueue:    peerWriteQueueBytes,
	}
	if calibratedGoMemoryLimitBytes != want.Limit {
		t.Fatalf("the calibration is %d bytes, want %d: 90 per cent of 4 GiB", int64(calibratedGoMemoryLimitBytes), want.Limit)
	}
	// The figures the comments and the drives quote, held here so a change to the arithmetic
	// that still agrees with the constants cannot move them unnoticed.
	if want.RoomBudget != 2_147_483_648 || want.DocumentReadCache != 268_435_456 || want.PeerWriteQueue != 73_400_320 {
		t.Fatalf("the constants are %+v, which is not the 4 GiB calibration this test and the drives are written against", want)
	}
	// What the limit leaves above the room budget and the cold reads' cache, three quarters of
	// which the load slots take: one slot of loadSlotBytes at the calibration, which is also
	// what keeps A3's arithmetic true now that an admission reserves nothing.
	if above := want.Limit - want.RoomBudget - want.DocumentReadCache; above != 1_449_551_462 || above*3/4/loadSlotBytes != 1 {
		t.Fatalf("the calibration leaves %d bytes above the room budget and the cold reads' cache, three quarters of which is %d slots: want 1,449,551,462 and one slot",
			above, above*3/4/loadSlotBytes)
	}
	if got := BoundsForLimit(calibratedGoMemoryLimitBytes); got != want {
		t.Errorf("the bounds at the calibration are %+v, want %+v", got, want)
	}
}

// At the production pod's limit the room budget is that same share of it, 16 GiB and a byte, and
// the load slots are what three quarters of what the limit leaves above the budget and the cache
// divides by loadSlotBytes: sixteen of them, where the calibration has one. The cold reads' cache
// and the write-queue budget are the two bounds that do not follow the limit.
func TestTheMemoryBoundsAtTheProductionPodsLimit(t *testing.T) {
	want := MemoryBounds{
		Limit:             productionGoMemoryLimitBytes,
		RoomBudget:        17_179_869_185,
		DocumentReadCache: 268_435_456,
		LoadSlots:         16,
		PeerWriteQueue:    62_914_560,
	}
	got := BoundsForLimit(productionGoMemoryLimitBytes)
	if got != want {
		t.Fatalf("the bounds at %d bytes are %+v, want %+v", int64(productionGoMemoryLimitBytes), got, want)
	}
	above := got.Limit - got.RoomBudget - got.DocumentReadCache
	if above != 13_475_459_890 || above*3/4 != 10_106_594_917 {
		t.Fatalf("the production limit leaves %d bytes above the room budget and the cache, three quarters of it %d: want 13,475,459,890 and 10,106,594,917",
			above, above*3/4)
	}
	// The quarter left out is the collector's and the request path's: the slots account for no
	// more than three quarters of what the limit leaves, and the rooms and the cache for the rest.
	if held := got.RoomBudget + got.DocumentReadCache + int64(got.LoadSlots)*loadSlotBytes; held > got.Limit {
		t.Errorf("the room budget, the cache and %d slots of %d come to %d, past the %d-byte limit", got.LoadSlots, int64(loadSlotBytes), held, got.Limit)
	}
}

// A limit the runtime holds none of (math.MaxInt64, which debug.SetMemoryLimit reports when
// nothing set one) and a caller that names none are the calibration: there is no figure to derive
// from, and the measured constants are what the service has always run under. So is a limit too
// small to leave anything above the room budget and the cache, which would otherwise leave no
// slot to load a document with.
func TestTheMemoryBoundsWithoutAReadableLimit(t *testing.T) {
	calibration := BoundsForLimit(calibratedGoMemoryLimitBytes)
	for _, row := range []struct {
		name  string
		limit int64
	}{
		{"no limit at all", math.MaxInt64},
		{"a caller that names none", 0},
		{"a negative limit", -1},
		{"a limit under the room budget and the cache", 512 << 20},
	} {
		t.Run(row.name, func(t *testing.T) {
			if got := BoundsForLimit(row.limit); got != calibration {
				t.Errorf("the bounds at %d are %+v, want the calibration's %+v", row.limit, got, calibration)
			}
		})
	}
}

// A service built at the production pod's limit admits rooms by that limit's budget, not by the
// calibration's: where the calibration admits twenty-six documents of a mebibyte of prose and
// refuses the twenty-seventh (TestTheRoomBudgetsArithmetic, which says why the last one is
// admitted over the limit), the production pod admits 203. This is the budget's own arithmetic,
// not a drive of the memory itself.
func TestAServiceAtTheProductionLimitAdmitsFarMoreRoomsThanTheCalibration(t *testing.T) {
	// A mebibyte of prose: a state of 3,035,240 bytes, the shape TestTheRoomBudgetsArithmetic
	// counts rooms of, charged at both copies.
	const stateBytes = 3_035_240
	weight := roomWeight(stateBytes)
	if weight != 84_986_720 {
		t.Fatalf("a mebibyte of prose is charged %d bytes, want 84,986,720: the shape this counts rooms of has moved", weight)
	}
	for _, row := range []struct {
		name   string
		limit  int64
		admits int64
	}{
		{"the calibration", calibratedGoMemoryLimitBytes, 26},
		{"the production pod's limit", productionGoMemoryLimitBytes, 203},
	} {
		t.Run(row.name, func(t *testing.T) {
			restorePodLoadSlots(t)
			service := New(Deps{GoMemoryLimit: row.limit})
			if service.budget.limit != BoundsForLimit(row.limit).RoomBudget {
				t.Fatalf("the service's budget holds %d bytes at a %d-byte limit, want %d",
					service.budget.limit, row.limit, BoundsForLimit(row.limit).RoomBudget)
			}
			admitted := int64(0)
			for {
				// Deadline in the past: the budget answers from what it holds now rather
				// than waiting for sweeps that have nothing to close.
				if _, err := service.budget.refusal(context.Background(), time.Now(), "room", 0); err != nil {
					break
				}
				service.budget.load(fmt.Sprintf("room-%d", admitted), stateBytes)
				admitted++
			}
			t.Logf("a budget of %d bytes admits %d rooms of a mebibyte of prose at %d bytes each",
				service.budget.limit, admitted, weight)
			if admitted != row.admits {
				t.Errorf("the budget admitted %d such rooms, want %d", admitted, row.admits)
			}
			if admitted <= 26 && row.limit == productionGoMemoryLimitBytes {
				t.Errorf("the production pod admitted %d rooms, no more than the 4 GiB calibration's twenty-six", admitted)
			}
		})
	}
}

// restorePodLoadSlots gives the process its load concurrency back at the end of t, for a test
// that builds a service at a limit other than the calibration, which sizes that concurrency from
// its own (New).
func restorePodLoadSlots(t *testing.T) {
	t.Helper()
	previous := podLoadSlots.Load()
	t.Cleanup(func() { podLoadSlots.Store(previous) })
}
