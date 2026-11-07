package pmdoc

import (
	"fmt"
	"unicode/utf8"
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

// byteAt16 is where UTF-16 position at falls in value, as a byte offset, and whether it falls
// inside a character: between the two units of one above U+FFFF, which is the surrogate pair a cut
// there would split. It counts units as len16 counts them, each byte that is not UTF-8 one unit,
// and a position at or past value's last unit is value's length.
func byteAt16(value string, at int) (int, bool) {
	units := 0
	for index, char := range value {
		if units >= at {
			return index, false
		}
		width := 1
		if char > 0xFFFF {
			width = 2
		}
		if units+width > at {
			return index, true
		}
		units += width
	}
	return len(value), false
}

// slice16 is value between UTF-16 positions from and to. A position inside a surrogate pair is a
// bug of this package's, not a fault in the caller's write, so it is a plain error: every range
// the matchers return ends between characters, and a slice cut inside a pair would hold half a
// character, with a splice around it writing the other half twice or not at all.
//
// It finds both ends in one reading of value up to to (byteAt16) and answers that much of value
// itself: encoding the whole text to UTF-16 and decoding the cut back held four more copies of it,
// and a splice cuts the textblock it writes in on both sides, so a one-word edit of a document
// whose paragraph is a mebibyte long carried about twenty-five mebibytes of them
// (sjawhar/legion#2519). A text a browser left holding a byte that is not UTF-8 reads back with
// one U+FFFD for each such byte, as its UTF-16 encoding did, so only that text is copied.
func slice16(value string, from, to int) (string, error) {
	start, startSplits := byteAt16(value, from)
	stop, stopSplits := byteAt16(value[start:], to-from)
	if startSplits || stopSplits {
		return "", fmt.Errorf("pmdoc: range %d-%d cuts a surrogate pair", from, to)
	}
	cut := value[start : start+stop]
	if !utf8.ValidString(cut) {
		return string([]rune(cut)), nil
	}
	return cut, nil
}

// context16 is value between UTF-16 positions from and to for a reader to see, each end moved
// outward off the inside of a surrogate pair so the text holds every character it touches whole.
//
// It finds both ends in value itself and answers that much of value, as slice16 does: encoding
// the whole text to UTF-16 and decoding the cut back held five and a half mebibytes for an
// eighty-unit window of a mebibyte of text, all of it to throw away. byteAt16 answers the byte
// offset of the character a position falls in and whether the position falls inside it, which is
// the cut that splits a surrogate pair: the character's own start is what from-- means, and the
// end of the character to falls inside is what to++ means, so each end moves outward off the
// inside of a pair by reading the character whole. A text a browser left holding a byte that is
// not UTF-8 reads back with one U+FFFD for each such byte, as its UTF-16 encoding did, so only
// such a text is copied.
func context16(value string, from, to int) string {
	// Where from falls inside a character, byteAt16 answers that character's own start, which is
	// where from-- leaves the window, and the window is one unit longer for it.
	start, startSplits := byteAt16(value, from)
	length := to - from
	if startSplits {
		length++
	}
	stop, stopSplits := byteAt16(value[start:], length)
	if stopSplits {
		_, width := utf8.DecodeRuneInString(value[start+stop:])
		stop += width
	}
	cut := value[start : start+stop]
	if !utf8.ValidString(cut) {
		return string([]rune(cut))
	}
	return cut
}
