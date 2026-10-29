package docs

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// anchoredWriteSections is how many numbered sections the benchmark document holds, each with one
// anchor slot and one free remark.
const anchoredWriteSections = 80

// anchoredWriteSection is the section every edit and upload rewrites, in the middle of the
// document so its anchors lie on both sides of it.
const anchoredWriteSection = 40

var anchoredWriteActor = model.Actor{Kind: "user", ID: "alice"}

// BenchmarkAnchoredWrite measures one write to a 150 KB document carrying N open anchored
// comments, for the three writes every open anchor is refreshed after: an edit through the edit
// route, a new anchored comment, and an upload of the whole document, which also re-anchors every
// row. A write whose cost grows with N walks the document once per anchor.
func BenchmarkAnchoredWrite(b *testing.B) {
	markdown := anchoredWriteMarkdown()
	for _, op := range []struct {
		name  string
		write func(ctx context.Context, fixture *anchoredWriteFixture, iteration int) error
	}{
		{"doc-edit", func(ctx context.Context, fixture *anchoredWriteFixture, _ int) error {
			_, err := fixture.service.ApplyOps(ctx, fixture.artifactID, []model.EditOp{{
				Op: "replace", Find: freeRemark(anchoredWriteSection), With: "Free remark was edited.",
			}}, anchoredWriteActor, nil)
			return err
		}},
		{"anchored-comment", func(ctx context.Context, fixture *anchoredWriteFixture, iteration int) error {
			_, err := fixture.service.MarkQuote(ctx, fixture.artifactID, MarkSpec{
				Kind: MarkComment, ID: fmt.Sprintf("bench-comment-%d", iteration), By: anchoredWriteActor,
			}, freeRemark(iteration%anchoredWriteSections), nil)
			return err
		}},
		{"upload", func(ctx context.Context, fixture *anchoredWriteFixture, iteration int) error {
			_, err := fixture.service.ReplaceText(ctx, fixture.artifactID, fixture.uploads[iteration%len(fixture.uploads)], anchoredWriteActor)
			return err
		}},
	} {
		for _, anchors := range []int{10, 40, 80} {
			b.Run(fmt.Sprintf("%s/%d", op.name, anchors), func(b *testing.B) {
				fixture := newAnchoredWriteFixture(b, markdown, anchors)
				fixture.assertRefreshesNothing(b, op.write)
				b.ReportAllocs()
				b.ResetTimer()
				for iteration := range b.N {
					b.StopTimer()
					// Each write rolls back: a committed one would leave the next iteration a
					// document and an event log that grew, and every anchor refresh the write
					// costs runs inside applyLive, before the commit, so the timer still covers it.
					tx, err := fixture.service.store.Pool.Begin(context.Background())
					if err != nil {
						b.Fatal(err)
					}
					joined, ledger := fixture.service.Join(context.Background(), tx)
					b.StartTimer()
					err = op.write(joined, fixture, iteration)
					b.StopTimer()
					_ = tx.Rollback(context.Background())
					ledger.Discard()
					if err != nil {
						b.Fatalf("%s write %d: %v", op.name, iteration, err)
					}
					b.StartTimer()
				}
			})
		}
	}
}

func freeRemark(section int) string {
	return fmt.Sprintf("Free remark %03d is unanchored.", section)
}

func anchoredClaim(section int) string {
	return fmt.Sprintf("Anchored claim %03d covers the review", section)
}

// anchoredWriteMarkdown is a deterministic document of about 150 KB in numbered sections, each
// with a heading, inline-marked paragraphs, a list, a table and a Go code block. Each section's
// anchor slot moves between its last paragraph, a list item and a table cell, so the anchors
// cover each kind of textblock, and its free remark stands in a paragraph no anchor covers, so a
// write there changes no anchor's quote.
func anchoredWriteMarkdown() string {
	const filler = "The reviewers read this part against the previous draft and the logs from the " +
		"staging run, and they agreed that the behaviour matches what the design describes for the " +
		"common path, though the rare path still needs a second look before anyone relies on it. "
	var doc strings.Builder
	for section := range anchoredWriteSections {
		claim := anchoredClaim(section)
		listItem, tableCell, closing := "Third point notes the open questions.", "pending", "Closing note for this section."
		switch section % 3 {
		case 0:
			closing = "Closing note: " + claim + " for this section."
		case 1:
			listItem = claim
		case 2:
			tableCell = claim
		}
		fmt.Fprintf(&doc, "## Section %03d: review notes\n\n", section)
		fmt.Fprintf(&doc, "Section %03d sets out **the main finding**, with *a caveat* about `config.Value%03d` and "+
			"a [reference](https://example.com/spec/%03d). %s%s\n\n", section, section, section, filler, filler)
		fmt.Fprintf(&doc, "%s%s %s\n\n", filler, freeRemark(section), filler)
		fmt.Fprintf(&doc, "- First point explains the **ordering** rule of part %03d.\n"+
			"- Second point covers *retries* and `timeouts`.\n- %s\n\n", section, listItem)
		fmt.Fprintf(&doc, "| Field | Meaning |\n| --- | --- |\n| `id_%03d` | the row's identity |\n| status | %s |\n\n", section, tableCell)
		fmt.Fprintf(&doc, "```go\nfunc section%03d(ctx context.Context) error {\n\tfor i := range %d {\n"+
			"\t\tif err := step(ctx, i); err != nil {\n\t\t\treturn fmt.Errorf(\"step %%d: %%w\", i, err)\n"+
			"\t\t}\n\t}\n\treturn nil\n}\n```\n\n", section, section+1)
		fmt.Fprintf(&doc, "%s%s\n\n", filler, closing)
	}
	return doc.String()
}

type anchoredWriteFixture struct {
	service    *Service
	artifactID string
	anchors    int
	// uploads are the document's own text with the middle section's free remark changed, two
	// ways, so consecutive uploads never send the text the document already holds.
	uploads []string
}

// newAnchoredWriteFixture stores the document in a fresh database, as artifact creation does (the
// seed and version 1 in one transaction), and anchors comments over anchors of its slots, spread
// evenly, each written as the comment route writes one: MarkQuote, then the row carrying the
// anchor MarkQuote returned, in one committed transaction.
func newAnchoredWriteFixture(b *testing.B, markdown string, anchors int) *anchoredWriteFixture {
	b.Helper()
	database := storetest.Open(b)
	service := New(Deps{Store: database, Events: events.NewBroker(), ServerURL: "https://dispatch.example", Settle: time.Hour})
	b.Cleanup(func() {
		if err := service.Shutdown(context.Background()); err != nil {
			b.Errorf("shutdown anchored write benchmark service: %v", err)
		}
	})
	fixture := &anchoredWriteFixture{service: service, anchors: anchors}
	fixture.commit(b, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `insert into projects (key, name) values ('BENCH', 'Benchmark')`); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			insert into issues (key, project_key, number, title, created_by, rank)
			values ('BENCH-1', 'BENCH', 1, 'Benchmark', '{"kind":"user","id":"alice"}', 'U')
		`); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `
			insert into artifacts (issue_key, project_key, slug, name, kind, is_primary, created_by)
			values ('BENCH-1', 'BENCH', 'benchmark', 'benchmark.md', 'doc', false, '{"kind":"user","id":"alice"}')
			returning id::text
		`).Scan(&fixture.artifactID); err != nil {
			return err
		}
		canonical, err := service.SeedText(ctx, fixture.artifactID, markdown, anchoredWriteActor)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `
			insert into artifact_versions (artifact_id, number, markdown, authors)
			values ($1, 1, $2, '[{"kind":"user","id":"alice"}]')
		`, fixture.artifactID, canonical)
		return err
	})
	for index := range anchors {
		slot := index * anchoredWriteSections / anchors
		id := fmt.Sprintf("00000000-0000-4000-8000-%012d", index+1)
		fixture.commit(b, func(ctx context.Context, tx pgx.Tx) error {
			anchored, err := service.MarkQuote(ctx, fixture.artifactID, MarkSpec{Kind: MarkComment, ID: id, By: anchoredWriteActor}, anchoredClaim(slot), nil)
			if err != nil {
				return err
			}
			if anchored.Quote != anchoredClaim(slot) {
				return fmt.Errorf("comment %d anchored %q, want slot %d's claim", index, anchored.Quote, slot)
			}
			anchor := model.Anchor{ArtifactID: fixture.artifactID, MarkID: id, Version: 1, Quote: anchored.Quote}
			if anchored.BlockID != "" {
				anchor.BlockID = &anchored.BlockID
			}
			encoded, err := json.Marshal(anchor)
			if err != nil {
				return err
			}
			// The comment's owner is the document's issue: a row names one owner (comments_one_owner).
			_, err = tx.Exec(ctx, `
				insert into comments (id, issue_key, author, body, anchor)
				values ($1, 'BENCH-1', '{"kind":"user","id":"alice"}', 'Anchored comment', $2)
			`, id, encoded)
			return err
		})
	}
	text, err := service.Text(context.Background(), fixture.artifactID)
	if err != nil {
		b.Fatal(err)
	}
	remark := freeRemark(anchoredWriteSection)
	if strings.Count(text, remark) != 1 {
		b.Fatalf("the document holds %q %d times, want once", remark, strings.Count(text, remark))
	}
	for _, variant := range []string{"Free remark was uploaded once.", "Free remark was uploaded twice."} {
		fixture.uploads = append(fixture.uploads, strings.Replace(text, remark, variant, 1))
	}
	return fixture
}

// commit runs write inside a transaction joined with Service.Join and commits it.
func (f *anchoredWriteFixture) commit(b *testing.B, write func(ctx context.Context, tx pgx.Tx) error) {
	b.Helper()
	ctx := context.Background()
	tx, err := f.service.store.Pool.Begin(ctx)
	if err != nil {
		b.Fatal(err)
	}
	defer tx.Rollback(ctx)
	joined, ledger := f.service.Join(ctx, tx)
	defer ledger.Discard()
	if err := write(joined, tx); err != nil {
		b.Fatal(err)
	}
	if err := ledger.Commit(ctx); err != nil {
		b.Fatal(err)
	}
}

// assertRefreshesNothing runs write once, rolled back, and fails unless it leaves exactly the
// fixture's open rows with their ids and anchors as they were and appends no event: a fixture whose
// stored anchors differ from what the document reads would time the refresh's row updates and
// events rather than its finding every anchor's mark.
func (f *anchoredWriteFixture) assertRefreshesNothing(b *testing.B, write func(context.Context, *anchoredWriteFixture, int) error) {
	b.Helper()
	ctx := context.Background()
	tx, err := f.service.store.Pool.Begin(ctx)
	if err != nil {
		b.Fatal(err)
	}
	defer tx.Rollback(ctx)
	joined, ledger := f.service.Join(ctx, tx)
	defer ledger.Discard()
	before, beforeEvents := f.openRows(b, tx)
	if len(before) != f.anchors {
		b.Fatalf("the fixture holds %d open rows, want %d", len(before), f.anchors)
	}
	if err := write(joined, f, 0); err != nil {
		b.Fatal(err)
	}
	after, afterEvents := f.openRows(b, tx)
	if !maps.Equal(before, after) || afterEvents != beforeEvents {
		b.Fatalf("the write changed the open rows from %d to %d (equal: %t) and appended %d events, want none changed and no event",
			len(before), len(after), maps.Equal(before, after), afterEvents-beforeEvents)
	}
}

// openRows is every open anchored comment's anchor by row id, and how many events the log holds.
func (f *anchoredWriteFixture) openRows(b *testing.B, tx pgx.Tx) (map[string]string, int) {
	b.Helper()
	ctx := context.Background()
	rows, err := tx.Query(ctx, `
		select id::text, anchor::text from comments
		where anchor->>'artifact_id' = $1 and not resolved
	`, f.artifactID)
	if err != nil {
		b.Fatal(err)
	}
	defer rows.Close()
	anchors := map[string]string{}
	for rows.Next() {
		var id, anchor string
		if err := rows.Scan(&id, &anchor); err != nil {
			b.Fatal(err)
		}
		anchors[id] = anchor
	}
	if err := rows.Err(); err != nil {
		b.Fatal(err)
	}
	var count int
	if err := tx.QueryRow(ctx, `select count(*) from events`).Scan(&count); err != nil {
		b.Fatal(err)
	}
	return anchors, count
}
