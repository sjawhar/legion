package pmdoc

import (
	"fmt"
	"html"
	"strconv"
	"strings"
	"unicode"

	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/parser"
	gmtext "github.com/yuin/goldmark/text"
	"github.com/yuin/goldmark/util"
)

var kindTypedDirective = ast.NewNodeKind("TypedDirective")
var kindUnsupportedDirective = ast.NewNodeKind("UnsupportedDirective")

type typedDirective struct {
	ast.BaseBlock
	Name   string
	Attrs  Attrs
	Closed bool
	indent int
	// fence is the number of colons the directive opened with; only a line of exactly as many
	// closes it, so a typed block written with a longer fence holds one written with a shorter.
	fence int
}

func (n *typedDirective) Dump(source []byte, level int) {
	ast.DumpHelper(n, source, level, nil, nil)
}

func (n *typedDirective) Kind() ast.NodeKind {
	return kindTypedDirective
}

type unsupportedDirective struct {
	ast.BaseBlock
	Reason string
}

func (n *unsupportedDirective) Dump(source []byte, level int) {
	ast.DumpHelper(n, source, level, nil, nil)
}

func (n *unsupportedDirective) Kind() ast.NodeKind {
	return kindUnsupportedDirective
}

type typedDirectiveParser struct{}

func (p *typedDirectiveParser) Trigger() []byte {
	return []byte{':'}
}

func (p *typedDirectiveParser) Open(_ ast.Node, reader gmtext.Reader, pc parser.Context) (ast.Node, parser.State) {
	line, _ := reader.PeekLine()
	indent, offset := util.IndentWidth(line, reader.LineOffset())
	if indent >= 4 || offset >= len(line) {
		return nil, parser.NoChildren
	}
	name, attrs, ok := parseTypedDirectiveOpen(strings.TrimRight(string(line[offset:]), "\r\n"))
	if !ok {
		return nil, parser.NoChildren
	}
	reader.AdvanceToEOL()
	return &typedDirective{Name: name, Attrs: attrs, indent: indent, fence: colonRun(line[offset:])}, parser.HasChildren
}

func (p *typedDirectiveParser) Continue(node ast.Node, reader gmtext.Reader, _ parser.Context) parser.State {
	directive, ok := node.(*typedDirective)
	if !ok {
		return parser.Close
	}
	line, _ := reader.PeekLine()
	indent, offset := util.IndentWidth(line, reader.LineOffset())
	// A fence closes the typed block when it is indented no further than the opener. The opener
	// of a typed block that begins a footnote definition stands after the definition's `]: `, one
	// column past where the definition's later lines start.
	if indent <= directive.indent && offset < len(line) && fenceColons(string(line[offset:])) == directive.fence {
		directive.Closed = true
		reader.AdvanceToEOL()
		return parser.Close
	}
	return parser.Continue | parser.HasChildren
}

func (p *typedDirectiveParser) Close(_ ast.Node, _ gmtext.Reader, _ parser.Context) {}

// closingColons is the longest line of colons written inside typed block n, whose own lines start
// at column on the written line, that the browser editor's parser could read as a fence closing n:
// a typed-fence line (TypedFenceLine) whose text starts at most three columns past column, even
// inside fenced code. Columns are the written line's: a list marker adds its width, and a tab
// advances to the next multiple of four from the column it stands at, so in a typed block two
// columns in, a tab reaches only two past it. A line in a blockquote begins with its `>` and closes
// nothing outside it. Such a line is a line of code, or the fence of a typed block nested where its
// fence is written so. That parser closes n at such a line of at least as many colons as n's fence,
// and this one at a line of exactly as many indented no further than n's opener, so a fence longer
// than every such line reads the same in both (typedFence).
func closingColons(n *Node, column int) int {
	longest := 0
	var visit func(node *Node, at int)
	visit = func(node *Node, at int) {
		if at-column > 3 {
			return
		}
		if _, typed := typedBlock(node.Type); typed {
			longest = max(longest, typedFence(node, at))
			return
		}
		switch node.Type {
		case "code_block":
			for _, text := range node.Children {
				for _, line := range strings.Split(string(lineEnds([]byte(text.Text))), "\n") {
					if colons := fenceColons(line); colons >= 3 && textColumn(line, at)-column <= 3 {
						longest = max(longest, colons)
					}
				}
			}
		case "blockquote", "footnote_definition":
		case "bullet_list", "ordered_list":
			start := int(num(node.Attrs["order"], 1))
			for index, item := range node.Children {
				marker := 2
				if node.Type == "ordered_list" {
					marker = len(strconv.Itoa(start+index)) + 2
				}
				for _, child := range item.Children {
					visit(child, at+marker)
				}
			}
		default:
			for _, child := range node.Children {
				visit(child, at)
			}
		}
	}
	for _, child := range n.Children {
		visit(child, column)
	}
	return longest
}

// textColumn is the column a line's text starts at when the line is written from column, a tab
// advancing to the next multiple of four.
func textColumn(line string, column int) int {
	for _, char := range line {
		switch char {
		case ' ':
			column++
		case '\t':
			column += 4 - column%4
		default:
			return column
		}
	}
	return column
}

// TypedFenceLine reports whether the browser editor's parser reads line, where it stands, as a
// typed block's fence: three or more colons with only spaces and tabs around them, and the carriage
// return of a line that ends in one, since a carriage return before a line feed is part of the
// line ending.
func TypedFenceLine(line string) bool {
	return fenceColons(line) >= 3
}

// fenceColons is the number of colons in line when they are all it holds but spaces, tabs and a
// line ending, and 0 otherwise.
func fenceColons(line string) int {
	return colonLine(strings.Trim(line, " \t\r\n"))
}

// colonLine is the length of line when it is colons alone, and 0 otherwise.
func colonLine(line string) int {
	if line == "" || strings.Trim(line, ":") != "" {
		return 0
	}
	return len(line)
}

// colonRun is the number of colons line begins with.
func colonRun(line []byte) int {
	count := 0
	for count < len(line) && line[count] == ':' {
		count++
	}
	return count
}

func (p *typedDirectiveParser) CanInterruptParagraph() bool {
	return true
}

func (p *typedDirectiveParser) CanAcceptIndentedLine() bool {
	return false
}

type unsupportedDirectiveParser struct{}

func (p *unsupportedDirectiveParser) Trigger() []byte {
	return []byte{':'}
}

func (p *unsupportedDirectiveParser) Open(_ ast.Node, reader gmtext.Reader, _ parser.Context) (ast.Node, parser.State) {
	line, _ := reader.PeekLine()
	indent, offset := util.IndentWidth(line, reader.LineOffset())
	if indent >= 4 || offset >= len(line) {
		return nil, parser.NoChildren
	}
	reason, ok := unsupportedDirectiveReason(strings.TrimRight(string(line[offset:]), "\r\n"))
	if !ok {
		return nil, parser.NoChildren
	}
	reader.AdvanceToEOL()
	return &unsupportedDirective{Reason: reason}, parser.NoChildren
}

func (p *unsupportedDirectiveParser) Continue(ast.Node, gmtext.Reader, parser.Context) parser.State {
	return parser.Close
}

func (p *unsupportedDirectiveParser) Close(ast.Node, gmtext.Reader, parser.Context) {}

func (p *unsupportedDirectiveParser) CanInterruptParagraph() bool {
	return true
}

func (p *unsupportedDirectiveParser) CanAcceptIndentedLine() bool {
	return false
}

func parseTypedDirectiveOpen(line string) (string, Attrs, bool) {
	colons := colonRun([]byte(line))
	if colons < 3 {
		return "", nil, false
	}
	rest := line[colons:]
	nameEnd := 0
	for nameEnd < len(rest) && directiveNameByte(rest[nameEnd]) {
		nameEnd++
	}
	if nameEnd == 0 || nameEnd == len(rest) || rest[nameEnd] != '{' || !strings.HasSuffix(rest, "}") {
		return "", nil, false
	}
	name := rest[:nameEnd]
	attrs, err := parseDirectiveAttributes(rest[nameEnd+1 : len(rest)-1])
	if err != nil {
		return "", nil, false
	}
	return name, attrs, true
}

func parseDirectiveAttributes(value string) (Attrs, error) {
	attrs := Attrs{}
	for offset := 0; offset < len(value); {
		for offset < len(value) && unicode.IsSpace(rune(value[offset])) {
			offset++
		}
		if offset == len(value) {
			break
		}
		switch value[offset] {
		case '#':
			start := offset + 1
			offset = start
			for offset < len(value) && directiveNameByte(value[offset]) {
				offset++
			}
			if start == offset || attrs[BlockIDAttr] != nil {
				return nil, fmt.Errorf("invalid id attribute")
			}
			attrs[BlockIDAttr] = value[start:offset]
		case '.':
			start := offset + 1
			offset = start
			for offset < len(value) && directiveNameByte(value[offset]) {
				offset++
			}
			if start == offset {
				return nil, fmt.Errorf("invalid class attribute")
			}
			class := value[start:offset]
			if previous, ok := attrs["class"].(string); ok {
				attrs["class"] = previous + " " + class
			} else {
				attrs["class"] = class
			}
		default:
			start := offset
			for offset < len(value) && directiveNameByte(value[offset]) {
				offset++
			}
			if start == offset || offset == len(value) || value[offset] != '=' {
				return nil, fmt.Errorf("expected key=\"value\"")
			}
			name := value[start:offset]
			offset++
			if offset == len(value) || value[offset] != '"' || attrs[name] != nil {
				return nil, fmt.Errorf("expected quoted value for %q", name)
			}
			offset++
			start = offset
			escaped := false
			for offset < len(value) {
				if value[offset] == '"' && !escaped {
					break
				}
				escaped = value[offset] == '\\' && !escaped
				if value[offset] != '\\' {
					escaped = false
				}
				offset++
			}
			if offset == len(value) {
				return nil, fmt.Errorf("unterminated value for %q", name)
			}
			unquoted, err := strconv.Unquote(value[start-1 : offset+1])
			if err != nil {
				return nil, fmt.Errorf("decode value for %q: %w", name, err)
			}
			attrs[name] = html.UnescapeString(unquoted)
			offset++
		}
		if offset < len(value) && !unicode.IsSpace(rune(value[offset])) {
			return nil, fmt.Errorf("attributes must be separated by whitespace")
		}
	}
	return attrs, nil
}

func directiveName(name string) bool {
	if name == "" {
		return false
	}
	for _, char := range name {
		if !(char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' || char >= '0' && char <= '9' || char == '_' || char == '-') {
			return false
		}
	}
	return true
}

func directiveNameByte(char byte) bool {
	return char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' || char >= '0' && char <= '9' || char == '_' || char == '-'
}

func unsupportedDirectiveReason(line string) (string, bool) {
	if strings.HasPrefix(line, ":::") {
		if line == ":::" {
			return "", false
		}
		if _, _, ok := parseTypedDirectiveOpen(line); !ok {
			return "typed block directives use :::name{...}; Pandoc fenced divs and malformed directives are not supported", true
		}
		return "", false
	}
	if strings.HasPrefix(line, "::") && len(line) > 2 && directiveNameByte(line[2]) {
		return "leaf directives (::name) are not supported", true
	}
	if strings.HasPrefix(line, ":") && len(line) > 1 && directiveNameByte(line[1]) && strings.Contains(line, "{") {
		return "text directives (:name{...}) are not supported", true
	}
	return "", false
}
