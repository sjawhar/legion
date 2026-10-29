package store

import (
	"slices"
	"strings"
)

// subjectIndex indexes the cached interests' topic patterns by subject token, one tree per machine
// id, so a subject is matched against a machine's interests in time that follows the subject's
// tokens and the patterns that share them rather than every cached interest. Its wildcard rules are
// routing.Match's: a "*" token matches any one token, a ">" token matches one or more remaining
// tokens (and ends the pattern, whatever follows it), and tokens split on every ".", an empty token
// included. Registry holds it under its mu and keeps it in step with its cache.
type subjectIndex struct {
	roots map[string]*subjectNode
	// entries is what each cache key is indexed under, so a change replaces it and an eviction
	// removes it.
	entries map[string]indexedInterest
}

// indexedInterest is one cache key's machine id and its distinct topic patterns.
type indexedInterest struct {
	machineID string
	patterns  []string
}

// subjectNode is one token position of a machine's tree.
type subjectNode struct {
	literal map[string]*subjectNode
	star    *subjectNode
	// full holds the keys whose pattern has ">" at this position; ends those whose pattern ends here.
	full map[string]struct{}
	ends map[string]struct{}
}

func newSubjectIndex() *subjectIndex {
	return &subjectIndex{roots: map[string]*subjectNode{}, entries: map[string]indexedInterest{}}
}

// set indexes key under item's machine id and topics, replacing what key was indexed under.
func (x *subjectIndex) set(key string, item Interest) {
	patterns := slices.Clone(item.Topics)
	slices.Sort(patterns)
	patterns = slices.Compact(patterns)
	if current, ok := x.entries[key]; ok {
		if current.machineID == item.MachineID && slices.Equal(current.patterns, patterns) {
			return
		}
		x.remove(key)
	}
	root := x.roots[item.MachineID]
	if root == nil {
		root = &subjectNode{}
		x.roots[item.MachineID] = root
	}
	for _, pattern := range patterns {
		root.insert(key, pattern)
	}
	x.entries[key] = indexedInterest{machineID: item.MachineID, patterns: patterns}
}

// remove drops key from the index, and every node it alone kept.
func (x *subjectIndex) remove(key string) {
	current, ok := x.entries[key]
	if !ok {
		return
	}
	delete(x.entries, key)
	root := x.roots[current.machineID]
	if root == nil {
		return
	}
	for _, pattern := range current.patterns {
		root.delete(key, pattern)
	}
	if root.empty() {
		delete(x.roots, current.machineID)
	}
}

// match calls visit with each key of machineID whose patterns match subject; a key with several
// matching patterns is visited once per pattern.
func (x *subjectIndex) match(machineID, subject string, visit func(key string)) {
	if root := x.roots[machineID]; root != nil {
		root.match(subject, false, visit)
	}
}

// matchAll calls visit with each key, on whatever machine, whose patterns match subject; a key
// with several matching patterns is visited once per pattern. A key belongs to one machine's tree,
// so no key is visited from two of them.
func (x *subjectIndex) matchAll(subject string, visit func(key string)) {
	for _, root := range x.roots {
		root.match(subject, false, visit)
	}
}

func (n *subjectNode) insert(key, pattern string) {
	node := n
	for token := range strings.SplitSeq(pattern, ".") {
		switch token {
		case ">":
			if node.full == nil {
				node.full = map[string]struct{}{}
			}
			node.full[key] = struct{}{}
			return
		case "*":
			if node.star == nil {
				node.star = &subjectNode{}
			}
			node = node.star
		default:
			child := node.literal[token]
			if child == nil {
				if node.literal == nil {
					node.literal = map[string]*subjectNode{}
				}
				child = &subjectNode{}
				node.literal[token] = child
			}
			node = child
		}
	}
	if node.ends == nil {
		node.ends = map[string]struct{}{}
	}
	node.ends[key] = struct{}{}
}

// delete drops key's pattern from the subtree at n, pruning the nodes left empty.
func (n *subjectNode) delete(key, pattern string) {
	token, rest, more := strings.Cut(pattern, ".")
	switch {
	case token == ">":
		delete(n.full, key)
		return
	case token == "*":
		if n.star == nil {
			return
		}
		n.star.deleteRest(key, rest, more)
		if n.star.empty() {
			n.star = nil
		}
	default:
		child := n.literal[token]
		if child == nil {
			return
		}
		child.deleteRest(key, rest, more)
		if child.empty() {
			delete(n.literal, token)
		}
	}
}

// deleteRest drops key's pattern from n, which its tokens before rest led to; more reports whether
// rest holds any.
func (n *subjectNode) deleteRest(key, rest string, more bool) {
	if !more {
		delete(n.ends, key)
		return
	}
	n.delete(key, rest)
}

func (n *subjectNode) empty() bool {
	return len(n.literal) == 0 && n.star == nil && len(n.full) == 0 && len(n.ends) == 0
}

// match visits the keys of n's subtree that the subject's remaining tokens match: rest holds them,
// and exhausted reports that none remain.
func (n *subjectNode) match(rest string, exhausted bool, visit func(string)) {
	if exhausted {
		for key := range n.ends {
			visit(key)
		}
		return
	}
	for key := range n.full {
		visit(key)
	}
	token, next, more := strings.Cut(rest, ".")
	if child := n.literal[token]; child != nil {
		child.match(next, !more, visit)
	}
	if n.star != nil {
		n.star.match(next, !more, visit)
	}
}
