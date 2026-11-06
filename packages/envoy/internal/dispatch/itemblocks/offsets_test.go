package itemblocks

import "testing"

func TestOffsetsCountUTF16Units(t *testing.T) {
	for _, tc := range []struct {
		name  string
		body  string
		bytes []int // byte offsets between characters
		units []int // the same offsets in UTF-16 units
	}{
		// The emoji is outside the Basic Multilingual Plane: four bytes, two units.
		{"an astral character", "a😀b", []int{0, 1, 5, 6}, []int{0, 1, 3, 4}},
		{"a two-byte character", "é.", []int{0, 2, 3}, []int{0, 1, 2}},
		// A CR LF is two bytes and two units: the offsets are into the body as stored.
		{"a CR LF body", "a\r\n\r\nb", []int{0, 1, 2, 3, 5, 6}, []int{0, 1, 2, 3, 5, 6}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for index, byteOffset := range tc.bytes {
				units, err := UTF16Offset(tc.body, byteOffset)
				if err != nil || units != tc.units[index] {
					t.Errorf("UTF16Offset(%q, %d) = %d, %v; want %d", tc.body, byteOffset, units, err, tc.units[index])
				}
				back, err := ByteOffset(tc.body, tc.units[index])
				if err != nil || back != byteOffset {
					t.Errorf("ByteOffset(%q, %d) = %d, %v; want %d", tc.body, tc.units[index], back, err, byteOffset)
				}
			}
		})
	}

	// An offset inside a character, or past either end, is refused.
	body := "a😀é"
	for _, byteOffset := range []int{2, 3, 4, 6, -1, 8} {
		if units, err := UTF16Offset(body, byteOffset); err == nil {
			t.Errorf("UTF16Offset(%q, %d) = %d, want refused", body, byteOffset, units)
		}
	}
	// Unit 2 falls between the two halves of the emoji's surrogate pair.
	for _, units := range []int{2, -1, 5} {
		if byteOffset, err := ByteOffset(body, units); err == nil {
			t.Errorf("ByteOffset(%q, %d) = %d, want refused", body, units, byteOffset)
		}
	}
	if byteOffset, err := ByteOffset(body, 4); err != nil || byteOffset != len(body) {
		t.Errorf("ByteOffset(%q, 4) = %d, %v; want the body's end, %d", body, byteOffset, err, len(body))
	}
}
