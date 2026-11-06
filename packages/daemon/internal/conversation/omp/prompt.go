package omp

import (
	"regexp"
	"strings"
	"unicode/utf8"
)

// The pinned Oh My Pi's context templates, byte for byte (each file ends in a newline): the
// compaction summary's and the handoff summary's (agent/src/compaction/prompts/
// compaction-summary-context.md, handoff-summary-context.md), the branch summary's
// (branch-summary-context.md), and the envelope of a steering user message
// (coding-agent/src/prompts/steering/user-interjection.md). Each holds one {{summary}} or
// {{message}}, which Oh My Pi's Handlebars fills without escaping.
const (
	compactionSummaryTemplate = "Prior model work/tool state available.\n" +
		"MUST build on prior work; NEVER duplicate prior work.\n\n<summary>\n{{summary}}\n</summary>\n"
	handoffSummaryTemplate = "Context replaced. The <handoff> below is a handoff document a prior instance of you wrote from the full conversation. It is your own working memory, not user input.\n" +
		"- First person inside it refers to you (the prior instance).\n" +
		"- \"Next Steps\" is your own resumed plan; re-check it against the latest user message before acting.\n" +
		"- The handoff already exists and is complete: NEVER write another handoff document unless the user explicitly asks.\n" +
		"MUST build on prior work; NEVER duplicate prior work.\n\n<handoff>\n{{summary}}\n</handoff>\n"
	branchSummaryTemplate = "Branch-return summary:\n\n<summary>\n{{summary}}\n</summary>\n"
	steeringTemplate      = "<system-notice>\n" +
		"User interjection during work: priority; supersedes conflicting prior instructions. Re-read; ensure current work reflects user intent.\n" +
		"</system-notice>\n{{message}}\n"
)

// render is Oh My Pi's prompt.render for a template holding one placeholder: the value put in
// as it is, then the post-render format.
func render(template, placeholder, value string) string {
	return formatPrompt(strings.Replace(template, placeholder, value, 1))
}

// formatPrompt is Oh My Pi's prompt format in its post-render phase with no option set (pi-utils
// prompt.ts format): each line's trailing whitespace trimmed, a table row's cell padding and a
// table separator's dashes compacted outside code fences, a run of two or more blank lines removed
// whole and a single blank line kept between text, blank lines dropped at the start, before a
// closing XML tag line and at the end.
func formatPrompt(content string) string {
	lines := strings.Split(content, "\n")
	result := make([]string, 0, len(lines))
	popBlanks := func() {
		for len(result) > 0 && result[len(result)-1] == "" {
			result = result[:len(result)-1]
		}
	}
	inCodeBlock := false
	for i := 0; i < len(lines); i++ {
		line := strings.TrimRightFunc(lines[i], isJSSpace)
		indent := 0
		for indent < len(line) && (line[indent] == ' ' || line[indent] == '\t') {
			indent++
		}
		if indent < len(line) && line[indent] >= utf8.RuneSelf {
			indent = len(line) - len(strings.TrimLeftFunc(line, isJSSpace))
		}
		rest := line[indent:]
		if strings.HasPrefix(rest, "```") || strings.HasPrefix(rest, "~~~") {
			inCodeBlock = !inCodeBlock
			result = append(result, line)
			continue
		}
		if inCodeBlock {
			result = append(result, line)
			continue
		}
		closing := false
		switch {
		case strings.HasPrefix(rest, "</"):
			closing = isClosingTag(rest)
		case strings.HasPrefix(rest, "|"):
			if tableSeparator.MatchString(rest) {
				line = line[:indent] + compactTableSeparator(rest)
			} else if isTableRow(rest) {
				line = line[:indent] + compactTableRow(rest)
			}
		}
		if indent >= len(line) {
			if i+1 >= len(lines) || strings.TrimFunc(lines[i+1], isJSSpace) == "" {
				popBlanks()
				j := i + 1
				for j < len(lines) && strings.TrimFunc(lines[j], isJSSpace) == "" {
					j++
				}
				i = j - 1
				continue
			}
			if len(result) == 0 || result[len(result)-1] == "" {
				continue
			}
		}
		if closing {
			popBlanks()
		}
		result = append(result, line)
	}
	popBlanks()
	return strings.Join(result, "\n")
}

// isClosingTag is whether s, which starts "</", is a closing tag of lower-case letters, '-' and
// '_' and nothing after it.
func isClosingTag(s string) bool {
	if len(s) < 4 || s[len(s)-1] != '>' {
		return false
	}
	for _, c := range []byte(s[2 : len(s)-1]) {
		if !(c >= 'a' && c <= 'z' || c == '-' || c == '_') {
			return false
		}
	}
	return true
}

// tableSeparator is the format's /^\|[-:\s|]+\|$/, \s being JavaScript's white space.
var tableSeparator = regexp.MustCompile(`^\|[-:\s\x{00A0}\x{1680}\x{2000}-\x{200A}\x{2028}\x{2029}\x{202F}\x{205F}\x{3000}\x{FEFF}|]+\|$`)

// isTableRow is the format's /^\|.*\|$/: '.' matches no line terminator.
func isTableRow(s string) bool {
	return len(s) >= 2 && strings.HasSuffix(s, "|") && !strings.ContainsAny(s, "\r\n\u2028\u2029")
}

func compactTableRow(line string) string {
	cells := strings.Split(line, "|")
	for i, cell := range cells {
		cells[i] = strings.TrimFunc(cell, isJSSpace)
	}
	return strings.Join(cells, "|")
}

func compactTableSeparator(line string) string {
	var cells []string
	for _, cell := range strings.Split(line, "|") {
		trimmed := strings.TrimFunc(cell, isJSSpace)
		if trimmed == "" {
			continue
		}
		left, right := strings.HasPrefix(trimmed, ":"), strings.HasSuffix(trimmed, ":")
		switch {
		case left && right:
			cells = append(cells, ":---:")
		case left:
			cells = append(cells, ":---")
		case right:
			cells = append(cells, "---:")
		default:
			cells = append(cells, "---")
		}
	}
	return "|" + strings.Join(cells, "|") + "|"
}

// isJSSpace is JavaScript's white space and line terminators, what String.prototype.trim and \s
// match.
func isJSSpace(r rune) bool {
	switch r {
	case '\t', '\n', '\v', '\f', '\r', ' ', 0xA0, 0x1680, 0x2028, 0x2029, 0x202F, 0x205F, 0x3000, 0xFEFF:
		return true
	}
	return r >= 0x2000 && r <= 0x200A
}
