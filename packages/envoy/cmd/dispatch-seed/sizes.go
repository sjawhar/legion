package main

import "math"

// sizes is how many rows of each kind one run writes. Every figure is the parent spec's Decision
// "Test database sizes" (ten times production) multiplied by Scale, except where the comment on
// the field names another source. A figure never falls below one, so the shape survives the small
// scale the CI test runs at: a data set with no message has no message arm of the graph.
type sizes struct {
	Scale float64

	// Projects is fixed at every scale. It is a shape, not a row count: the spec's "one project
	// with 8,000 of the 20,000 issues" is a statement about the share of one project in ten, and
	// a scale that rounded ten projects down to one would hold no such share and would leave the
	// per-project term statistics with one row to build.
	Projects int
	// BigProjectIssues is the one project of 8,000 issues; the rest are spread over the others.
	BigProjectIssues int
	Issues           int
	// BlockedBy is the 19,800 blocked_by links. HubLinks of them are the heavily linked issue's.
	BlockedBy int
	// HubLinks is the children and the blockers of the one heavily linked issue. 99 of each is
	// the subject sjawhar/legion#2177 measured, so a before-and-after is comparable with its
	// table; it overrides the spec's cap of 30 children for that one issue. A scaled project too
	// small to hold 99 children gets as many as it has issues to spare.
	HubLinks int

	// Documents is the 40,000 `artifacts`, every one a `doc`; Versions the 200,000
	// `artifact_versions` over them. Each issue has a primary document and the rest are extra
	// documents of an issue or of a project.
	Documents int
	Versions  int
	// ManyVersionDocuments have 28 versions each; LargeDocuments have a latest version of
	// LargeVersionMin to LargeVersionMax bytes. The plan's Decision "The large documents are
	// large only at their latest version" holds every other version of them to the ordinary band:
	// the other reading is about 4 GB of markdown, which does not fit the disk.
	ManyVersionDocuments int
	LargeDocuments       int

	Asks         int
	AskFollowers int
	Comments     int
	Messages     int
	Events       int
	Sessions     int

	// Mentions is the 100,000 rows of `refs`, and BlockMentions the 50,000 rows spread over
	// `block_refs`, `comment_block_refs` and `message_block_refs`. Every mention a document, a
	// comment or a message writes lies inside one of its blocks, so it writes a row of both
	// tables; an ask has no blocks, so its mentions are the rows of `refs` alone that make the
	// first figure twice the second.
	DocumentMentions int
	CommentMentions  int
	MessageMentions  int
	AskMentions      int

	// Components is the components of each project, over one architecture snapshot each.
	Components int
}

// versionBandBytes is the markdown size band of an ordinary document version, and
// largeVersionBytes the band of the latest version of each of the 400 large documents. Both are
// the spec's Decision "Test database sizes".
const (
	versionBandMin = 1 << 10
	versionBandMax = 20 << 10
	largeBandMin   = 100 << 10
	largeBandMax   = 660 << 10
)

// blocksPerLatestVersion is the identified blocks the latest version of every document holds: a
// heading, seventeen paragraphs and two tables, which is the twenty blocks the spec gives with
// the tenth of them tables it asks for. A table is one block however many rows it holds: only a
// block the renderer writes through its block path carries an offset, and a table's rows and its
// cells are written inside it (renderVersion).
const (
	blocksPerLatestVersion = 20
	paragraphsPerVersion   = 17
	tablesPerVersion       = 2
)

// newSizes is the data set at scale, every count rounded to the nearest row and held at one.
func newSizes(scale float64) sizes {
	at := func(full int) int { return scaled(full, scale) }
	s := sizes{
		Scale:                scale,
		Projects:             10,
		BigProjectIssues:     at(8_000),
		Issues:               at(20_000),
		BlockedBy:            at(19_800),
		HubLinks:             99,
		Documents:            at(40_000),
		Versions:             at(200_000),
		ManyVersionDocuments: at(4_000),
		LargeDocuments:       at(400),
		Asks:                 at(80_000),
		AskFollowers:         at(80_000),
		Comments:             at(370_000),
		Messages:             at(60_000),
		Events:               at(1_250_000),
		Sessions:             at(2_000),
		DocumentMentions:     at(25_000),
		CommentMentions:      at(20_000),
		MessageMentions:      at(5_000),
		AskMentions:          at(50_000),
		Components:           at(200),
	}
	// Each issue owns a primary document, so the extra documents are what is left; and a scale
	// that leaves fewer documents than issues still gives every issue its primary one.
	if s.Documents < s.Issues {
		s.Documents = s.Issues
	}
	if s.BigProjectIssues > s.Issues {
		s.BigProjectIssues = s.Issues
	}
	if s.ManyVersionDocuments > s.Documents {
		s.ManyVersionDocuments = s.Documents
	}
	if s.LargeDocuments > s.Documents {
		s.LargeDocuments = s.Documents
	}
	// A document has at least one version, and the documents with 28 need theirs.
	if least := s.ManyVersionDocuments*versionsPerManyVersionDocument + (s.Documents - s.ManyVersionDocuments); s.Versions < least {
		s.Versions = least
	}
	// The hub cannot have more children or more blockers than the big project has other issues.
	if room := s.BigProjectIssues - 1; room < s.HubLinks {
		s.HubLinks = max(room, 1)
	}
	if s.BlockedBy < s.HubLinks {
		s.BlockedBy = s.HubLinks
	}
	return s
}

// versionsPerManyVersionDocument is the 28 versions the spec gives 4,000 of the documents. It
// does not scale: it is the shape of one document, not a count of rows.
const versionsPerManyVersionDocument = 28

// scaled is full at scale, rounded to the nearest row and never below one: a count rounded to
// zero would empty an arm of the graph the measurements read.
func scaled(full int, scale float64) int {
	return max(1, int(math.Round(float64(full)*scale)))
}

// versions is how many versions document index holds: 28 for the first ManyVersionDocuments, and
// the rest of the budget shared evenly over the others, the lower-numbered documents taking the
// remainder, so the total is exactly Versions at every scale.
func (s sizes) versions(index int) int {
	if index < s.ManyVersionDocuments {
		return versionsPerManyVersionDocument
	}
	rest := s.Versions - s.ManyVersionDocuments*versionsPerManyVersionDocument
	others := s.Documents - s.ManyVersionDocuments
	if others <= 0 {
		return versionsPerManyVersionDocument
	}
	position := index - s.ManyVersionDocuments
	return share(position, others, rest)
}

// share is how many of total the item at position of n items takes, so the shares over every
// position sum to exactly total and differ by at most one: the whole-number split a count that
// must hit its figure exactly needs.
func share(position, n, total int) int {
	if n <= 0 {
		return 0
	}
	return (position+1)*total/n - position*total/n
}
