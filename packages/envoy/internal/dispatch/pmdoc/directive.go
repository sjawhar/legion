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
	return &typedDirective{Name: name, Attrs: attrs, indent: indent}, parser.HasChildren
}

func (p *typedDirectiveParser) Continue(node ast.Node, reader gmtext.Reader, _ parser.Context) parser.State {
	directive, ok := node.(*typedDirective)
	if !ok {
		return parser.Close
	}
	line, _ := reader.PeekLine()
	indent, offset := util.IndentWidth(line, reader.LineOffset())
	if indent == directive.indent && offset < len(line) && strings.TrimSpace(string(line[offset:])) == ":::" {
		directive.Closed = true
		reader.AdvanceToEOL()
		return parser.Close
	}
	return parser.Continue | parser.HasChildren
}

func (p *typedDirectiveParser) Close(_ ast.Node, _ gmtext.Reader, _ parser.Context) {}

// TypedFenceLineInCode returns a line of code inside a typed block that the browser editor's
// parser reads as the typed block's closing fence, and false when block holds none. That parser
// ends a typed block at a typed-fence line (typedFenceLine) whose text starts at most three columns
// past the typed block's own content column, even inside a fenced code block it holds. Columns are the written line's: a blockquote's `> ` adds two, a list marker its
// width, a footnote definition four, and a tab advances to the next multiple of four from the column
// it stands at. A line inside a blockquote begins with its `>`, so it closes no typed block outside
// that blockquote.
func TypedFenceLineInCode(block *Node) (string, bool) {
	// fence is the content column of the nearest typed block a line could close, or -1.
	var find func(node *Node, column, fence int) (string, bool)
	find = func(node *Node, column, fence int) (string, bool) {
		if _, ok := typedBlock(node.Type); ok {
			fence = column
		}
		switch node.Type {
		case "code_block":
			if fence < 0 {
				return "", false
			}
			for _, text := range node.Children {
				for _, line := range strings.Split(text.Text, "\n") {
					if typedFenceLine(line) && textColumn(line, column)-fence <= 3 {
						return line, true
					}
				}
			}
			return "", false
		case "blockquote":
			column, fence = column+2, -1
		case "footnote_definition":
			column += 4
		case "bullet_list", "ordered_list":
			start := int(num(node.Attrs["order"], 1))
			for index, item := range node.Children {
				marker := 2
				if node.Type == "ordered_list" {
					marker = len(strconv.Itoa(start+index)) + 2
				}
				for _, child := range item.Children {
					if line, ok := find(child, column+marker, fence); ok {
						return line, true
					}
				}
			}
			return "", false
		}
		for _, child := range node.Children {
			if line, ok := find(child, column, fence); ok {
				return line, true
			}
		}
		return "", false
	}
	return find(block, 0, -1)
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

// typedFenceLine reports whether the browser editor's parser reads line, where it stands, as a
// typed block's fence: three or more colons with only spaces and tabs around them, and the carriage
// return of a line that ends in one, since a carriage return before a line feed is part of the
// line ending.
func typedFenceLine(line string) bool {
	return colonLine(strings.Trim(line, " \t\r")) >= 3
}

// colonLine is the length of line when it is colons alone, and 0 otherwise.
func colonLine(line string) int {
	if line == "" || strings.Trim(line, ":") != "" {
		return 0
	}
	return len(line)
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
	if !strings.HasPrefix(line, ":::") {
		return "", nil, false
	}
	rest := line[3:]
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
