package text

import (
	"bytes"
	"strings"
	"unicode/utf8"
)

// Storable reports whether value is text PostgreSQL can store: UTF-8 holding no U+0000. Its text
// type refuses a byte sequence that is not UTF-8, and its text and jsonb both refuse U+0000, so a
// statement given anything else fails.
func Storable(value string) bool {
	return strings.IndexByte(value, 0) < 0 && utf8.ValidString(value)
}

// StorableReplacement is value with each byte PostgreSQL cannot store replaced by U+FFFD: invalid
// UTF-8 is made valid first, then each U+0000 becomes U+FFFD. A recorded error can come from an
// input parser, so it passes through this before it reaches a text or jsonb column.
func StorableReplacement(value string) string {
	return strings.ReplaceAll(strings.ToValidUTF8(value, "\uFFFD"), "\x00", "\uFFFD")
}

// StorableBytes is Storable for text held as bytes, such as an uploaded file.
func StorableBytes(value []byte) bool {
	return bytes.IndexByte(value, 0) < 0 && utf8.Valid(value)
}
