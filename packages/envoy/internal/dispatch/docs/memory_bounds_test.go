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
		PressureReserve:   pressureReserveBytes,
		ApplyHeap:         applyHeapBound,
		CopyHeap:          copyHeapBound,
		HeapSlack:         heapSlackBytes,
		ApplyTooHeavy:     applyTooHeavyBytes,
		PeerWriteQueue:    peerWriteQueueBytes,
	}
	if calibratedGoMemoryLimitBytes != want.Limit {
		t.Fatalf("the calibration is %d bytes, want %d: 90 per cent of 4 GiB", int64(calibratedGoMemoryLimitBytes), want.Limit)
	}
	// The figures the comments and the drives quote, held here so a change to the arithmetic
	// that still agrees with the constants cannot move them unnoticed.
	if want.RoomBudget != 2_147_483_648 || want.DocumentReadCache != 268_435_456 || want.PressureReserve != 268_435_456 ||
		want.ApplyHeap != 393_705_335 || want.CopyHeap != 393_705_336 || want.HeapSlack != 393_705_335 ||
		want.ApplyTooHeavy != 1_181_116_006 || want.PeerWriteQueue != 62_914_560 {
		t.Fatalf("the constants are %+v, which is not the 4 GiB calibration this test and the drives are written against", want)
	}
	if got := BoundsForLimit(calibratedGoMemoryLimitBytes); got != want {
		t.Errorf("the bounds at the calibration are %+v, want %+v", got, want)
	}
}

// At the production pod's limit every bound that follows the limit is that same share of it: the
// room budget 16 GiB and a byte, the pressure reserve 2 GiB, and the three parts of what is left
// above them a third each of 11,327,976,242. The cold reads' cache and the write-queue budget are
// the two that do not follow it.
func TestTheMemoryBoundsAtTheProductionPodsLimit(t *testing.T) {
	want := MemoryBounds{
		Limit:             productionGoMemoryLimitBytes,
		RoomBudget:        17_179_869_185,
		DocumentReadCache: 268_435_456,
		PressureReserve:   2_147_483_648,
		ApplyHeap:         3_775_992_080,
		CopyHeap:          3_775_992_082,
		HeapSlack:         3_775_992_080,
		ApplyTooHeavy:     11_327_976_242,
		PeerWriteQueue:    62_914_560,
	}
	got := BoundsForLimit(productionGoMemoryLimitBytes)
	if got != want {
		t.Fatalf("the bounds at %d bytes are %+v, want %+v", int64(productionGoMemoryLimitBytes), got, want)
	}
	if sum := got.ApplyHeap + got.CopyHeap + got.HeapSlack; sum != got.ApplyTooHeavy {
		t.Errorf("the three parts above the reserve come to %d, want all of the %d the limit leaves", sum, got.ApplyTooHeavy)
	}
	if whole := got.RoomBudget + got.DocumentReadCache + got.PressureReserve + got.ApplyTooHeavy; whole != got.Limit {
		t.Errorf("the bounds account for %d bytes of the %d-byte limit: every byte of it has a named holder", whole, got.Limit)
	}
}

// A limit the runtime holds none of (math.MaxInt64, which debug.SetMemoryLimit reports when
// nothing set one) and a caller that names none are the calibration: there is no figure to derive
// from, and the measured constants are what the service has always run under. So is a limit too
// small to leave anything above the room budget, the cache and the reserve, which would otherwise
// make the three heap bounds zero and refuse every apply.
func TestTheMemoryBoundsWithoutAReadableLimit(t *testing.T) {
	calibration := BoundsForLimit(calibratedGoMemoryLimitBytes)
	for _, row := range []struct {
		name  string
		limit int64
	}{
		{"no limit at all", math.MaxInt64},
		{"a caller that names none", 0},
		{"a negative limit", -1},
		{"a limit under the cache and the reserve", 512 << 20},
	} {
		t.Run(row.name, func(t *testing.T) {
			if got := BoundsForLimit(row.limit); got != calibration {
				t.Errorf("the bounds at %d are %+v, want the calibration's %+v", row.limit, got, calibration)
			}
		})
	}
}

// A service built at the production pod's limit admits rooms by that limit's budget, not by the
// calibration's: where the calibration admits thirty-seven documents of a mebibyte of prose and
// refuses the thirty-eighth (TestTheRoomBudgetsArithmetic), the production pod admits 298 of them.
// This is the budget's own arithmetic, the same reservation a socket's admission makes
// (storedRoomReservation), not a drive of the memory itself.
func TestAServiceAtTheProductionLimitAdmitsFarMoreRoomsThanTheCalibration(t *testing.T) {
	// A mebibyte of prose: 57,600 elements from 77,600 structs, charged at both copies, the
	// shape TestTheRoomBudgetsArithmetic counts rooms of.
	weight := roomWeight(copyHeap(1<<20, 57_600, 77_600), 0, 0)
	if weight != 57_600_000 {
		t.Fatalf("a mebibyte of prose reserves %d bytes, want 57,600,000: the shape this counts rooms of has moved", weight)
	}
	for _, row := range []struct {
		name   string
		limit  int64
		admits int64
	}{
		{"the calibration", calibratedGoMemoryLimitBytes, 37},
		{"the production pod's limit", productionGoMemoryLimitBytes, 298},
	} {
		t.Run(row.name, func(t *testing.T) {
			restorePodCopyHeaps(t)
			service := New(Deps{GoMemoryLimit: row.limit})
			if service.budget.limit != BoundsForLimit(row.limit).RoomBudget {
				t.Fatalf("the service's budget holds %d bytes at a %d-byte limit, want %d",
					service.budget.limit, row.limit, BoundsForLimit(row.limit).RoomBudget)
			}
			admitted := int64(0)
			for {
				room := fmt.Sprintf("room-%d", admitted)
				// Deadline in the past: the budget answers from what it holds now rather
				// than waiting for sweeps that have nothing to close.
				reservation, err := service.budget.reserve(context.Background(), time.Now(), room, weight)
				if err != nil {
					break
				}
				if reservation == nil {
					t.Fatalf("room %d was admitted without a reservation", admitted+1)
				}
				admitted++
			}
			t.Logf("a budget of %d bytes admits %d rooms of a mebibyte of prose at %d bytes each",
				service.budget.limit, admitted, weight)
			if admitted != row.admits {
				t.Errorf("the budget admitted %d such rooms, want %d", admitted, row.admits)
			}
			if admitted <= 37 && row.limit == productionGoMemoryLimitBytes {
				t.Errorf("the production pod admitted %d rooms, no more than the 4 GiB calibration's thirty-seven", admitted)
			}
		})
	}
}

// restorePodCopyHeaps gives the process its copy-heap bound back at the end of t, for a test that
// builds a service at a limit other than the calibration, which sizes that bound from its own
// (New).
func restorePodCopyHeaps(t *testing.T) {
	t.Helper()
	previous := podCopyHeaps.Load()
	t.Cleanup(func() { podCopyHeaps.Store(previous) })
}
