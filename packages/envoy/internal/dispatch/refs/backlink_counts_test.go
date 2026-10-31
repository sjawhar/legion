package refs

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/explaintest"
)

// backlinkFixture is one graph that fills every arm of graph_edges: each of the view's 23 arms
// emits at least one edge over it, and every node kind nodeLoaders resolves has a node with an
// inbound edge. The three tests below read it, so a count, its plan and the view's own shape are
// all held to the same rows.
type backlinkFixture struct {
	database *store.Store
	// manyArms is the issue several arms of the view point at - a child issue, a blocking
	// issue, an attached document and a comment's mention - so that a count that lost an arm
	// differs from the view's rather than agreeing with it by having nothing to lose.
	manyArms string
	// heldOnce is the issue a comment cites from one current block and, redundantly, from its
	// own body: the block's edge is counted and the comment's own is not (heldByABlock), so the
	// issue has exactly one inbound edge from that comment.
	heldOnce string
	// heldTwice is the issue the same comment cites from two current blocks and from its own
	// body: each block's edge is counted, so the issue has two.
	heldTwice string
	// removedBlock is the issue the same comment cites only from a block a later revision
	// removed, and from its own body. The view's comment_block_refs arm drops the removed block
	// (position is not null) while heldByABlock's probe carries no position filter and still
	// suppresses the comment's own edge, so the link is in no count at all: it is the one row
	// that tells a count copying that asymmetry from one correcting it.
	removedBlock string
}

// newBacklinkFixture writes the fixture into a fresh database. The rows are written as SQL
// because the arms they fill are the view's reading of the schema, not of any writer this
// package has: a row written through a service would say what that service writes today, and the
// count is held to the view.
func newBacklinkFixture(t *testing.T) backlinkFixture {
	t.Helper()
	database := storetest.Open(t)
	ctx := context.Background()
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin fixture transaction: %v", err)
	}
	defer tx.Rollback(ctx)

	createReferenceProject(t, tx, "CORE")
	for number, issue := range []struct {
		key    string
		parent *string
	}{
		{key: "CORE-1"},
		{key: "CORE-2", parent: new("CORE-1")},
		{key: "CORE-3"},
		{key: "CORE-4"},
	} {
		execFixture(t, tx, "create "+issue.key, `
			insert into issues (key, project_key, number, title, parent_key, created_by, rank)
			values ($1, 'CORE', $2, 'Backlink fixture', $3, '{"kind":"user","id":"alice"}', 'U')
		`, issue.key, number+1, issue.parent)
	}
	// issues.parent_key: child_of. issue_links: blocked_by.
	execFixture(t, tx, "block CORE-1", `
		insert into issue_links (issue_key, kind, target_key) values ('CORE-3', 'blocked_by', 'CORE-1')
	`)

	// artifacts.issue_key: attached_to.
	specID, specRef := createReferenceArtifact(t, tx, "CORE-1", "CORE", "spec")
	_, notesRef := createReferenceArtifact(t, tx, "", "CORE", "notes")
	var notesID string
	if err := tx.QueryRow(ctx, `select id::text from artifacts where ref_key = $1`, notesRef).Scan(&notesID); err != nil {
		t.Fatalf("read the notes document's id: %v", err)
	}

	// asks and comments, anchored on the notes document and owned by it: anchored_to, owned_by.
	anchoredAsk := scanFixture(t, tx, "create the anchored ask", `
		insert into asks (issue_key, author, question, anchor)
		values ('CORE-1', '{"kind":"user","id":"alice"}', 'anchored?', jsonb_build_object('artifact_id', $1::text))
		returning id::text
	`, notesID)
	execFixture(t, tx, "create the owned ask", `
		insert into asks (artifact_id, author, question)
		values ($1::uuid, '{"kind":"user","id":"alice"}', 'owned?')
	`, notesID)
	execFixture(t, tx, "create the anchored comment", `
		insert into comments (issue_key, author, body, anchor)
		values ('CORE-1', '{"kind":"user","id":"alice"}', 'anchored', jsonb_build_object('artifact_id', $1::text))
	`, notesID)
	ownedComment := scanFixture(t, tx, "create the owned comment", `
		insert into comments (artifact_id, author, body)
		values ($1::uuid, '{"kind":"user","id":"alice"}', 'owned')
		returning id::text
	`, notesID)

	// comments.reply_to, comments.ask_id, messages.in_reply_to: replies_to.
	citingComment := scanFixture(t, tx, "create the reply comment", `
		insert into comments (issue_key, author, body, reply_to)
		values ('CORE-1', '{"kind":"user","id":"alice"}', 'a reply that cites', $1::uuid)
		returning id::text
	`, ownedComment)
	execFixture(t, tx, "create the ask reply", `
		insert into comments (issue_key, author, body, ask_id)
		values ('CORE-1', '{"kind":"user","id":"alice"}', 'an answer', $1::uuid)
	`, anchoredAsk)
	rootMessage := scanFixture(t, tx, "create the root message", `
		insert into messages (issue_key, author, body)
		values ('CORE-1', '{"kind":"user","id":"alice"}', 'root')
		returning id::text
	`)
	execFixture(t, tx, "create the message reply", `
		insert into messages (issue_key, author, body, in_reply_to)
		values ('CORE-1', '{"kind":"user","id":"alice"}', 'reply', $1::uuid)
	`, rootMessage)

	// ask_followers: followed_by.
	execFixture(t, tx, "follow the anchored ask", `
		insert into ask_followers (ask_id, session_id) values ($1::uuid, 'session-backlink')
	`, anchoredAsk)

	// components: part_of, depends_on; issue_component_members: affects.
	sourceID := scanFixture(t, tx, "create the architecture source", `
		insert into architecture_sources (project_key, repo, branch, installation_id, created_by)
		values ('CORE', 'sjawhar/core', 'main', 1, '{"kind":"user","id":"alice"}')
		returning id::text
	`)
	snapshotID := scanFixture(t, tx, "create the architecture snapshot", `
		insert into architecture_snapshots (project_key, commit, files) values ('CORE', 'abc123', '{}'::jsonb)
		returning id::text
	`)
	for _, component := range []struct{ id, parent string }{{id: "core"}, {id: "api", parent: "core"}} {
		execFixture(t, tx, "create component "+component.id, `
			insert into components (project_key, id, title, prose, parent, snapshot_id, source_id)
			values ('CORE', $1, $1, '', nullif($2, ''), $3::bigint, $4::bigint)
		`, component.id, component.parent, snapshotID, sourceID)
	}
	execFixture(t, tx, "make api depend on core", `
		insert into component_depends (project_key, from_id, to_id) values ('CORE', 'api', 'core')
	`)
	execFixture(t, tx, "give CORE-1 components", `
		insert into issue_components (issue_key, mode) values ('CORE-1', 'explicit')
	`)
	execFixture(t, tx, "make CORE-1 affect core", `
		insert into issue_component_members (issue_key, project_key, component_id) values ('CORE-1', 'CORE', 'core')
	`)

	// document_blocks: a document block's part_of. block_refs: a document block's mention.
	specBlock := specRef + "#b1"
	notesBlock := notesRef + "#b1"
	for _, block := range []struct{ artifact, node string }{{artifact: specID, node: specBlock}, {artifact: notesID, node: notesBlock}} {
		execFixture(t, tx, "index a block of "+block.node, `
			insert into document_blocks (artifact_id, block_id, node_id, type, text_sha256, excerpt, changed_version, changed_at)
			values ($1::uuid, 'b1', $2, 'paragraph', 'sha', 'excerpt', 1, now())
		`, block.artifact, block.node)
	}
	execFixture(t, tx, "cite the notes block from the spec block", `
		insert into block_refs (artifact_id, block_id, from_node, to_kind, to_id, kind, created_version)
		values ($1::uuid, 'b1', $2, 'block', $3, 'mentions', 1)
	`, specID, specBlock, notesBlock)

	// comment_blocks: a comment block's part_of; comment_block_refs: its mentions. b3 is the
	// block a later revision removed, which keeps its row with no position and its reference.
	blockComment := scanFixture(t, tx, "create the comment that holds blocks", `
		insert into comments (issue_key, author, body)
		values ('CORE-1', '{"kind":"user","id":"alice"}', 'three paragraphs')
		returning id::text
	`)
	for _, block := range []struct {
		id       string
		position *int
	}{{id: "b1", position: new(0)}, {id: "b2", position: new(1)}, {id: "b3"}} {
		execFixture(t, tx, "index comment block "+block.id, `
			insert into comment_blocks (comment_id, block_id, node_id, position, type, text_sha256, excerpt,
			                            made_at, changed_revision, changed_at, removed_at)
			values ($1::uuid, $2, 'comment/' || $1 || '#' || $2, $3, 'paragraph', 'sha', 'excerpt',
			        now(), 1, now(), case when $3::int is null then now() end)
		`, blockComment, block.id, block.position)
	}
	for _, reference := range []struct{ block, issue string }{
		{block: "b1", issue: "CORE-2"},
		{block: "b1", issue: "CORE-3"},
		{block: "b2", issue: "CORE-3"},
		{block: "b3", issue: "CORE-4"},
	} {
		execFixture(t, tx, "cite "+reference.issue+" from comment block "+reference.block, `
			insert into comment_block_refs (comment_id, block_id, from_node, to_kind, to_id, kind)
			values ($1::uuid, $2, 'comment/' || $1 || '#' || $2, 'issue', $3, 'mentions')
		`, blockComment, reference.block, reference.issue)
	}
	// The comment's own mention of each issue its blocks cite: the view holds both edges and the
	// count keeps the block's alone, except for the removed block, whose edge the view drops
	// while the probe suppressing the comment's own still finds its reference.
	for _, issue := range []string{"CORE-2", "CORE-3", "CORE-4"} {
		insertReference(t, tx, "comment", blockComment, "issue", issue)
	}

	// message_blocks: a message block's part_of; message_block_refs: its mentions.
	messageBlock := "message/" + rootMessage + "#b1"
	execFixture(t, tx, "index a message block", `
		insert into message_blocks (message_id, block_id, node_id, position, type, text_sha256, excerpt, made_at, changed_at)
		values ($1::uuid, 'b1', $2, 0, 'paragraph', 'sha', 'excerpt', now(), now())
	`, rootMessage, messageBlock)
	execFixture(t, tx, "cite the notes document from a message block", `
		insert into message_block_refs (message_id, block_id, from_node, to_kind, to_id, kind)
		values ($1::uuid, 'b1', $2, 'artifact', $3, 'mentions')
	`, rootMessage, messageBlock, notesRef)

	// comments.replies_to_block and messages.replies_to_block: a reply that names a block.
	commentBlock := "comment/" + blockComment + "#b1"
	execFixture(t, tx, "reply to a comment block", `
		insert into comments (issue_key, author, body, replies_to_block)
		values ('CORE-1', '{"kind":"user","id":"alice"}', 'about that paragraph', $1)
	`, commentBlock)
	execFixture(t, tx, "reply to a message block", `
		insert into messages (issue_key, author, body, replies_to_block)
		values ('CORE-1', '{"kind":"user","id":"alice"}', 'about that paragraph', $1)
	`, messageBlock)

	// refs: the mention arm. The reply comment holds no blocks, so each of its mentions is
	// counted where it is written; the document's and the message's are each held by a block of
	// theirs and counted there instead.
	for _, target := range [][2]string{
		{"issue", "CORE-1"},
		{"artifact", notesRef},
		{"ask", anchoredAsk},
		{"comment", ownedComment},
		{"message", rootMessage},
		{"component", "CORE/core"},
		{"session", "session-backlink"},
		{"block", notesBlock},
		{"comment_block", commentBlock},
		{"message_block", messageBlock},
	} {
		insertReference(t, tx, "comment", citingComment, target[0], target[1])
	}
	insertReference(t, tx, "artifact", specID, "block", notesBlock)
	insertReference(t, tx, "message", rootMessage, "artifact", notesRef)
	// A source of a node kind this build cannot address, written straight into refs as a newer
	// build would leave it: the mention arm's from_kind filter is what keeps it out of a count.
	insertReference(t, tx, "future", "x#y", "issue", "CORE-1")

	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit fixture: %v", err)
	}
	return backlinkFixture{
		database:     database,
		manyArms:     "CORE-1",
		heldOnce:     "CORE-2",
		heldTwice:    "CORE-3",
		removedBlock: "CORE-4",
	}
}

func execFixture(t *testing.T, tx pgx.Tx, what, statement string, args ...any) {
	t.Helper()
	if _, err := tx.Exec(context.Background(), statement, args...); err != nil {
		t.Fatalf("%s: %v", what, err)
	}
}

func scanFixture(t *testing.T, tx pgx.Tx, what, statement string, args ...any) string {
	t.Helper()
	var id string
	if err := tx.QueryRow(context.Background(), statement, args...).Scan(&id); err != nil {
		t.Fatalf("%s: %v", what, err)
	}
	return id
}

// backlinkCountOracle is the statement BacklinkCounts ran before it read each arm through its own
// index: the whole graph_edges view, filtered and grouped. It is held here as a literal, copied
// from main rather than built from this package's helpers, on purpose - built from the helpers,
// a change to the count's own builders would move the oracle and the count together and the
// comparison would pass whatever either said. $1 is the target kind, $2 the ids, $3 the node
// kinds this build reads; $4, when the statement carries backlinkCountOracleExcluded, is the
// excluded edge kinds.
const backlinkCountOracle = `select g.to_id, count(*)::int from graph_edges g
where g.to_kind = $1 and g.to_id = any($2) and g.from_kind = any($3)
  and not (g.kind = 'mentions' and (
    (g.from_kind = 'artifact' and exists (select 1 from block_refs b where b.to_kind = g.to_kind and b.to_id = g.to_id and b.kind = g.kind and b.artifact_id::text = g.from_id))
    or (g.from_kind = 'comment' and exists (select 1 from comment_block_refs b where b.to_kind = g.to_kind and b.to_id = g.to_id and b.kind = g.kind and b.comment_id::text = g.from_id))
    or (g.from_kind = 'message' and exists (select 1 from message_block_refs b where b.to_kind = g.to_kind and b.to_id = g.to_id and b.kind = g.kind and b.message_id::text = g.from_id))))`

// backlinkCountOracleExcluded is the oracle's exclusion predicate, the one part of the statement
// a caller's exclusions add.
const backlinkCountOracleExcluded = `
  and not (g.kind = any($4))`

const backlinkCountOracleGroup = `
group by g.to_id`

// oracleBacklinkCounts counts the inbound edges of ids over the whole view (backlinkCountOracle).
func oracleBacklinkCounts(t *testing.T, q Queryer, toKind string, ids, excludeKinds []string) map[string]int {
	t.Helper()
	statement := backlinkCountOracle
	args := []any{toKind, ids, nodeKinds}
	if len(excludeKinds) > 0 {
		statement += backlinkCountOracleExcluded
		args = append(args, excludeKinds)
	}
	rows, err := q.Query(context.Background(), statement+backlinkCountOracleGroup, args...)
	if err != nil {
		t.Fatalf("count %s backlinks over the view: %v", toKind, err)
	}
	defer rows.Close()
	counts := map[string]int{}
	for rows.Next() {
		var id string
		var count int
		if err := rows.Scan(&id, &count); err != nil {
			t.Fatalf("scan a %s backlink count over the view: %v", toKind, err)
		}
		counts[id] = count
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the %s backlink counts over the view: %v", toKind, err)
	}
	return counts
}

// viewBacklinkTargets is every node of toKind the view points at, read from the view itself so
// that each test covers whatever arms the fixture filled without naming an id, plus one address
// nothing points at, which must be absent from every answer.
func viewBacklinkTargets(t *testing.T, q Queryer, toKind string) []string {
	t.Helper()
	rows, err := q.Query(context.Background(), `select distinct to_id from graph_edges where to_kind = $1 order by to_id`, toKind)
	if err != nil {
		t.Fatalf("read the %s nodes the view points at: %v", toKind, err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan a %s node the view points at: %v", toKind, err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the %s nodes the view points at: %v", toKind, err)
	}
	if len(ids) == 0 {
		t.Fatalf("no arm of the view points at a %s node: the fixture leaves that kind's arms empty", toKind)
	}
	return append(ids, "nothing-points-at-this")
}

// backlinkExclusionCases is every exclusion a node kind is counted under: none; mentions, which
// every kind's arms carry; each edge kind this kind's own structural arms emit, so that at least
// one case drops an arm the kind really has; and, for an ask, the exclusion every ask surface
// passes.
func backlinkExclusionCases(toKind string) [][]string {
	cases := [][]string{nil, {"mentions"}}
	for _, arm := range backlinkArms[toKind] {
		if !slices.ContainsFunc(cases, func(kinds []string) bool { return slices.Equal(kinds, []string{arm.kind}) }) {
			cases = append(cases, []string{arm.kind})
		}
	}
	if toKind == "ask" {
		cases = append(cases, AskBacklinkExclusions)
	}
	return cases
}

// A count read arm by arm is the view's count. For every node kind this build resolves, under no
// exclusion and under each exclusion the kind's arms carry, BacklinkCounts answers exactly what
// the whole view answers over the same addresses (backlinkCountOracle).
//
// Two of the fixture's rows make the comparison able to fail rather than merely agree: one node
// is pointed at through several arms, so a dropped arm shows as a lower count; and the issue
// cited only from a block a revision removed is counted zero by both, so the asymmetry between
// the view's position filter and the probe that suppresses the citing comment's own edge is
// pinned, not reproduced by accident.
func TestBacklinkCountsReadArmByArmAgreeWithTheView(t *testing.T) {
	fixture := newBacklinkFixture(t)
	ctx := context.Background()
	pool := fixture.database.Pool

	var arms int
	if err := pool.QueryRow(ctx, `
		select count(*) from (
			select distinct g.from_kind, g.kind from graph_edges g where g.to_kind = 'issue' and g.to_id = $1
		) pair
	`, fixture.manyArms).Scan(&arms); err != nil {
		t.Fatalf("count the arms pointing at %s: %v", fixture.manyArms, err)
	}
	// Each distinct (from_kind, kind) pair of an inbound edge is a separate arm of the view, so
	// more than one pair is more than one arm.
	if arms < 3 {
		t.Fatalf("%s is pointed at through %d (from_kind, kind) pairs; want at least 3, so a dropped arm changes its count", fixture.manyArms, arms)
	}

	for _, toKind := range nodeKinds {
		ids := viewBacklinkTargets(t, pool, toKind)
		if toKind == "issue" {
			// The removed block's issue is in no arm of the view, so the view's own list does
			// not name it; it is asked for by name.
			ids = append(ids, fixture.removedBlock)
		}
		for _, exclusions := range backlinkExclusionCases(toKind) {
			counts, err := BacklinkCounts(ctx, pool, toKind, ids, exclusions)
			if err != nil {
				t.Fatalf("count %s backlinks excluding %v: %v", toKind, exclusions, err)
			}
			want := oracleBacklinkCounts(t, pool, toKind, ids, exclusions)
			if !maps.Equal(counts, want) {
				t.Errorf("%s backlink counts excluding %v = %v; the view says %v", toKind, exclusions, counts, want)
			}
			if _, ok := counts[fixture.removedBlock]; ok {
				t.Errorf("%s backlink counts excluding %v count %s, whose only citation is held by a removed block: %v",
					toKind, exclusions, fixture.removedBlock, counts)
			}
		}
	}

	counts, err := BacklinkCounts(ctx, pool, "issue", []string{fixture.heldOnce, fixture.heldTwice, fixture.removedBlock}, nil)
	if err != nil {
		t.Fatalf("count the block-held issues' backlinks: %v", err)
	}
	want := map[string]int{fixture.heldOnce: 1, fixture.heldTwice: 2}
	if !maps.Equal(counts, want) {
		t.Errorf("block-held issue backlink counts = %v; want %v: each link counted once where a current block holds it, and none for the removed block", counts, want)
	}
}

// A node kind with no arms declared is refused rather than counted from its mention arms alone,
// and every kind this build resolves is declared, so no node a list shows is counted short.
func TestBacklinkCountsRefuseANodeKindWithNoDeclaredArms(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	if _, err := BacklinkCounts(ctx, database.Pool, "pull_request", []string{"x"}, nil); err == nil {
		t.Fatal("counting the backlinks of a node kind with no declared arms answered without an error")
	}
	for _, kind := range nodeKinds {
		if _, ok := backlinkArms[kind]; !ok {
			t.Errorf("nodeLoaders resolves %q but backlinkArms declares no arms for it, so every count of that kind is refused", kind)
		}
	}
}

// planSettings is what every plan below is read under. Sequential scans off alone is not enough:
// the planner can still hash or merge a whole relation into a join, reading it under a node that
// carries no index condition, which is the unkeyed read this test exists to catch. With the two
// join strategies that can do that off as well, what is left is the nested loop over an index
// each arm is written for.
var planSettings = []string{
	"set local enable_seqscan = off",
	"set local enable_hashjoin = off",
	"set local enable_mergejoin = off",
}

// Every arm of the count's statement reaches its table through an index, and through an index
// that leads with the column the arm is keyed on, under a custom plan (built knowing the bound
// values) and under a generic plan alike. The generic plan is the one at risk: it cannot assume
// what an address parameter holds, so an arm whose index depends on the bound value loses it
// there, and a fixture this small never reaches a generic plan on its own
// (api.TestListIssueAsksOpenQueryUsesAsksOpenIndex says why), so the mode is forced.
func TestBacklinkCountStatementReadsEveryArmThroughAnIndex(t *testing.T) {
	fixture := newBacklinkFixture(t)
	ctx := context.Background()
	connection, err := fixture.database.Pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire a connection for the plans: %v", err)
	}
	defer connection.Release()

	firstKeys := map[string]string{}
	prepared := 0
	for _, toKind := range nodeKinds {
		ids := viewBacklinkTargets(t, fixture.database.Pool, toKind)
		for _, exclusions := range backlinkExclusionCases(toKind) {
			statement, ok := backlinkCountSQL(toKind, exclusions)
			if !ok {
				t.Fatalf("no arms declared for %s, the kind nodeLoaders resolves", toKind)
			}
			args := []any{ids, nodeKinds}
			literals := []string{textArrayLiteral(ids), textArrayLiteral(nodeKinds)}
			if len(exclusions) > 0 {
				args = append(args, exclusions)
				literals = append(literals, textArrayLiteral(exclusions))
			}
			prepared++
			name := fmt.Sprintf("backlink_count_%d", prepared)
			for mode, plan := range map[string]json.RawMessage{
				"custom":  customPlan(t, connection, statement, args),
				"generic": genericPlan(t, connection, name, statement, literals),
			} {
				if unkeyed := explaintest.ScansWithoutIndexCond(t, plan); len(unkeyed) > 0 {
					t.Errorf("the %s backlink count excluding %v reads %s under its %s plan, which no index serves:\n%s",
						toKind, exclusions, strings.Join(unkeyed, ", "), mode, plan)
				}
				// Carrying an index condition is not the same as being served by the index: a
				// scan whose qual names a column the index does not lead with reads the whole
				// index and filters it. Take the index off ask_followers.session_id and the
				// check above still passes, because the planner then reads the table through
				// ask_followers_pkey, (ask_id, session_id), as a full index scan with the
				// session_id qual in its Index Cond. So every scan is held to its index's own
				// first key column, the one column an index narrows on.
				for _, scan := range explaintest.IndexScans(t, plan) {
					if scan.Cond == "" {
						// A scan carrying no condition at all is the check above's to name.
						continue
					}
					firstKey := indexFirstKey(t, fixture.database.Pool, firstKeys, scan.Index)
					if !condNamesFirstKey(scan.Cond, firstKey) {
						t.Errorf("the %s backlink count excluding %v reads %s under its %s plan on `%s`, which names nothing of that index's first key column `%s`:\n%s",
							toKind, exclusions, scan, mode, scan.Cond, firstKey, plan)
					}
				}
			}
		}
	}
}

// indexFirstKey is the first key column of the index named index, as the index itself declares
// it: a plain column reads `session_id`, and an expression index reads its expression,
// `((ask_id)::text)`. Each index is read from the catalog once and kept in cache, since the
// plans of the ten node kinds name the same indexes over and over.
func indexFirstKey(t *testing.T, q Queryer, cache map[string]string, index string) string {
	t.Helper()
	if definition, ok := cache[index]; ok {
		return definition
	}
	var definition string
	statement := `select pg_get_indexdef(c.oid, 1, true) from pg_class c where c.relname = $1`
	if err := q.QueryRow(context.Background(), statement, index).Scan(&definition); err != nil {
		t.Fatalf("read the first key column of %s: %v", index, err)
	}
	cache[index] = definition
	return definition
}

// indexKeyNoise matches what a first key column's definition writes that names no column: a
// string literal, and the type a cast names.
var indexKeyNoise = regexp.MustCompile(`'[^']*'|::"?[a-z_][a-z0-9_]*"?`)

// indexKeyWord matches one lower-case identifier of a key definition or of a plan's Index Cond.
// A keyword Postgres deparses is upper case and so matches nothing, and a word of a longer
// identifier is not one of these: `parent` does not match inside `parent_key`.
var indexKeyWord = regexp.MustCompile(`[a-z_][a-z0-9_]*`)

// condNamesFirstKey reports whether cond, a scan's Index Cond, reads firstKey, its index's first
// key column as indexFirstKey reads it. The two are compared by the identifiers of the key
// rather than as strings, because the plan writes the same expression its own way: it qualifies
// each column with the query's alias and spaces and casts as it likes, so the names are all the
// two spellings share. Dropping the string literals and cast types of the key definition leaves
// its columns and its lower-case function names, and one of those appearing as a whole word of
// cond is the scan reading the key.
func condNamesFirstKey(cond, firstKey string) bool {
	words := indexKeyWord.FindAllString(cond, -1)
	for _, identifier := range indexKeyWord.FindAllString(indexKeyNoise.ReplaceAllString(firstKey, " "), -1) {
		if slices.Contains(words, identifier) {
			return true
		}
	}
	return false
}

// customPlan is the plan Postgres builds for statement knowing the values bound to it.
func customPlan(t *testing.T, connection *pgxpool.Conn, statement string, args []any) json.RawMessage {
	t.Helper()
	ctx := context.Background()
	tx, err := connection.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the custom plan's transaction: %v", err)
	}
	defer tx.Rollback(ctx)
	for _, setting := range planSettings {
		if _, err := tx.Exec(ctx, setting); err != nil {
			t.Fatalf("%s: %v", setting, err)
		}
	}
	var raw []byte
	if err := tx.QueryRow(ctx, "explain (format json) "+statement, args...).Scan(&raw); err != nil {
		t.Fatalf("explain the backlink count: %v", err)
	}
	return onePlan(t, raw)
}

// genericPlan is the plan Postgres builds for statement without knowing the values it will be
// executed with, the plan a prepared statement settles on. EXECUTE takes no bind parameters, so
// each argument is written into it as a literal; the prepared statement outlives its transaction
// and is deallocated by name on the one connection that holds it.
func genericPlan(t *testing.T, connection *pgxpool.Conn, name, statement string, literals []string) json.RawMessage {
	t.Helper()
	ctx := context.Background()
	tx, err := connection.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the generic plan's transaction: %v", err)
	}
	defer tx.Rollback(ctx)
	for _, setting := range append(planSettings, "set local plan_cache_mode = force_generic_plan") {
		if _, err := tx.Exec(ctx, setting); err != nil {
			t.Fatalf("%s: %v", setting, err)
		}
	}
	types := strings.Repeat("text[], ", len(literals))
	if _, err := tx.Exec(ctx, "prepare "+name+" ("+strings.TrimSuffix(types, ", ")+") as "+statement); err != nil {
		t.Fatalf("prepare the backlink count: %v", err)
	}
	var raw []byte
	if err := tx.QueryRow(ctx, "explain (format json) execute "+name+"("+strings.Join(literals, ", ")+")").Scan(&raw); err != nil {
		t.Fatalf("explain the prepared backlink count: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit the generic plan's transaction: %v", err)
	}
	if _, err := connection.Exec(ctx, "deallocate "+name); err != nil {
		t.Fatalf("deallocate the prepared backlink count: %v", err)
	}
	return onePlan(t, raw)
}

// onePlan is the one plan node of an EXPLAIN (FORMAT JSON) answer.
func onePlan(t *testing.T, raw []byte) json.RawMessage {
	t.Helper()
	var plans []struct {
		Plan json.RawMessage `json:"Plan"`
	}
	if err := json.Unmarshal(raw, &plans); err != nil {
		t.Fatalf("decode the explain answer: %v", err)
	}
	if len(plans) != 1 {
		t.Fatalf("explain answered %d plans; want one:\n%s", len(plans), raw)
	}
	return plans[0].Plan
}

// textArrayLiteral writes values as a SQL text[] literal, which is how an EXECUTE carries them:
// EXECUTE takes no bind parameters, so each value is written into the statement, with the
// backslashes and double quotes an array literal escapes and the single quotes a string literal
// doubles.
func textArrayLiteral(values []string) string {
	elements := make([]string, 0, len(values))
	for _, value := range values {
		escaped := strings.ReplaceAll(value, `\`, `\\`)
		escaped = strings.ReplaceAll(escaped, `"`, `\"`)
		elements = append(elements, `"`+escaped+`"`)
	}
	return `'` + strings.ReplaceAll(`{`+strings.Join(elements, ",")+`}`, `'`, `''`) + `'::text[]`
}

// graphViewArm is one arm of graph_edges as the view's own text declares it: the first relation
// its FROM names, and the literal value of each of its from_kind, kind and to_kind select items,
// empty where the arm selects a column instead.
type graphViewArm struct {
	table    string
	fromKind string
	kind     string
	toKind   string
}

func (a graphViewArm) String() string {
	return fmt.Sprintf("from %s: from_kind=%q kind=%q to_kind=%q", a.table, a.fromKind, a.kind, a.toKind)
}

// graphViewArms is every arm of graph_edges, as this package's count knows them. The count unions
// the arms that can point at the kind it is asked for, so an arm the view gains and the count
// does not know is a count silently low: this list is what makes that a failing test rather than
// a wrong number.
var graphViewArms = []graphViewArm{
	{table: "refs"},
	{table: "issues", fromKind: "issue", kind: "child_of", toKind: "issue"},
	{table: "issue_links", fromKind: "issue", kind: "blocked_by", toKind: "issue"},
	{table: "artifacts", fromKind: "artifact", kind: "attached_to", toKind: "issue"},
	{table: "asks", fromKind: "ask", kind: "anchored_to", toKind: "artifact"},
	{table: "comments", fromKind: "comment", kind: "anchored_to", toKind: "artifact"},
	{table: "asks", fromKind: "ask", kind: "owned_by", toKind: "artifact"},
	{table: "comments", fromKind: "comment", kind: "owned_by", toKind: "artifact"},
	{table: "comments", fromKind: "comment", kind: "replies_to", toKind: "comment"},
	{table: "comments", fromKind: "comment", kind: "replies_to", toKind: "ask"},
	{table: "messages", fromKind: "message", kind: "replies_to", toKind: "message"},
	{table: "ask_followers", fromKind: "ask", kind: "followed_by", toKind: "session"},
	{table: "components", fromKind: "component", kind: "part_of", toKind: "component"},
	{table: "component_depends", fromKind: "component", kind: "depends_on", toKind: "component"},
	{table: "issue_component_members", fromKind: "issue", kind: "affects", toKind: "component"},
	{table: "block_refs", fromKind: "block"},
	{table: "document_blocks", fromKind: "block", kind: "part_of", toKind: "artifact"},
	{table: "comment_blocks", fromKind: "comment_block", kind: "part_of", toKind: "comment"},
	{table: "message_blocks", fromKind: "message_block", kind: "part_of", toKind: "message"},
	{table: "comment_block_refs", fromKind: "comment_block"},
	{table: "message_block_refs", fromKind: "message_block"},
	{table: "comments", fromKind: "comment", kind: "replies_to", toKind: "comment_block"},
	{table: "messages", fromKind: "message", kind: "replies_to", toKind: "message_block"},
}

// The view cannot gain an arm the count does not know. graph_edges' own text is parsed back into
// one descriptor per arm and held to graphViewArms, and graphViewArms is held to the count's
// declarations: every literal-kind arm to an entry of backlinkArms, every column-kind arm to one
// of the four tables mentionBacklinkArms reads, and every literal source kind to a kind
// nodeLoaders resolves, which is what makes BacklinkCounts' source filter on the refs arm alone
// exact.
func TestTheGraphViewGainsNoArmTheBacklinkCountDoesNotKnow(t *testing.T) {
	fixture := newBacklinkFixture(t)
	var definition string
	if err := fixture.database.Pool.QueryRow(context.Background(),
		`select pg_get_viewdef('graph_edges'::regclass, true)`).Scan(&definition); err != nil {
		t.Fatalf("read the graph_edges definition: %v", err)
	}

	got := parseGraphViewArms(t, definition)
	gotText, wantText := armTexts(got), armTexts(graphViewArms)
	if !slices.Equal(gotText, wantText) {
		t.Fatalf("graph_edges' arms are not the arms this test knows.\nthe view:\n  %s\nthis test:\n  %s\n"+
			"Declare each new arm in backlinkArms (or, when its to_kind is a column, in mentionBacklinkArms), "+
			"add it to graphViewArms in this test, and fill it in newBacklinkFixture.",
			strings.Join(gotText, "\n  "), strings.Join(wantText, "\n  "))
	}

	// An arm whose to_kind is a literal is a structural arm, and backlinkArms must declare it
	// under that kind with that edge kind. They are compared as multisets because one kind can
	// hold the same edge kind twice: an artifact is anchored_to and owned_by from both asks and
	// comments.
	var declared, viewed []string
	for kind, arms := range backlinkArms {
		for _, arm := range arms {
			declared = append(declared, kind+"/"+arm.kind)
		}
	}
	mentionTables := map[string]bool{}
	for _, arm := range mentionBacklinkArms("issue", func(string) string { return "" }) {
		match := regexp.MustCompile(`\bfrom (\w+)`).FindStringSubmatch(arm)
		if match == nil {
			t.Fatalf("a mention arm names no table: %s", arm)
		}
		mentionTables[match[1]] = false
	}
	for _, arm := range got {
		if arm.toKind != "" {
			viewed = append(viewed, arm.toKind+"/"+arm.kind)
		} else if _, ok := mentionTables[arm.table]; !ok {
			t.Errorf("the view's arm over %s selects its to_kind as a column, but mentionBacklinkArms does not read %s", arm.table, arm.table)
		} else {
			mentionTables[arm.table] = true
		}
		if arm.fromKind != "" && !slices.Contains(nodeKinds, arm.fromKind) {
			t.Errorf("the view's arm over %s writes from_kind %q, which nodeLoaders does not resolve: "+
				"BacklinkCounts filters the source kind on the refs arm alone, which is exact only while every other arm's source kind is one this build reads",
				arm.table, arm.fromKind)
		}
	}
	for table, seen := range mentionTables {
		if !seen {
			t.Errorf("mentionBacklinkArms reads %s, but no arm of the view selects its to_kind as a column from it", table)
		}
	}
	slices.Sort(declared)
	slices.Sort(viewed)
	if !slices.Equal(declared, viewed) {
		t.Errorf("backlinkArms declares %v; the view's literal-kind arms are %v", declared, viewed)
	}
}

func armTexts(arms []graphViewArm) []string {
	texts := make([]string, 0, len(arms))
	for _, arm := range arms {
		texts = append(texts, arm.String())
	}
	slices.Sort(texts)
	return texts
}

// armLiteralItem matches a select item that writes a literal, `'<value>'::text AS <name>`, and
// armColumnItem one that selects a column, `<alias>.<name>`. A select item of graph_edges is one
// or the other; anything else fails the parse rather than passing as a shape it did not read.
var (
	armLiteralItem = regexp.MustCompile(`^'([^']*)'::text AS (\w+)$`)
	armColumnItem  = regexp.MustCompile(`^[a-z_][a-z0-9_]*\.(\w+)$`)
)

// parseGraphViewArms reads pg_get_viewdef's pretty-printed graph_edges back into one descriptor
// per arm. That form puts each top-level UNION ALL on a line of its own with no indentation, and
// no arm of this view holds a union of its own, so splitting on such a line splits the arms; each
// arm then opens with ` SELECT <item>,` and one four-space-indented line per further select item,
// and its `   FROM ` line names the relation it reads first.
func parseGraphViewArms(t *testing.T, definition string) []graphViewArm {
	t.Helper()
	body := strings.TrimSuffix(strings.TrimRight(definition, "\n"), ";")
	var arms []graphViewArm
	for index, text := range strings.Split(body, "\nUNION ALL\n") {
		where := fmt.Sprintf("arm %d of graph_edges", index+1)
		lines := strings.Split(text, "\n")
		if !strings.HasPrefix(lines[0], " SELECT ") {
			t.Fatalf("%s does not open with a SELECT: %q", where, lines[0])
		}
		items := []string{strings.TrimPrefix(lines[0], " SELECT ")}
		table := ""
		for _, line := range lines[1:] {
			if rest, found := strings.CutPrefix(line, "   FROM "); found {
				fields := strings.Fields(rest)
				if len(fields) == 0 {
					t.Fatalf("%s names no relation after its FROM: %q", where, line)
				}
				table = fields[0]
				break
			}
			if !strings.HasPrefix(line, "    ") {
				t.Fatalf("%s holds a line that is neither a select item nor its FROM: %q", where, line)
			}
			items = append(items, strings.TrimPrefix(line, "    "))
		}
		if table == "" {
			t.Fatalf("%s has no FROM line:\n%s", where, text)
		}
		if len(items) != 7 {
			t.Fatalf("%s selects %d items; want the view's 7, one per column:\n%s", where, len(items), text)
		}
		for position, item := range items {
			last := position == len(items)-1
			if strings.HasSuffix(item, ",") == last {
				t.Fatalf("%s: select item %q is not one item on one line", where, item)
			}
			items[position] = strings.TrimSuffix(item, ",")
		}
		arms = append(arms, graphViewArm{
			table:    table,
			fromKind: armLiteral(t, where, items[0], "from_kind"),
			kind:     armLiteral(t, where, items[2], "kind"),
			toKind:   armLiteral(t, where, items[3], "to_kind"),
		})
	}
	return arms
}

// armLiteral is the value a select item writes as a literal, or "" when it selects a column. The
// item must name the column it fills, so that a reordered select list fails the parse instead of
// reading one arm's kind as another's.
func armLiteral(t *testing.T, where, item, column string) string {
	t.Helper()
	if match := armLiteralItem.FindStringSubmatch(item); match != nil {
		if match[2] != column {
			t.Fatalf("%s: select item %q fills %s, not %s", where, item, match[2], column)
		}
		return match[1]
	}
	if match := armColumnItem.FindStringSubmatch(item); match != nil {
		if match[1] != column {
			t.Fatalf("%s: select item %q fills %s, not %s", where, item, match[1], column)
		}
		return ""
	}
	t.Fatalf("%s: select item %q for %s is neither a '<value>'::text AS <name> literal nor a column reference", where, item, column)
	return ""
}
