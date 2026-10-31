package main

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/text"
)

// writeDocuments writes every document with its versions and indexes the latest version of each,
// the only version a block index records (refs.IndexBlocks) and the only one search, the
// related-item check and every anchor read look at.
func (r *seedRun) writeDocuments(ctx context.Context) error {
	return r.inBatches(ctx, "documents", len(r.plan.documents), batchDocuments,
		func(ctx context.Context, tx pgx.Tx, from, to int) error {
			for index := from; index < to; index++ {
				if err := r.writeDocument(ctx, tx, index); err != nil {
					return fmt.Errorf("document %s: %w", r.plan.documents[index].RefKey, err)
				}
			}
			return nil
		})
}

// writeDocument writes one document, its versions and the index of its latest version. The
// latest version's markdown is the markdown the renderer wrote (RenderWithBlockOffsets), not the
// markdown the renderer was given, because refs.IndexBlocks indexes `markdown[block.From:block.To]`
// and those offsets are into that render: a row holding anything else would index the wrong
// bytes.
func (r *seedRun) writeDocument(ctx context.Context, tx pgx.Tx, index int) error {
	document := r.plan.documents[index]
	var issueKey *string
	if document.Issue >= 0 {
		issueKey = &r.plan.issues[document.Issue].Key
	}
	projectKey := r.plan.projects[document.Project].Key
	created := timeAt(index, len(r.plan.documents))
	if _, err := tx.Exec(ctx, `
		insert into artifacts (id, issue_key, project_key, slug, name, kind, is_primary, created_by, created_at, last_seq)
		values ($1, $2, $3, $4, $5, 'doc', $6, $7::jsonb, $8, 0)
	`, document.ID, issueKey, projectKey, document.Slug,
		r.plan.vocabulary.phrase(r.random, 4), document.Primary, seedActor, created); err != nil {
		return fmt.Errorf("artifact row: %w", err)
	}

	var latest string
	var blocks []model.ArtifactBlock
	rows := make([][]any, 0, document.Versions)
	for number := 1; number <= document.Versions; number++ {
		bytes, band := r.versionBytes(document, number)
		r.bands[band]++
		r.startDocument()
		markdown := r.plan.documentMarkdown(index, number, r.random, bytes)
		if number == document.Versions {
			rendered, offsets, tableRow, err := renderVersion(markdown)
			if err != nil {
				return err
			}
			if r.tableRow != "" && r.tableRow != tableRow {
				return fmt.Errorf("the table-row block id moved from %s to %s: every latest version is laid out alike, so an anchor written for one names the same row in each",
					r.tableRow, tableRow)
			}
			r.tableRow = tableRow
			latest, blocks = rendered, offsets
			markdown = rendered
		}
		rows = append(rows, []any{
			uuidFor("artifact_version", index*versionsPerManyVersionDocument+number),
			document.ID, number, markdown, "[]", number%7 == 0,
			created.Add(time.Duration(number-1) * versionStep),
		})
	}
	if _, err := tx.CopyFrom(ctx, pgx.Identifier{"artifact_versions"},
		[]string{"id", "artifact_id", "number", "markdown", "authors", "named", "created_at"},
		pgx.CopyFromRows(rows)); err != nil {
		return fmt.Errorf("version rows: %w", err)
	}

	versionAt := created.Add(time.Duration(document.Versions-1) * versionStep)
	located := text.ExtractAt(latest, serverURL)
	moved, err := refs.IndexBlocks(ctx, tx, document.ID, document.Versions, versionAt, latest, blocks, located)
	if err != nil {
		return fmt.Errorf("index blocks: %w", err)
	}
	// ReplaceDocumentCounted stores each mention's excerpt at this version number, which is the
	// document's highest: refs.MarkStaleExcerpts re-owes a mention whose excerpt_version is
	// below max(artifact_versions.number), so a seed that wrote 1 would leave every one of the
	// 28-version documents owed (the plan's answer 7).
	if _, err := refs.ReplaceDocumentCounted(ctx, tx, document.ID, document.Versions, versionAt,
		text.Refs(located), moved, refs.MentionExcerpts(latest, blocks, located)); err != nil {
		return fmt.Errorf("index references: %w", err)
	}
	if document.Primary {
		if err := docs.RecordTaskProgressMarkdown(ctx, tx, document.ID, latest); err != nil {
			return fmt.Errorf("record task progress: %w", err)
		}
	}
	r.blockTotal += len(blocks)
	for _, block := range blocks {
		if block.Type == "table" {
			r.tableTotal++
		}
	}
	return nil
}

// versionStep is how far apart two versions of one document are written.
const versionStep = 36 * time.Hour

// renderVersion parses the generated markdown and renders it back with its blocks' offsets, the
// pair a version write stores and indexes together (docs' indexDocument). It also answers the
// block id of the first table's first body row, which is what an anchor on a table row names.
//
// Only a block the renderer writes through its block path carries an offset, so a table is one
// block and its rows and its cells are none, although pmdoc stamps an id on each of them
// (EnsureBlockIDsCount). That is why a table-row anchor names a block id that `document_blocks`
// holds no row for, exactly as a production anchor on a table row does.
//
// It refuses a render whose shape is not the one the seed lays out, so a change in pmdoc that
// moved a block shows up here rather than as a wrong anchor in the data set.
func renderVersion(markdown string) (string, []model.ArtifactBlock, string, error) {
	tree, err := pmdoc.Parse(markdown)
	if err != nil {
		return "", nil, "", fmt.Errorf("parse the generated markdown: %w", err)
	}
	rendered, offsets, err := pmdoc.RenderWithBlockOffsets(tree)
	if err != nil {
		return "", nil, "", fmt.Errorf("render the generated markdown: %w", err)
	}
	if len(offsets) != blocksPerLatestVersion {
		return "", nil, "", fmt.Errorf("the latest version rendered %d blocks, not the %d the seed lays out",
			len(offsets), blocksPerLatestVersion)
	}
	blocks := make([]model.ArtifactBlock, len(offsets))
	tables := 0
	firstTable := ""
	for index, offset := range offsets {
		blocks[index] = model.ArtifactBlock{ID: offset.ID, Type: offset.Type, From: offset.From, To: offset.To}
		if offset.Type == "table" {
			tables++
			if firstTable == "" {
				firstTable = offset.ID
			}
		}
	}
	if blocks[1].ID != firstParagraphBlock || blocks[1].Type != "paragraph" || tables != tablesPerVersion {
		return "", nil, "", fmt.Errorf("the latest version's blocks are not laid out as the seed's mentions expect: block 2 is %s of type %s, and it holds %d tables",
			blocks[1].ID, blocks[1].Type, tables)
	}
	rows, err := pmdoc.TableRows(tree)
	if err != nil {
		return "", nil, "", fmt.Errorf("read the table rows of the generated markdown: %w", err)
	}
	if len(rows[firstTable]) < 2 {
		return "", nil, "", fmt.Errorf("the first table of the latest version has no body row")
	}
	return rendered, blocks, rows[firstTable][1].ID, nil
}

// versionBytes is how much markdown version number of document holds, and the band the report
// counts it in. Only the latest version of a large document is large: every other version of it
// is an ordinary one. The other reading is about 4 GB of markdown, which does not fit the disk
// the measurement cluster runs on (the plan's Decision "The large documents are large only at
// their latest version").
func (r *seedRun) versionBytes(document documentPlan, number int) (int, string) {
	if document.Large && number == document.Versions {
		return largeBandMin + r.random.IntN(largeBandMax-largeBandMin), "100-660 KB"
	}
	switch draw := r.random.Float64(); {
	case draw < 0.5:
		return versionBandMin + r.random.IntN(4<<10), "1-5 KB"
	case draw < 0.8:
		return 5<<10 + r.random.IntN(5<<10), "5-10 KB"
	default:
		return 10<<10 + r.random.IntN(10<<10), "10-20 KB"
	}
}
