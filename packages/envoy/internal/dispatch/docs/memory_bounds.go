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
// (Deps.GoMemoryLimit). The service reads them for its room budget, the cold reads' cache, the
// pod's load concurrency and ygo's per-peer write-queue budget (New); a service built without a
// limit runs at the calibration, which is what every test of this package and every memory drive
// of cmd/dispatch measures.
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
	// LoadSlots is how many whole-document copies the pod builds at once (loadSlots): one slot
	// covers one whole-document decode, one live write's fork or one apply of an update over
	// unslottedApplyBytes, and a taker that finds every slot held waits for one before it takes
	// a database connection, its request's own deadline answering 503 (errWriteBusy). It is what
	// the limit leaves above the room budget and the cold reads' cache, three quarters of it,
	// divided by loadSlotBytes: 1 at the calibration and 16 at the 32Gi production limit
	// (30,923,764,531, whose 13,475,459,890 above the two leaves 10,106,594,917 to the slots).
	// The quarter left out is the collector's and the request path's.
	//
	// One slot at the calibration is not only a memory bound: it is what keeps A3's "the
	// refusals begin within one room of the budget's own arithmetic" true now that an admission
	// reserves nothing for the document it is about to load. With one cold load at a time the
	// budget sees each room's weight before the next load begins, so fifty admissions at once
	// serialise into one after another, each weighed against the rooms the ones before it left
	// charged, rather than fifty of them reading the same headroom at the same moment.
	LoadSlots int
	// PeerWriteQueue is ygo's Server.PeerWriteQueueBytes, the bytes of frames one browser's
	// socket may hold queued (peerWriteQueueBytes). It does not scale: see that constant.
	PeerWriteQueue int64
}

// loadSlotBytes is the heap one slot of the pod's load concurrency allows its holder: 600 MB, one
// whole-document decode, one fork or one heavy apply. The heaviest whole-document decode measured
// holds 320,762,656 bytes above what it leaves behind, the heaviest honest apply the decode
// budget allows is about 470 MB, and a fork is lighter than a decode, so a slot of this size
// covers whichever of the three takes it
// (dispatch://INFRA-179/artifact/infra179-simplify-measured-md).
const loadSlotBytes = 600_000_000

// BoundsForLimit is the bounds a Go memory limit of limit gives the document service. It is pure,
// so a test builds a service at the limit it wants to weigh, and it is the one place every bound
// is derived: what any other code reads of the calibration it reads from calibratedBounds.
//
// At the calibration every figure is the one this package's measurements were taken against,
// exactly, which TestTheMemoryBoundsAtTheCalibrationAreTodaysConstants holds it to. A limit of
// math.MaxInt64 is the runtime's "no limit", and zero is a caller that names none
// (Deps.GoMemoryLimit): both run at the calibration, since a process with no limit has no figure
// to derive from.
//
// What the limit leaves above the room budget and the cold reads' cache - 1,449,551,462 bytes at
// the calibration - the load slots take three quarters of, and the last quarter is left to the
// collector and the request path, which no bound of this package weighs.
//
// A limit so small that it leaves nothing above the room budget and the cold reads' cache - under
// about 2.4 GB - would leave no slot to load a document with. No Dispatch process runs in that
// memory whatever its bounds say, so those limits take the calibration too.
func BoundsForLimit(limit int64) MemoryBounds {
	if limit <= 0 || limit == math.MaxInt64 {
		limit = calibratedGoMemoryLimitBytes
	}
	bounds := MemoryBounds{
		Limit:             limit,
		RoomBudget:        scaledToLimit(limit, roomBudgetBytes),
		DocumentReadCache: documentReadBudget,
		PeerWriteQueue:    peerWriteQueueBytes,
	}
	above := limit - bounds.RoomBudget - bounds.DocumentReadCache
	if above <= 0 {
		return BoundsForLimit(calibratedGoMemoryLimitBytes)
	}
	bounds.LoadSlots = max(1, int(above*3/4/loadSlotBytes))
	return bounds
}

// calibratedGoMemoryLimitBytes is the Go memory limit every bound in this package is calibrated
// at, and the limit a service runs under when its process holds none: 90 per cent of a
// container's 4 GiB, 3,865,470,566 bytes, which is what Dispatch's deployment gave it while these
// figures were measured. The production pod is larger now - 32Gi, whose 90 per cent is
// 30,923,764,531 (setGoMemoryLimit, cmd/dispatch/main.go) - and a process holding another limit
// runs under this calibration scaled to that limit (BoundsForLimit), not under these figures.
const calibratedGoMemoryLimitBytes = 4 << 30 * 9 / 10

// calibratedBounds is the calibration itself, the bounds of a process holding
// calibratedGoMemoryLimitBytes: what a service no boot configured runs under, which is every test
// of this package's (calibratedService), and what every figure this package measured was measured
// against. It is the one place the calibrated bounds are named: no constant of this package
// derives one of them a second way.
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
