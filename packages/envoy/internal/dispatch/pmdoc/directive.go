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
