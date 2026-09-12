// Package rank creates sortable fractional keys for Dispatch issue ordering.
package rank

const alphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

// Between returns a base-62 key that sorts strictly after prev and strictly
// before next. An empty bound is unbounded. Repeated insertions at either end
// remain possible because the key space extends by a digit when adjacent keys
// leave no midpoint digit.
func Between(prev, next string) string {
	if prev != "" && next != "" && prev >= next {
		panic("rank bounds must be ordered")
	}
	if prev == "" {
		return before(next)
	}
	if next == "" {
		return prev + "U"
	}

	common := 0
	for common < len(prev) && common < len(next) && prev[common] == next[common] {
		common++
	}
	if common == len(prev) {
		return prev + before(next[common:])
	}

	prevDigit := digit(prev[common])
	nextDigit := digit(next[common])
	if nextDigit-prevDigit > 1 {
		return prev[:common] + string(alphabet[(prevDigit+nextDigit)/2])
	}
	return prev + "U"
}

func before(next string) string {
	if next == "" {
		return "U"
	}
	upper := digit(next[0])
	if upper > 1 {
		return string(alphabet[upper/2])
	}
	return string(alphabet[0]) + before(next[1:])
}

func digit(char byte) int {
	switch {
	case char >= '0' && char <= '9':
		return int(char - '0')
	case char >= 'A' && char <= 'Z':
		return int(char-'A') + 10
	case char >= 'a' && char <= 'z':
		return int(char-'a') + 36
	default:
		panic("rank contains non-base62 character")
	}
}
