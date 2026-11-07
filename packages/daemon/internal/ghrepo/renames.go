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
	// movedTo is each pair's `to`, keyed by the lowercased `<owner>/<name>` of its `from` alone,
	// so a lookup answers in one direction where `partner` answers in both. `Serving` must tell a
	// pair's `from` from its `to` and sits on the path of every GitHub call the daemon makes, so
	// the direction is a map built once here rather than a scan of `pairs` per call. Nil in the
	// zero value.
	movedTo map[string]Repository
}

// NewRenames is the equivalence pairs configure, or a refusal naming the pair it cannot hold: a
// pair whose names are one repository, a repository renamed twice (as a pair's from or as its to),
// a chain, where one pair's to is another's from, and one owner's repositories moved to two
// owners. The map holds one pair per repository, a flat equivalence of two names, so `Same` and
// `Partner` answer the same from either side. No pair may carry the zero Repository, since that
// names no repository at all.
//
// The last refusal is the one rule about owners rather than names, and it is here because this is
// the only place that holds every pair. `OwnerAliases` is keyed by the owner a pair moved a
// repository away from, so two pairs leaving one owner for two owners would write that key twice
// and the last would win, silently: a role's gh bundle holds one directory per owner, so that
// owner's directory would carry a token minted for whichever pair the map happened to keep, and
// the other pair's repository would be unreachable from it with nothing said. Which repository a
// recorded name resolves to no longer turns on that map — `Serving` reads the pair itself and the
// repositories the project serves — so this refusal guards the alias map alone. Several pairs out
// of one owner to that one owner are a transfer of several repositories, which is configured as
// it is, and several owners moving to one owner key one alias each, so only this shape is
// refused.
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
			case strings.EqualFold(pair.From.Owner(), earlier.From.Owner()) && !strings.EqualFold(pair.To.Owner(), earlier.To.Owner()):
				return Renames{}, fmt.Errorf("rename %d (%s) and rename %d (%s) move repositories of %s to two owners, %s and %s: an owner's repositories moved to one owner, since OwnerAliases keys an alias by %s, the owner a pair moved a repository away from, and the second pair would overwrite the first silently", index, pair.describe(), seen, earlier.describe(), pair.From.Owner(), pair.To.Owner(), earlier.To.Owner(), pair.From.Owner())
			}
		}
	}
	if len(pairs) == 0 {
		return Renames{}, nil
	}
	renames := Renames{
		pairs:   slices.Clone(pairs),
		partner: make(map[string]Repository, 2*len(pairs)),
		movedTo: make(map[string]Repository, len(pairs)),
	}
	for _, pair := range renames.pairs {
		from := strings.ToLower(pair.From.String())
		renames.partner[from] = pair.To
		renames.partner[strings.ToLower(pair.To.String())] = pair.From
		// The `from` side alone, since this is the lookup `Serving` reads to tell a name recorded
		// before a transfer from the name GitHub has now.
		renames.movedTo[from] = pair.To
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

// MovedTo is the repository a configured pair moved `recorded` to, and whether a pair moved it:
// the pair's `to` when `recorded` is that pair's `from`, compared without case as GitHub compares
// names, and the zero Repository with false for every other name, the pair's `to` among them. It
// is the direction-aware half of the equivalence, the half `Serving` needs: `Partner` answers from
// either side, which cannot tell a name recorded before a transfer from the one GitHub has now.
func (r Renames) MovedTo(recorded Repository) (Repository, bool) {
	// The zero value every deployment that configures no rename carries holds a nil map, and the
	// joining and lowercasing below allocate whenever the name carries GitHub's capitals:
	// `Serving` is on the path of every GitHub call the daemon makes, so the nil map answers
	// before either.
	if r.movedTo == nil {
		return Repository{}, false
	}
	to, moved := r.movedTo[strings.ToLower(recorded.String())]
	return to, moved
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

// OwnerAliases is the owner each configured rename moved a repository away from, keyed by that
// owner, lowercased, and answering the owner it moved to, lowercased too — one entry for each
// pair that resolves, which is each pair whose `to` the project serves and whose `from` it does
// not. A pair whose two owners are one owner is no alias, since a rename inside one owner moves
// nothing between installations.
//
// A GitHub App is installed on an owner, so once a transfer moves a repository to another owner,
// the installation that covers it is that owner's: a token minted for the owner a record names
// would be minted on an installation the repository has left. The alias is how a caller that
// holds an owner and no repository — a role's gh bundle, which is one directory per owner — names
// the owner whose installation holds the moved repository now.
//
// A pair resolves here exactly when the rename rule resolves it, so this loop asks that one rule
// rather than writing it a second time: `Serving` answers the pair's `to` for its `from` when
// the project serves the `to` and does not serve the `from`, and `Serving`'s own doc is where
// the reason `served` is asked of a pair's two repositories, and never of their owners, is
// written. A deployment that configures no rename holds no pair, so it asks `served` nothing
// and answers nil.
func (r Renames) OwnerAliases(served func(Repository) bool) map[string]string {
	var aliases map[string]string
	for _, pair := range r.pairs {
		from, to := strings.ToLower(pair.From.Owner()), strings.ToLower(pair.To.Owner())
		if from == to || r.Serving(pair.From, served) != pair.To {
			continue
		}
		if aliases == nil {
			aliases = make(map[string]string, len(r.pairs))
		}
		aliases[from] = to
	}
	return aliases
}

// Serving is the repository GitHub holds `recorded` under now: the other name of `recorded`'s
// configured pair exactly when `recorded` is that pair's `from`, the project serves the pair's
// `to`, and the project does not serve the pair's `from`. Every other recorded name, the pair's
// `to` and a name no pair holds included, is answered unchanged.
//
// A record keeps the repository it was written under for its whole life, so every GitHub call the
// daemon makes for a tree opened before a transfer starts from a name whose owner's installation
// no longer covers the repository. The token is what that costs: GitHub answers the old address
// itself (a REST call to it redirects, and a GraphQL query resolves the repository), while no
// token of the old owner reaches it. This is the one place a recorded name is mapped to the one
// GitHub holds, and callers take both halves of the answer: its owner, which is the half they
// cannot do without, and its name, which keeps a write off a redirect and names the repository
// GitHub holds in whatever the caller writes down.
//
// The rule reads the repositories the project serves, one at a time, and never the set of owners
// those repositories have. The owner is the wrong question to ask: a project that serves one more
// repository of the old owner serves no more of the moved one, yet an owner test reads that as
// the old owner being the project's own and stops resolving the pair. That is the production
// failure this rule replaces — one repository added to a project's mapping turned a live pair off
// and left 47 trees unable to push for 42 minutes.
//
// The rule is directional, and each of its clauses earns its keep. A record naming the pair's
// `to` is never answered with the `from`: the `to` is the name GitHub has, so that record needs
// no resolution, and answering the other name would address the repository the transfer moved it
// away from, this failure in reverse. And when the project serves both names, each is a
// repository of its own — somebody made a repository at the old location, which deletes GitHub's
// redirect — so each is addressed under the name it was recorded with.
//
// A deployment that configures no rename holds no pair, so it answers `recorded` without asking
// `served` anything, and so does a name no pair moved a repository away from.
func (r Renames) Serving(recorded Repository, served func(Repository) bool) Repository {
	to, moved := r.MovedTo(recorded)
	if !moved || !served(to) || served(recorded) {
		return recorded
	}
	return to
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
