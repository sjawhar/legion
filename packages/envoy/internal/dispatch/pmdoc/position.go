package pmdoc

import "unicode/utf16"

func len16(value string) int {
	return len(utf16.Encode([]rune(value)))
}

func slice16(value string, from, to int) string {
	return string(utf16.Decode(utf16.Encode([]rune(value))[from:to]))
}
