// Package dispatchbody answers whether Dispatch would refuse a body, for a caller outside this
// module. A caller that must know before it writes anything — one whose post is recorded first
// and sent later, where a refusal costs the text rather than the call — asks here and refuses
// its own caller with Dispatch's own answer, which that caller can act on by sending a shorter
// text.
//
// The answer is the server's own. Check splits the body with the splitter behind every Dispatch
// route that takes one (internal/dispatch/itemblocks) and returns its refusal unchanged, so a
// caller refuses exactly what Dispatch refuses rather than approximating it with a reader of its
// own. That splitter is an internal package, unreachable from another module; this package is the
// one public door onto it, which is why it exists at all.
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
// build a text at the bound rather than ask about one it already holds. Nothing here checks a
// body against them: the server enforces both itself on every route that takes a body, and Check
// reads them through the server's own splitter.
const (
	BlockUnits = contracts.DispatchBlockUnits
	ItemBlocks = contracts.DispatchItemBlocks
)
