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
	// ApplyHeap is the most estimated heap (peerWeighing.applyHeap) the weighed applies of
	// browsers' updates hold at once (applyHeaps): a third of what the limit leaves above the
	// room budget, the cold reads' cache and the pressure reserve, 393,705,335 bytes at the
	// calibration, about 375 MiB. A weighed apply whose estimate passes it is never refused for
	// it: it waits for the whole bound and then holds all of it, so it runs alone
	// (weighedHeaps.hold), as a copy past CopyHeap does. Only an estimate past ApplyTooHeavy is
	// refused (errApplyTooHeavy).
	//
	// It is not in the spec: it is the plan's departure 8, large applies taking turns, weighed by
	// each apply's estimate rather than counted in slots. Every browser's first sync hands over
	// its whole state, and ygo's apply decodes every struct of it, and every value of each,
	// before decodeAndPark drops what the room already holds (sjawhar/ygo v1.54.1-sami.3,
	// crdt/update.go:807-942, the values at :1222-1263), so fifty browsers reconnecting at once
	// to rooms of heavy documents would otherwise decode fifty handoffs at the same moment. A
	// count of slots sized for the heaviest handoff could not bound an update the editor never
	// sends, whose apply allocates in proportion to the values it carries whatever the room
	// holds: 25,165,779 bytes of null values in one struct for a client whose every clock the
	// room held allocated 805,307,736 bytes in one apply, though it inserted nothing and added no
	// struct. Measured on 2026-10-28 (TestEveryApplysTransientHeapIsWithinItsEstimate), the
	// 5,360,879-byte handoff of the heaviest document the server's own writes can leave is
	// estimated at 71,356,658 bytes into the room holding it, against the 33,410,168 it allocates
	// there, so five such handoffs apply at once, and at 234,999,499 into a fresh room, whose
	// budget gives it the per-frame ceiling (perFrameScanCeiling) for the 49,156 items decoding a
	// whole document into one holding nothing scans, against the 113,178,024 its apply allocates
	// there, so one applies at a time. The 9,577,606-byte handoff of that document grown through
	// the creep allowance (maxMessageBytes,
	// TestTheHeaviestWholeStateFrameABrowserCanGrowFitsTheMessageFloor) is estimated at
	// 157,797,184 into the room holding it, so two apply at once, and at 482,930,134 into a fresh
	// room, past the bound, so it runs alone.
	ApplyHeap int64
	// CopyHeap is the most the pod's full-state decodes are granted at once (copyHeaps): the
	// third of that remainder that neither ApplyHeap nor HeapSlack takes, so that every byte of
	// it has a named holder, 393,705,336 bytes at the calibration, about 375 MiB.
	//
	// Every load of a document at or near the decode cap measured holds less above what it leaves
	// than the whole bound: the heaviest load measured held 303,430,664 bytes live beside its two
	// copies (copyHeapPerByte), and the heaviest decode of any path 320,762,656, 72,942,680 under
	// the calibration's bound. Weighed at copyHeapPerByte, a mebibyte of prose (145,691,520
	// bytes) decodes two at a time there; a run of a million keystrokes (383,220,432) one at a
	// time; the agent-rewritten and long-edited documents at the decode cap weigh more than the
	// whole bound (602,726,112 and 692,509,680), and each waits for all of it and runs alone,
	// never refused, since a stored document must always load (heapGrant.cover).
	CopyHeap int64
	// HeapSlack is the third of that remainder neither bound grants, 393,705,335 bytes at the
	// calibration: the slack for what no bound weighs - the request path, the applies of
	// browsers' updates under unweighedApplyHeapBytes, one for each browser's read loop at most,
	// the measurer's parse of a rendering, up to about 130 MiB for a mebibyte of the heaviest
	// shape (pmdoc's nodeGuard), and the collector, which needs headroom under the Go
	// memory limit to free garbage before it reaches it. An apply estimated past ApplyHeap, which
	// runs alone, may reach into it too, up to ApplyTooHeavy. Nothing reads it; it is named so
	// the boot line accounts for the whole limit.
	//
	// Nothing in Dispatch reads or sets GOGC: the collector runs at the runtime's own ratio, and
	// what holds the pod inside its memory is the Go memory limit the runtime holds. At that
	// default the heap goal is twice the live heap, which past half the limit is the limit
	// itself, so a server whose budget is full keeps every byte of garbage the limit allows - the
	// budget's unused room, both heap bounds and this slack together - and the memory it holds
	// parks under the limit. That memory is what a pod's kernel charges it: what the runtime
	// holds less what it has released, which is the figure the limit bounds. Measured on
	// 2026-11-03 over ten runs of the real-process drive of the worst case the bounds allow
	// (cmd/dispatch, TestTheHeaviestApplyBesideAFullCopyBoundStaysUnderThePodsMemory, three
	// steps: the budget filled, the cold reads' cache filled, the heaviest handoffs applying
	// beside full copy bounds), it peaked by step at 2,724 to 2,955, 3,608 to 3,661 and 3,670 to
	// 3,840 MiB of the pod's 4,096, 256 MiB under it at the most, each run taking 344 to 372 s
	// and the keystrokes a person typed through both heavy steps answered in a 99th percentile
	// of 141 to 208 ms. What the limit itself bounds the heaviest step held by 2,210 to
	// 173,549,698 bytes past it, the collector's overshoot of a limit it steers to rather than
	// stops at, which the drive allows a margin for (podGoMemoryLimitMargin). That overshoot is
	// not the merge with main's load that no longer replays a document's whole edit history:
	// measured the same day, the same step read 653,362 bytes past the limit at the last commit
	// before that merge and 28,547,106 and 90,572,882 at the commit this comment once quoted a
	// run of 2026-11-01 at, 2,856, 3,620 and 3,237 MiB, which that commit charges at 2,706 to
	// 2,714, 3,621 to 3,624 and 3,684 to 3,776 MiB on this pod today. What one run of that step
	// reads says little: the collector's share of two processors decides it.
	//
	// The VmRSS those runs read was 2,727 to 2,963, 3,610 to 3,663 and 4,279 to 4,639 MiB, each
	// above the charge, and is not
	// what a pod is charged: this repository's sandboxes run under gVisor, whose VmRSS keeps every
	// page the runtime has handed back unless the madvise(MADV_DONTNEED) that released it covered
	// 2 MiB or more, and the scavenger releases in smaller pieces (cmd/dispatch, peakResident,
	// which reads the charged figure from the server's own memory trace). Dispatch's own pods are
	// not sandboxed - the GKE Autopilot deployment names no runtimeClassName and no GKE Sandbox
	// node pool (newDispatchKubernetes) - so what is charged there is what the limit
	// bounds. The 4,302 and 4,424 MiB earlier runs of that drive recorded were VmRSS read on this
	// sandbox, not memory any pod was charged.
	HeapSlack int64
	// ApplyTooHeavy is the estimate past which an update is refused as too heavy to apply
	// (errApplyTooHeavy): all of that remainder, both bounds and the slack together, so no apply
	// is refused that could run with every other weighed heap at rest, 1,181,116,006 bytes at the
	// calibration. An update estimated past ApplyHeap and within it is never refused: it waits
	// for the whole apply bound and then runs alone (weighedHeaps.hold). The estimate bounds what
	// an apply allocates from above: the heaviest server-written document's handoff is estimated
	// at 71,356,658 bytes and allocates 33,410,168, and no handoff of the editor's is estimated
	// past three times what it allocates (TestEveryApplysTransientHeapIsWithinItsEstimate).
	//
	// It stays above the estimate of every honest whole-state handoff at F, the heaviest frame
	// the hook lets a browser hand over (heaviestHandoffBound), measured on 2026-10-28 into the
	// room holding each (TestAnHonestHandoffAtTheHeaviestFrameIsNeverRefusedAsTooHeavy):
	// 904,378,600 bytes for the heaviest, the long-edited document typed on until 4,096 structs
	// short of the decode cap and padded to F by text nested under the margin, 276,737,406 under
	// the calibration's line; 657,900,131 for the long-edited fixture padded the same way, and
	// 404,291,526 for a paragraph. A frame of null values at the message floor, 56,623,059 bytes,
	// which the editor never sends, is estimated at 3,737,127,910
	// (TestAnUpdateHeavierThanAnyApplyMayHoldIsNeverApplied), past it, and refused.
	ApplyTooHeavy int64
	// PeerWriteQueue is ygo's Server.PeerWriteQueueBytes, the bytes of frames one browser's
	// socket may hold queued (peerWriteQueueBytes). It does not scale: see that constant.
	PeerWriteQueue int64
}

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
// What the limit leaves above the room budget, the cold reads' cache and the pressure reserve the
// budget's charges may pass its limit by - 1,181,116,006 bytes at the calibration - the
// architect's ruling splits three ways: the apply-heap bound (ApplyHeap), the copy-heap bound
// (CopyHeap) and slack named for what neither bound weighs (HeapSlack), a third each. The
// ruling's words are "a quarter of what the limit leaves above the reserve to each bound, and a
// quarter as named slack", which come to three quarters and leave the last quarter to nothing; a
// third each is taken as what "the three-way split" means, so that every byte of it has a named
// holder.
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
