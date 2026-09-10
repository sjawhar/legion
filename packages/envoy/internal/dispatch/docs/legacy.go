package docs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"
	"unicode/utf16"

	"github.com/jackc/pgx/v5"
	"github.com/reearth/ygo/crdt"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

const legacyMigrationVersion = 8

// LegacyReport describes one document's migration readiness without modifying it.
type LegacyReport struct {
	ArtifactID string
	IssueKey   string
	Name       string
	State      string
	ParseError error
	Anchors    int
	Resolvable int
}

type legacyDocument struct {
	id       string
	issueKey string
	name     string
}

type legacyAnchor struct {
	Version  int    `json:"version"`
	Quote    string `json:"quote"`
	From     int    `json:"from"`
	To       int    `json:"to"`
	Orphaned bool   `json:"orphaned"`
}

type legacyAnchorRow struct {
	table      string
	id         string
	author     model.Actor
	anchor     legacyAnchor
	body       string
	resolved   bool
	suggestion *model.Suggestion
	createdAt  time.Time
	replyTo    *string
}

type migratedAnchor struct {
	row    legacyAnchorRow
	anchor model.Anchor
}

// InspectLegacyDocuments reports whether every document is already a Proof tree
// and whether legacy markdown and anchors can be resolved without writing data.
func InspectLegacyDocuments(ctx context.Context, database *store.Store) ([]LegacyReport, error) {
	documents, err := legacyDocuments(ctx, database)
	if err != nil {
		return nil, err
	}

	reports := make([]LegacyReport, 0, len(documents))
	for _, document := range documents {
		report, err := inspectLegacyDocument(ctx, database, document)
		if err != nil {
			return nil, err
		}
		reports = append(reports, report)
	}
	return reports, nil
}

func inspectLegacyDocument(ctx context.Context, database *store.Store, document legacyDocument) (LegacyReport, error) {
	report := LegacyReport{ArtifactID: document.id, IssueKey: document.issueKey, Name: document.name}
	state, tree, _, parseErr, err := loadLegacyDocument(ctx, database, document.id)
	if err != nil {
		return LegacyReport{}, fmt.Errorf("inspect document %s: %w", document.id, err)
	}
	report.State = state
	report.ParseError = parseErr

	anchors, err := legacyAnchorRows(ctx, database.Pool, document.id, false)
	if err != nil {
		return LegacyReport{}, fmt.Errorf("inspect document %s anchors: %w", document.id, err)
	}
	report.Anchors = len(anchors)
	if parseErr != nil {
		return report, nil
	}
	for _, anchor := range anchors {
		if _, err := pmdoc.FindQuote(tree, anchor.anchor.Quote, nil, nil); err == nil {
			report.Resolvable++
		}
	}
	return report, nil
}

// MigrateLegacyDocuments converts legacy Y.Text rooms and offset anchors once.
// It records its migration marker only after every document conversion commits.
func MigrateLegacyDocuments(ctx context.Context, database *store.Store) error {
	applied, err := legacyMigrationApplied(ctx, database)
	if err != nil {
		return err
	}
	if applied {
		return nil
	}

	documents, err := legacyDocuments(ctx, database)
	if err != nil {
		return err
	}
	versioned := NewPgVersioned(database)
	for _, document := range documents {
		if err := migrateLegacyDocument(ctx, database, versioned, document); err != nil {
			return err
		}
	}
	if err := recordLegacyMigration(ctx, database); err != nil {
		return err
	}
	return nil
}

func migrateLegacyDocument(ctx context.Context, database *store.Store, versioned *PgVersioned, document legacyDocument) error {
	state, tree, markdown, parseErr, err := loadLegacyDocument(ctx, database, document.id)
	if err != nil {
		return fmt.Errorf("migrate document %s (%s/%s): %w", document.id, document.issueKey, document.name, err)
	}
	if state == "tree" {
		return nil
	}
	if parseErr != nil {
		return fmt.Errorf("migrate document %s (%s/%s): %w", document.id, document.issueKey, document.name, parseErr)
	}

	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("migrate document %s (%s/%s): begin transaction: %w", document.id, document.issueKey, document.name, err)
	}
	defer tx.Rollback(ctx)
	if err := lockLegacyDocument(ctx, tx, document); err != nil {
		return err
	}
	rows, err := legacyAnchorRows(ctx, tx, document.id, true)
	if err != nil {
		return fmt.Errorf("migrate document %s (%s/%s): list legacy anchors: %w", document.id, document.issueKey, document.name, err)
	}
	projections, err := legacyCommentProjections(ctx, tx, rows)
	if err != nil {
		return fmt.Errorf("migrate document %s (%s/%s): project legacy comments: %w", document.id, document.issueKey, document.name, err)
	}

	converted := make([]migratedAnchor, len(rows))
	for index, row := range rows {
		converted[index] = migratedAnchor{
			row: row,
			anchor: model.Anchor{
				ArtifactID: document.id,
				MarkID:     row.id,
				Version:    row.anchor.Version,
				Quote:      row.anchor.Quote,
				Orphaned:   row.anchor.Orphaned,
			},
		}
	}

	fresh := crdt.New()
	fragment := fresh.GetXmlFragment(fragmentName)
	marks := fresh.GetMap(marksMapName)
	if err := fresh.TransactE(func(transaction *crdt.Transaction) error {
		if err := pmdoc.Update(transaction, fragment, tree); err != nil {
			return fmt.Errorf("write Proof tree: %w", err)
		}
		for index := range converted {
			item := &converted[index]
			if !item.anchor.Orphaned {
				near, err := legacyNear(tree, markdown, item.anchor.Quote, item.row.anchor.From)
				if err != nil {
					if legacyTargetError(err) {
						item.anchor.Orphaned = true
						continue
					}
					return fmt.Errorf("position legacy anchor %s: %w", item.row.id, err)
				}
				if _, err := markQuoteInTxn(transaction, fragment, tree, item.anchor.Quote, nil, near, MarkSpec{
					Kind: legacyMarkKind(item.row),
					ID:   item.row.id,
					By:   item.row.author,
				}); err != nil {
					if legacyTargetError(err) {
						item.anchor.Orphaned = true
					} else {
						return fmt.Errorf("mark legacy anchor %s: %w", item.row.id, err)
					}
				}
			}
		}
		for id, projection := range projections {
			marks.Set(transaction, id, projection)
		}
		return nil
	}); err != nil {
		return fmt.Errorf("migrate document %s (%s/%s): %w", document.id, document.issueKey, document.name, err)
	}

	for _, item := range converted {
		encoded, err := json.Marshal(item.anchor)
		if err != nil {
			return fmt.Errorf("migrate document %s (%s/%s): encode %s anchor: %w", document.id, document.issueKey, document.name, item.row.table, err)
		}
		if err := updateLegacyAnchor(ctx, tx, item.row.table, item.row.id, encoded); err != nil {
			return fmt.Errorf("migrate document %s (%s/%s): %w", document.id, document.issueKey, document.name, err)
		}
	}
	for _, table := range []string{"doc_updates", "doc_checkpoints", "doc_snapshots"} {
		if _, err := tx.Exec(ctx, "delete from "+table+" where artifact_id = $1", document.id); err != nil {
			return fmt.Errorf("migrate document %s (%s/%s): clear %s: %w", document.id, document.issueKey, document.name, table, err)
		}
	}
	if _, err := versioned.AppendUpdateTx(ctx, tx, document.id, crdt.EncodeStateAsUpdateV1(fresh, nil)); err != nil {
		return fmt.Errorf("migrate document %s (%s/%s): append Proof document: %w", document.id, document.issueKey, document.name, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("migrate document %s (%s/%s): commit: %w", document.id, document.issueKey, document.name, err)
	}
	return nil
}

func legacyDocuments(ctx context.Context, database *store.Store) ([]legacyDocument, error) {
	rows, err := database.Pool.Query(ctx, `
		select a.id::text, a.issue_key, a.name
		from artifacts a
		where a.kind = 'doc'
		order by a.created_at, a.id
	`)
	if err != nil {
		return nil, fmt.Errorf("list legacy documents: %w", err)
	}
	defer rows.Close()

	var documents []legacyDocument
	for rows.Next() {
		var document legacyDocument
		if err := rows.Scan(&document.id, &document.issueKey, &document.name); err != nil {
			return nil, fmt.Errorf("scan legacy document: %w", err)
		}
		documents = append(documents, document)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list legacy documents: %w", err)
	}
	return documents, nil
}

func loadLegacyDocument(ctx context.Context, database *store.Store, artifactID string) (state string, tree *pmdoc.Node, markdown string, parseErr, err error) {
	loaded, err := NewPgVersioned(database).Load(ctx, artifactID)
	if err != nil {
		return "", nil, "", nil, fmt.Errorf("load persisted document: %w", err)
	}
	doc := crdt.New()
	if len(loaded.Update) > 0 {
		if err := crdt.ApplyUpdateV1(doc, loaded.Update, nil); err != nil {
			return "", nil, "", nil, fmt.Errorf("decode persisted document: %w", err)
		}
	}
	if doc.GetXmlFragment(fragmentName).Len() > 0 {
		tree, err := pmdoc.Read(doc.GetXmlFragment(fragmentName))
		if err != nil {
			return "", nil, "", nil, fmt.Errorf("read Proof document: %w", err)
		}
		return "tree", tree, "", nil, nil
	}
	markdown = doc.GetText("content").ToString()
	tree, parseErr = pmdoc.Parse(markdown)
	return "legacy", tree, markdown, parseErr, nil
}

func lockLegacyDocument(ctx context.Context, tx pgx.Tx, document legacyDocument) error {
	var issueKey string
	if err := tx.QueryRow(ctx, `select key from issues where key = $1 for update`, document.issueKey).Scan(&issueKey); err != nil {
		return fmt.Errorf("migrate document %s (%s/%s): lock issue: %w", document.id, document.issueKey, document.name, err)
	}
	var artifactID string
	if err := tx.QueryRow(ctx, `select id::text from artifacts where id = $1 for update`, document.id).Scan(&artifactID); err != nil {
		return fmt.Errorf("migrate document %s (%s/%s): lock artifact: %w", document.id, document.issueKey, document.name, err)
	}
	return nil
}

func legacyAnchorRows(ctx context.Context, q interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}, artifactID string, onlyUnmigrated bool) ([]legacyAnchorRow, error) {
	predicate := ""
	if onlyUnmigrated {
		predicate = " and not (anchor ? 'mark_id')"
	}
	asks, err := q.Query(ctx, `
		select id::text, author, anchor
		from asks
		where anchor is not null and anchor->>'artifact_id' = $1`+predicate, artifactID)
	if err != nil {
		return nil, fmt.Errorf("list ask anchors: %w", err)
	}
	defer asks.Close()

	rows := make([]legacyAnchorRow, 0)
	for asks.Next() {
		var row legacyAnchorRow
		var author, anchor []byte
		if err := asks.Scan(&row.id, &author, &anchor); err != nil {
			return nil, fmt.Errorf("scan ask anchor: %w", err)
		}
		row.table = "asks"
		if err := decodeLegacyAnchor(&row, author, anchor); err != nil {
			return nil, fmt.Errorf("decode ask anchor: %w", err)
		}
		rows = append(rows, row)
	}
	if err := asks.Err(); err != nil {
		return nil, fmt.Errorf("list ask anchors: %w", err)
	}

	comments, err := q.Query(ctx, `
		select id::text, author, body, anchor, resolved, suggestion, created_at, reply_to::text
		from comments
		where anchor is not null and anchor->>'artifact_id' = $1`+predicate, artifactID)
	if err != nil {
		return nil, fmt.Errorf("list comment anchors: %w", err)
	}
	defer comments.Close()
	for comments.Next() {
		var row legacyAnchorRow
		var author, anchor, suggestion []byte
		if err := comments.Scan(&row.id, &author, &row.body, &anchor, &row.resolved, &suggestion, &row.createdAt, &row.replyTo); err != nil {
			return nil, fmt.Errorf("scan comment anchor: %w", err)
		}
		row.table = "comments"
		if err := decodeLegacyAnchor(&row, author, anchor); err != nil {
			return nil, fmt.Errorf("decode comment anchor: %w", err)
		}
		if len(suggestion) > 0 {
			row.suggestion = &model.Suggestion{}
			if err := json.Unmarshal(suggestion, row.suggestion); err != nil {
				return nil, fmt.Errorf("decode comment suggestion: %w", err)
			}
		}
		rows = append(rows, row)
	}
	if err := comments.Err(); err != nil {
		return nil, fmt.Errorf("list comment anchors: %w", err)
	}
	return rows, nil
}

func decodeLegacyAnchor(row *legacyAnchorRow, author, anchor []byte) error {
	if err := json.Unmarshal(author, &row.author); err != nil {
		return fmt.Errorf("decode author: %w", err)
	}
	if err := json.Unmarshal(anchor, &row.anchor); err != nil {
		return fmt.Errorf("decode anchor: %w", err)
	}
	return nil
}

func legacyCommentProjections(ctx context.Context, tx pgx.Tx, rows []legacyAnchorRow) (map[string]map[string]any, error) {
	projections := make(map[string]map[string]any)
	for _, row := range rows {
		if row.table != "comments" || row.replyTo != nil {
			continue
		}
		replies, err := legacyReplies(ctx, tx, row.id)
		if err != nil {
			return nil, err
		}
		record := MarkRecord{
			Kind:      "comment",
			By:        ActorRef(row.author),
			CreatedAt: row.createdAt.UTC().Format(time.RFC3339Nano),
			Text:      row.body,
			Resolved:  row.resolved,
			Replies:   replies,
		}
		if row.suggestion != nil {
			record.Kind = "replace"
			record.Content = row.suggestion.ReplaceWith
			record.Status = "pending"
			if row.suggestion.Accepted != nil {
				if *row.suggestion.Accepted {
					record.Status = "accepted"
				} else {
					record.Status = "rejected"
				}
			}
		}
		plain, err := toPlain(record)
		if err != nil {
			return nil, fmt.Errorf("encode comment %s projection: %w", row.id, err)
		}
		projections[row.id] = plain
	}
	return projections, nil
}

func legacyReplies(ctx context.Context, tx pgx.Tx, rootID string) ([]MarkReply, error) {
	rows, err := tx.Query(ctx, `
		with recursive replies as (
			select id, author, body, created_at
			from comments where reply_to = $1
			union all
			select c.id, c.author, c.body, c.created_at
			from comments c join replies r on c.reply_to = r.id
		)
		select author, body, created_at
		from replies
		order by created_at, id
	`, rootID)
	if err != nil {
		return nil, fmt.Errorf("list replies for comment %s: %w", rootID, err)
	}
	defer rows.Close()

	var replies []MarkReply
	for rows.Next() {
		var author []byte
		var body string
		var createdAt time.Time
		if err := rows.Scan(&author, &body, &createdAt); err != nil {
			return nil, fmt.Errorf("scan reply for comment %s: %w", rootID, err)
		}
		var actor model.Actor
		if err := json.Unmarshal(author, &actor); err != nil {
			return nil, fmt.Errorf("decode reply author for comment %s: %w", rootID, err)
		}
		replies = append(replies, MarkReply{
			By:   ActorRef(actor),
			Text: body,
			At:   createdAt.UTC().Format(time.RFC3339Nano),
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list replies for comment %s: %w", rootID, err)
	}
	return replies, nil
}

func updateLegacyAnchor(ctx context.Context, tx pgx.Tx, table, id string, anchor []byte) error {
	switch table {
	case "asks", "comments":
		if _, err := tx.Exec(ctx, "update "+table+" set anchor = $2 where id = $1", id, anchor); err != nil {
			return fmt.Errorf("update %s anchor: %w", table, err)
		}
		return nil
	default:
		return fmt.Errorf("unsupported anchor table %q", table)
	}
}

func legacyMarkKind(row legacyAnchorRow) MarkKind {
	if row.table == "asks" {
		return MarkAsk
	}
	if row.suggestion != nil {
		return MarkSuggestion
	}
	return MarkComment
}

func legacyTargetError(err error) bool {
	if errors.Is(err, pmdoc.ErrTargetNotFound) {
		return true
	}
	var ambiguous *pmdoc.ErrTargetAmbiguous
	return errors.As(err, &ambiguous)
}

// legacyNear translates a legacy UTF-16 source offset into a Proof position
// through the tree parsed from that same source document.
func legacyNear(tree *pmdoc.Node, markdown, quote string, from int) (*int, error) {
	occurrence, ok := legacyQuoteOccurrence(markdown, quote, from)
	if !ok {
		return nil, nil
	}
	range_, err := pmdoc.FindQuote(tree, quote, &occurrence, nil)
	if err != nil {
		return nil, err
	}
	_, positions, err := pmdoc.Render(tree)
	if err != nil {
		return nil, err
	}
	markdownPosition, ok := positions.ToMd(range_.From)
	if !ok {
		return nil, fmt.Errorf("quote %q has no markdown position", quote)
	}
	near := positions.ToPM(markdownPosition)
	return &near, nil
}

func legacyQuoteOccurrence(markdown, quote string, from int) (int, bool) {
	haystack := utf16.Encode([]rune(markdown))
	needle := utf16.Encode([]rune(quote))
	if len(needle) == 0 || len(needle) > len(haystack) {
		return 0, false
	}
	occurrence := 0
	for index := 0; index+len(needle) <= len(haystack); index++ {
		if !sameUTF16(haystack[index:index+len(needle)], needle) {
			continue
		}
		if index == from {
			return occurrence, true
		}
		occurrence++
	}
	return 0, false
}

func sameUTF16(left, right []uint16) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func legacyMigrationApplied(ctx context.Context, database *store.Store) (bool, error) {
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("begin legacy migration check: %w", err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, "select pg_advisory_xact_lock($1)", int64(8150001)); err != nil {
		return false, fmt.Errorf("lock legacy migration check: %w", err)
	}
	var applied bool
	if err := tx.QueryRow(ctx, "select exists(select 1 from schema_migrations where version = $1)", legacyMigrationVersion).Scan(&applied); err != nil {
		return false, fmt.Errorf("check legacy migration marker: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("commit legacy migration check: %w", err)
	}
	return applied, nil
}

func recordLegacyMigration(ctx context.Context, database *store.Store) error {
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin record legacy migration: %w", err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, "select pg_advisory_xact_lock($1)", int64(8150001)); err != nil {
		return fmt.Errorf("lock record legacy migration: %w", err)
	}
	result, err := tx.Exec(ctx, "insert into schema_migrations (version) values ($1) on conflict do nothing", legacyMigrationVersion)
	if err != nil {
		return fmt.Errorf("record legacy migration marker: %w", err)
	}
	if result.RowsAffected() > 0 {
		slog.Info("dispatch: recorded legacy document migration", "version", legacyMigrationVersion)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit legacy migration marker: %w", err)
	}
	return nil
}
