package pmdoc

import "unicode/utf16"

type Span struct {
	MdFrom int
	MdTo   int
	PmFrom int
}

type PositionMap struct {
	spans []Span
}

func (p *PositionMap) ToPM(md int) int {
	best := 0
	for _, s := range p.spans {
		switch {
		case md >= s.MdFrom && md <= s.MdTo:
			return s.PmFrom + (md - s.MdFrom)
		case md < s.MdFrom:
			return s.PmFrom
		}
		best = s.PmFrom + (s.MdTo - s.MdFrom)
	}
	return best
}

func (p *PositionMap) ToMd(pm int) (int, bool) {
	for i := range p.spans {
		s := p.spans[i]
		end := s.PmFrom + (s.MdTo - s.MdFrom)
		if pm < s.PmFrom || pm > end {
			continue
		}
		if pm == end && i+1 < len(p.spans) && p.spans[i+1].PmFrom == pm {
			continue
		}
		return s.MdFrom + (pm - s.PmFrom), true
	}
	return 0, false
}

func len16(value string) int {
	return len(utf16.Encode([]rune(value)))
}

func slice16(value string, from, to int) string {
	return string(utf16.Decode(utf16.Encode([]rune(value))[from:to]))
}
