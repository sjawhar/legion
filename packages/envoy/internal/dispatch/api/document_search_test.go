package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/embed"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/explaintest"
)

// documentSearchIndex is the GIN index migration 20261101182447 builds over
// document_search.search. Every statement that matches a tsquery on the document arm reads that
// table through it; before migration 20261101182447 each of them read every document's latest
// version through a lateral join over artifact_versions, whose search column has no index and
// can have none.
const documentSearchIndex = "document_search_vector"

const (
	// documentSearchWord is in the latest version of two of the project's documents, and in its
	// issue, a comment, an ask and a message, so every arm of the keyword union answers it.
	documentSearchWord = "zebra"
	// documentSearchDocumentWord is in documents alone, so a probe that answers it answered from
	// the document arm and no other.
	documentSearchDocumentWord = "plateau"
	// documentSearchOldWord is in one document's first version and nowhere else. Search reads
	// only the latest version of a document, so no statement may answer it.
	documentSearchOldWord = "quagga"
)

// documentSearchStatement is one of R1's five statements: the package's own text, the text the
// release before this change ran over the same rows (document_search_oracle_test.go), and the
// arguments each is run with. sorted says the statement declares no order of its own, so its
// rows are compared as a set; holds and lacks are what the rows must and must not name, which
// is what says the comparison decided something rather than comparing two empty answers.
type documentSearchStatement struct {
	name      string
	statement string
	oracle    string
	arguments []any
	sorted    bool
	holds     []string
	lacks     []string
}

// documentSearchFixture is the corpus of R1's plan and equality tests: one project whose
// documents, issue, comment, ask and message all hold documentSearchWord, a document whose first
// version alone holds documentSearchOldWord, and a second project holding the same word, which
// every statement's project filter must leave out.
type documentSearchFixture struct {
	handler  http.Handler
	database *store.Store
	embedder *fakeEmbedder
	project  string
	issueKey string
	spec     string
	notes    string
	atlas    string
}

const (
	documentSearchTitle     = "Zebra census plan"
	documentSearchSpec      = "The herd crosses the plateau in spring."
	documentSearchNotesOld  = "# Field notes\n\nQuagga sightings in the north.\n"
	documentSearchNotesNew  = "# Field notes\n\nZebra sightings in the north.\n"
	documentSearchAtlasText = "# Atlas\n\nZebra ranges of the plateau.\n"
)

func newDocumentSearchFixture(t *testing.T) documentSearchFixture {
	t.Helper()
	// The issue and the newest field notes are the only embedded texts with a vector near the
	// query's, so the meaning arms of the two meaning statements answer rows rather than nothing.
	embedder := &fakeEmbedder{vectors: map[string][]float32{
		issueEmbeddingText(documentSearchTitle, documentSearchSpec): angledVector(0.9, 2),
		documentSearchNotesNew: angledVector(0.8, 3),
	}}
	handler, database, _ := newTestServer(t, testServerOptions{embedder: embedder})
	issue := createInteractionIssue(t, handler, "DOCS", documentSearchTitle, documentSearchSpec)
	// The same name twice: the first call creates the document, the second writes a version of
	// it, so the word of the first version is in no version search reads.
	notes := uploadIssueDocument(t, handler, issue.Key, "field-notes.md", documentSearchNotesOld)
	uploadIssueDocument(t, handler, issue.Key, "field-notes.md", documentSearchNotesNew)
	atlas := createProjectDocument(t, handler, "DOCS", "atlas.md", documentSearchAtlasText)

	comment := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/comments", map[string]any{
		"body": "The zebra count is stable.",
	}, "alice")
	if comment.Code != http.StatusCreated {
		t.Fatalf("create the comment: status=%d body=%s", comment.Code, comment.Body.String())
	}
	ask := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/asks", map[string]any{
		"question": "Count the zebra herd again?",
	}, "alice")
	if ask.Code != http.StatusCreated {
		t.Fatalf("create the ask: status=%d body=%s", ask.Code, ask.Body.String())
	}
	createIssueMessage(t, handler, issue.Key, map[string]any{"body": "The zebra survey is booked."}, "alice")

	// A second project holding the same word, so every statement's project filter has something
	// to leave out.
	if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{
		"key": "OPS", "name": "Operations",
	}, "alice"); response.Code != http.StatusCreated {
		t.Fatalf("create the second project: status=%d body=%s", response.Code, response.Body.String())
	}
	createProjectDocument(t, handler, "OPS", "range-log.md", "# Range log\n\nZebra sightings on the plateau.\n")

	// The bulk the planner needs, which no read above returns: on a handful of documents every
	// plan costs about the same and the planner's choice says nothing, and production's shape is
	// a few matches in a table holding many. They are written as SQL, the only part of the
	// fixture that is not a writer's own work, and 20261101182454's trigger fills document_search
	// for them as it does for the rest.
	ctx := context.Background()
	for _, bulk := range []struct{ what, statement string }{
		{"the other documents", `
			insert into artifacts (project_key, slug, name, kind, created_by)
			select 'DOCS', 'sheet-' || n, 'Sheet ' || n || '.md', 'doc', '{"kind":"user","id":"alice"}'
			from generate_series(1, 400) n
		`},
		{"their versions", `
			insert into artifact_versions (artifact_id, number, markdown, authors)
			select a.id, 1, '# ' || a.name || chr(10) || chr(10) || 'Routine survey notes for the northern sector, sheet ' || a.slug || '.' || chr(10), '[]'
			from artifacts a where a.slug like 'sheet-%'
		`},
	} {
		if _, err := database.Pool.Exec(ctx, bulk.statement); err != nil {
			t.Fatalf("seed %s: %v", bulk.what, err)
		}
	}

	processAllPending(t, database, embedder)
	if _, err := database.Pool.Exec(ctx, "analyze"); err != nil {
		t.Fatalf("analyze the fixture: %v", err)
	}
	return documentSearchFixture{
		handler: handler, database: database, embedder: embedder,
		project: "DOCS", issueKey: issue.Key, spec: issue.PrimaryArtifactID, notes: notes.ID, atlas: atlas.ID,
	}
}

// statements is R1's five, each with the arguments its caller passes: search's two with
// runFusedSearchQuery's and runMeaningSearch's, the related-item check's two with
// relatedSearch's, and the related-terms probe with probeRelatedCandidates'. The check's $1 is a
// tsquery already built (relatedQOpener), never re-stemmed, so it is written here as the picker
// writes it.
func (f documentSearchFixture) statements() []documentSearchStatement {
	word := documentSearchWord
	searchArguments := []any{word, f.project, searchDefaultLimit, firstTerm(word), headlineOptions,
		0, contracts.SearchKindDepth, searchFusionK}
	meaning := []any{embed.Literal(queryVector()), searchMeaningFloor, f.embedder.Identity()}
	relatedArguments := []any{"'" + word + "'", f.project, suggestionSearchDepth, word, headlineOptions,
		0, contracts.SearchKindDepth, searchFusionK}
	lexemes, weights := []string{word}, []float64{2.5}
	documents := []string{f.notes, f.atlas}
	return []documentSearchStatement{
		{
			name:      "search, keywords only",
			statement: searchQuery,
			oracle:    mainSearchQuery,
			arguments: searchArguments,
			holds:     documents,
		},
		{
			name:      "search, meaning and keywords",
			statement: searchQueryMeaning,
			oracle:    mainSearchQueryMeaning,
			arguments: slices.Concat(searchArguments, meaning),
			holds:     documents,
		},
		{
			name:      "the related-item check's keyword stage",
			statement: relatedKeywordQuery,
			oracle:    mainRelatedKeywordQuery,
			arguments: slices.Concat(relatedArguments, []any{lexemes, weights}),
			holds:     documents,
		},
		{
			name:      "the related-item check's meaning stage",
			statement: relatedMeaningQuery,
			oracle:    mainRelatedMeaningQuery,
			arguments: slices.Concat(relatedArguments, meaning, []any{lexemes, weights}),
			holds:     documents,
		},
		{
			name:      "the related-terms probe",
			statement: relatedProbeSQL,
			oracle:    mainRelatedProbeSQL,
			arguments: []any{f.project, []string{word, documentSearchDocumentWord, documentSearchOldWord}, "", (*string)(nil)},
			// The probe declares no order, so its answer is a set.
			sorted: true,
			// Only documents hold documentSearchDocumentWord, so answering it is the document
			// arm's own work; documentSearchOldWord is in an old version alone.
			holds: []string{documentSearchWord, documentSearchDocumentWord},
			lacks: []string{documentSearchOldWord},
		},
	}
}

// A1. Each statement of R1 reaches its document arm through document_search's GIN index, and no
// read of artifact_versions in its plan tests a tsquery: that test is what the new table exists
// to move off a table no index can serve. Naming the index is the assertion, not merely the
// absence of a sequential scan - a scan can carry an index condition on a column its index does
// not lead with and still read the whole index - so the scan's condition is held to the index's
// own first key column too. Sequential scans are off for the plan: a fixture this small is read
// faster whole than through any index, so this says the statement's shape permits the index scan,
// which is the claim under test.
func TestEveryDocumentArmSearchesTheDocumentSearchIndex(t *testing.T) {
	fixture := newDocumentSearchFixture(t)
	firstKeys := explaintest.NewIndexKeys(fixture.database.Pool)
	for _, statement := range fixture.statements() {
		t.Run(statement.name, func(t *testing.T) {
			plan := planWithoutSeqScans(t, fixture.database, statement.statement, statement.arguments...)
			firstKeys.RequireIndexKeyed(t, plan, documentSearchIndex, statement.name, explaintest.CondRequired)
			if matched := artifactVersionsTestingATSQuery(t, plan); len(matched) > 0 {
				t.Fatalf("%s still tests a tsquery against artifact_versions at %s:\n%s",
					statement.name, strings.Join(matched, ", "), plan)
			}
		})
	}
}

// The negative control of the test above: main's own statement, over the same rows and the same
// arguments, searches no document_search index and does test a tsquery against artifact_versions.
// Without it a plan assertion that happened to hold for any statement would pass unnoticed.
func TestMainsDocumentArmSearchesNoDocumentSearchIndex(t *testing.T) {
	fixture := newDocumentSearchFixture(t)
	statement := fixture.statements()[0]
	plan := planWithoutSeqScans(t, fixture.database, statement.oracle, statement.arguments...)
	for _, scan := range explaintest.IndexScans(t, plan) {
		if scan.Index == documentSearchIndex {
			t.Fatalf("main's keyword search reads %s, a table it does not know:\n%s", documentSearchIndex, plan)
		}
	}
	if matched := artifactVersionsTestingATSQuery(t, plan); len(matched) == 0 {
		t.Fatalf("main's keyword search tests no tsquery against artifact_versions, so the assertion it is the control for decides nothing:\n%s", plan)
	}
}

// A2. Each statement of R1 answers exactly what the released statement answers over the same
// rows. The oracle is that released text held in the test, not a statement built from this
// package's helpers, so an edit that changed what the document arm returns would move one side
// and not the other.
func TestEveryDocumentArmAnswersWhatMainsStatementDoes(t *testing.T) {
	fixture := newDocumentSearchFixture(t)
	ctx := context.Background()
	for _, statement := range fixture.statements() {
		t.Run(statement.name, func(t *testing.T) {
			want := documentSearchRows(t, ctx, fixture.database, statement.oracle, statement.arguments, nil)
			got := documentSearchRows(t, ctx, fixture.database, statement.statement, statement.arguments,
				[]string{"set local enable_seqscan = off"})
			if statement.sorted {
				slices.Sort(want)
				slices.Sort(got)
			}
			if !slices.Equal(got, want) {
				t.Fatalf("%s answered\n%v\nwant main's rows\n%v", statement.name, got, want)
			}
			answer := strings.Join(want, "\n")
			for _, held := range statement.holds {
				if !strings.Contains(answer, held) {
					t.Fatalf("%s named nothing of %q, so the comparison decided nothing:\n%v", statement.name, held, want)
				}
			}
			for _, lacked := range statement.lacks {
				if strings.Contains(answer, lacked) {
					t.Fatalf("%s named %q, which only an old version of a document holds:\n%v", statement.name, lacked, want)
				}
			}
		})
	}
}

// documentSearchPlanNode is what artifactVersionsTestingATSQuery reads of one plan node: the
// relation it reads and every condition the plan can write a match operator into. explaintest
// answers which index each scan reads and under which condition, which is the other half of the
// plan assertions above; the question here is a different one - whether any read of one named
// relation tests a tsquery - so it reads the conditions no index scan carries too.
type documentSearchPlanNode struct {
	NodeType    string                   `json:"Node Type"`
	Relation    string                   `json:"Relation Name"`
	IndexCond   string                   `json:"Index Cond"`
	RecheckCond string                   `json:"Recheck Cond"`
	Filter      string                   `json:"Filter"`
	JoinFilter  string                   `json:"Join Filter"`
	Plans       []documentSearchPlanNode `json:"Plans"`
}

// artifactVersionsTestingATSQuery names every node of the plan that tests a tsquery (@@) over a
// subtree reading artifact_versions. The test is not always written on the scan: main's document
// arm reads the latest version through the unique key and tests `v.search @@ q.tsq` as the join
// filter of the lateral's Nested Loop, which names no relation of its own, so the condition is
// matched against the relations below it rather than against the node's own. The page's lateral
// still reads artifact_versions for the snippet's markdown and the meaning candidates for a
// version's created_at, and neither tests a tsquery, so the assertion is about the match and not
// about the table.
func artifactVersionsTestingATSQuery(t *testing.T, planJSON json.RawMessage) []string {
	t.Helper()
	var root documentSearchPlanNode
	if err := json.Unmarshal(planJSON, &root); err != nil {
		t.Fatalf("decode the plan tree: %v", err)
	}
	var matched []string
	var walk func(node documentSearchPlanNode)
	walk = func(node documentSearchPlanNode) {
		for _, condition := range []string{node.IndexCond, node.RecheckCond, node.Filter, node.JoinFilter} {
			if strings.Contains(condition, "@@") && readsRelation(node, "artifact_versions") {
				matched = append(matched, fmt.Sprintf("%s, `%s`", node.NodeType, condition))
			}
		}
		for _, child := range node.Plans {
			walk(child)
		}
	}
	walk(root)
	return matched
}

// readsRelation reports whether node or anything under it reads the relation named relation.
func readsRelation(node documentSearchPlanNode, relation string) bool {
	if node.Relation == relation {
		return true
	}
	for _, child := range node.Plans {
		if readsRelation(child, relation) {
			return true
		}
	}
	return false
}

// documentSearchRows runs statement under settings and returns one string per row, in the order
// the statement answers them.
func documentSearchRows(t *testing.T, ctx context.Context, database *store.Store, statement string, arguments []any, settings []string) []string {
	t.Helper()
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the read's transaction: %v", err)
	}
	defer tx.Rollback(ctx)
	for _, setting := range settings {
		if _, err := tx.Exec(ctx, setting); err != nil {
			t.Fatalf("%s: %v", setting, err)
		}
	}
	rows, err := tx.Query(ctx, statement, arguments...)
	if err != nil {
		t.Fatalf("run the statement: %v", err)
	}
	defer rows.Close()
	read := []string{}
	for rows.Next() {
		values, err := rows.Values()
		if err != nil {
			t.Fatalf("read a row: %v", err)
		}
		read = append(read, fmt.Sprint(values...))
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate the statement: %v", err)
	}
	return read
}
