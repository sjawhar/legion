package pmdoc

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

type renderer struct {
	b                strings.Builder
	inlineCodeFence  string
	inlineCodePadded bool
	err              error
	blockOffsets     []BlockOffset
}

// BlockOffset identifies one rendered block in byte offsets of the markdown.
type BlockOffset struct {
	ID   string
	Type string
	From int
	To   int
}

// Render returns doc as canonical Markdown. A mark the rendering does not write
// (renderedMarkTypes) cannot change it: the document is rendered with its anchor marks stripped,
// which merges the text runs an anchor split, and an escape is decided over a whole run.
func Render(doc *Node) (string, error) {
	r, err := render(doc)
	if err != nil {
		return "", err
	}
	return r.b.String(), nil
}

// RenderWithBlockOffsets renders a document and records each identified block's
// byte range in the returned markdown.
func RenderWithBlockOffsets(doc *Node) (string, []BlockOffset, error) {
	r, err := render(doc)
	if err != nil {
		return "", nil, err
	}
	return r.b.String(), r.blockOffsets, nil
}

func render(doc *Node) (*renderer, error) {
	if doc == nil || doc.Type != "doc" {
		if doc == nil {
			return nil, fmt.Errorf("%w: Render wants a doc, got nil", ErrSchema)
		}
		return nil, fmt.Errorf("%w: Render wants a doc, got %q", ErrSchema, doc.Type)
	}
	if err := doc.Validate(); err != nil {
		return nil, err
	}
	// Anchor marks render nothing, but one that starts or ends inside a word splits its text into
	// runs, and an escape is decided within one run: rendering the document without them merges
	// the runs, so a mark never changes the markdown (`snake_case`, never `snake\_case`).
	doc = StripAnchorMarks(doc)
	if len(doc.Children) == 1 && doc.Children[0].Type == "paragraph" && len(doc.Children[0].Children) == 0 {
		return &renderer{}, nil
	}
	r := &renderer{}
	r.blocks(doc.Children, "")
	if r.err != nil {
		return nil, r.err
	}
	return r, nil
}

func (r *renderer) blocks(nodes []*Node, prefix string) {
	for i, n := range nodes {
		if i > 0 {
			r.writeSyntax("\n" + strings.TrimRight(prefix, " ") + "\n")
		}
		r.block(n, prefix)
	}
	if len(nodes) > 0 {
		r.writeSyntax("\n")
	}
}

func (r *renderer) blocksNoTrailing(nodes []*Node, prefix string) {
	for i, n := range nodes {
		if i > 0 {
			r.writeSyntax("\n" + strings.TrimRight(prefix, " ") + "\n" + prefix)
		}
		r.block(n, prefix)
	}
}

func (r *renderer) block(n *Node, prefix string) {
	start := r.b.Len()
	blockOffset := -1
	if n.Type != "doc" && !isInlineNodeType(n.Type) {
		if id, ok := n.Attrs[BlockIDAttr].(string); ok && id != "" {
			blockOffset = len(r.blockOffsets)
			r.blockOffsets = append(r.blockOffsets, BlockOffset{ID: id, Type: n.Type, From: start})
		}
	}
	defer func() {
		if blockOffset >= 0 {
			r.blockOffsets[blockOffset].To = r.b.Len()
		}
	}()
	if r.err != nil {
		return
	}
	switch n.Type {
	case "paragraph":
		r.inline(n.Children, prefix)
	case "heading":
		level := int(num(n.Attrs["level"], 1))
		r.writeSyntax(strings.Repeat("#", level) + " ")
		r.inline(n.Children, prefix)
	case "blockquote":
		r.writeSyntax("> ")
		r.blocksNoTrailing(n.Children, prefix+"> ")
	case "bullet_list", "ordered_list":
		r.list(n, prefix)
	case "code_block":
		language, _ := n.Attrs["language"].(string)
		fence := codeBlockFence(n)
		r.writeSyntax(fence + language + "\n" + prefix)
		for _, child := range n.Children {
			if child.Type != "text" {
				r.err = fmt.Errorf("%w: code block contains %q", ErrSchema, child.Type)
				return
			}
			r.writeCodeText(child, prefix)
		}
		r.writeSyntax("\n" + prefix + fence)
	case "hr":
		r.writeSyntax("---")
	case "frontmatter":
		for _, child := range n.Children {
			if child.Type != "text" {
				r.err = fmt.Errorf("%w: frontmatter contains %q", ErrSchema, child.Type)
				return
			}
			r.writeText(child.Text)
		}
	case "table":
		r.table(n, prefix)
	case "footnote_definition":
		label, _ := n.Attrs["label"].(string)
		r.writeSyntax("[^" + escapeFootnoteLabel(label) + "]: ")
		r.blocksNoTrailing(n.Children, prefix+"    ")
	default:
		typ, typed := typedBlock(n.Type)
		if !typed {
			r.err = fmt.Errorf("%w: cannot render block %q", ErrSchema, n.Type)
			return
		}
		attrs, err := renderTypedAttributes(n, typ)
		if err != nil {
			r.err = err
			return
		}
		r.writeSyntax(":::" + n.Type + "{" + attrs + "}\n" + prefix)
		r.blocksNoTrailing(n.Children, prefix)
		r.writeSyntax("\n" + prefix + ":::")
	}
}

func (r *renderer) list(n *Node, prefix string) {
	start := 1
	if n.Type == "ordered_list" {
		start = int(num(n.Attrs["order"], 1))
	}
	for index, item := range n.Children {
		if item.Type != "list_item" {
			r.err = fmt.Errorf("%w: list contains %q", ErrSchema, item.Type)
			return
		}
		if index > 0 {
			r.writeSyntax("\n" + prefix)
			if n.Attrs["spread"] == true {
				r.writeSyntax("\n" + strings.TrimRight(prefix, " ") + "\n" + prefix)
			}
		}
		marker := "- "
		if n.Type == "ordered_list" {
			marker = fmt.Sprintf("%d. ", start+index)
		}
		r.writeSyntax(marker)
		if checked, ok := item.Attrs["checked"].(bool); ok {
			if checked {
				r.writeSyntax("[x] ")
			} else {
				r.writeSyntax("[ ] ")
			}
		}
		indent := prefix + strings.Repeat(" ", len(marker))
		for childIndex, child := range item.Children {
			if childIndex > 0 {
				if item.Attrs["spread"] == true {
					r.writeSyntax("\n" + strings.TrimRight(prefix, " ") + "\n" + indent)
				} else {
					r.writeSyntax("\n" + indent)
				}
			}
			r.block(child, indent)
		}
	}
}

func (r *renderer) table(table *Node, prefix string) {
	if len(table.Children) == 0 || table.Children[0].Type != "table_header_row" {
		r.err = fmt.Errorf("%w: table requires a header row", ErrSchema)
		return
	}
	header := table.Children[0]
	r.tableRow(header, true, prefix)
	r.writeSyntax("\n" + prefix + "| ")
	for i, cell := range header.Children {
		if i > 0 {
			r.writeSyntax(" | ")
		}
		r.writeSyntax(tableAlignment(cell.Attrs["alignment"]))
	}
	r.writeSyntax(" |")
	for _, row := range table.Children[1:] {
		r.writeSyntax("\n" + prefix)
		r.tableRow(row, false, prefix)
	}
}

func (r *renderer) tableRow(row *Node, header bool, prefix string) {
	want := "table_cell"
	rowType := "table_row"
	if header {
		want = "table_header"
		rowType = "table_header_row"
	}
	if row.Type != rowType {
		r.err = fmt.Errorf("%w: unexpected table row %q", ErrSchema, row.Type)
		return
	}
	r.writeSyntax("| ")
	for i, cell := range row.Children {
		if cell.Type != want {
			r.err = fmt.Errorf("%w: table row contains %q", ErrSchema, cell.Type)
			return
		}
		if i > 0 {
			r.writeSyntax(" | ")
		}
		if len(cell.Children) != 1 || cell.Children[0].Type != "paragraph" {
			r.err = fmt.Errorf("%w: table cell requires one paragraph", ErrSchema)
			return
		}
		r.tableCellInline(cell.Children[0].Children, prefix)
	}
	r.writeSyntax(" |")
}

func (r *renderer) inline(nodes []*Node, prefix string) {
	r.inlineWithEscapes(nodes, prefix, false)
}

func (r *renderer) tableCellInline(nodes []*Node, prefix string) {
	r.inlineWithEscapes(nodes, prefix, true)
}

func (r *renderer) inlineWithEscapes(nodes []*Node, prefix string, escapePipes bool) {
	var active []Mark
	atLineStart := true
	for index, n := range nodes {
		if r.err != nil {
			return
		}
		switch n.Type {
		case "text":
			next := visibleMarks(n.Marks)
			hasLink := containsMark(next, "link")
			bareURL := isBareURLLink(n, next, escapePipes)
			if bareURL {
				next = withoutMark(next, "link")
			}
			common := sharedMarks(active, next)
			for i := len(active) - 1; i >= common; i-- {
				r.closeInlineMark(active[i], escapePipes)
			}
			for _, mark := range next[common:] {
				r.openInlineMark(mark, nodes, index)
			}
			active = next
			r.writeInlineText(n, &atLineStart, escapePipes, !hasLink)
		case "hardbreak":
			r.closeMarks(active, escapePipes)
			active = nil
			r.writeSyntax("\\\n" + prefix)
			atLineStart = true
		case "image":
			r.closeMarks(active, escapePipes)
			active = nil
			src, _ := n.Attrs["src"].(string)
			alt, _ := n.Attrs["alt"].(string)
			title, _ := n.Attrs["title"].(string)
			r.writeSyntax("![" + escapeTablePipes(strings.ReplaceAll(alt, "]", "\\]"), escapePipes) + "](" + escapeLinkDestination(src, escapePipes) + titleSuffix(title, escapePipes) + ")")
			atLineStart = false
		case "html":
			r.closeMarks(active, escapePipes)
			active = nil
			value, _ := n.Attrs["value"].(string)
			r.writeSyntax(escapeTableHTMLPipes(value, escapePipes))
			atLineStart = false
		case "footnote_reference":
			r.closeMarks(active, escapePipes)
			active = nil
			label, _ := n.Attrs["label"].(string)
			r.writeSyntax("[^" + escapeFootnoteLabel(label) + "]")
			atLineStart = false
		default:
			r.err = fmt.Errorf("%w: cannot render inline %q", ErrSchema, n.Type)
		}
	}
	r.closeMarks(active, escapePipes)
}

func (r *renderer) writeCodeText(node *Node, prefix string) {
	value := node.Text
	for offset := 0; offset < len(value); {
		newline := strings.IndexByte(value[offset:], '\n')
		if newline < 0 {
			r.writeText(value[offset:])
			return
		}
		end := offset + newline + 1
		r.writeText(value[offset:end])
		r.writeSyntax(prefix)
		offset = end
	}
}

func codeBlockFence(node *Node) string {
	longest := 0
	for _, child := range node.Children {
		for run, char := 0, 0; ; {
			if char < len(child.Text) && child.Text[char] == '`' {
				run++
				if run > longest {
					longest = run
				}
				char++
				continue
			}
			if char == len(child.Text) {
				break
			}
			run = 0
			char++
		}
	}
	if longest < 2 {
		longest = 2
	}
	return strings.Repeat("`", longest+1)
}

func (r *renderer) closeMarks(marks []Mark, escapePipes bool) {
	for i := len(marks) - 1; i >= 0; i-- {
		r.closeInlineMark(marks[i], escapePipes)
	}
}

func (r *renderer) openInlineMark(mark Mark, nodes []*Node, index int) {
	if mark.Type != "inlineCode" {
		r.writeSyntax(openMark(mark))
		return
	}
	r.inlineCodeFence, r.inlineCodePadded = inlineCodeFence(nodes, index)
	r.writeSyntax(r.inlineCodeFence)
	if r.inlineCodePadded {
		r.writeSyntax(" ")
	}
}

func (r *renderer) closeInlineMark(mark Mark, escapePipes bool) {
	if mark.Type != "inlineCode" {
		r.writeSyntax(closeMark(mark, escapePipes))
		return
	}
	if r.inlineCodePadded {
		r.writeSyntax(" ")
	}
	r.writeSyntax(r.inlineCodeFence)
	r.inlineCodeFence = ""
	r.inlineCodePadded = false
}

func inlineCodeFence(nodes []*Node, index int) (string, bool) {
	var content strings.Builder
	for _, node := range nodes[index:] {
		if node.Type != "text" || !nodeHasMark(node, "inlineCode") {
			break
		}
		content.WriteString(node.Text)
	}
	value := content.String()
	longest, run := 0, 0
	for _, char := range value {
		if char == '`' {
			run++
			if run > longest {
				longest = run
			}
		} else {
			run = 0
		}
	}
	return strings.Repeat("`", longest+1), inlineCodePadding(value)
}

func inlineCodePadding(value string) bool {
	if strings.HasPrefix(value, "`") || strings.HasSuffix(value, "`") {
		return true
	}
	return strings.HasPrefix(value, " ") && strings.HasSuffix(value, " ") && strings.Trim(value, " ") != ""
}

func (r *renderer) writeSyntax(value string) {
	r.b.WriteString(value)
}

func (r *renderer) writeText(value string) {
	r.b.WriteString(value)
}

func (r *renderer) writeInlineText(node *Node, atLineStart *bool, escapePipes, escapeURLs bool) {
	if nodeHasMark(node, "inlineCode") {
		r.writeText(escapeTablePipes(node.Text, escapePipes))
		*atLineStart = strings.HasSuffix(node.Text, "\n")
		return
	}

	value := node.Text
	segmentStart := 0
	// Where the current line begins inside this node, or -1 when the line began in an earlier
	// one: a block marker is only a marker at the start of its own line.
	lineStart := -1
	if *atLineStart {
		lineStart = 0
	}
	for byteOffset, char := range value {
		width := utf8.RuneLen(char)
		if needsInlineEscape(value, byteOffset, char, lineStart, escapePipes, escapeURLs) {
			r.writeText(value[segmentStart:byteOffset])
			if char == '&' {
				r.writeText("&")
				r.writeSyntax("amp;")
			} else {
				r.writeSyntax("\\")
				r.writeText(value[byteOffset : byteOffset+width])
			}
			segmentStart = byteOffset + width
		}
		if char == '\n' {
			lineStart = byteOffset + width
		}
		*atLineStart = char == '\n'
	}
	r.writeText(value[segmentStart:])
}

func needsInlineEscape(value string, offset int, char rune, lineStart int, escapePipes, escapeURLs bool) bool {
	switch char {
	case '\\':
		return offset+1 < len(value) && isASCIIPunctuation(value[offset+1])
	case '*':
		return (blockStart(value, lineStart, offset) && markerTerminator(value, offset+1)) ||
			emphasisDelimiter(value, offset, '*')
	case '_':
		return emphasisDelimiter(value, offset, '_')
	case '`':
		return true
	case '[':
		return linkOpener(value, offset)
	case '(':
		return offset > 0 && value[offset-1] == ']'
	case ']':
		return false
	case '<':
		return angleConstruct(value, offset)
	case '&':
		return entityReference(value, offset)
	case ':':
		return escapeURLs && urlSchemeColon(value, offset)
	case '|':
		return escapePipes
	case '#':
		return blockStart(value, lineStart, offset) && atxHeadingRun(value, offset)
	case '>':
		return blockStart(value, lineStart, offset)
	case '-', '+':
		return blockStart(value, lineStart, offset) && markerTerminator(value, offset+1)
	case '.', ')':
		return orderedListMarkerPunctuation(value, offset, lineStart)
	default:
		return false
	}
}

// blockStart reports whether offset opens its own line: everything back to lineStart is
// indentation the parser skips, up to three spaces or a run of tabs. A marker one space in is
// still the marker — AGENTC-193's own payload was indented — and a line that began in an earlier
// text node (lineStart < 0) is never a block start here.
func blockStart(value string, lineStart, offset int) bool {
	if lineStart < 0 || lineStart > offset {
		return false
	}
	spaces := 0
	for index := lineStart; index < offset; index++ {
		switch value[index] {
		case ' ':
			spaces++
		case '\t':
		default:
			return false
		}
	}
	return spaces <= 3
}

// markerTerminator reports whether offset ends a list marker: the parser opens an item on a
// marker followed by a space, a tab, or the end of the line.
func markerTerminator(value string, offset int) bool {
	return offset >= len(value) || value[offset] == ' ' || value[offset] == '\t' || value[offset] == '\n'
}

// atxHeadingRun reports whether value opens an ATX heading marker at offset: one to six hashes
// ending the line or followed by a space or a tab. Escaping the first hash is enough to keep the
// whole run text, and without it a `## ` a replacement wrote into a paragraph reads back as a
// heading — or, inside a list item, as markdown the Proof schema refuses to import at all.
func atxHeadingRun(value string, offset int) bool {
	end := offset
	for end < len(value) && value[end] == '#' {
		end++
	}
	return end-offset <= 6 && markerTerminator(value, end)
}

func emphasisDelimiter(value string, offset int, delimiter byte) bool {
	start, end := offset, offset+1
	for start > 0 && value[start-1] == delimiter {
		start--
	}
	for end < len(value) && value[end] == delimiter {
		end++
	}
	before := start > 0 && isASCIIAlphaNumeric(value[start-1])
	after := end < len(value) && isASCIIAlphaNumeric(value[end])
	return (delimiter != '_' || !before || !after) && (before || after)
}

func linkOpener(value string, offset int) bool {
	closing := strings.IndexByte(value[offset+1:], ']')
	if closing < 0 {
		return false
	}
	closing += offset + 1
	return linkCloser(value, closing)
}

func linkCloser(value string, offset int) bool {
	return offset+1 < len(value) && (value[offset+1] == '(' || value[offset+1] == ':')
}

func isBareURLLink(node *Node, marks []Mark, escapePipes bool) bool {
	for _, mark := range marks {
		if mark.Type != "link" {
			continue
		}
		href, _ := mark.Attrs["href"].(string)
		title, _ := mark.Attrs["title"].(string)
		return (!escapePipes || !strings.Contains(node.Text, "|")) && href == node.Text && title == "" && isBareAutolink(node.Text)
	}
	return false
}

func isBareAutolink(value string) bool {
	if !strings.HasPrefix(value, "http://") && !strings.HasPrefix(value, "https://") {
		return false
	}
	if strings.ContainsAny(value, " \t\r\n()<>") {
		return false
	}
	return !strings.ContainsAny(value[len(value)-1:], ".,!?;:")
}

func containsMark(marks []Mark, markType string) bool {
	for _, mark := range marks {
		if mark.Type == markType {
			return true
		}
	}
	return false
}

func withoutMark(marks []Mark, markType string) []Mark {
	out := marks[:0]
	for _, mark := range marks {
		if mark.Type != markType {
			out = append(out, mark)
		}
	}
	return out
}

func angleConstruct(value string, offset int) bool {
	if offset+1 >= len(value) || !isASCIIAlphaNumeric(value[offset+1]) && value[offset+1] != '/' && value[offset+1] != '!' && value[offset+1] != '?' {
		return false
	}
	return strings.IndexByte(value[offset+1:], '>') >= 0
}

func entityReference(value string, offset int) bool {
	end := strings.IndexByte(value[offset+1:], ';')
	if end < 0 {
		return false
	}
	for _, char := range value[offset+1 : offset+end+1] {
		if !isASCIIAlphaNumeric(byte(char)) && char != '#' {
			return false
		}
	}
	return true
}

func urlSchemeColon(value string, offset int) bool {
	return strings.HasSuffix(value[:offset], "http") || strings.HasSuffix(value[:offset], "https")
}

func isASCIIAlphaNumeric(value byte) bool {
	return value >= 'a' && value <= 'z' || value >= 'A' && value <= 'Z' || value >= '0' && value <= '9'
}

func isASCIIPunctuation(value byte) bool {
	return value >= '!' && value <= '/' || value >= ':' && value <= '@' || value >= '[' && value <= '`' || value >= '{' && value <= '~'
}

// orderedListMarkerPunctuation reports whether the `.` or `)` at offset closes an ordered-list
// marker: digits back to the start of an indented line, and a marker terminator after it.
func orderedListMarkerPunctuation(value string, offset int, lineStart int) bool {
	if !markerTerminator(value, offset+1) {
		return false
	}
	start := offset
	for start > 0 && value[start-1] >= '0' && value[start-1] <= '9' {
		start--
	}
	return start < offset && blockStart(value, lineStart, start)
}

func nodeHasMark(node *Node, markType string) bool {
	for _, mark := range node.Marks {
		if mark.Type == markType {
			return true
		}
	}
	return false
}

// renderedMarkTypes are the marks canonical Markdown writes. Every other mark the schema allows
// (markTypes) is an anchor, invisible to the rendering, and StripAnchorMarks removes exactly the
// marks that are not here: what renders is one list, not two of opposite polarity that a new
// invisible mark could fall between.
var renderedMarkTypes = map[string]bool{
	"link": true, "strong": true, "emphasis": true, "strike_through": true, "inlineCode": true,
}

func visibleMarks(marks []Mark) []Mark {
	out := make([]Mark, 0, len(marks))
	for _, mark := range marks {
		if renderedMarkTypes[mark.Type] {
			out = append(out, mark)
		}
	}
	sortMarks(out)
	for i := range out {
		for j := i + 1; j < len(out); j++ {
			if renderMarkRank(out[j].Type) < renderMarkRank(out[i].Type) {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out
}

func sharedMarks(left, right []Mark) int {
	limit := len(left)
	if len(right) < limit {
		limit = len(right)
	}
	for i := range limit {
		if left[i].Type != right[i].Type || !attrsEqual(left[i].Attrs, right[i].Attrs) {
			return i
		}
	}
	return limit
}

func renderMarkRank(markType string) int {
	switch markType {
	case "link":
		return 0
	case "strong":
		return 1
	case "emphasis":
		return 2
	case "strike_through":
		return 3
	case "inlineCode":
		return 4
	default:
		return 5
	}
}

func openMark(mark Mark) string {
	switch mark.Type {
	case "link":
		return "["
	case "strong":
		return "**"
	case "emphasis":
		return "*"
	case "strike_through":
		return "~~"
	case "inlineCode":
		return "`"
	default:
		return ""
	}
}

func closeMark(mark Mark, escapePipes bool) string {
	if mark.Type == "link" {
		href, _ := mark.Attrs["href"].(string)
		title, _ := mark.Attrs["title"].(string)
		return "](" + escapeLinkDestination(href, escapePipes) + titleSuffix(title, escapePipes) + ")"
	}
	return openMark(mark)
}

func escapeLinkDestination(href string, escapePipes bool) string {
	if escapePipes {
		href = escapeTableLinkDestination(href)
	}
	if strings.ContainsAny(href, " ()") {
		return "<" + href + ">"
	}
	return href
}

func titleSuffix(title string, escapePipes bool) string {
	if title == "" {
		return ""
	}
	title = strings.ReplaceAll(title, "\\", "\\\\")
	title = strings.ReplaceAll(title, "\"", "\\\"")
	return " \"" + escapeTablePipes(title, escapePipes) + "\""
}

func escapeTablePipes(value string, escapePipes bool) string {
	if !escapePipes {
		return value
	}
	return strings.ReplaceAll(value, "|", "\\|")
}

func escapeTableHTMLPipes(value string, escapePipes bool) string {
	if !escapePipes {
		return value
	}
	return strings.ReplaceAll(value, "|", "&#124;")
}

func escapeTableLinkDestination(value string) string {
	if !strings.ContainsAny(value, "\\|") {
		return value
	}
	var rendered strings.Builder
	rendered.Grow(len(value))
	for index := range len(value) {
		char := value[index]
		if char == '\\' && index+1 < len(value) && isASCIIPunctuation(value[index+1]) {
			rendered.WriteByte('\\')
		}
		if char == '|' {
			rendered.WriteByte('\\')
		}
		rendered.WriteByte(char)
	}
	return rendered.String()
}
func escapeFootnoteLabel(label string) string {
	return escapeTableSyntaxPipes(label)
}

func escapeTableSyntaxPipes(value string) string {
	if !strings.Contains(value, "|") {
		return value
	}
	var escaped bool
	var rendered strings.Builder
	rendered.Grow(len(value))
	for _, char := range value {
		if char == '|' && !escaped {
			rendered.WriteByte('\\')
		}
		rendered.WriteRune(char)
		escaped = char == '\\'
	}
	return rendered.String()
}

func tableAlignment(value any) string {
	alignment, _ := value.(string)
	switch alignment {
	case "center":
		return ":---:"
	case "right":
		return "---:"
	default:
		return ":---"
	}
}

func num(value any, fallback float64) float64 {
	switch number := value.(type) {
	case float64:
		return number
	case float32:
		return float64(number)
	case int:
		return float64(number)
	case int64:
		return float64(number)
	default:
		return fallback
	}
}
