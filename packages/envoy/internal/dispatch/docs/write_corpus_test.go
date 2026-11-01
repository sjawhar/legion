package docs

// The write corpus differential runs the document writes the API makes - an anchored comment, a
// suggestion, and an edit with its version, each one committed transaction in the API's order -
// on every spec of a directory of markdown specs, at two revisions, and dumps what each write
// answered and left, so a change to how the writes read and render the document can be judged by
// comparing the two dumps byte for byte (cmp). The test skips unless DISPATCH_TOKEN_CORPUS names
// the directory. The file is copied unchanged into the older revision, so it uses only what both
// revisions have: the service's exported writes and reads, and the package's own test helpers.
// runWriteCorpus is shared with the reads test, which observes every operation it runs.

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// writeCorpusLine is one spec's record in the dump: what each pass wrote, in pass order.
type writeCorpusLine struct {
	File   string            `json:"file"`
	Passes []writeCorpusPass `json:"passes"`
}

// writeCorpusPass is one document's run of the writes: warm, with the room loaded before each
// transaction, or cold, with no room holding the document when each transaction begins.
type writeCorpusPass struct {
	Mode           string             `json:"mode"`
	Steps          []writeCorpusStep  `json:"steps"`
	SuggestionKind string             `json:"suggestionKind"`
	Blocks         []writeCorpusBlock `json:"blocks"`
}

// writeCorpusStep is one committed transaction: what its anchored write answered, the version it
// wrote, whether its edit changed the document, and the document's token and markdown afterwards.
type writeCorpusStep struct {
	Name           string               `json:"name"`
	Anchored       *writeCorpusAnchored `json:"anchored,omitempty"`
	Version        *writeCorpusVersion  `json:"version,omitempty"`
	Changed        *bool                `json:"changed,omitempty"`
	Outcome        *EditOutcome         `json:"outcome,omitempty"`
	Refusal        string               `json:"refusal,omitempty"`
	Token          string               `json:"token"`
	MarkdownSha256 string               `json:"markdownSha256"`
}

type writeCorpusAnchored struct {
	Quote   string `json:"quote"`
	BlockID string `json:"blockId"`
}

// writeCorpusVersion is a version a step wrote, its markdown read back from the store.
type writeCorpusVersion struct {
	Number         int    `json:"number"`
	Wrote          bool   `json:"wrote"`
	MarkdownSha256 string `json:"markdownSha256"`
}

type writeCorpusBlock struct {
	ID    string `json:"id"`
	Type  string `json:"type"`
	From  int    `json:"from"`
	To    int    `json:"to"`
	Token string `json:"token"`
}

// writeCorpusModes are the passes runWriteCorpus makes, each on a document of its own.
var writeCorpusModes = []string{"warm", "cold"}

var (
	writeCorpusUploader = model.Actor{Kind: "session", ID: "session-write-corpus-uploader"}
	writeCorpusReviewer = model.Actor{Kind: "user", ID: "write-corpus-reviewer"}
	writeCorpusEditor   = model.Actor{Kind: "session", ID: "session-write-corpus-editor"}
)

// writeCorpusCreatedAt is the time every projected comment carries, as timestampValue formats a
// comment row's created_at, fixed so both revisions write the same marks.
const writeCorpusCreatedAt = "2026-10-28T00:00:00Z"

// newWriteCorpusService is a service with a store of its own whose settlement waits an hour, so
// no settlement runs while the writes do.
func newWriteCorpusService(t *testing.T) *Service {
	t.Helper()
	service, _ := newTestService(t)
	service.settle = time.Hour
	return service
}

// writeCorpusBlockIDs has every block id minted from here on numbered in mint order under prefix,
// so both revisions mint the same ids; the test's cleanup restores UUIDs.
func writeCorpusBlockIDs(prefix string) {
	var next atomic.Int64
	pmdoc.SetBlockIDGenerator(func() string {
		return fmt.Sprintf("%s-%d", prefix, next.Add(1))
	})
}

// seedWriteCorpusDocument creates a document holding markdown as an upload creates one: the
// artifact row, the service's seed of its live document, and its first version recording the
// seed's canonical markdown, credited to the uploader, all in one committed transaction.
func seedWriteCorpusDocument(t *testing.T, service *Service, slug, markdown string) string {
	t.Helper()
	ctx := context.Background()
	tx, err := service.store.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the upload: %v", err)
	}
	defer tx.Rollback(ctx)
	var artifactID string
	if err := tx.QueryRow(ctx, `
		insert into artifacts (issue_key, project_key, slug, name, kind, is_primary, created_by)
		values ('DOC-1', 'DOC', $1, $2, 'doc', false, $3)
		returning id::text
	`, slug, slug+".md", writeCorpusUploader).Scan(&artifactID); err != nil {
		t.Fatalf("create the document: %v", err)
	}
	joined, ledger := service.Join(ctx, tx)
	defer ledger.Discard()
	canonical, err := service.SeedText(joined, artifactID, markdown, writeCorpusUploader)
	if err != nil {
		t.Fatalf("seed the document: %v", err)
	}
	authors := []model.Actor{writeCorpusUploader}
	if _, err := tx.Exec(ctx, `
		insert into artifact_versions (artifact_id, number, markdown, authors)
		values ($1, 1, $2, $3)
	`, artifactID, canonical, authors); err != nil {
		t.Fatalf("write the document's first version: %v", err)
	}
	ledger.WroteVersion(artifactID, model.Version{Number: 1, Authors: authors})
	if err := ledger.Commit(ctx); err != nil {
		t.Fatalf("commit the upload: %v", err)
	}
	return artifactID
}

// writeCorpusQuotes is the text each write anchors to or replaces, each in a paragraph of its
// own and found exactly once in the document.
type writeCorpusQuotes struct {
	comment, suggestion, edit string
}

// chooseWriteCorpusQuotes picks, in document order, phrases that occur exactly once in tree
// (FindQuote): the comment's and the edit's in the first two top-level paragraphs that hold one,
// and the suggestion's in the first paragraph inside a list item or table cell that holds one, or
// the third top-level paragraph where the spec has none. A phrase is the opening of an unmarked
// text node - its first sentence, at most twelve words and at least five - written in letters,
// digits, spaces and plain punctuation only, so the edit's replacement reads as the same text.
func chooseWriteCorpusQuotes(t *testing.T, tree *pmdoc.Node) writeCorpusQuotes {
	t.Helper()
	var topLevel []string
	var nested string
	var visit func(node *pmdoc.Node, depth int, inCell bool)
	visit = func(node *pmdoc.Node, depth int, inCell bool) {
		if node.Type == "paragraph" {
			if (depth != 1 || len(topLevel) == 3) && (!inCell || nested != "") {
				return
			}
			for _, child := range node.Children {
				quote, ok := writeCorpusPhrase(child)
				if !ok {
					continue
				}
				if _, err := pmdoc.FindQuote(tree, quote, nil, nil); err != nil {
					continue
				}
				if depth == 1 {
					topLevel = append(topLevel, quote)
				} else {
					nested = quote
				}
				return
			}
			return
		}
		switch node.Type {
		case "list_item", "table_cell", "table_header":
			inCell = true
		}
		for _, child := range node.Children {
			if len(topLevel) == 3 && nested != "" {
				return
			}
			visit(child, depth+1, inCell)
		}
	}
	visit(tree, 0, false)
	if nested == "" && len(topLevel) == 3 {
		nested = topLevel[2]
	}
	if len(topLevel) < 2 || nested == "" {
		t.Fatalf("the document has %d top-level and %t nested paragraphs holding a phrase found once; the writes need three", len(topLevel), nested != "")
	}
	return writeCorpusQuotes{comment: topLevel[0], suggestion: nested, edit: topLevel[1]}
}

// writeCorpusPhrase is node's opening phrase when node is an unmarked text node that has one (see
// chooseWriteCorpusQuotes).
func writeCorpusPhrase(node *pmdoc.Node) (string, bool) {
	if node.Type != "text" || len(node.Marks) != 0 {
		return "", false
	}
	words := strings.Fields(node.Text)
	end := min(len(words), 12)
	for index, word := range words[:end] {
		if strings.HasSuffix(word, ".") || strings.HasSuffix(word, "?") || strings.HasSuffix(word, "!") {
			end = index + 1
			break
		}
	}
	if end < 5 {
		return "", false
	}
	phrase := strings.Join(words[:end], " ")
	if !strings.Contains(node.Text, phrase) {
		return "", false
	}
	for index, r := range phrase {
		letter := unicode.IsLetter(r)
		if index == 0 && !letter {
			return "", false
		}
		if !letter && !unicode.IsDigit(r) && !strings.ContainsRune(" ,.;:'?!()-", r) {
			return "", false
		}
	}
	return phrase, true
}

func writeCorpusSha256(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

// runWriteCorpus runs, on a fresh document of service holding markdown for each pass
// (writeCorpusModes), the three transactions the API runs for an anchored comment, a suggestion
// and a doc-edit, and returns what they answered and left. observe, when set, is called inside
// each transaction before its first operation with "<mode>/<transaction>/begin" and after each
// operation with "<mode>/<transaction>/<operation>".
func runWriteCorpus(t *testing.T, service *Service, specName, markdown string, observe func(step string)) writeCorpusLine {
	t.Helper()
	t.Cleanup(func() { pmdoc.SetBlockIDGenerator(nil) })
	if observe == nil {
		observe = func(string) {}
	}
	ctx := context.Background()
	line := writeCorpusLine{File: specName}
	for _, mode := range writeCorpusModes {
		started := time.Now()
		writeCorpusBlockIDs(mode)
		artifactID := seedWriteCorpusDocument(t, service, "write-corpus-"+mode, markdown)
		seeded := time.Since(started)
		quotes := chooseWriteCorpusQuotes(t, persistedProofTree(t, service.store, artifactID))
		pass := writeCorpusPass{Mode: mode}
		timings := []string{fmt.Sprintf("seed %s", seeded.Round(time.Millisecond))}

		// transaction runs write as one committed transaction named name, with the room loaded
		// first (warm) or none holding the document (cold), and records the step it fills in with
		// the markdown of the version it wrote, read back from the store, and the document's token
		// and markdown, once it commits.
		transaction := func(name string, write func(ctx context.Context, step *writeCorpusStep, op func(string)) error) {
			t.Helper()
			if mode == "cold" {
				closeDocumentRoom(t, service, artifactID)
			} else if err := service.warmLiveDocument(ctx, artifactID); err != nil {
				t.Fatalf("%s %s: load the document's room: %v", mode, name, err)
			}
			step := writeCorpusStep{Name: name}
			began := time.Now()
			if err := joinedWrite(service, artifactID, func(ctx context.Context) error {
				observe(mode + "/" + name + "/begin")
				return write(ctx, &step, func(operation string) { observe(mode + "/" + name + "/" + operation) })
			}); err != nil {
				t.Fatalf("%s %s: %v", mode, name, err)
			}
			timings = append(timings, fmt.Sprintf("%s %s", name, time.Since(began).Round(time.Millisecond)))
			text, token, err := service.TextWithToken(ctx, artifactID)
			if err != nil {
				t.Fatalf("%s %s: read the document: %v", mode, name, err)
			}
			step.Token, step.MarkdownSha256 = token, writeCorpusSha256(text)
			if step.Version != nil {
				step.Version.MarkdownSha256 = writeCorpusSha256(versionMarkdown(t, service.store, artifactID, step.Version.Number))
			}
			pass.Steps = append(pass.Steps, step)
		}

		// anchor writes the API's root comment on quote, a suggestion replacing it with
		// replaceWith when that is set: the mark, the anchor's version, the suggestion's kind,
		// and the mark's projection (resolveAnchor, then comment_create's commentMarkRecord).
		anchor := func(ctx context.Context, step *writeCorpusStep, op func(string), kind MarkKind, id, quote, body, replaceWith string) error {
			anchored, err := service.MarkQuote(ctx, artifactID, MarkSpec{Kind: kind, ID: id, By: writeCorpusReviewer}, quote, nil)
			if err != nil {
				return fmt.Errorf("MarkQuote: %w", err)
			}
			op("MarkQuote")
			step.Anchored = &writeCorpusAnchored{Quote: anchored.Quote, BlockID: anchored.BlockID}
			version, err := service.SnapshotVersion(ctx, artifactID, writeCorpusReviewer)
			if err != nil {
				return fmt.Errorf("SnapshotVersion: %w", err)
			}
			op("SnapshotVersion")
			step.Version = &writeCorpusVersion{Number: version.Version.Number, Wrote: version.Wrote}
			record := MarkRecord{
				Kind:      "comment",
				By:        ActorRef(writeCorpusReviewer),
				CreatedAt: writeCorpusCreatedAt,
				Text:      body,
				Replies:   make([]MarkReply, 0),
			}
			if kind == MarkSuggestion {
				suggestionKind, err := service.SuggestionKind(ctx, artifactID, id)
				if err != nil {
					return fmt.Errorf("SuggestionKind: %w", err)
				}
				op("SuggestionKind")
				pass.SuggestionKind = suggestionKind
				record.Kind, record.Content, record.Status = suggestionKind, replaceWith, "pending"
			}
			if err := service.ProjectMark(ctx, artifactID, id, record, writeCorpusReviewer); err != nil {
				return fmt.Errorf("ProjectMark: %w", err)
			}
			op("ProjectMark")
			return nil
		}

		transaction("comment", func(ctx context.Context, step *writeCorpusStep, op func(string)) error {
			return anchor(ctx, step, op, MarkComment, "c-1", quotes.comment, "A corpus comment.", "")
		})
		transaction("suggestion", func(ctx context.Context, step *writeCorpusStep, op func(string)) error {
			return anchor(ctx, step, op, MarkSuggestion, "s-1", quotes.suggestion, "A corpus suggestion.", quotes.suggestion+" (suggested)")
		})
		// The edit runs as editArtifact runs one with no summary: the operations, then a snapshot
		// once they changed the document.
		transaction("edit", func(ctx context.Context, step *writeCorpusStep, op func(string)) error {
			edit, err := service.ApplyOps(ctx, artifactID, []model.EditOp{
				{Op: "replace", Find: quotes.edit, With: quotes.edit + " (corpus edit)"},
			}, writeCorpusEditor, nil)
			if err != nil {
				return fmt.Errorf("ApplyOps: %w", err)
			}
			op("ApplyOps")
			step.Changed = &edit.Changed
			if !edit.Changed {
				return nil
			}
			version, err := service.SnapshotVersion(ctx, artifactID, writeCorpusEditor)
			if err != nil {
				return fmt.Errorf("SnapshotVersion: %w", err)
			}
			op("SnapshotVersion")
			step.Version = &writeCorpusVersion{Number: version.Version.Number, Wrote: version.Wrote}
			return nil
		})

		// The edit kinds the route serves beside a replace, each its own committed transaction and
		// each resolved against what the one before it left: an insert after a block, a batch of
		// two operations, an edit under a document precondition, one under a block precondition,
		// a replace the document refuses as unreadable, a delete of a block by id and a delete of
		// a found sentence. Each records the answer the route gives, or the words it refuses with.
		const (
			inserted  = "Corpus inserted paragraph."
			edited    = "Corpus inserted paragraph, edited."
			second    = "Corpus second paragraph."
			qualified = "Corpus second paragraph, conditioned."
		)
		// apply runs ops as the route runs them and records the outcome, or the refusal's words
		// when refuse is true, in which case the transaction must leave the document as it was.
		apply := func(ops []model.EditOp, precondition *model.EditPrecondition, refuse bool) func(ctx context.Context, step *writeCorpusStep, op func(string)) error {
			return func(ctx context.Context, step *writeCorpusStep, op func(string)) error {
				outcome, err := service.ApplyOps(ctx, artifactID, ops, writeCorpusEditor, precondition)
				op("ApplyOps")
				if err != nil {
					if !refuse {
						return fmt.Errorf("ApplyOps: %w", err)
					}
					step.Refusal = err.Error()
					return nil
				}
				if refuse {
					return fmt.Errorf("ApplyOps was expected to refuse, and answered %+v", outcome)
				}
				step.Outcome = &outcome
				return nil
			}
		}
		firstBlock := func(t *testing.T) model.ArtifactBlock {
			t.Helper()
			_, blocks, err := service.TextWithBlocks(ctx, artifactID)
			if err != nil || len(blocks) == 0 {
				t.Fatalf("%s: read the document's blocks: %v (%d blocks)", mode, err, len(blocks))
			}
			return blocks[0]
		}
		blockHolding := func(t *testing.T, quote string) model.ArtifactBlock {
			t.Helper()
			id, err := service.BlockForQuote(ctx, artifactID, quote)
			if err != nil {
				t.Fatalf("%s: the block holding %q: %v", mode, quote, err)
			}
			_, blocks, err := service.TextWithBlocks(ctx, artifactID)
			if err != nil {
				t.Fatalf("%s: read the document's blocks: %v", mode, err)
			}
			for _, block := range blocks {
				if block.ID == id {
					return block
				}
			}
			t.Fatalf("%s: block %q holding %q is in no read of the document", mode, id, quote)
			return model.ArtifactBlock{}
		}

		anchorBlock := firstBlock(t)
		transaction("insert", func(ctx context.Context, step *writeCorpusStep, op func(string)) error {
			return apply([]model.EditOp{
				{Op: "insert", After: "block:" + anchorBlock.ID, Markdown: inserted},
			}, nil, false)(ctx, step, op)
		})
		transaction("batch", func(ctx context.Context, step *writeCorpusStep, op func(string)) error {
			return apply([]model.EditOp{
				{Op: "replace", Find: inserted, With: edited},
				{Op: "insert", After: edited, Markdown: second},
			}, nil, false)(ctx, step, op)
		})
		documentToken := ""
		transaction("precondition-document", func(ctx context.Context, step *writeCorpusStep, op func(string)) error {
			_, token, err := service.TextWithToken(ctx, artifactID)
			if err != nil {
				return fmt.Errorf("TextWithToken: %w", err)
			}
			documentToken = token
			return apply([]model.EditOp{
				{Op: "replace", Find: second, With: qualified},
			}, &model.EditPrecondition{Document: token}, false)(ctx, step, op)
		})
		transaction("precondition-block", func(ctx context.Context, step *writeCorpusStep, op func(string)) error {
			block := blockHolding(t, qualified)
			return apply([]model.EditOp{
				{Op: "replace", Find: qualified, With: qualified + " Twice."},
			}, &model.EditPrecondition{Blocks: []model.EditBlockPrecondition{{ID: block.ID, Token: block.Token}}}, false)(ctx, step, op)
		})
		transaction("precondition-stale", func(ctx context.Context, step *writeCorpusStep, op func(string)) error {
			return apply([]model.EditOp{
				{Op: "replace", Find: edited, With: edited + " Again."},
			}, &model.EditPrecondition{Document: documentToken}, true)(ctx, step, op)
		})
		transaction("refused-unreadable", func(ctx context.Context, step *writeCorpusStep, op func(string)) error {
			return apply([]model.EditOp{
				{Op: "replace", Find: edited, With: "<div>x</div>"},
			}, nil, true)(ctx, step, op)
		})
		transaction("delete-block", func(ctx context.Context, step *writeCorpusStep, op func(string)) error {
			block := blockHolding(t, edited)
			return apply([]model.EditOp{{Op: "delete", Block: block.ID}}, nil, false)(ctx, step, op)
		})
		transaction("delete-found", func(ctx context.Context, step *writeCorpusStep, op func(string)) error {
			return apply([]model.EditOp{{Op: "delete", Find: qualified + " Twice."}}, nil, false)(ctx, step, op)
		})

		_, blocks, err := service.TextWithBlocks(ctx, artifactID)
		if err != nil {
			t.Fatalf("%s: read the document's blocks: %v", mode, err)
		}
		pass.Blocks = make([]writeCorpusBlock, len(blocks))
		for index, block := range blocks {
			pass.Blocks[index] = writeCorpusBlock{ID: block.ID, Type: block.Type, From: block.From, To: block.To, Token: block.Token}
		}
		line.Passes = append(line.Passes, pass)
		// The timing is only logged, never dumped: it differs on every run and would break cmp.
		t.Logf("%s %s (%d bytes, %d blocks): %s; total %s", specName, mode, len(markdown), len(blocks),
			strings.Join(timings, ", "), time.Since(started).Round(time.Millisecond))
	}
	return line
}

func TestWriteCorpus(t *testing.T) {
	dir := os.Getenv("DISPATCH_TOKEN_CORPUS")
	if dir == "" {
		t.Skip("DISPATCH_TOKEN_CORPUS unset; set it to a directory of markdown specs to dump the writes on them")
	}
	outPath := os.Getenv("DISPATCH_WRITE_CORPUS_OUT")
	if outPath == "" {
		t.Fatal("DISPATCH_WRITE_CORPUS_OUT must name the file the dump is written to")
	}
	names, err := tokenCorpusSpecs(dir)
	if err != nil {
		t.Fatalf("DISPATCH_TOKEN_CORPUS: %v", err)
	}
	out, err := os.Create(outPath)
	if err != nil {
		t.Fatalf("DISPATCH_WRITE_CORPUS_OUT: %v", err)
	}
	defer out.Close()
	writer := bufio.NewWriter(out)
	for _, name := range names {
		source, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		var line writeCorpusLine
		if !t.Run(name, func(t *testing.T) {
			line = runWriteCorpus(t, newWriteCorpusService(t), name, string(source), nil)
		}) {
			t.FailNow()
		}
		encoded, err := json.Marshal(line)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		writer.Write(encoded)
		writer.WriteByte('\n')
	}
	if err := writer.Flush(); err != nil {
		t.Fatalf("DISPATCH_WRITE_CORPUS_OUT: %v", err)
	}
	if err := out.Close(); err != nil {
		t.Fatalf("DISPATCH_WRITE_CORPUS_OUT: %v", err)
	}
}
