package docs

import (
	"math"
	"math/bits"
)

// MemoryBounds are the memory bounds one document service runs under, derived from the Go memory
// limit its process holds (BoundsForLimit). Every figure in this package that bounds memory is
// calibrated at one limit, calibratedGoMemoryLimitBytes, the 90 per cent of 4 GiB the deployment
// gave Dispatch when they were measured; a process whose limit is another figure runs under that
// calibration scaled to its own limit, so a pod of 32Gi holds rooms in proportion to its memory
// rather than refusing the thirty-eighth mebibyte of documents with tens of gigabytes free.
//
// cmd/dispatch derives them once at boot from the limit the runtime holds after setGoMemoryLimit
// (debug.SetMemoryLimit(-1), so an operator's GOMEMLIMIT is followed as well as the container's
// limit), names each in a line of its own and hands the limit to the service
// (Deps.GoMemoryLimit). The service reads them for its room budget, the pressure reserve its
// full-budget rule allows, its apply-heap bound, the pod's copy-heap bound and ygo's per-peer
// write-queue budget (New); a service built without a limit runs at the calibration, which is
// what every test of this package and every memory drive of cmd/dispatch measures.
type MemoryBounds struct {
	// Limit is the Go memory limit the bounds were derived from: what the process holds, or
	// calibratedGoMemoryLimitBytes where it holds none.
	Limit int64
	// RoomBudget is the most heap the open rooms are charged together (roomBudget,
	// roomBudgetBytes at the calibration): the limit's share of it, 2 GiB of 3,865,470,566.
	RoomBudget int64
	// DocumentReadCache is the most weight the cold reads' rendering cache holds
	// (documentReadBudget). It does not scale: it is a cache of whole renderings, each at most
	// pmdoc.MaxDocumentBytes, and 256 MiB of them is what a request path's reads reuse.
	DocumentReadCache int64
	// PressureReserve is how far past the room budget's limit the charges of the delete-only
	// updates admitted while the budget is full may take it (refusedUnderPressure,
	// pressureReserveBytes at the calibration): the limit's share of it, 256 MiB of
	// 3,865,470,566, since what fills it is a share of the rooms the budget holds.
	PressureReserve int64
	// ApplyHeap is the most estimated heap the weighed applies of browsers' updates hold at once
	// (applyHeaps, applyHeapBound at the calibration): a third of what the limit leaves above
	// the room budget, the cold reads' cache and the pressure reserve.
	ApplyHeap int64
	// CopyHeap is the most the pod's full-state decodes are granted at once (copyHeaps,
	// copyHeapBound at the calibration): the third of that remainder that neither ApplyHeap nor
	// HeapSlack takes, so that every byte of it has a named holder.
	CopyHeap int64
	// HeapSlack is the third of that remainder neither bound grants (heapSlackBytes at the
	// calibration): the request path's, the applies under unweighedApplyHeapBytes, the
	// measurer's parse, and the headroom the collector needs under the limit. Nothing reads it;
	// it is named so the boot line accounts for the whole limit.
	HeapSlack int64
	// ApplyTooHeavy is the estimate past which an update is refused as too heavy to apply
	// (errApplyTooHeavy, applyTooHeavyBytes at the calibration): all of that remainder, both
	// bounds and the slack together, so no apply is refused that could run with every other
	// weighed heap at rest.
	ApplyTooHeavy int64
	// PeerWriteQueue is ygo's Server.PeerWriteQueueBytes, the bytes of frames one browser's
	// socket may hold queued (peerWriteQueueBytes). It does not scale: see that constant.
	PeerWriteQueue int64
}

// BoundsForLimit is the bounds a Go memory limit of limit gives the document service. It is pure,
// so a test builds a service at the limit it wants to weigh.
//
// At the calibration every figure is the constant it is calibrated as, exactly, which
// TestTheMemoryBoundsAtTheCalibrationAreTodaysConstants holds it to. A limit of math.MaxInt64 is
// the runtime's "no limit", and zero is a caller that names none (Deps.GoMemoryLimit): both run
// at the calibration, since a process with no limit has no figure to derive from.
//
// A limit so small that it leaves nothing above the room budget, the cold reads' cache and the
// pressure reserve - under about 716 MB, less than twice that cache - would make the three heap
// bounds zero or negative, which would refuse every apply rather than bound one. No Dispatch
// process runs in that memory whatever its bounds say, so those limits take the calibration too.
func BoundsForLimit(limit int64) MemoryBounds {
	if limit <= 0 || limit == math.MaxInt64 {
		limit = calibratedGoMemoryLimitBytes
	}
	bounds := MemoryBounds{
		Limit:             limit,
		RoomBudget:        scaledToLimit(limit, roomBudgetBytes),
		DocumentReadCache: documentReadBudget,
		PressureReserve:   scaledToLimit(limit, pressureReserveBytes),
		PeerWriteQueue:    peerWriteQueueBytes,
	}
	above := limit - bounds.RoomBudget - bounds.DocumentReadCache - bounds.PressureReserve
	if above <= 0 {
		return BoundsForLimit(calibratedGoMemoryLimitBytes)
	}
	bounds.ApplyHeap = above / 3
	bounds.HeapSlack = above / 3
	bounds.CopyHeap = above - bounds.ApplyHeap - bounds.HeapSlack
	bounds.ApplyTooHeavy = above
	return bounds
}

// calibratedBounds is the calibration itself, the bounds of a process holding
// calibratedGoMemoryLimitBytes: what a service no boot configured runs under (Service.bounds).
var calibratedBounds = BoundsForLimit(calibratedGoMemoryLimitBytes)

// scaledToLimit is part, a figure of the calibration, as the same share of limit:
// limit x part / calibratedGoMemoryLimitBytes, rounded down. The product passes int64 at any
// limit over about 4 GiB, so it is taken in 128 bits. bits.Div64 panics where the product's high
// word is at or past the divisor, or the quotient passes 64 bits; neither can happen here: every
// part scaled is at most the room budget's 2 GiB, so the product of any int64 limit and a part
// is under 2^94 and its high word under 2^30, below the calibration itself, and the quotient is
// under the limit.
func scaledToLimit(limit, part int64) int64 {
	hi, lo := bits.Mul64(uint64(limit), uint64(part))
	scaled, _ := bits.Div64(hi, lo, calibratedGoMemoryLimitBytes)
	return int64(scaled)
}
