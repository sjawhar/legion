package docs

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// uploadAnchor is one open anchor a test places before an upload: a comment, or the one ask a
// document may hold in these tests (insertAnchoredAsk's fixed id).
type uploadAnchor struct {
	kind  MarkKind
	quote string
}

// placedAnchor is an open anchor as the room held it before the upload: its row, its mark's range
// and its mark's attributes.
type placedAnchor struct {
	uploadAnchor
	id     string
	table  string
	range_ pmdoc.Range
	attrs  pmdoc.Attrs
}

// placeUploadAnchors marks each of anchors in the room with an open row behind it, as a comment or
// an ask anchors itself, and returns them as the room then holds them.
func placeUploadAnchors(t *testing.T, service *Service, artifactID string, anchors []uploadAnchor) []placedAnchor {
	t.Helper()
	placed := make([]placedAnchor, 0, len(anchors))
	for index, anchor := range anchors {
		var id, table string
		switch anchor.kind {
		case MarkAsk:
			id, table = insertAnchoredAsk(t, service, artifactID, anchor.quote), "asks"
		case MarkComment:
			id = insertAnchoredCommentWithID(t, service, artifactID, fmt.Sprintf("00000000-0000-4000-8000-%012d", 100+index), anchor.quote)
			table = "comments"
		default:
			t.Fatalf("no row anchors a %s mark", anchor.kind)
		}
		placed = append(placed, placedAnchor{uploadAnchor: anchor, id: id, table: table})
	}
	before := liveTree(t, service, artifactID)
	for index := range placed {
		anchor := &placed[index]
		range_, quote, found := pmdoc.FindMark(before, string(anchor.kind), anchor.id)
		if !found || quote != anchor.quote {
			t.Fatalf("%s %s before the upload covers %q found=%t, want %q", anchor.kind, anchor.id, quote, found, anchor.quote)
		}
		attrs, found := pmdoc.MarkAttrs(before, string(anchor.kind), anchor.id)
		if !found {
			t.Fatalf("%s %s before the upload has no attributes", anchor.kind, anchor.id)
		}
		anchor.range_, anchor.attrs = range_, attrs
	}
	return placed
}

// treeMarkdown is tree rendered for a failure message, or why it does not render.
func treeMarkdown(tree *pmdoc.Node) string {
	markdown, err := pmdoc.Render(tree)
	if err != nil {
		return fmt.Sprintf("(does not render: %v)", err)
	}
	return markdown
}

func sameAttrs(t *testing.T, got, want pmdoc.Attrs) bool {
	t.Helper()
	gotJSON, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("encode attributes: %v", err)
	}
	wantJSON, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("encode attributes: %v", err)
	}
	return string(gotJSON) == string(wantJSON)
}

// An upload over open anchors leaves the room the text it uploaded with each anchor on the text
// it re-anchors to: the quote's occurrence nearest where the anchor stood, under the attributes
// the anchor had. An anchor whose quote the upload deleted leaves the document, and its row
// orphans at settlement.
func TestAnUploadLeavesTheAnchorsOnTheTextItKeeps(t *testing.T) {
	for _, test := range []struct {
		name, seed, upload string
		anchors            []uploadAnchor
		orphaned           []string
	}{
		{
			name:   "anchors in table cells",
			seed:   "Intro.\n\n| Name | Value |\n| --- | --- |\n| alpha | first cell |\n| beta | second cell |\n",
			upload: "Intro, revised.\n\n| Name | Value |\n| --- | --- |\n| alpha | first cell |\n| beta | other cell |\n",
			anchors: []uploadAnchor{
				{kind: MarkComment, quote: "first cell"},
				{kind: MarkComment, quote: "beta"},
				{kind: MarkAsk, quote: "alpha"},
			},
		},
		{
			name:   "two overlapping anchors of one type",
			seed:   "The quick brown fox jumps over the lazy dog\n",
			upload: "The quick brown fox leaps over the lazy dog\n",
			anchors: []uploadAnchor{
				{kind: MarkComment, quote: "quick brown fox"},
				{kind: MarkComment, quote: "brown"},
			},
		},
		{
			name:   "a quote that moved",
			seed:   "The quick brown fox\n\nA slow brown cat\n",
			upload: "A new first paragraph.\n\nThe very quick brown fox\n\nA slow brown cat\n",
			anchors: []uploadAnchor{
				{kind: MarkComment, quote: "brown fox"},
				{kind: MarkAsk, quote: "slow brown"},
			},
		},
		{
			name:   "a quote the upload deletes",
			seed:   "The quick brown fox jumps\n",
			upload: "The quick brown cat jumps\n",
			anchors: []uploadAnchor{
				{kind: MarkComment, quote: "fox"},
				{kind: MarkComment, quote: "quick"},
			},
			orphaned: []string{"fox"},
		},
		{
			name:   "a comment whose quote now lands in a code block",
			seed:   "Intro.\n\nrun the tests\n",
			upload: "Intro.\n\n```\nrun the tests\n```\n",
			anchors: []uploadAnchor{
				{kind: MarkComment, quote: "run the tests"},
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			service, artifactID := newTestService(t)
			service.settle = time.Hour
			seedServiceText(t, service, artifactID, test.seed)
			placed := placeUploadAnchors(t, service, artifactID, test.anchors)
			before := liveTree(t, service, artifactID)
			target, err := parseReplacing(before, test.upload)
			if err != nil {
				t.Fatalf("parse the upload: %v", err)
			}

			if _, err := joinedReplaceText(service, artifactID, test.upload, model.Actor{Kind: "user", ID: "alice"}); err != nil {
				t.Fatalf("upload: %v", err)
			}
			after := liveTree(t, service, artifactID)
			if got, want := pmdoc.StripAnchorMarks(after), pmdoc.StripAnchorMarks(target); !got.Equal(want) {
				t.Fatalf("the room after the upload, its anchors stripped =\n%s\nwant the upload's tree\n%s", treeMarkdown(got), treeMarkdown(want))
			}
			orphaned := map[string]bool{}
			for _, quote := range test.orphaned {
				orphaned[quote] = true
			}
			for _, anchor := range placed {
				from := anchor.range_.From
				want, err := pmdoc.FindQuote(target, anchor.quote, nil, &from)
				if errors.Is(err, pmdoc.ErrTargetNotFound) {
					if !orphaned[anchor.quote] {
						t.Fatalf("the upload drops %q, which the case does not expect to orphan", anchor.quote)
					}
					if _, _, found := pmdoc.FindMark(after, string(anchor.kind), anchor.id); found {
						t.Errorf("%s %s over the deleted %q is still in the room", anchor.kind, anchor.id, anchor.quote)
					}
					continue
				}
				if err != nil {
					t.Fatalf("find %q in the upload: %v", anchor.quote, err)
				}
				if orphaned[anchor.quote] {
					t.Fatalf("the upload keeps %q, which the case expects to orphan", anchor.quote)
				}
				got, _, found := pmdoc.FindMark(after, string(anchor.kind), anchor.id)
				if !found || got != want {
					t.Errorf("%s %s over %q after the upload = %+v found=%t, want %+v", anchor.kind, anchor.id, anchor.quote, got, found, want)
				}
				if attrs, found := pmdoc.MarkAttrs(after, string(anchor.kind), anchor.id); !found || !sameAttrs(t, attrs, anchor.attrs) {
					t.Errorf("%s %s attributes after the upload = %v, want %v", anchor.kind, anchor.id, attrs, anchor.attrs)
				}
			}

			settleCurrentGeneration(t, service, artifactID)
			for _, anchor := range placed {
				if stored := loadAnchor(t, service, anchor.table, anchor.id); stored.Orphaned != orphaned[anchor.quote] {
					t.Errorf("%s %s row after settlement orphaned=%t, want %t", anchor.kind, anchor.id, stored.Orphaned, orphaned[anchor.quote])
				}
			}
		})
	}
}

// An upload that moves an open anchor's quote where its mark is not allowed - an ask's into a code
// block, a comment's into the front matter, which allows no anchor - and leaves it nowhere else is
// refused as the caller's markdown (ErrInvalidMarkdown, which the routes answer 400), naming the
// anchor and each kind of place its text landed in with how many such places, the kinds
// ordered by their first occurrence, nearest to where the anchor stood rather than in document
// order, so one pass over the markdown resolves it, never as a document outside its schema (a 500
// or a repair), and the room keeps the tree it held.
func TestAnUploadMovingAnAnchorsQuoteWhereItsMarkIsNotAllowedIsRefused(t *testing.T) {
	for _, test := range []struct {
		name, seed, upload, quote, want string
		kind                            MarkKind
	}{
		{
			name: "an ask into a code block", kind: MarkAsk, quote: "run the tests",
			seed: "Intro.\n\nrun the tests\n", upload: "Intro.\n\n```\nrun the tests\n```\n",
			want: `the open ask anchored on "run the tests" would land in a code block`,
		},
		{
			name: "a comment into the front matter", kind: MarkComment, quote: "Hello world",
			seed: "Hello world\n\nIntro.\n", upload: "---\ntitle: Hello world\n---\n\nIntro.\n",
			want: `the open comment anchored on "Hello world" would land in the front matter`,
		},
		{
			name: "an ask into the front matter and a code block", kind: MarkAsk, quote: "run the tests",
			seed:   "Intro.\n\nrun the tests\n",
			upload: "---\ntitle: run the tests\n---\n\nIntro.\n\n```\nrun the tests\n```\n",
			want:   `the open ask anchored on "run the tests" would land in the front matter and a code block`,
		},
		{
			name: "an ask into a code block where it stood and the front matter far above", kind: MarkAsk, quote: "run the tests",
			seed: "Filler one opens the document and says nothing of the tests.\n\nFiller two says as little, at as much length.\n\nFiller three carries on in the same vein.\n\nrun the tests\n",
			upload: "---\ntitle: run the tests\n---\n\nFiller one opens the document and says nothing of the tests.\n\n" +
				"Filler two says as little, at as much length.\n\nFiller three carries on in the same vein.\n\n```\nrun the tests\n```\n",
			want: `the open ask anchored on "run the tests" would land in a code block and the front matter`,
		},
		{
			name: "an ask into two code blocks", kind: MarkAsk, quote: "run the tests",
			seed:   "Intro.\n\nrun the tests\n",
			upload: "Intro.\n\n```\nrun the tests\n```\n\nAnd again:\n\n```\nrun the tests\n```\n",
			want: `the open ask anchored on "run the tests" would land in two code blocks, where its mark is not allowed, ` +
				`and the upload leaves its quote nowhere it may stand; move that text out of them, or resolve the ask first`,
		},
		{
			name: "an ask into one code block twice", kind: MarkAsk, quote: "run the tests",
			seed:   "Intro.\n\nrun the tests\n",
			upload: "Intro.\n\n```\nrun the tests\nand run the tests\n```\n",
			want: `the open ask anchored on "run the tests" would land in a code block, where its mark is not allowed, ` +
				`and the upload leaves its quote nowhere it may stand; move that text out of it, or resolve the ask first`,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			service, artifactID := newTestService(t)
			service.settle = time.Hour
			seedServiceText(t, service, artifactID, test.seed)
			placeUploadAnchors(t, service, artifactID, []uploadAnchor{{kind: test.kind, quote: test.quote}})
			before := liveTree(t, service, artifactID)

			_, err := joinedReplaceText(service, artifactID, test.upload, model.Actor{Kind: "user", ID: "alice"})
			if !errors.Is(err, ErrInvalidMarkdown) || errors.Is(err, ErrDocSchema) || errors.Is(err, ErrDocOutsideSchema) ||
				!strings.Contains(err.Error(), test.want) {
				t.Fatalf("the upload = %v, want ErrInvalidMarkdown saying %q", err, test.want)
			}
			if got := liveTree(t, service, artifactID); !got.EqualWithBlockIDs(before) {
				t.Fatalf("the room after the refused upload =\n%s\nwant the tree it held\n%s", treeMarkdown(got), treeMarkdown(before))
			}
		})
	}
}

// An upload that copies an open anchor's quote where its mark is not allowed - into the front
// matter, into a code block - and keeps the original text is taken, and the anchor stays on the
// original: its nearest occurrence that may carry the mark, even where a copy stands nearer to
// where the anchor stood.
func TestAnUploadCopyingAnAnchorsQuoteWhereItsMarkIsNotAllowedKeepsTheAnchorOnTheOriginal(t *testing.T) {
	for _, test := range []struct {
		name, seed, upload, quote, holder string
		kind                              MarkKind
	}{
		{
			name: "a heading's comment copied into the front matter", kind: MarkComment, quote: "Rollout plan",
			seed: "# Rollout plan\n\nShip it.\n", upload: "---\ntitle: Rollout plan\n---\n\n# Rollout plan\n\nShip it.\n",
			holder: "heading",
		},
		{
			name: "an ask's quote copied into a code block above it", kind: MarkAsk, quote: "run the tests",
			seed: "Intro.\n\nrun the tests\n", upload: "Intro.\n\n```\nrun the tests\n```\n\nrun the tests\n",
			holder: "paragraph",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			service, artifactID := newTestService(t)
			service.settle = time.Hour
			seedServiceText(t, service, artifactID, test.seed)
			placed := placeUploadAnchors(t, service, artifactID, []uploadAnchor{{kind: test.kind, quote: test.quote}})

			if _, err := joinedReplaceText(service, artifactID, test.upload, model.Actor{Kind: "user", ID: "alice"}); err != nil {
				t.Fatalf("the upload = %v, want it taken", err)
			}
			tree := liveTree(t, service, artifactID)
			range_, quote, found := pmdoc.FindMark(tree, string(test.kind), placed[0].id)
			if !found || quote != test.quote {
				t.Fatalf("the %s after the upload covers %q found=%t, want %q", test.kind, quote, found, test.quote)
			}
			at, ok := pmdoc.ContainingTextblock(tree, range_.From)
			if !ok || at.Node.Type != test.holder {
				t.Fatalf("the %s after the upload stands in a textblock found=%t of type %q, want the original %s", test.kind, ok, holderType(at), test.holder)
			}
		})
	}
}

func holderType(at pmdoc.TextblockAt) string {
	if at.Node == nil {
		return ""
	}
	return at.Node.Type
}

// Only marks the schema allows nowhere their quote stands are the caller's, and the refusal names
// each kind of place it stands in with how many such places, the kinds in the order they were
// tried, so one pass over the markdown resolves the upload; any other refusal to re-anchor -
// one among places the schema allows no mark, and a mark type no open anchor carries - is the
// server's or the stored anchor's: a plain ErrDocSchema, which the routes answer 500 and log, never
// a document outside its schema that a route would offer to repair, and never an invalid-markdown
// answer.
func TestAReanchorErrorIsTheCallersOnlyWhenTheMarkIsNotAllowed(t *testing.T) {
	ask := anchoredMark{markType: string(MarkAsk), anchor: model.Anchor{Quote: "run the tests"}}
	notAllowed := &pmdoc.MarkNotAllowedError{Mark: string(MarkAsk), Textblock: "code_block", At: 40}
	inAnotherCodeBlock := &pmdoc.MarkNotAllowedError{Mark: string(MarkAsk), Textblock: "code_block", At: 70}
	inFrontMatter := &pmdoc.MarkNotAllowedError{Mark: string(MarkAsk), At: 1}
	serverFault := fmt.Errorf("%w: unsupported Yjs text embed", pmdoc.ErrSchema)
	for _, test := range []struct {
		name   string
		causes []error
		want   string
	}{
		{
			name:   "one place",
			causes: []error{notAllowed},
			want:   `the open ask anchored on "run the tests" would land in a code block, where its mark is not allowed, and the upload leaves its quote nowhere it may stand; move that text out of it, or resolve the ask first`,
		},
		{
			name:   "two places, the nearer first",
			causes: []error{inFrontMatter, notAllowed},
			want:   `the open ask anchored on "run the tests" would land in the front matter and a code block, where its mark is not allowed, and the upload leaves its quote nowhere it may stand; move that text out of them, or resolve the ask first`,
		},
		{
			name:   "two places of one kind",
			causes: []error{notAllowed, inAnotherCodeBlock},
			want:   `the open ask anchored on "run the tests" would land in two code blocks, where its mark is not allowed, and the upload leaves its quote nowhere it may stand; move that text out of them, or resolve the ask first`,
		},
		{
			name:   "one place its quote stands in twice",
			causes: []error{notAllowed, notAllowed},
			want:   `the open ask anchored on "run the tests" would land in a code block, where its mark is not allowed, and the upload leaves its quote nowhere it may stand; move that text out of it, or resolve the ask first`,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := reanchorError(ask, test.causes...)
			if !errors.Is(err, ErrInvalidMarkdown) || errors.Is(err, ErrDocSchema) {
				t.Fatalf("reanchorError(not allowed) = %v, want ErrInvalidMarkdown alone", err)
			}
			if got := strings.TrimPrefix(err.Error(), ErrInvalidMarkdown.Error()+": "); got != test.want {
				t.Fatalf("reanchorError said %q, want %q", got, test.want)
			}
		})
	}
	for _, test := range []struct {
		name   string
		mark   anchoredMark
		causes []error
	}{
		{name: "a mark the writer refuses as a mark", mark: ask, causes: []error{fmt.Errorf("%w: mark %q", pmdoc.ErrSchema, "bogus")}},
		{name: "a fragment the writer cannot read", mark: ask, causes: []error{serverFault}},
		{name: "a mark type no open anchor carries", mark: anchoredMark{markType: "proofAuthored"}, causes: []error{notAllowed}},
		{name: "a place that allows no mark and a refusal the server owns", mark: ask, causes: []error{notAllowed, serverFault}},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := reanchorError(test.mark, test.causes...)
			if !errors.Is(err, ErrDocSchema) || errors.Is(err, ErrDocOutsideSchema) || errors.Is(err, ErrInvalidMarkdown) {
				t.Fatalf("reanchorError = %v, want a plain ErrDocSchema", err)
			}
		})
	}
}

// durableHistory is the document's stored history once every update its room applied is durable:
// its rows and their bytes, each update kept as the room applied it until a fold.
func durableHistory(t *testing.T, service *Service, artifactID string) (rows, bytes int) {
	t.Helper()
	waitForLandedAppends(t, service, artifactID)
	return storedUpdateCount(t, service.store, artifactID), storedUpdateBytes(t, service.store, artifactID)
}

// An upload that changes one token far from every open anchor writes the room an update little
// larger than that token: the anchored text it leaves unchanged keeps its anchors as they were,
// rather than losing and regaining each, a re-formatting per anchor that the room and its stored
// history would keep for the document's life.
func TestAnUploadChangingOneTokenWritesASmallUpdateOverOpenAnchors(t *testing.T) {
	const anchors, uploads, perUploadBound = 20, 3, 2048
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	paragraphs := make([]string, 0, anchors)
	placing := make([]uploadAnchor, 0, anchors)
	for index := range anchors {
		paragraphs = append(paragraphs, fmt.Sprintf("Paragraph %d says topic%02d plainly and moves on.", index, index))
		placing = append(placing, uploadAnchor{kind: MarkComment, quote: fmt.Sprintf("topic%02d plainly", index)})
	}
	document := func(revision int) string {
		return fmt.Sprintf("Revision token r%d\n\n%s\n", revision, strings.Join(paragraphs, "\n\n"))
	}
	seedServiceText(t, service, artifactID, document(0))
	placed := placeUploadAnchors(t, service, artifactID, placing)

	rowsBefore, bytesBefore := durableHistory(t, service, artifactID)
	for revision := 1; revision <= uploads; revision++ {
		if _, err := joinedReplaceText(service, artifactID, document(revision), model.Actor{Kind: "user", ID: "alice"}); err != nil {
			t.Fatalf("upload revision %d: %v", revision, err)
		}
	}
	rowsAfter, bytesAfter := durableHistory(t, service, artifactID)
	if rowsAfter < rowsBefore+uploads {
		t.Fatalf("the stored history went from %d rows to %d over %d uploads: a fold ran, and the bytes no longer measure the uploads", rowsBefore, rowsAfter, uploads)
	}
	perUpload := (bytesAfter - bytesBefore) / uploads
	t.Logf("each upload of one token over %d open anchors wrote %d bytes", anchors, perUpload)
	if perUpload >= perUploadBound {
		t.Fatalf("each upload of one token over %d open anchors wrote %d bytes, want under %d", anchors, perUpload, perUploadBound)
	}
	after := liveTree(t, service, artifactID)
	for _, anchor := range placed {
		if got, quote, found := pmdoc.FindMark(after, string(anchor.kind), anchor.id); !found || got != anchor.range_ || quote != anchor.quote {
			t.Errorf("comment %s after the uploads = %+v %q found=%t, want %+v %q", anchor.id, got, quote, found, anchor.range_, anchor.quote)
		}
	}
}
