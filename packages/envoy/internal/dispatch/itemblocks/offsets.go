package itemblocks

import (
	"fmt"
	"unicode/utf8"
)

// The blocks routes and the dashboard editor count a block's offsets as a JavaScript string does,
// in UTF-16 code units; Block counts them in bytes of the UTF-8 body. An offset on either side must
// fall between two characters: one inside a UTF-8 sequence, or between the two units of a
// surrogate pair, is refused. A byte that is not valid UTF-8 counts as one character of one unit,
// as the JSON encoder writes it as U+FFFD.

// UTF16Offset is byteOffset, a byte offset into body, as a count of UTF-16 code units.
func UTF16Offset(body string, byteOffset int) (int, error) {
	if byteOffset < 0 || byteOffset > len(body) {
		return 0, fmt.Errorf("byte offset %d is outside the body of %d bytes", byteOffset, len(body))
	}
	units := 0
	at := 0
	for at < byteOffset {
		r, size := utf8.DecodeRuneInString(body[at:])
		at += size
		units += utf16Units(r)
	}
	if at != byteOffset {
		return 0, fmt.Errorf("byte offset %d is inside a character", byteOffset)
	}
	return units, nil
}

// ByteOffset is utf16, a count of UTF-16 code units into body, as a byte offset.
func ByteOffset(body string, utf16 int) (int, error) {
	if utf16 < 0 {
		return 0, fmt.Errorf("offset %d is outside the body", utf16)
	}
	units := 0
	at := 0
	for units < utf16 && at < len(body) {
		r, size := utf8.DecodeRuneInString(body[at:])
		at += size
		units += utf16Units(r)
	}
	switch {
	case units > utf16:
		return 0, fmt.Errorf("offset %d is inside a character", utf16)
	case units < utf16:
		return 0, fmt.Errorf("offset %d is outside the body of %d UTF-16 units", utf16, units)
	}
	return at, nil
}

// utf16Units is how many UTF-16 code units r takes: two for a character outside the Basic
// Multilingual Plane (a surrogate pair), else one.
func utf16Units(r rune) int {
	if r >= 0x10000 {
		return 2
	}
	return 1
}
