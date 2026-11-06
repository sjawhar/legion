package claude

import (
	"encoding/json"
	"slices"
	"time"

	"github.com/sjawhar/legion/daemon/internal/conversation"
)

// loaded is a transcript as Claude Code's loader holds it after reading every line: its message
// map in line order, where a resume continues from, and the metadata the reader keeps.
type loaded struct {
	order  []*entry
	byUUID map[string]*entry
	// last is the uuid of the last non-sidechain message line (the loader's "last row").
	last string
	// checkpoint is the leafUuid of the last last-prompt line since the last compaction boundary,
	// explicit whether that line said so, cleared whether an explicit null emptied the session
	// with no message after it.
	checkpoint string
	explicit   bool
	cleared    bool
	title      string
	// model is the model of the last assistant line a model wrote (not Claude Code's synthetic
	// reply or an API error record), effort the last effort an assistant line records.
	model  string
	effort string
}

// load reads the parsed lines as Claude Code's transcript loader processes them, in order: a
// progress line is dropped and its children re-parented to its parent; a sidechain line (a
// subagent's) is left out; a message line replaces an earlier one of its uuid in place, as a
// JavaScript Map's set does.
func load(entries []*entry) (*loaded, error) {
	l := &loaded{byUUID: map[string]*entry{}}
	progress, position := map[string]string{}, map[string]int{}
	for _, e := range entries {
		switch {
		case e.Type == "progress":
			parent := e.parent()
			if mapped, ok := progress[parent]; ok {
				parent = mapped
			}
			progress[e.UUID] = parent
		case transcriptTypes[e.Type]:
			if mapped, ok := progress[e.parent()]; ok {
				e.ParentUUID = nil
				if mapped != "" {
					e.ParentUUID = &mapped
				}
			}
			if e.IsSidechain {
				continue
			}
			if i, ok := position[e.UUID]; ok {
				l.order[i] = e
			} else {
				position[e.UUID] = len(l.order)
				l.order = append(l.order, e)
			}
			l.byUUID[e.UUID] = e
			if !(e.Type == "attachment" && e.Attachment != nil && e.Attachment.Type == "fork_briefing") {
				l.last = e.UUID
				l.explicit, l.cleared = false, false
			}
			if e.isBoundary() {
				l.checkpoint, l.explicit = "", false
			}
			if e.Type == "assistant" && e.Message != nil && e.Message.Model != syntheticModel && !e.IsAPIErrorMessage {
				l.model = e.Message.Model
				if e.Effort != "" {
					l.effort = e.Effort
				}
			}
		case e.Type == "last-prompt":
			var leaf *string
			if len(e.LeafUUID) > 0 && json.Unmarshal(e.LeafUUID, &leaf) != nil {
				return nil, conversation.Unknown("a last-prompt line whose leafUuid is not a uuid")
			}
			switch {
			case leaf != nil && *leaf != "":
				l.explicit = e.Explicit || l.explicit && *leaf == l.checkpoint
				l.checkpoint, l.cleared = *leaf, false
			case len(e.LeafUUID) > 0 && leaf == nil && e.Explicit:
				l.cleared, l.checkpoint, l.explicit = true, "", false
			}
		case e.Type == "custom-title" && e.SessionID != "":
			l.title = e.CustomTitle
		case e.Type == "content-replacement" && e.AgentID == "" && len(e.Replacements) > 0:
			// Claude Code puts a replacement in place of its tool result only while its tool
			// result budget is on, a setting the transcript does not record.
			return nil, conversation.Unknown("a content replacement of %d tool results", len(e.Replacements))
		}
	}
	return l, nil
}

// relink applies the last compaction boundary as the loader does: the messages it preserved are
// re-linked after its anchor (the summary) in order, the message that followed the anchor now
// follows the last of them, and every message line before the boundary that was not preserved is
// dropped, its children following the last preserved message. It returns the uuid of the last
// preserved message, "" when the last boundary preserved none. A transcript none of whose
// boundaries preserved messages is left as it is: its chain ends at the boundary, whose parent is
// null.
func (l *loaded) relink() string {
	boundary, preservedAt := -1, -1
	var meta *compactMetadata
	for i, e := range l.order {
		if e.isBoundary() {
			boundary = i
			if m := e.CompactMetadata; m != nil && (m.PreservedMessages != nil || m.PreservedSegment != nil) {
				meta, preservedAt = m, i
			}
		}
	}
	if meta == nil {
		return ""
	}
	var anchor string
	var preserved []string
	if preservedAt == boundary {
		var ok bool
		anchor, preserved, ok = l.preservedOf(meta)
		if !ok {
			// The loader keeps the transcript as it is when it cannot walk the segment.
			return ""
		}
		for _, uuid := range preserved {
			if l.byUUID[uuid] == nil {
				return ""
			}
		}
	}
	keep := map[string]bool{}
	for _, uuid := range preserved {
		keep[uuid] = true
	}
	if len(preserved) > 0 {
		tail, parent := preserved[len(preserved)-1], anchor
		for _, uuid := range preserved {
			l.setParent(uuid, parent)
			parent = uuid
		}
		for _, e := range l.order {
			if e.parent() == anchor && e.UUID != preserved[0] {
				l.setParent(e.UUID, tail)
			}
		}
	}
	var dropped []string
	for i, e := range l.order {
		if i < boundary && !keep[e.UUID] {
			dropped = append(dropped, e.UUID)
		}
	}
	for _, uuid := range dropped {
		delete(l.byUUID, uuid)
	}
	l.order = slices.DeleteFunc(l.order, func(e *entry) bool { return l.byUUID[e.UUID] != e })
	if len(preserved) == 0 {
		return ""
	}
	tail := preserved[len(preserved)-1]
	if len(dropped) > 0 {
		gone := map[string]bool{}
		for _, uuid := range dropped {
			gone[uuid] = true
		}
		for _, e := range l.order {
			if e.isMessage() && gone[e.parent()] {
				l.setParent(e.UUID, tail)
			}
		}
	}
	return tail
}

// preservedOf is the anchor and uuids a boundary preserved: its list, else its segment walked from
// tail to head.
func (l *loaded) preservedOf(m *compactMetadata) (string, []string, bool) {
	if m.PreservedMessages != nil {
		return m.PreservedMessages.AnchorUUID, m.PreservedMessages.UUIDs, true
	}
	seg := m.PreservedSegment
	var walked []string
	seen := map[string]bool{}
	for e := l.byUUID[seg.TailUUID]; e != nil && !seen[e.UUID]; e = l.byUUID[e.parent()] {
		seen[e.UUID] = true
		walked = append(walked, e.UUID)
		if e.UUID == seg.HeadUUID {
			slices.Reverse(walked)
			return seg.AnchorUUID, walked, true
		}
	}
	return "", nil, false
}

// setParent re-links the line uuid names; the reader owns every parsed line, so it is changed in
// place.
func (l *loaded) setParent(uuid, parent string) {
	e := l.byUUID[uuid]
	e.ParentUUID = nil
	if parent != "" {
		e.ParentUUID = &parent
	}
}

// descends is whether from's parent chain reaches ancestor (or from is it).
func (l *loaded) descends(from, ancestor string) bool {
	seen := map[string]bool{}
	for uuid := from; uuid != "" && !seen[uuid]; {
		if uuid == ancestor {
			return true
		}
		seen[uuid] = true
		e := l.byUUID[uuid]
		if e == nil {
			return false
		}
		uuid = e.parent()
	}
	return false
}

// messageAt is the first user or assistant line on uuid's parent chain, itself included.
func (l *loaded) messageAt(uuid string) *entry {
	seen := map[string]bool{}
	for e := l.byUUID[uuid]; e != nil && !seen[e.UUID]; e = l.byUUID[e.parent()] {
		seen[e.UUID] = true
		if e.isMessage() {
			return e
		}
	}
	return nil
}

// leaf is the message a resume continues from, chosen as the loader chooses it: the last
// last-prompt checkpoint, or the last row when that descends from the checkpoint, walked up to its
// first user or assistant line; after a compaction that preserved messages, the one leaf message
// no other message follows, or, of several, the checkpoint's or the last row's. nil for a
// transcript of no messages, or one an explicit null checkpoint emptied.
func (l *loaded) leaf(relinked string) (*entry, error) {
	if l.cleared {
		return nil, nil
	}
	explicit := l.explicit && l.byUUID[l.checkpoint] != nil
	if relinked == "" || explicit {
		start := ""
		if l.byUUID[l.checkpoint] != nil {
			start = l.checkpoint
		}
		if start != "" && !l.explicit && l.byUUID[l.last] != nil && l.last != start && l.descends(l.last, start) {
			start = l.last
		}
		if relinked == "" && start == "" {
			start = l.last
		}
		if m := l.messageAt(start); m != nil {
			return m, nil
		}
	}
	parents, messageParents := map[string]bool{}, map[string]bool{}
	for _, e := range l.order {
		if p := e.parent(); p != "" {
			parents[p] = true
			if e.isMessage() {
				messageParents[p] = true
			}
		}
	}
	var leaves []*entry
	for _, e := range l.order {
		if parents[e.UUID] {
			continue
		}
		if m := l.messageAt(e.UUID); m != nil && !messageParents[m.UUID] && !slices.Contains(leaves, m) {
			leaves = append(leaves, m)
		}
	}
	if len(leaves) <= 1 {
		if len(leaves) == 0 {
			return nil, nil
		}
		return leaves[0], nil
	}
	start := l.last
	if l.checkpoint != "" && slices.ContainsFunc(leaves, func(e *entry) bool { return e.UUID == l.checkpoint }) {
		start = l.checkpoint
	}
	if m := l.messageAt(start); m != nil {
		return m, nil
	}
	return nil, conversation.Unknown("%d leaves and no checkpoint to choose one", len(leaves))
}

// chain is the conversation from its root to leaf as the loader builds it: leaf's parent chain
// (a parent the map lacks replaced by the latest line at most five seconds older, as the loader
// falls back), then the sibling lines of each assistant message the chain holds only part of and
// the tool results answering them (parallel tool calls branch), then every line after leaf (the
// attachments and notices of its turn), oldest first.
func (l *loaded) chain(leaf *entry) []*entry {
	var chain []*entry
	seen := map[string]bool{}
	for e := leaf; e != nil; {
		if seen[e.UUID] {
			break
		}
		seen[e.UUID] = true
		chain = append(chain, e)
		parent := e.parent()
		if parent == "" {
			break
		}
		next := l.byUUID[parent]
		if next == nil || seen[next.UUID] {
			next = l.nearestBefore(e, seen)
		}
		e = next
	}
	slices.Reverse(chain)
	chain = l.recoverParallel(chain, seen)
	return append(chain, l.descendants(leaf, seen)...)
}

// nearestBefore is the loader's fallback for a missing parent: the line not yet on the chain whose
// timestamp is the closest at or before e's, within five seconds.
func (l *loaded) nearestBefore(e *entry, seen map[string]bool) *entry {
	at, err := time.Parse(time.RFC3339Nano, e.Timestamp)
	if err != nil {
		return nil
	}
	var best *entry
	bestGap := time.Duration(1<<63 - 1)
	for _, c := range l.order {
		if seen[c.UUID] {
			continue
		}
		t, err := time.Parse(time.RFC3339Nano, c.Timestamp)
		if err != nil {
			continue
		}
		if gap := at.Sub(t); gap >= 0 && gap <= 5*time.Second && gap < bestGap {
			best, bestGap = c, gap
		}
	}
	return best
}

// recoverParallel puts back what parallel tool calls leave off the chain. Claude Code writes each
// tool_use block of a message on a line of its own, chained one after another, and each tool
// result as a child of the line holding its call, so the chain from the leaf runs through one
// result only. After the last chain line of each assistant message come its lines the chain lacks
// and the results of its calls the chain lacks, in line order.
func (l *loaded) recoverParallel(chain []*entry, seen map[string]bool) []*entry {
	groups := map[string][]*entry{}
	results := map[string][]*entry{}
	for _, e := range l.order {
		switch {
		case e.Type == "assistant" && e.Message != nil && e.Message.ID != "":
			groups[e.Message.ID] = append(groups[e.Message.ID], e)
		case e.Type == "user" && hasToolResult(e) && e.parent() != "":
			results[e.parent()] = append(results[e.parent()], e)
		}
	}
	done := map[string]bool{}
	var out []*entry
	for i, e := range chain {
		out = append(out, e)
		if e.Type != "assistant" || e.Message == nil || e.Message.ID == "" || done[e.Message.ID] {
			continue
		}
		id := e.Message.ID
		if i+1 < len(chain) && chain[i+1].Type == "assistant" && chain[i+1].Message != nil && chain[i+1].Message.ID == id {
			continue
		}
		done[id] = true
		var siblings, answers []*entry
		for _, member := range groups[id] {
			if !seen[member.UUID] {
				siblings = append(siblings, member)
			}
		}
		for _, member := range groups[id] {
			for _, r := range results[member.UUID] {
				if !seen[r.UUID] && !slices.Contains(answers, r) {
					answers = append(answers, r)
				}
			}
		}
		for _, r := range append(siblings, answers...) {
			seen[r.UUID] = true
			out = append(out, r)
		}
	}
	return out
}

// descendants are the lines after leaf none of the chain holds, breadth first, oldest first.
func (l *loaded) descendants(leaf *entry, seen map[string]bool) []*entry {
	children := map[string][]*entry{}
	for _, e := range l.order {
		if p := e.parent(); p != "" {
			children[p] = append(children[p], e)
		}
	}
	var found []*entry
	queue := []string{leaf.UUID}
	for len(queue) > 0 {
		uuid := queue[0]
		queue = queue[1:]
		for _, c := range children[uuid] {
			if seen[c.UUID] {
				continue
			}
			seen[c.UUID] = true
			found = append(found, c)
			queue = append(queue, c.UUID)
		}
	}
	slices.SortStableFunc(found, func(a, b *entry) int {
		switch {
		case a.Timestamp < b.Timestamp:
			return -1
		case a.Timestamp > b.Timestamp:
			return 1
		}
		return 0
	})
	return found
}
