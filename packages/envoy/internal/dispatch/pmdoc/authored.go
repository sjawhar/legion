package pmdoc

import (
	"sort"

	"github.com/reearth/ygo/crdt"
)

// ClockRun is a half-open run of one client's clocks, [From, To), covering the characters one of
// its text items holds. An item split by a concurrent insert becomes two items whose runs are
// contiguous, so a set of runs describes the same characters however the item list is cut up.
type ClockRun struct {
	From uint64
	To   uint64
}

// AuthoredText is where one client's live text sits in a document: per block id, the runs of that
// client's clocks its live text items cover inside the element carrying the id. Text a fragment
// holds outside any block is keyed by the empty string, and a block id two elements carry (a
// browser move can leave one until EnsureBlockIDs repairs it) collects both elements' runs, since
// the text is still in a block with that id.
type AuthoredText map[string][]ClockRun

// AuthoredTextRuns reports where client's live text from clock since onward sits in frag. An
// item's clock run lies wholly on one side of a clock the client had already reached, splits
// included, so since is the exact line between the text one update inserted and everything the
// client wrote before it - and it costs nothing, where reading the whole document a second time
// to subtract what was there costs a walk. With only non-nil it reads just those block ids, which
// is what a check that already knows which blocks it wrote needs: reading one block's items
// instead of a thousand document's is the difference between microseconds and a millisecond,
// since every text node costs a reflective read of the item list.
//
// The walk is lock-free - Children, GetAttributeValue and the text item list each are - so it runs
// inside the caller's Yjs transaction, which holds the document mutex and would deadlock on a
// locking read. since, which the caller takes from the document's state vector, does not: it is
// read before the transaction opens.
func AuthoredTextRuns(frag *crdt.YXmlFragment, client crdt.ClientID, since uint64, only map[string]struct{}) AuthoredText {
	authored := AuthoredText{}
	authored.collect(frag, client, "", since, only)
	for block, runs := range authored {
		authored[block] = normalizeRuns(runs)
	}
	return authored
}

func (a AuthoredText) collect(frag *crdt.YXmlFragment, client crdt.ClientID, block string, since uint64, only map[string]struct{}) {
	for _, child := range frag.Children() {
		switch node := child.(type) {
		case *crdt.YXmlElement:
			inner := block
			if value, ok := node.GetAttributeValue(BlockIDAttr); ok {
				if id, ok := value.(string); ok && id != "" {
					inner = id
				}
			}
			a.collect(&node.YXmlFragment, client, inner, since, only)
		case *crdt.YXmlText:
			if only != nil {
				if _, wanted := only[block]; !wanted {
					continue
				}
			}
			for item := yTextStart(node); item != nil; item = item.Right {
				if item.Deleted || item.ID.Client != client || item.ID.Clock < since {
					continue
				}
				content, isString := item.Content.(*crdt.ContentString)
				if !isString {
					continue
				}
				a[block] = append(a[block], ClockRun{From: item.ID.Clock, To: item.ID.Clock + uint64(content.Len())})
			}
		}
	}
}

// Missing names the blocks of want whose text is no longer all live inside an element carrying
// that block id in have: a concurrent change deleted it, left it under a deleted ancestor, or
// moved it into another block.
func (want AuthoredText) Missing(have AuthoredText) map[string]struct{} {
	missing := map[string]struct{}{}
	for block, runs := range want {
		if len(subtractRuns(runs, have[block])) > 0 {
			missing[block] = struct{}{}
		}
	}
	return missing
}

// normalizeRuns sorts runs by clock and merges the ones that touch or overlap, so two run sets
// describing the same characters compare the same however their items were split.
func normalizeRuns(runs []ClockRun) []ClockRun {
	if len(runs) < 2 {
		return runs
	}
	sort.Slice(runs, func(i, j int) bool { return runs[i].From < runs[j].From })
	merged := runs[:1]
	for _, run := range runs[1:] {
		last := &merged[len(merged)-1]
		if run.From <= last.To {
			if run.To > last.To {
				last.To = run.To
			}
			continue
		}
		merged = append(merged, run)
	}
	return merged
}

// subtractRuns is the part of runs that from does not cover. Both sides are normalized.
func subtractRuns(runs, from []ClockRun) []ClockRun {
	if len(runs) == 0 {
		return nil
	}
	runs, from = normalizeRuns(runs), normalizeRuns(from)
	var rest []ClockRun
	index := 0
	for _, run := range runs {
		at := run.From
		for index < len(from) && from[index].To <= at {
			index++
		}
		for cursor := index; cursor < len(from) && from[cursor].From < run.To; cursor++ {
			if from[cursor].From > at {
				rest = append(rest, ClockRun{From: at, To: from[cursor].From})
			}
			if from[cursor].To > at {
				at = from[cursor].To
			}
		}
		if at < run.To {
			rest = append(rest, ClockRun{From: at, To: run.To})
		}
	}
	return rest
}

// BlocksGainingText names the blocks of after whose own inline content gained characters
// relative to prior, the same text map this function returned for the tree before it. That is
// the write pmdoc.Update makes: updateYText diffs a textblock's own text and inserts what the
// diff adds, so a block whose text only shrank, or that changed only its attributes, writes
// nothing a concurrent change could then remove. A block after holds under an id prior did not
// is a block the write created, and counts whatever its text says. A block with no id is named
// by nothing and so is named here by nothing either; a caller that must answer for such text
// answers for it as the batch's (docs.editBatch.writes). text is after's own map, for the next
// comparison.
func BlocksGainingText(prior map[string]string, after *Node) (ids []string, text map[string]string) {
	text = map[string]string{}
	forEachBlockText(after, func(id, own string) {
		if id == "" {
			return
		}
		text[id] = own
		was, held := prior[id]
		if !held {
			if own != "" {
				ids = append(ids, id)
			}
			return
		}
		if _, _, insert := simpleDiff(was, own); insert != "" {
			ids = append(ids, id)
		}
	})
	return ids, text
}

// BlockText is every block's own inline content by block id.
func BlockText(tree *Node) map[string]string {
	text := map[string]string{}
	forEachBlockText(tree, func(id, own string) {
		if id != "" {
			text[id] = own
		}
	})
	return text
}

// forEachBlockText visits every block of tree with the text of its own inline content: the
// concatenation of its direct text children, which is the string pmdoc.Update diffs.
//
// It walks the tree itself rather than through Walk, which computes each node's ProseMirror
// position and so recomputes nodeSize over every subtree: on a thousand-block document that costs
// milliseconds, and this runs once per operation on the uncontended path.
func forEachBlockText(tree *Node, visit func(id, text string)) {
	if tree == nil {
		return
	}
	if tree.Type != "doc" && !isInlineNodeType(tree.Type) {
		id, _ := tree.Attrs[BlockIDAttr].(string)
		visit(id, ownInlineText(tree))
	}
	if tree.Type == "text" || isInlineNodeType(tree.Type) {
		return
	}
	for _, child := range tree.Children {
		forEachBlockText(child, visit)
	}
}

// ownInlineText is a block's own inline content as one string, without allocating for the usual
// single-run block.
func ownInlineText(node *Node) string {
	own := ""
	for _, child := range node.Children {
		if child.Type != "text" {
			continue
		}
		if own == "" {
			own = child.Text
			continue
		}
		own += child.Text
	}
	return own
}
