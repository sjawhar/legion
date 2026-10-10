package docs

import (
	"log/slog"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// askServerState is an ask block's server attributes (askServerAttributeNames) as it holds them.
type askServerState [len(askServerAttributeNames)]struct {
	value   any
	present bool
}

func readAskServerState(node *pmdoc.Node) askServerState {
	var state askServerState
	for index, name := range askServerAttributeNames {
		state[index].value, state[index].present = node.Attrs[name]
	}
	return state
}

// restore gives node back the server attributes state holds.
func (state askServerState) restore(node *pmdoc.Node) {
	for index, name := range askServerAttributeNames {
		if state[index].present {
			node.Attrs[name] = state[index].value
		} else {
			delete(node.Attrs, name)
		}
	}
}

// serverRewrite is an ask block whose server attributes settlement repaired to agree with its ask:
// the document of that ask when the block is a copy of it, "" otherwise (copiedAskSources), the
// attributes as settlement found them, whether the repair wrote an answer, the index of the repair
// in the reconciliation's repairs, and the index in its events of the block.repaired the repair
// emitted, or -1 for a new ask's block or a copy, whose repair emits none.
type serverRewrite struct {
	node   *pmdoc.Node
	ask    model.Ask
	copied string
	found  askServerState
	answer bool
	repair int
	event  int
}

// withholdAnswers keeps out of the document each answer settlement wrote back into its block that
// the document has no room for: one that would leave it past what one upload may hold and bigger
// than before, the document's rendering as settlement read it (weigh, which weighs a caller's write
// the same way). An answer is stored on its ask as well as in its block, and a block that leaves
// the document and returns gets its answer back from the ask, so a returning block is the answer's
// text arriving without the answer route that weighs it: twenty-four answers of 900 KB, each
// weighed against a document their deleted blocks had left small, came back in one 1,540-byte edit
// as a 21.6 MB document. Where the answers do not all fit, every one is left out, and each is
// given back in document order while the document still has room for it (returnAnswersWithRoom).
// A block whose answer stays out is repaired without it, so it still says who answered and when,
// and the ask keeps the answer; a later settlement of a document with room for it writes it back.
// A repair that then changes nothing is dropped with its block.repaired. A document that does not
// render cannot be weighed, so its answers all stay out, and the settlement that renders it next
// fails as it would have.
func (r *settlementReconciliation) withholdAnswers(artifactID string, tree *pmdoc.Node, before string) {
	var answered []int
	for index, rewrite := range r.rewrites {
		if rewrite.answer {
			answered = append(answered, index)
		}
	}
	if len(answered) == 0 {
		return
	}
	after, err := renderTree(tree)
	rendered := err == nil
	if rendered {
		if err = weighRendering(before, after); err == nil {
			return
		}
	}
	// changes says whether each block repaired without its answer still changes from what
	// settlement found.
	changes := make(map[int]bool, len(answered))
	for _, index := range answered {
		rewrite := r.rewrites[index]
		rewrite.found.restore(rewrite.node)
		changes[index], _ = setAskServerAttributes(rewrite.node, rewrite.ask, rewrite.copied, true)
	}
	returned := map[int]bool{}
	if rendered {
		returned = r.returnAnswersWithRoom(tree, before, answered)
	}
	slog.Warn("dispatch: settlement withholds restored answers from their blocks", "room", artifactID,
		"withheld", len(answered)-len(returned), "answers", len(answered), "reason", err)
	droppedRepairs, droppedEvents := map[int]bool{}, map[int]bool{}
	for _, index := range answered {
		rewrite := r.rewrites[index]
		switch {
		case returned[index]:
		case changes[index]:
			r.repairs[rewrite.repair].set = askServerAttributes(rewrite.ask, rewrite.copied, true)
		default:
			droppedRepairs[rewrite.repair] = true
			if rewrite.event >= 0 {
				droppedEvents[rewrite.event] = true
			}
		}
	}
	r.repairs = without(r.repairs, droppedRepairs)
	r.events = without(r.events, droppedEvents)
}

// returnAnswersWithRoom gives back, in document order, the answer of each rewrite answered names
// (r.rewrites), all of them left out of tree, while the document still has room for it, and
// reports which it gave back. An answer lengthens its block's directive line, which carries every
// attribute quoted, and makes no element, so the document's rendering grows by what the block's own
// rendering does: the document is rendered once, without them, and each answer is weighed by its
// block alone.
func (r *settlementReconciliation) returnAnswersWithRoom(tree *pmdoc.Node, before string, answered []int) map[int]bool {
	returned := map[int]bool{}
	left, err := renderTree(tree)
	if err != nil {
		return returned
	}
	document, was := renderingOf(left), renderingOf(before)
	for _, index := range answered {
		rewrite := &r.rewrites[index]
		grown, err := rewrite.withAnswer(document)
		if err == nil {
			err = weigh(was, grown)
		}
		if err != nil {
			setAskServerAttributes(rewrite.node, rewrite.ask, rewrite.copied, true)
			continue
		}
		document = grown
		returned[index] = true
	}
	return returned
}

// withAnswer gives the rewrite's block, its answer left out, the answer back, and is document grown
// by what that adds to the block's own rendering.
func (rewrite *serverRewrite) withAnswer(document rendering) (rendering, error) {
	alone := &pmdoc.Node{Type: "doc", Children: []*pmdoc.Node{rewrite.node}}
	withheld, err := renderTree(alone)
	setAskServerAttributes(rewrite.node, rewrite.ask, rewrite.copied, false)
	if err != nil {
		return rendering{}, err
	}
	answered, err := renderTree(alone)
	if err != nil {
		return rendering{}, err
	}
	return document.longer(len(answered) - len(withheld)), nil
}

// without is items but those at the indexes dropped names, in their order, in items' own array.
func without[T any](items []T, dropped map[int]bool) []T {
	if len(dropped) == 0 {
		return items
	}
	kept := items[:0]
	for index, item := range items {
		if !dropped[index] {
			kept = append(kept, item)
		}
	}
	return kept
}
