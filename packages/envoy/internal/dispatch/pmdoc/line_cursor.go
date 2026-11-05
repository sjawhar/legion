package pmdoc

// A line read in columns, as micromark reads it: the cursor below is what every container's
// continuation and every blankness test of the port runs on (prepare_list.go).

// lineCursor is a position inside one line, counted in columns: a tab spans as many columns as it
// takes to reach the next multiple of four, and a container takes part of one where its own
// columns end inside it (micromark's preprocess.js turns a tab into a tab code and the virtual
// spaces after it, which factorySpace counts one by one).
type lineCursor struct {
	source []byte
	i      int
	end    int
	col    int
	// tab is how many columns of the tab at i are taken, 0 where no tab is half taken.
	tab int
}

// offset is the position micromark records here: consuming a tab moves it one byte on, and the
// virtual spaces that stand for the tab's remaining columns move it no further
// (micromark/dev/lib/create-tokenizer.js, consume; preprocess.js).
func (c *lineCursor) offset() int {
	if c.tab > 0 {
		return c.i + 1
	}
	return c.i
}

func (m *lineMachine) cursorAt(line int) lineCursor {
	start := m.lines.starts[line]
	end := start + len(m.lines.text(line))
	for end > start && m.source[end-1] == '\n' {
		end--
	}
	return lineCursor{source: m.source, i: start, end: end}
}

// at is the byte under the cursor, or 0 at the line's end.
func (c *lineCursor) at() byte {
	if c.i >= c.end {
		return 0
	}
	return c.source[c.i]
}

// space reports whether the cursor stands on a space or a tab.
func (c *lineCursor) space() bool {
	char := c.at()
	return char == ' ' || char == '\t'
}

// take consumes one column of whitespace, one column of a tab where a tab stands there.
func (c *lineCursor) take() {
	if c.source[c.i] == '\t' {
		c.col++
		c.tab++
		if c.col%tabWidth == 0 {
			c.i++
			c.tab = 0
		}
		return
	}
	c.i++
	c.col++
}

// step consumes one character that is no tab.
func (c *lineCursor) step() {
	c.i++
	c.col++
}

// toEnd takes the cursor to the line's end.
func (c *lineCursor) toEnd() { c.i = c.end }

// blankFrom reports whether the rest of the line is whitespace, which is micromark's blankLine
// check (micromark-core-commonmark/dev/lib/blank-line.js).
func (m *lineMachine) blankFrom(cursor lineCursor) bool {
	for index := cursor.i; index < cursor.end; index++ {
		if m.source[index] != ' ' && m.source[index] != '\t' {
			return false
		}
	}
	return true
}
