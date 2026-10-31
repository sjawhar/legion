package main

import (
	"math"
	"math/rand/v2"
	"sort"
	"strings"
)

// vocabulary is the fixed word list every seeded text is drawn from, with the Zipf weight of each
// word. A text drawn from one flat list puts almost every word in almost every document, which
// leaves no rare word for a search measurement to use and no selectivity for a plan to turn on
// (the review's finding 4). A Zipf list gives both: word 1 is in most documents and word 20,000
// in a handful.
type vocabulary struct {
	words []string
	// cumulative[i] is the summed weight of words 0..i, and cumulative[len-1] the total, so a
	// draw is one uniform number and one binary search.
	cumulative []float64
}

// vocabularySize is the 20,000 words the plan's answer 4 names. zipfExponent 1 is the classic
// Zipf law: the rank-n word is drawn 1/n as often as the first.
const (
	vocabularySize = 20_000
	zipfExponent   = 1.0
)

// The words are built from these three tables, 20 x 25 x 40 = 20,000 distinct lowercase ASCII
// words of three to seven letters. None ends in a letter the English snowball stemmer strips, so
// a word is its own lexeme in a tsvector and a search for it finds what holds it.
var (
	wordOnsets = []string{"b", "br", "d", "dr", "f", "fl", "g", "gl", "k", "kr",
		"m", "n", "p", "pl", "r", "s", "sk", "t", "tr", "v"}
	wordNuclei = []string{"a", "e", "i", "o", "u", "ai", "au", "ea", "ei", "eo", "ia", "ie", "io",
		"iu", "oa", "oe", "oi", "ou", "ua", "ue", "ui", "uo", "ya", "ye", "yo"}
	wordCodas = []string{"b", "d", "f", "g", "k", "l", "m", "n", "p", "r", "t", "v", "x", "z",
		"bd", "bl", "dn", "dr", "fn", "gl", "kl", "kn", "lm", "ln", "lp", "lt", "mb", "mp",
		"nd", "nk", "nt", "pl", "pt", "rb", "rd", "rk", "rl", "rm", "rn", "rt"}
)

// newVocabulary builds the word list and its weights. The words are in rank order, word 0 the
// most frequent, so a caller that wants a rare word asks for a high index.
func newVocabulary() *vocabulary {
	v := &vocabulary{
		words:      make([]string, 0, vocabularySize),
		cumulative: make([]float64, 0, vocabularySize),
	}
	total := 0.0
	for index := range vocabularySize {
		onset := wordOnsets[index%len(wordOnsets)]
		nucleus := wordNuclei[(index/len(wordOnsets))%len(wordNuclei)]
		coda := wordCodas[(index/(len(wordOnsets)*len(wordNuclei)))%len(wordCodas)]
		v.words = append(v.words, onset+nucleus+coda)
		total += weightOfRank(index + 1)
		v.cumulative = append(v.cumulative, total)
	}
	return v
}

// weightOfRank is the Zipf weight of the rank-th word, ranks counted from one. Go specifies
// Pow(x, 1) as x, so at the exponent of 1 the constant holds this is exactly 1/rank, and the
// weight stays right for any exponent the constant is moved to.
func weightOfRank(rank int) float64 {
	return 1 / math.Pow(float64(rank), zipfExponent)
}

// word draws one word.
func (v *vocabulary) word(random *rand.Rand) string {
	target := random.Float64() * v.cumulative[len(v.cumulative)-1]
	index := sort.SearchFloat64s(v.cumulative, target)
	if index >= len(v.words) {
		index = len(v.words) - 1
	}
	return v.words[index]
}

// sentence writes a sentence of words words into builder: the first word capitalised, the rest
// lower-case, and a full stop at the end.
func (v *vocabulary) sentence(builder *strings.Builder, random *rand.Rand, words int) {
	for index := range words {
		if index > 0 {
			builder.WriteByte(' ')
		}
		word := v.word(random)
		if index == 0 {
			builder.WriteString(strings.ToUpper(word[:1]))
			builder.WriteString(word[1:])
			continue
		}
		builder.WriteString(word)
	}
	builder.WriteByte('.')
}

// paragraph writes sentences until the builder has grown by at least bytes, and returns what it
// wrote. A paragraph is one line: a line feed inside it would split it into two blocks.
func (v *vocabulary) paragraph(builder *strings.Builder, random *rand.Rand, bytes int) {
	start := builder.Len()
	for first := true; builder.Len()-start < bytes; first = false {
		if !first {
			builder.WriteByte(' ')
		}
		v.sentence(builder, random, 6+random.IntN(10))
	}
}

// phrase is words words of ordinary text, for a title or a table cell, with no full stop. A cell
// holds no `|`, which would end it, and no line feed, which would end the row.
func (v *vocabulary) phrase(random *rand.Rand, words int) string {
	var builder strings.Builder
	for index := range words {
		if index > 0 {
			builder.WriteByte(' ')
		}
		word := v.word(random)
		if index == 0 {
			builder.WriteString(strings.ToUpper(word[:1]))
			builder.WriteString(word[1:])
			continue
		}
		builder.WriteString(word)
	}
	return builder.String()
}

// wordAtRank is the word of the given rank, ranks counted from one, for the rare, medium and
// common words the command prints.
func (v *vocabulary) wordAtRank(rank int) string {
	return v.words[min(max(rank, 1), len(v.words))-1]
}
