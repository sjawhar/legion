package store

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sjawhar/envoy/internal/dispatch/store/searchtest"
)

// A text whose whole search vector would pass Postgres's limit on one tsvector is written, and
// indexed by the words that open it, in every table search reads (LEGION-505). Before 0087 its
// write failed with `string is too long for tsvector`, so a document holding such text could
// never version, and an issue titled with it, or an ask, comment or message holding it, was never
// stored. An issue's key is indexed whole whatever its title holds.
func TestTextPastTheSearchVectorLimitIsWrittenAndIndexedFromItsOpening(t *testing.T) {
	ctx := context.Background()
	store := openEmptyTestStore(t)
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	seedSearchDocument(t, ctx, store)
	text := searchtest.DistinctWords(100_000, "\n")

	var pgErr *pgconn.PgError
	if _, err := store.Pool.Exec(ctx, `select to_tsvector('english', $1)`, text); !errors.As(err, &pgErr) || pgErr.Code != "54000" {
		t.Fatalf("to_tsvector of the text = %v, want 54000 string is too long for tsvector: the text must pass the limit", err)
	}

	for _, row := range []struct{ table, insert, vector string }{
		{"artifact_versions",
			`insert into artifact_versions (artifact_id, number, markdown, authors) select id, 1, $1, '[]' from artifacts`,
			`select search from artifact_versions where number = 1`},
		{"issues",
			`insert into issues (key, project_key, number, title, created_by, rank) values ('CORE-2', 'CORE', 2, $1, '{"kind":"user","id":"alice"}', 'B')`,
			`select search from issues where key = 'CORE-2'`},
		{"comments",
			`insert into comments (issue_key, author, body) values ('CORE-1', '{"kind":"user","id":"alice"}', $1)`,
			`select search from comments`},
		{"asks",
			`insert into asks (issue_key, author, question) values ('CORE-1', '{"kind":"user","id":"alice"}', $1)`,
			`select search from asks`},
		{"messages",
			`insert into messages (issue_key, author, body) values ('CORE-1', '{"kind":"session","id":"s1"}', $1)`,
			`select search from messages`},
	} {
		if _, err := store.Pool.Exec(ctx, row.insert, text); err != nil {
			t.Errorf("%s: write text past the search vector limit: %v", row.table, err)
			continue
		}
		var opening bool
		if err := store.Pool.QueryRow(ctx, `select (`+row.vector+`) @@ to_tsquery('english', 'w000001')`).Scan(&opening); err != nil {
			t.Fatalf("%s: read the vector: %v", row.table, err)
		}
		if !opening {
			t.Errorf("%s: the vector does not hold w000001, the text's first word", row.table)
		}
	}

	var key bool
	if err := store.Pool.QueryRow(ctx, `select search @@ to_tsquery('english', 'core') from issues where key = 'CORE-2'`).Scan(&key); err != nil {
		t.Fatalf("read the issue's vector: %v", err)
	}
	if !key {
		t.Error("the issue's vector does not hold its key")
	}
}

// A text cut to fit is cut between two words. Postgres's parser reads what is left of a word cut
// short as a word of its own, so a cut at a character count inside `w050000` indexed a lexeme the
// text does not hold, and a search for it found the text. `intro ` moves every word off the halves'
// boundaries, which the fixed width of `w000001 w000002 …` alone would put between two words.
func TestATextCutToFitIsIndexedByWholeWords(t *testing.T) {
	ctx := context.Background()
	store := openEmptyTestStore(t)
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	var fragments []string
	var opening bool
	if err := store.Pool.QueryRow(ctx, `
		select coalesce(array_agg(lexeme) filter (where lexeme <> 'intro' and lexeme !~ '^w[0-9]{6}$'), '{}'),
		       bool_or(lexeme = 'w000001')
		  from unnest(tsvector_to_array(search_vector('', $1))) as lexeme`,
		"intro "+searchtest.DistinctWords(100_000, "\n")).Scan(&fragments, &opening); err != nil {
		t.Fatalf("index the text: %v", err)
	}
	if len(fragments) != 0 {
		t.Errorf("the vector holds %q, which are not words of the text", fragments)
	}
	if !opening {
		t.Error("the vector does not hold w000001, the text's first numbered word")
	}
}

// search_vector runs in any query, the duplicate check's included. Its exception block opens a
// subtransaction, which a parallel query refuses in its leader as in its workers ("cannot start
// subtransactions during a parallel operation"), so the function is parallel unsafe and no plan
// that calls it runs in parallel. debug_parallel_query has Postgres plan every query it can in
// parallel, so this read fails if the function is ever marked parallel safe or restricted.
func TestSearchVectorRunsInAQueryPostgresWouldRunInParallel(t *testing.T) {
	ctx := context.Background()
	store := openEmptyTestStore(t)
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	seedSearchDocument(t, ctx, store)
	tx, err := store.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, "set local debug_parallel_query = on"); err != nil {
		t.Fatalf("plan every query in parallel: %v", err)
	}
	var titles int
	if err := tx.QueryRow(ctx, `select count(*) from issues where search_vector('', search_text(title)) @@ to_tsquery('english', 'search')`).Scan(&titles); err != nil {
		t.Fatalf("read titles through search_vector under a parallel plan: %v", err)
	}
	if titles != 1 {
		t.Fatalf("%d titles hold 'search', want the seeded issue's", titles)
	}
}

// captureNotices sends slog's default logger, Debug included, to a buffer until t ends, and
// returns a function that lists the lines naming a Postgres notice at level.
func captureNotices(t *testing.T) (*bytes.Buffer, func(level string) []string) {
	t.Helper()
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return &logs, func(level string) []string {
		var lines []string
		for line := range strings.Lines(logs.String()) {
			if strings.Contains(line, "level="+level) && strings.Contains(line, "dispatch: postgres notice") {
				lines = append(lines, line)
			}
		}
		return lines
	}
}

// A write whose text search_vector cuts to fit logs one WARN line naming the cut - the text's
// length, how much of it the vector holds and the limit, never the text - with the context naming
// the trigger that cut it. A write that fits logs no notice. Before 0090 the cut was silent, and
// before store.Open logged notices Postgres's warning went nowhere.
func TestATruncatingWriteLogsOneWarning(t *testing.T) {
	ctx := context.Background()
	store := openEmptyTestStore(t)
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	seedSearchDocument(t, ctx, store)
	logs, notices := captureNotices(t)

	text := searchtest.DistinctWords(100_000, "\n")
	if _, err := store.Pool.Exec(ctx, `insert into artifact_versions (artifact_id, number, markdown, authors) select id, 1, $1, '[]' from artifacts`, text); err != nil {
		t.Fatalf("write text past the search vector limit: %v", err)
	}
	warnings := notices("WARN")
	if len(warnings) != 1 {
		t.Fatalf("the truncating write logged %d WARN notices, want 1:\n%s", len(warnings), logs.String())
	}
	warning := warnings[0]
	for _, want := range []string{"severity=WARNING", "a text of 800000 characters to its first", "1048575-byte limit", "artifact_versions_search()"} {
		if !strings.Contains(warning, want) {
			t.Errorf("the warning %q lacks %q", warning, want)
		}
	}
	if strings.Contains(warning, "w000001") {
		t.Errorf("the warning %q holds the text", warning)
	}

	logs.Reset()
	if _, err := store.Pool.Exec(ctx, `insert into artifact_versions (artifact_id, number, markdown, authors) select id, 2, 'Calibrate the astrolabes.', '[]' from artifacts`); err != nil {
		t.Fatalf("write text that fits: %v", err)
	}
	if logged := logs.String(); logged != "" {
		t.Errorf("a write that fits logged:\n%s", logged)
	}
}

// Postgres's own text search raises NOTICEs on ordinary writes and searches: to_tsvector drops a
// word past 2,047 bytes with "word is too long to be indexed", and a long hash or base64 run in a
// comment is such a word. Those are logged at Debug, never at Info or above, so ordinary traffic
// cannot flood the log; and the notice holds no text of the comment.
func TestAWordTooLongToIndexLogsAtDebug(t *testing.T) {
	ctx := context.Background()
	store := openEmptyTestStore(t)
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	seedSearchDocument(t, ctx, store)
	logs, notices := captureNotices(t)

	body := "Calibrate the astrolabes " + strings.Repeat("f", 3000) + " before launch."
	if _, err := store.Pool.Exec(ctx, `insert into comments (issue_key, author, body) values ('CORE-1', '{"kind":"user","id":"alice"}', $1)`, body); err != nil {
		t.Fatalf("write a comment holding a word past 2,047 bytes: %v", err)
	}
	debug := notices("DEBUG")
	if len(debug) != 1 || !strings.Contains(debug[0], "word is too long to be indexed") {
		t.Fatalf("the comment logged %d Debug notices, want 1 naming the dropped word:\n%s", len(debug), logs.String())
	}
	if strings.Contains(debug[0], "fff") {
		t.Errorf("the notice %q holds the comment's text", debug[0])
	}
	for _, level := range []string{"INFO", "WARN", "ERROR"} {
		if lines := notices(level); len(lines) != 0 {
			t.Errorf("the comment logged %d notices at %s, want none:\n%s", len(lines), level, logs.String())
		}
	}
}
