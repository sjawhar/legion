package routing

import "strings"

func Match(pattern string, topic string) bool {
	pp := strings.Split(pattern, ".")
	tp := strings.Split(topic, ".")

	for len(pp) > 0 {
		part := pp[0]
		pp = pp[1:]

		if part == ">" {
			return true
		}
		if len(tp) == 0 {
			return false
		}
		if part != "*" && part != tp[0] {
			return false
		}
		tp = tp[1:]
	}

	return len(tp) == 0
}
