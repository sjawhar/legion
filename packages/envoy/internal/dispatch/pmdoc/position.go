package pmdoc

import (
	"fmt"
	"unicode/utf16"
)

// len16 is how many UTF-16 units value takes, which is the length of utf16.Encode([]rune(value)),
// counted without building either slice. Ranging over a string decodes it exactly as []rune does:
// each invalid byte becomes one U+FFFD, and no rune is a surrogate or above U+10FFFF, so every rune
// is one unit and only one above U+FFFF adds a second.
func len16(value string) int {
	units := 0
	for _, r := range value {
		units++
		if r > 0xFFFF {
			units++
		}
	}
	return units
}

// units16 is how many UTF-16 units runes take, counted as ygo counts a text's length: a character
// above U+FFFF is two units, any other one (U+FFFD, which stands for each invalid byte, included).
func units16(runes []rune) int {
	units := 0
	for _, char := range runes {
		if char > 0xFFFF {
			units += 2
		} else {
			units++
		}
	}
	return units
}

// splitsPair reports whether position at, an index between UTF-16 units, falls inside a surrogate
// pair: the unit before it is a high surrogate and the unit after it a low one. A cut there leaves
// two halves that no Go string holds, and each reads back as U+FFFD.
func splitsPair(units []uint16, at int) bool {
	return at > 0 && at < len(units) && units[at-1]&0xFC00 == 0xD800 && units[at]&0xFC00 == 0xDC00
}

// slice16 is value between UTF-16 positions from and to. A position inside a surrogate pair is a
// bug of this package's, not a fault in the caller's write, so it is a plain error: every range
// the matchers return ends between characters, and a slice cut inside a pair would hold half a
// character, with a splice around it writing the other half twice or not at all.
func slice16(value string, from, to int) (string, error) {
	units := utf16.Encode([]rune(value))
	if splitsPair(units, from) || splitsPair(units, to) {
		return "", fmt.Errorf("pmdoc: range %d-%d cuts a surrogate pair", from, to)
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
