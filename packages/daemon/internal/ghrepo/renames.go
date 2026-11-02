package ghrepo

import (
	"fmt"
	"slices"
	"strings"
)

// Rename is one configured pair: the repository as it was, and the one it moved to.
type Rename struct{ From, To Repository }

// describe is the pair as a refusal names it, `<from> -> <to>`.
func (r Rename) describe() string { return r.From.String() + " -> " + r.To.String() }

// Renames is the equivalence a deployment configures (`repository_renames`): the pairs it treats
// as one repository wherever a name is compared, because the repository moved and GitHub's events
// name the new one while the records Legion already keeps name the old. The zero value holds none
// and answers exactly as strings.EqualFold does, so a deployment that configures no rename reads
// exactly as one with no equivalence at all.
//
// The equivalence is read-side only: nothing it answers rewrites a record, so a tree keeps the
// repository name it was recorded under for its whole life, and the pair must stay configured for
// as long as any record names the old repository.
type Renames struct {
	// pairs are the configured pairs, in configured order.
	pairs []Rename
	// partner is each repository's other name, keyed by the lowercased `<owner>/<name>`: both
	// ways of every pair, so one lookup answers from either side. Nil in the zero value.
	partner map[string]Repository
}

// NewRenames is the equivalence pairs configure, or a refusal naming the pair it cannot hold: a
// pair whose names are one repository, a repository renamed twice (as a pair's from or as its to),
// and a chain, where one pair's to is another's from. The map holds one pair per repository, a
// flat equivalence of two names, so `Same` and `Partner` answer the same from either side. No pair
// may carry the zero Repository, since that names no repository at all.
func NewRenames(pairs []Rename) (Renames, error) {
	for index, pair := range pairs {
		switch {
		case pair.From.IsZero():
			return Renames{}, fmt.Errorf("rename %d names no repository in from: a rename is the repository as it was and the one it moved to", index)
		case pair.To.IsZero():
			return Renames{}, fmt.Errorf("rename %d names no repository in to: a rename is the repository as it was and the one it moved to", index)
		case strings.EqualFold(pair.From.String(), pair.To.String()):
			return Renames{}, fmt.Errorf("rename %d (%s) renames a repository to itself: from and to name two repositories, compared without case", index, pair.describe())
		}
		for seen, earlier := range pairs[:index] {
			switch {
			case strings.EqualFold(pair.From.String(), earlier.From.String()):
				return Renames{}, fmt.Errorf("rename %d (%s) and rename %d (%s) both rename %s: a repository moved to one name", index, pair.describe(), seen, earlier.describe(), pair.From)
			case strings.EqualFold(pair.To.String(), earlier.To.String()):
				return Renames{}, fmt.Errorf("rename %d (%s) and rename %d (%s) both name %s as the repository moved to: a repository moved from one name", index, pair.describe(), seen, earlier.describe(), pair.To)
			case strings.EqualFold(pair.From.String(), earlier.To.String()):
				return Renames{}, fmt.Errorf("rename %d (%s) chains onto rename %d (%s) through %s: a rename pairs two names, never a chain of three", index, pair.describe(), seen, earlier.describe(), pair.From)
			case strings.EqualFold(pair.To.String(), earlier.From.String()):
				return Renames{}, fmt.Errorf("rename %d (%s) chains onto rename %d (%s) through %s: a rename pairs two names, never a chain of three", index, pair.describe(), seen, earlier.describe(), pair.To)
			}
		}
	}
	if len(pairs) == 0 {
		return Renames{}, nil
	}
	renames := Renames{pairs: slices.Clone(pairs), partner: make(map[string]Repository, 2*len(pairs))}
	for _, pair := range renames.pairs {
		renames.partner[strings.ToLower(pair.From.String())] = pair.To
		renames.partner[strings.ToLower(pair.To.String())] = pair.From
	}
	return renames, nil
}

// Same is whether a and b name one repository: the same name, compared as GitHub compares names,
// without case, or the two names of one configured pair, each side compared without case too. It
// takes strings because every comparison the daemon makes holds a name as it arrived — an event's
// `<owner>/<name>`, a record's column — and a string that is no `<owner>/<name>` at all is equal
// to nothing but itself, since only a parsed repository is ever a pair's side.
func (r Renames) Same(a, b string) bool {
	if strings.EqualFold(a, b) {
		return true
	}
	partner, found := r.Partner(a)
	return found && strings.EqualFold(partner.String(), b)
}

// Partner is the other name of name's pair, and whether a pair names it. It answers from either
// side: the repository as it was answers the one it moved to, and the other way round.
func (r Renames) Partner(name string) (Repository, bool) {
	partner, found := r.partner[strings.ToLower(name)]
	return partner, found
}

// Lookups is the names a read keyed by repository must try for name, in the order it must try
// them: name itself, as the fact that carries it spells it, and then the other name of the pair
// that names it, when a pair does. It is a lookup order, not an equality — `Same` answers whether
// two names are one repository, while this answers what to look under when a record may be held
// under either — and it is the one place that order is written, so every read keyed by a
// repository is one loop over it and none derives the rule again.
//
// A deployment that configures no rename, and a name no pair holds, answer the one name, so such
// a read stays exactly the one it was. A configured name answers two, and the second read is made
// only when the first finds nothing.
func (r Renames) Lookups(name string) []string {
	if partner, found := r.Partner(name); found {
		return []string{name, partner.String()}
	}
	return []string{name}
}

// Pairs are the configured pairs, in configured order, as a copy no caller can use to change the
// equivalence.
func (r Renames) Pairs() []Rename { return slices.Clone(r.pairs) }

// IsZero is whether r holds no pair, the zero value a deployment that configures none carries.
func (r Renames) IsZero() bool { return len(r.pairs) == 0 }
