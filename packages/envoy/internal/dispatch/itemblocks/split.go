// Package itemblocks holds the blocks of comments and messages: how a body splits into blocks, and
// how an edit keeps their ids.
package itemblocks

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"

	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// Block is one top-level block of an item's body: its Proof type and its text, the slice of the
// body as its author wrote it, body[From:To] in bytes, and Hash(Text), its identity hash. The
// stored text, the excerpt and the identity hash all read this slice, never the canonical
// rendering, which can differ from what was written.
type Block struct {
	Type     string
	Text     string
	Hash     string
	From, To int
}

// Split reads body as the document parser reads a document and returns its top-level blocks in
// order, each the body's text from the start of the line where the block starts to where the next
// one starts, less the spaces, tabs and line endings that end it. A body the parser refuses, or
// one whose blocks' starts it cannot place, splits at blank lines into blocks of type "text", so
// every body splits and no item is refused for its shape. A body of only whitespace has no blocks.
func Split(body string) []Block {
	// The parser answers one empty paragraph for a body of only whitespace, which holds none of
	// the body.
	if strings.TrimSpace(body) == "" {
		return nil
	}
	var blocks []Block
	ok := false
	if doc, starts, err := pmdoc.ParseWithSourceStarts(body); err == nil {
		blocks, ok = bySourceStarts(body, doc, starts)
	}
	if !ok {
		blocks = byBlankLines(body)
	}
	for index := range blocks {
		blocks[index].Hash = Hash(blocks[index].Text)
	}
	return blocks
}

// bySourceStarts cuts body at the parser's block starts. Each start is taken back to the start of
// its line: every top-level block begins a line, and the parser counts a tab before a block as the
// columns it spans, so its start can land inside the line. It reports false unless every start is
// known, the starts strictly increase, and every block holds text.
func bySourceStarts(body string, doc *pmdoc.Node, starts []int) ([]Block, bool) {
	if len(starts) != len(doc.Children) {
		return nil, false
	}
	blocks := make([]Block, len(starts))
	for index, start := range starts {
		if start < 0 || start > len(body) {
			return nil, false
		}
		start = strings.LastIndexByte(body[:start], '\n') + 1
		if index > 0 && start <= blocks[index-1].From {
			return nil, false
		}
		blocks[index] = Block{Type: doc.Children[index].Type, From: start}
	}
	for index := range blocks {
		end := len(body)
		if index+1 < len(blocks) {
			end = blocks[index+1].From
		}
		text := strings.TrimRight(body[blocks[index].From:end], " \t\r\n")
		if text == "" {
			return nil, false
		}
		blocks[index].Text = text
		blocks[index].To = blocks[index].From + len(text)
	}
	return blocks, true
}

// byBlankLines cuts body at blank lines, lines of only spaces, tabs and carriage returns, into
// blocks of type "text", each trimmed of the carriage returns and line feeds around it.
func byBlankLines(body string) []Block {
	var blocks []Block
	from := -1 // the start of the block being read, -1 between blocks
	to := 0    // the end of its last line that is not blank
	for start := 0; start < len(body); {
		end := strings.IndexByte(body[start:], '\n')
		if end < 0 {
			end = len(body)
		} else {
			end += start
		}
		if strings.Trim(body[start:end], " \t\r") == "" {
			blocks = appendText(blocks, body, from, to)
			from = -1
		} else {
			if from < 0 {
				from = start
			}
			to = end
		}
		start = end + 1
	}
	return appendText(blocks, body, from, to)
}

// appendText appends body[from:to], trimmed of its outer carriage returns and line feeds, as a
// block of type "text", unless from is -1 or nothing is left.
func appendText(blocks []Block, body string, from, to int) []Block {
	if from < 0 {
		return blocks
	}
	for from < to && (body[from] == '\r' || body[from] == '\n') {
		from++
	}
	for to > from && (body[to-1] == '\r' || body[to-1] == '\n') {
		to--
	}
	if from == to {
		return blocks
	}
	return append(blocks, Block{Type: "text", Text: body[from:to], From: from, To: to})
}

// Hash is a block's identity: BodySHA256 of text with each CR LF read as LF, so a block that only
// changed its line endings (a textarea writes LF where the body had CR LF) keeps it.
func Hash(text string) string {
	return BodySHA256(strings.ReplaceAll(text, "\r\n", "\n"))
}

// BodySHA256 is the hex SHA-256 of an item's body as stored, byte for byte: the state row's
// body_sha256, which says which body the item's blocks were last indexed from.
func BodySHA256(body string) string {
	sum := sha256.Sum256([]byte(body))
	return hex.EncodeToString(sum[:])
}
