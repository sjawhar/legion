package text

import "unicode/utf16"

func encode16(value string) []uint16 {
	return utf16.Encode([]rune(value))
}

// Len16 returns the number of UTF-16 code units in value.
func Len16(value string) int {
	return len(encode16(value))
}

// Slice16 returns the UTF-16 range [from, to) from value. Callers validate
// range bounds against Len16 before slicing.
func Slice16(value string, from, to int) string {
	units := encode16(value)
	return string(utf16.Decode(units[from:to]))
}
