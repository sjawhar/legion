package pmdoc

import (
	"fmt"
	"unicode/utf16"
)

func len16(value string) int {
	return len(utf16.Encode([]rune(value)))
}

// splitsPair reports whether position at, an index between UTF-16 units, falls inside a surrogate
// pair: the unit before it is a high surrogate and the unit after it a low one. A cut there leaves
// two halves that no Go string holds, and each reads back as U+FFFD.
func splitsPair(units []uint16, at int) bool {
	return at > 0 && at < len(units) && units[at-1]&0xFC00 == 0xD800 && units[at]&0xFC00 == 0xDC00
}

// slice16 is value between UTF-16 positions from and to. A position inside a surrogate pair is
// refused, since a slice cut there would hold half a character and a splice around it would write
// the other half twice or not at all; every range the matchers return ends between characters.
func slice16(value string, from, to int) (string, error) {
	units := utf16.Encode([]rune(value))
	if splitsPair(units, from) || splitsPair(units, to) {
		return "", fmt.Errorf("%w: range %d-%d cuts a surrogate pair", ErrSchema, from, to)
	}
	return string(utf16.Decode(units[from:to])), nil
}

// context16 is value between UTF-16 positions from and to for a reader to see, each end moved
// outward off the inside of a surrogate pair so the text holds every character it touches whole.
func context16(value string, from, to int) string {
	units := utf16.Encode([]rune(value))
	if splitsPair(units, from) {
		from--
	}
	if splitsPair(units, to) {
		to++
	}
	return string(utf16.Decode(units[from:to]))
}
