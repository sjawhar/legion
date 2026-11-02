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
	// The zero value every deployment that configures no rename carries holds a nil map, and the
	// lowercasing below allocates whenever the name carries GitHub's capitals: this comparison is
	// made wherever the daemon compares a repository, so the nil map answers before it.
	if r.partner == nil {
		return Repository{}, false
	}
	partner, found := r.partner[strings.ToLower(name)]
	return partner, found
}

// Under runs read for name and, when it answers the zero value with no error, again for the other
// name of name's configured pair. It is the one place the order of a read keyed by a repository is
// written: the name the fact carries, as the fact spells it, and then the pair's other name, since
// a record keeps the repository it was written under for its whole life while the facts that
// arrive after a transfer name the one it moved to. It is a lookup order, not an equality — `Same`
// answers whether two names are one repository, while this reads what may be held under either.
//
// The zero value of T is what read answers when it finds nothing: a nil *record.PullRequest, a
// false. So a read that finds something, and a read that fails, is answered at once and the second
// read is never made. A deployment that configures no rename, and a name no pair holds, make
// exactly the one read they always made, and neither allocates: the order is two calls, never a
// slice of names.
func Under[T comparable](r Renames, name string, read func(string) (T, error)) (T, error) {
	var zero T
	found, err := read(name)
	if err != nil || found != zero {
		return found, err
	}
	partner, paired := r.Partner(name)
	if !paired {
		return zero, nil
	}
	return read(partner.String())
}

// OwnerAliases is each owner a configured rename moved a repository away from, by the owner it
// moved to — the pairs whose `to` owner `owned` answers true for and whose `from` owner it does
// not — and the pairs whose two owners are both owned, which are no alias.
//
// It is the one rule for whose installation holds a renamed repository now. A GitHub App is
// installed on an owner, so once a transfer moves a repository to another owner, the installation
// that covers it is that owner's: a token minted for the owner a record names would be minted on
// an installation the repository has left. A rename inside one owner moves nothing between
// installations and is therefore no alias. A pair whose `to` owner the project does not own is
// served by nothing here, and a pair whose two owners are both the project's is a conflict rather
// than an alias — each of those owners keeps its own token, since its own repositories are the
// project's — which the caller reports as it sees fit.
//
// `owned` is asked of each pair's two owners, lowercased, and answers whatever the caller counts
// as the project's owners, compared without case as GitHub compares owners. A deployment that
// configures no rename asks it nothing and answers nil for both.
func (r Renames) OwnerAliases(owned func(owner string) bool) (aliases map[string]string, conflicts []Rename) {
	for _, pair := range r.pairs {
		from, to := strings.ToLower(pair.From.Owner()), strings.ToLower(pair.To.Owner())
		switch {
		case from == to || !owned(to):
			continue
		case owned(from):
			conflicts = append(conflicts, pair)
		default:
			if aliases == nil {
				aliases = make(map[string]string, len(r.pairs))
			}
			aliases[from] = to
		}
	}
	return aliases, conflicts
}

// Serving is the repository GitHub holds `recorded` under now: the other name of its configured
// pair when that name's owner is an alias target of OwnerAliases, and `recorded` itself otherwise.
//
// A record keeps the repository it was written under for its whole life, so every GitHub call the
// daemon makes for a tree opened before a transfer starts from a name GitHub no longer serves and
// an owner whose installation no longer covers it. This is the one place that name is mapped to
// the one GitHub holds, for the token's owner and for the address together, so a REST write or a
// GraphQL query reaches the repository rather than silently addressing nobody.
//
// A name no pair holds, a pair whose owners OwnerAliases makes no alias of (one inside a single
// owner, one whose `to` owner the project does not own, one between two owners of the project),
// and the repository a pair moved to, which is already the served name, each answer `recorded`
// unchanged; a deployment that configures no rename answers it without asking `owned` at all.
func (r Renames) Serving(recorded Repository, owned func(owner string) bool) Repository {
	partner, found := r.Partner(recorded.String())
	if !found {
		return recorded
	}
	aliases, _ := r.OwnerAliases(owned)
	if to, alias := aliases[strings.ToLower(recorded.Owner())]; alias && strings.EqualFold(to, partner.Owner()) {
		return partner
	}
	return recorded
}

// Pairs are the configured pairs, in configured order, as a copy no caller can use to change the
// equivalence.
func (r Renames) Pairs() []Rename { return slices.Clone(r.pairs) }

// IsZero is whether r holds no pair, the zero value a deployment that configures none carries.
func (r Renames) IsZero() bool { return len(r.pairs) == 0 }

// MustRename is the pair from and to name, for a caller who wrote both as literals, a test's
// fixture: it panics on a name Parse refuses (MustParse).
func MustRename(from, to string) Rename {
	return Rename{From: MustParse(from), To: MustParse(to)}
}

// MustRenames is NewRenames of pairs a caller wrote as literals, a test's fixture: it panics on
// pairs NewRenames refuses.
func MustRenames(pairs ...Rename) Renames {
	renames, err := NewRenames(pairs)
	if err != nil {
		panic(err)
	}
	return renames
}
