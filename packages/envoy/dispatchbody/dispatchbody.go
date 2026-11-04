// Package dispatchbody answers whether Dispatch would refuse a body, for a caller outside this
// module. The Legion daemon posts two texts it cannot afford to lose — a root close's reason and
// the merger's READY packet — through an outbox, which records the fact first and posts later: a
// body the server refuses there is refused where nobody is left to shorten it, so the reason or
// the packet reaches no issue and the only trace is a notice. Both calls ask here instead, before
// anything is recorded, and refuse their caller with Dispatch's own answer, which that caller can
// act on by sending a shorter text.
//
// The answer is the server's own. Check splits the body with the splitter behind every Dispatch
// route that takes one (internal/dispatch/itemblocks) and returns its refusal unchanged, so the
// daemon refuses exactly what Dispatch refuses rather than approximating it with a reader of its
// own. That splitter is an internal package, unreachable from another module; this package is the
// one public door onto it, which is why it exists at all.
//
// A caller measures the text its post will carry, not the body alone. The daemon's outbox appends
// a marker naming the row to every message it posts, and that marker is a block of its own after
// a body that ends in a paragraph and code inside one that ends in an open fence, so a body that
// passes a bound can be a post that does not. Both callers measure record.LongestPost of their
// text for that reason: the close route of packages/daemon/internal/api (closeRoot) and the
// merger's READY in its handoff completion.
package dispatchbody

import (
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/itemblocks"
)

// Check answers the first bound Dispatch holds body to that it passes, or nil when it passes
// none: an *itemblocks.CountExceeded for a body of more blocks than ItemBlocks, else an
// *itemblocks.CapExceeded naming the first block over BlockUnits. The refusal is the server's,
// wording and all, over the field name "body", which is what the server's own body routes pass,
// so a caller's refusal of a text reads exactly as Dispatch's refusal of it would.
func Check(body string) error {
	return itemblocks.Check("body", body)
}

// BlockUnits is the longest one block of a body may be, in UTF-16 code units as Dispatch counts
// them, and ItemBlocks the most blocks one body may hold. They are here for a caller that must
// build a text at the bound rather than ask about one it already holds. Nothing checks a body
// against them: the server enforces both itself on every route that takes a body, Check reads
// them through the server's own splitter, and the Legion daemon no longer carries a copy of
// either value.
const (
	BlockUnits = contracts.DispatchBlockUnits
	ItemBlocks = contracts.DispatchItemBlocks
)
