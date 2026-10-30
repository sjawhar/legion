package pmdoc

import (
	"bytes"
	"html"
	"strings"
	"unicode/utf8"

	"github.com/yuin/goldmark/util"
)

// decodeMarkdownText is text as written in markdown, read as the browser editor's parser reads
// it: in one pass from the left, a backslash before ASCII punctuation is that character, so an
// escaped `&` opens no reference, and a character reference (characterReferenceAt) is the
// characters it names. Everything else is kept as written.
func decodeMarkdownText(value []byte) string {
	decoded, _ := decodeMarkdown(value, false)
	return decoded
}

// decodeMarkdown is decodeMarkdownText's text of value, and where withOffsets the offset in value
// each of its offsets, and its end, begins at, -1 inside what one escape or character reference
// decodes to; the offsets are nil where value holds no backslash and no `&`, so that each is its
// own.
func decodeMarkdown(value []byte, withOffsets bool) (string, []int32) {
	if !bytes.ContainsAny(value, "\\&") {
		return string(value), nil
	}
	var decoded strings.Builder
	decoded.Grow(len(value))
	var offsets []int32
	if withOffsets {
		offsets = make([]int32, 0, len(value)+1)
	}
	for index := 0; index < len(value); index++ {
		if withOffsets {
			offsets = append(offsets, int32(index))
		}
		switch value[index] {
		case '\\':
			if index+1 < len(value) && util.IsPunct(value[index+1]) {
				index++
			}
		case '&':
			if length, characters, ok := characterReferenceAt(value, index); ok {
				decoded.WriteString(characters)
				if withOffsets {
					for range len(characters) - 1 {
						offsets = append(offsets, -1)
					}
				}
				index += length - 1
				continue
			}
		}
		decoded.WriteByte(value[index])
	}
	if withOffsets {
		offsets = append(offsets, int32(len(value)))
	}
	return decoded.String(), offsets
}

// characterReferenceAt reports whether the `&` at offset at opens a character reference the
// browser editor's parser decodes, how many bytes it takes, and the characters it names. A
// reference is shaped as characterReferenceLength reads it. A numeric one names its code point, or
// U+FFFD where the parser refuses the code point (numericReferenceCharacter). A named one is
// decoded as HTML decodes it, but for `&nLt;` and `&nGt;`, which Go's table lacks, and counts only
// when that changed it and did not stop short of its `;`, as `&notit;` decodes to `¬it;`; `&semi;`
// alone names a `;`. Anything else is not a reference and stays text.
func characterReferenceAt[T string | []byte](value T, at int) (length int, characters string, ok bool) {
	length = characterReferenceLength(value, at)
	if length == 0 {
		return 0, "", false
	}
	if value[at+1] == '#' {
		start, base := at+2, 10
		if value[start] == 'x' || value[start] == 'X' {
			start, base = start+1, 16
		}
		code := 0
		for index := start; index < at+length-1; index++ {
			digit, _ := digitValue(value[index], base)
			code = code*base + digit
		}
		return length, string(numericReferenceCharacter(code)), true
	}
	reference := string(value[at : at+length])
	// Go's html table omits these two (src/html/entity.go, a TODO); character-entities names them.
	switch reference {
	case "&nLt;":
		return length, "\u226A\u20D2", true
	case "&nGt;":
		return length, "\u226B\u20D2", true
	}
	decoded := html.UnescapeString(reference)
	if decoded == reference || strings.HasSuffix(decoded, ";") && reference != "&semi;" {
		return 0, "", false
	}
	return length, decoded, true
}

// characterReferenceLength is how many bytes the character reference that the `&` at offset at
// opens takes, or 0 where it opens none. A reference is shaped as `&name;`, a name being an ASCII
// letter then ASCII letters and digits, `&#` and one to seven decimal digits then `;`, or `&#x` or
// `&#X` and one to six hexadecimal digits then `;`; whether the parser decodes it is
// characterReferenceAt's to say.
func characterReferenceLength[T string | []byte](value T, at int) int {
	end := at + 1
	if end >= len(value) {
		return 0
	}
	if value[end] == '#' {
		end++
		base, limit := 10, 7
		if end < len(value) && (value[end] == 'x' || value[end] == 'X') {
			end++
			base, limit = 16, 6
		}
		start := end
		for end < len(value) && end-start < limit {
			if _, isDigit := digitValue(value[end], base); !isDigit {
				break
			}
			end++
		}
		if end == start {
			return 0
		}
	} else {
		if !isASCIILetter(value[end]) {
			return 0
		}
		for end < len(value) && isASCIIAlphaNumeric(value[end]) {
			end++
		}
	}
	if end >= len(value) || value[end] != ';' {
		return 0
	}
	return end + 1 - at
}

// numericReferenceCharacter is the character a numeric reference to code names for the browser
// editor's parser: U+FFFD for a control character other than tab, line feed, form feed and
// carriage return, a surrogate, a noncharacter, or a code past U+10FFFF, and the code point
// otherwise.
func numericReferenceCharacter(code int) rune {
	if code < 9 || code == 11 || code > 13 && code < 32 || code > 126 && code < 160 ||
		code > 55295 && code < 57344 || code > 64975 && code < 65008 ||
		code&0xFFFF == 0xFFFF || code&0xFFFF == 0xFFFE || code > 0x10FFFF {
		return utf8.RuneError
	}
	return rune(code)
}

// digitValue is the value of the digit char in base 10 or 16, and whether char is one.
func digitValue(char byte, base int) (int, bool) {
	switch {
	case char >= '0' && char <= '9':
		return int(char - '0'), true
	case base == 16 && char >= 'a' && char <= 'f':
		return int(char-'a') + 10, true
	case base == 16 && char >= 'A' && char <= 'F':
		return int(char-'A') + 10, true
	}
	return 0, false
}
