package docs

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	ygws "github.com/reearth/ygo/provider/websocket"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

func TestApplyOpsEditsLiveDocumentAndSettlesVersion(t *testing.T) {
	service, artifactID := newTestService(t)
	seedServiceText(t, service, artifactID, "one two one")
	actor := model.Actor{Kind: "session", ID: "session-0123456789abcdef"}
	first := 0
	applied, err := service.ApplyOps(context.Background(), artifactID, []model.EditOp{
		{Op: "replace", Find: "two", With: "TWO"},
		{Op: "insert", Markdown: "!", After: "end"},
		{Op: "delete", Find: "one ", Occurrence: &first},
	}, actor, nil)
	if err != nil || applied.Applied != 3 || !applied.Changed {
		t.Fatalf("applied = %#v, %v", applied, err)
	}
	waitForDocumentText(t, service, artifactID, "TWO one\n\n!\n")
	version := waitForDocumentVersion(t, service.store, artifactID, 2)
	if len(version.Authors) != 1 || version.Authors[0] != actor {
		t.Fatalf("authors = %#v", version.Authors)
	}
}

// An edit that leaves the canonical markdown as it was is not a failure and not a change: the
// route mints no version for it, so ApplyOps carries the batch's own verdict out.
func TestApplyOpsReportsABatchThatWroteNoUpdatesAsUnchanged(t *testing.T) {
	service, artifactID := newTestService(t)
	seedServiceText(t, service, artifactID, "before")
	result, err := service.ApplyOps(context.Background(), artifactID, []model.EditOp{
		{Op: "replace", Find: "before", With: "before"},
	}, model.Actor{Kind: "session", ID: "session-0123456789abcdef"}, nil)
	if err != nil {
		t.Fatalf("no-op edit: %v", err)
	}
	if result.Applied != 1 || result.Changed || !reflect.DeepEqual(result.Unchanged, []int{0}) {
		t.Fatalf("no-op edit = %#v, want 1 applied, unchanged, naming operation 0", result)
	}
	waitForDocumentText(t, service, artifactID, "before\n")
}

// The other no-op shape: operations that do write Yjs updates (here a fresh block id) and still
// leave the canonical markdown byte-identical. It has to report exactly like the first.
func TestApplyOpsReportsABatchThatWroteUpdatesAndNoMarkdownAsUnchanged(t *testing.T) {
	service, artifactID := newTestService(t)
	seedServiceText(t, service, artifactID, "before")
	blocksBefore, err := service.Blocks(context.Background(), artifactID)
	if err != nil {
		t.Fatalf("read blocks: %v", err)
	}
	first := 0
	result, err := service.ApplyOps(context.Background(), artifactID, []model.EditOp{
		{Op: "insert", Markdown: "before", After: "end"},
		{Op: "delete", Find: "before", Occurrence: &first},
	}, model.Actor{Kind: "session", ID: "session-0123456789abcdef"}, nil)
	if err != nil {
		t.Fatalf("round-trip edit: %v", err)
	}
	if result.Applied != 2 || result.Changed {
		t.Fatalf("round-trip edit = %#v, want 2 applied and unchanged", result)
	}
	waitForDocumentText(t, service, artifactID, "before\n")
	blocksAfter, err := service.Blocks(context.Background(), artifactID)
	if err != nil {
		t.Fatalf("read blocks: %v", err)
	}
	if len(blocksBefore) != 1 || len(blocksAfter) != 1 || blocksBefore[0].ID == blocksAfter[0].ID {
		t.Fatalf("block ids %v -> %v; the round trip must write a new block, not nothing", blocksBefore, blocksAfter)
	}
}

func TestVerifyTableAnchorSnapshotsRejectsMarkAddedAfterPrevalidation(t *testing.T) {
	tree, err := parseInput("| Key | Value |\n| --- | --- |\n| delete | row |\n| retain | row |\n")
	if err != nil {
		t.Fatalf("parse table: %v", err)
	}
	tableID, _ := tree.Children[0].Attrs[pmdoc.BlockIDAttr].(string)
	op := model.EditOp{Op: "delete_row", Block: tableID, Index: []byte("1")}
	snapshot, marks, table, err := tableAnchorCheck(tree, op)
	if err != nil || !table || len(marks) != 0 {
		t.Fatalf("table snapshot = %#v marks=%#v err=%v", snapshot, marks, err)
	}
	var markText func(*pmdoc.Node) bool
	markText = func(node *pmdoc.Node) bool {
		if node.Type == "text" && node.Text == "delete" {
			node.Marks = append(node.Marks, pmdoc.Mark{
				Type: string(MarkComment), Attrs: pmdoc.Attrs{"id": "comment-1", "by": "user:alice"},
			})
			return true
		}
		for _, child := range node.Children {
			if markText(child) {
				return true
			}
		}
		return false
	}
	if !markText(tree) {
		t.Fatal("delete cell text missing")
	}
	err = verifyTableAnchorSnapshots(tree, []model.EditOp{op}, []tableAnchorSnapshot{snapshot})
	var invalid *ErrInvalidOp
	if !errors.As(err, &invalid) || invalid.Field != "index" || !strings.Contains(invalid.Reason, "changed during validation") {
		t.Fatalf("table snapshot validation error = %v", err)
	}
}

func TestLockTableAnchorRowsRejectsCommentReopenedAfterPrevalidation(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	seedServiceText(t, service, artifactID, "| Key | Value |\n| --- | --- |\n| delete | row |\n| retain | row |\n")
	const commentID = "00000000-0000-4000-8000-000000000009"
	anchored, err := service.MarkQuote(context.Background(), artifactID, MarkSpec{
		Kind: MarkComment, ID: commentID, By: model.Actor{Kind: "user", ID: "alice"},
	}, "delete", nil)
	if err != nil {
		t.Fatalf("mark table cell: %v", err)
	}
	var blockID *string
	if anchored.BlockID != "" {
		blockID = &anchored.BlockID
	}
	anchorJSON, err := json.Marshal(model.Anchor{ArtifactID: artifactID, MarkID: commentID, Version: 1, Quote: anchored.Quote, BlockID: blockID})
	if err != nil {
		t.Fatalf("encode anchor: %v", err)
	}
	if _, err := service.store.Pool.Exec(context.Background(), `
		insert into comments (id, issue_key, author, body, anchor, resolved)
		values ($1, 'DOC-1', '{"kind":"user","id":"alice"}', 'Table discussion', $2, true)
	`, commentID, anchorJSON); err != nil {
		t.Fatalf("create resolved table comment: %v", err)
	}
	tree := liveTree(t, service, artifactID)
	tableID, _ := tree.Children[0].Attrs[pmdoc.BlockIDAttr].(string)
	ops := []model.EditOp{{Op: "delete_row", Block: tableID, Index: []byte("1")}}
	snapshots, err := service.prevalidateOperations(context.Background(), artifactID, tree, ops)
	if err != nil {
		t.Fatalf("prevalidate resolved comment: %v", err)
	}
	if _, err := service.store.Pool.Exec(context.Background(), `update comments set resolved = false where id = $1`, commentID); err != nil {
		t.Fatalf("reopen table comment: %v", err)
	}
	tx, err := service.store.Pool.Begin(context.Background())
	if err != nil {
		t.Fatalf("begin edit transaction: %v", err)
	}
	defer tx.Rollback(context.Background())
	err = lockTableAnchorRows(context.Background(), tx, artifactID, snapshots)
	var invalid *ErrInvalidOp
	if !errors.As(err, &invalid) || invalid.Field != "index" || !strings.Contains(invalid.Reason, "active anchors") {
		t.Fatalf("reopened table comment lock error = %v", err)
	}

	updateDone := make(chan error, 1)
	go func() {
		_, err := service.store.Pool.Exec(context.Background(), `update comments set body = 'Updated discussion' where id = $1`, commentID)
		updateDone <- err
	}()
	waitForLockWait(t, context.Background(), service.store, "%update comments%", updateDone)
	if err := tx.Rollback(context.Background()); err != nil {
		t.Fatalf("release table share lock: %v", err)
	}
	select {
	case err := <-updateDone:
		if err != nil {
			t.Fatalf("comment body update: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("comment body update remained blocked after the edit transaction released its share lock")
	}
}

func TestConditionalEditAdmissionBoundsQueuedRequests(t *testing.T) {
	service, artifactID := newTestService(t)
	release, err := service.AcquireConditionalEdit(context.Background(), artifactID)
	if err != nil {
		t.Fatalf("acquire first conditional edit: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var queued sync.WaitGroup
	for range maxQueuedConditionalEdits - 1 {
		queued.Add(1)
		go func() {
			defer queued.Done()
			_, _ = service.AcquireConditionalEdit(ctx, artifactID)
		}()
	}
	gateValue, ok := service.conditionalGates.Load(artifactID)
	if !ok {
		t.Fatal("conditional edit gate not registered")
	}
	gate := gateValue.(*conditionalEditGate)
	deadline := time.Now().Add(time.Second)
	for queueDepth(gate) != maxQueuedConditionalEdits && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := queueDepth(gate); got != maxQueuedConditionalEdits {
		t.Fatalf("queued conditional edits = %d, want %d", got, maxQueuedConditionalEdits)
	}
	if _, err := service.AcquireConditionalEdit(context.Background(), artifactID); !errors.Is(err, ErrPreconditionBusy) {
		t.Fatalf("overflow conditional edit = %v, want ErrPreconditionBusy", err)
	}
	cancel()
	queued.Wait()
	release()
	if _, exists := service.conditionalGates.Load(artifactID); exists {
		t.Fatal("idle conditional edit gate was retained")
	}

}
func queueDepth(gate *conditionalEditGate) int {
	gate.mu.Lock()
	defer gate.mu.Unlock()
	return gate.queued
}

func TestApplyOperationRetypesAParagraphInPlace(t *testing.T) {
	pmdoc.SetBlockIDGenerator(func() string { return "generated-body" })
	t.Cleanup(func() { pmdoc.SetBlockIDGenerator(nil) })
	tree, err := pmdoc.Parse("Which transport should we expose?\n")
	if err != nil {
		t.Fatalf("parse source paragraph: %v", err)
	}
	blockID, _ := tree.Children[0].Attrs[pmdoc.BlockIDAttr].(string)

	retyped, err := applyOperation(tree, model.EditOp{
		Op:         "retype",
		Block:      blockID,
		Type:       "ask",
		Attributes: map[string]any{"multiple": true, "urgency": "high"},
	})
	if err != nil {
		t.Fatalf("retype paragraph: %v", err)
	}
	got, err := pmdoc.Render(retyped)
	if err != nil {
		t.Fatalf("render retyped paragraph: %v", err)
	}
	want := ":::ask{#" + blockID + " urgency=\"high\" multiple=\"true\" state=\"open\"}\nWhich transport should we expose?\n:::\n"
	if got != want {
		t.Fatalf("retyped markdown = %q, want %q", got, want)
	}
}

func TestApplyOperationLabelsInvalidRetypeFields(t *testing.T) {
	tree, err := pmdoc.Parse("Which transport should we expose?\n")
	if err != nil {
		t.Fatalf("parse source paragraph: %v", err)
	}
	blockID, _ := tree.Children[0].Attrs[pmdoc.BlockIDAttr].(string)
	for _, test := range []struct {
		name  string
		op    model.EditOp
		field string
	}{
		{name: "unknown type", op: model.EditOp{Op: "retype", Block: blockID, Type: "missing"}, field: "type"},
		{name: "invalid attributes", op: model.EditOp{Op: "retype", Block: blockID, Type: "ask", Attributes: map[string]any{"urgency": "now"}}, field: "attributes"},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := applyOperation(tree, test.op)
			var invalid *ErrInvalidOp
			if !errors.As(err, &invalid) || invalid.Field != test.field {
				t.Fatalf("retype error = %v, want invalid %s", err, test.field)
			}
		})
	}
}

func TestSetBlockAttributesWritesTypedBlockState(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	seedServiceText(t, service, artifactID, ":::ask{#ask-1 multiple=\"false\" state=\"open\" urgency=\"med\"}\nShip it?\n:::\n")
	actor := model.Actor{Kind: "user", ID: "alice"}

	err := service.SetBlockAttributes(context.Background(), artifactID, "ask-1", map[string]any{
		"answer":      "Yes.",
		"answered_at": "2026-09-12T13:20:00Z",
		"answered_by": "alice",
		"selected":    []string{"Yes"},
		"state":       "answered",
	}, actor)
	if err != nil {
		t.Fatalf("write ask state: %v", err)
	}
	waitForDocumentText(t, service, artifactID, ":::ask{#ask-1 urgency=\"med\" multiple=\"false\" state=\"answered\" answered_by=\"alice\" answered_at=\"2026-09-12T13:20:00Z\" selected=\"[&#x22;Yes&#x22;]\" answer=\"Yes.\"}\nShip it?\n:::\n")
}

func TestApplyOpsRejectsAmbiguousTargetWithoutChangingDocument(t *testing.T) {
	service, artifactID := newTestService(t)
	seedServiceText(t, service, artifactID, "same same")
	_, err := service.ApplyOps(context.Background(), artifactID, []model.EditOp{{Op: "replace", Find: "same", With: "changed"}}, model.Actor{Kind: "session", ID: "session-0123456789abcdef"}, nil)
	var ambiguous *ErrQuoteAmbiguous
	if !errors.As(err, &ambiguous) || ambiguous.Quote != "same" {
		t.Fatalf("ambiguous edit error = %v, want the quote named", err)
	}
	if len(ambiguous.Candidates) != 2 {
		t.Fatalf("ambiguous candidates = %#v, want two candidates", ambiguous.Candidates)
	}
	waitForDocumentText(t, service, artifactID, "same same\n")
}

func TestApplyOpsResolvesAgainstDocumentInsideApply(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	seedServiceText(t, service, artifactID, "# First")
	if _, err := service.ReplaceText(context.Background(), artifactID, "base", model.Actor{Kind: "user", ID: "alice"}); err != nil {
		t.Fatalf("prepare document: %v", err)
	}
	entered := make(chan struct{})
	release := make(chan struct{})
	service.srv.OnInject = func(ctx context.Context, info ygws.InjectInfo) error {
		select {
		case <-entered:
		default:
			close(entered)
			<-release
		}
		return service.allowInject(ctx, info)
	}

	result := make(chan error, 1)
	go func() {
		_, err := service.ApplyOps(context.Background(), artifactID, []model.EditOp{{Op: "insert", Markdown: "!", After: "end"}}, model.Actor{Kind: "session", ID: "session-0123456789abcdef"}, nil)
		result <- err
	}()
	<-entered
	editLiveTree(t, service, artifactID, replaceRun("base", "base browser"))
	close(release)
	if err := <-result; err != nil {
		t.Fatalf("apply operation: %v", err)
	}
	waitForDocumentText(t, service, artifactID, "base browser\n\n!\n")
}

func TestApplyOpsInsertsAtHeadingsAndEdgesAndReplacesInlineText(t *testing.T) {
	service, artifactID := newTestService(t)
	seedServiceText(t, service, artifactID, "# Title\n\nBody text.\n")
	_, err := service.ApplyOps(context.Background(), artifactID, []model.EditOp{
		{Op: "insert", Markdown: "Intro.", After: "heading:Title"},
		{Op: "replace", Find: "text.", With: "text. more"},
		{Op: "insert", Markdown: "- item", Before: "start"},
	}, model.Actor{Kind: "session", ID: "session-0123456789abcdef"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	waitForDocumentText(t, service, artifactID, "- item\n\n# Title\n\nIntro.\n\nBody text. more\n")
}

func TestApplyOpsRejectsMarkdownOutsideProofSchema(t *testing.T) {
	service, artifactID := newTestService(t)
	seedServiceText(t, service, artifactID, "keep")
	_, err := service.ApplyOps(context.Background(), artifactID, []model.EditOp{{Op: "replace", Find: "keep", With: "one\n\ntwo"}}, model.Actor{Kind: "session", ID: "session-0123456789abcdef"}, nil)
	var invalid *ErrInvalidOp
	if !errors.As(err, &invalid) || invalid.Field != "with" || !strings.Contains(invalid.Reason, "replace is inline") {
		t.Fatalf("err = %v", err)
	}
	waitForDocumentText(t, service, artifactID, "keep\n")
}

func TestApplyOperationInsertsParagraphAfterTableContainingCellAnchor(t *testing.T) {
	tree, err := parseInput("| Key | Value |\n| --- | --- |\n| A10 | old |\n\nAfter.\n")
	if err != nil {
		t.Fatal(err)
	}
	next, err := applyOperation(tree, model.EditOp{
		Op:       "insert",
		After:    "A10",
		Markdown: "Inserted paragraph",
	})
	if err != nil {
		t.Fatal(err)
	}
	markdown, err := renderTree(next)
	if err != nil {
		t.Fatal(err)
	}
	const want = "| Key | Value |\n| :--- | :--- |\n| A10 | old |\n\nInserted paragraph\n\nAfter.\n"
	if markdown != want {
		t.Fatalf("paragraph after cell anchor = %q, want %q", markdown, want)
	}
}

func TestApplyOperationInsertsParagraphAfterParagraphContainingAnchor(t *testing.T) {
	tree, err := parseInput("Before anchor after.\n\nNext.\n")
	if err != nil {
		t.Fatal(err)
	}
	next, err := applyOperation(tree, model.EditOp{
		Op:       "insert",
		After:    "anchor",
		Markdown: "Inserted paragraph",
	})
	if err != nil {
		t.Fatal(err)
	}
	markdown, err := renderTree(next)
	if err != nil {
		t.Fatal(err)
	}
	const want = "Before anchor after.\n\nInserted paragraph\n\nNext.\n"
	if markdown != want {
		t.Fatalf("paragraph after in-paragraph anchor = %q, want %q", markdown, want)
	}
}

func TestApplyOperationInsertsInlineMarkdownAsOwnParagraph(t *testing.T) {
	tree, err := parseInput("Before anchor after.\n")
	if err != nil {
		t.Fatal(err)
	}
	next, err := applyOperation(tree, model.EditOp{
		Op:       "insert",
		After:    "anchor",
		Markdown: " **bold**",
	})
	if err != nil {
		t.Fatal(err)
	}
	markdown, err := renderTree(next)
	if err != nil {
		t.Fatal(err)
	}
	const want = "Before anchor after.\n\n**bold**\n"
	if markdown != want {
		t.Fatalf("inline markdown insert = %q, want %q", markdown, want)
	}
}

func TestApplyOperationExtendsTableAfterCellAnchor(t *testing.T) {
	tree, err := parseInput("| Key | Value |\n| --- | --- |\n| A10 | old |\n")
	if err != nil {
		t.Fatal(err)
	}
	next, err := applyOperation(tree, model.EditOp{
		Op:       "insert",
		After:    "A10",
		Markdown: "| A11 | new |",
	})
	if err != nil {
		t.Fatal(err)
	}
	markdown, err := renderTree(next)
	if err != nil {
		t.Fatal(err)
	}
	const want = "| Key | Value |\n| :--- | :--- |\n| A10 | old |\n| A11 | new |\n"
	if markdown != want {
		t.Fatalf("table-row insertion = %q, want %q", markdown, want)
	}
}

func TestApplyOperationRejectsFindTargetSpanningTextblocks(t *testing.T) {
	tree, err := parseInput("one\n\ntwo\n")
	if err != nil {
		t.Fatal(err)
	}
	for _, op := range []model.EditOp{
		{Op: "replace", Find: "one two", With: "changed"},
		{Op: "delete", Find: "one two"},
	} {
		_, err := applyOperation(tree, op)
		if !errors.Is(err, pmdoc.ErrTargetSpansBlocks) {
			t.Fatalf("%s spanning textblocks error = %v, want ErrTargetSpansBlocks", op.Op, err)
		}
	}
}

func TestApplyOperationMatchesPlainTextInsideInlineCodeAndLinks(t *testing.T) {
	tests := []struct {
		name     string
		markdown string
		anchor   string
		want     string
	}{
		{
			name:     "inline code",
			markdown: "before `code anchor` after\n",
			anchor:   "code anchor",
			want:     "before updated after\n",
		},
		{
			name:     "link",
			markdown: "before [link anchor](https://example.com) after\n",
			anchor:   "link anchor",
			want:     "before updated after\n",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			tree, err := parseInput(test.markdown)
			if err != nil {
				t.Fatal(err)
			}
			next, err := applyOperation(tree, model.EditOp{
				Op:   "replace",
				Find: test.anchor,
				With: "updated",
			})
			if err != nil {
				t.Fatal(err)
			}
			markdown, err := renderTree(next)
			if err != nil {
				t.Fatal(err)
			}
			if markdown != test.want {
				t.Fatalf("replace %q = %q, want %q", test.anchor, markdown, test.want)
			}
		})
	}
}

func TestApplyOperationExplainsRenderedQuoteMatching(t *testing.T) {
	tree, err := parseInput("Use `config` with care.\n\nUse it sparingly.\n\nUnrelated paragraph.\n\nAnother one.\n")
	if err != nil {
		t.Fatal(err)
	}
	_, err = applyOperations(tree, []model.EditOp{{
		Op:   "replace",
		Find: "Use `missing`",
		With: "updated",
	}})
	var missing *ErrQuoteNotFound
	if !errors.As(err, &missing) {
		t.Fatalf("rendered quote error = %v, want *ErrQuoteNotFound", err)
	}
	want := []string{"Use config with care.", "Use it sparingly.", "Unrelated paragraph."}
	if missing.Quote != "Use `missing`" || !reflect.DeepEqual(missing.Nearest, want) {
		t.Fatalf("rendered quote miss = %q, nearest %q; want the quote and %q", missing.Quote, missing.Nearest, want)
	}
}

const askFixture = ":::ask{#decision urgency=\"high\" multiple=\"false\" state=\"open\"}\nWhich transport?\n:::\n"

func TestApplyOperationDeletesABlockByID(t *testing.T) {
	tree, err := parseInput("# Title\n\n" + askFixture + "\n1. only\n\nAfter.\n")
	if err != nil {
		t.Fatal(err)
	}
	listID, _ := tree.Children[2].Attrs[pmdoc.BlockIDAttr].(string)
	next, err := applyOperations(tree, []model.EditOp{
		{Op: "delete", Block: "decision"},
		{Op: "delete", Block: listID},
	})
	if err != nil {
		t.Fatal(err)
	}
	markdown, err := renderTree(next.tree)
	if err != nil {
		t.Fatal(err)
	}
	if markdown != "# Title\n\nAfter.\n" {
		t.Fatalf("after block deletes = %q", markdown)
	}
	if _, err := applyOperation(tree, model.EditOp{Op: "delete", Block: "missing"}); !errors.Is(err, pmdoc.ErrTargetNotFound) {
		t.Fatalf("unknown block error = %v, want ErrTargetNotFound", err)
	}
	_, err = applyOperation(tree, model.EditOp{Op: "delete"})
	var invalid *ErrInvalidOp
	if !errors.As(err, &invalid) || invalid.Field != "find or block" {
		t.Fatalf("delete without a target error = %v, want invalid find or block", err)
	}
}

func TestApplyOperationsExplainsBlockCascadeWithinBatch(t *testing.T) {
	tree, err := parseInput("- Parent\n  - Child\n")
	if err != nil {
		t.Fatal(err)
	}
	parentID, _ := tree.Children[0].Attrs[pmdoc.BlockIDAttr].(string)
	childID, _ := tree.Children[0].Children[0].Attrs[pmdoc.BlockIDAttr].(string)
	removed, err := pmdoc.BlockDescendantIDs(tree, parentID)
	if err != nil {
		t.Fatalf("find delete cascade: %v", err)
	}
	foundChild := false
	for _, blockID := range removed {
		if blockID == childID {
			foundChild = true
			break
		}
	}
	if !foundChild {
		t.Fatalf("delete cascade targets = %q, want child %q", removed, childID)
	}
	_, err = applyOperations(tree, []model.EditOp{
		{Op: "delete", Block: parentID},
		{Op: "delete", Block: childID},
	})
	var invalid *ErrInvalidOp
	if !errors.As(err, &invalid) || invalid.Field != "block" ||
		!strings.Contains(invalid.Reason, `block "`+childID+`" was removed by operation 0 as a cascade of delete {block:"`+parentID+`"}`) {
		t.Fatalf("cascade batch error = %v", err)
	}
}

// LEGION-140: a delete whose match is a textblock's entire text removes the block; a list
// emptied of every item disappears; a partial match keeps the block with its remaining text.
func TestApplyOperationDeletingABlocksWholeTextRemovesTheBlock(t *testing.T) {
	const list = "# Acceptance\n\n- first criterion\n- second criterion\n- third criterion\n\n## Next\n"
	for _, test := range []struct {
		name     string
		markdown string
		ops      []model.EditOp
		want     string
	}{
		{
			name:     "whole item text removes the item",
			markdown: list,
			ops:      []model.EditOp{{Op: "delete", Find: "second criterion"}},
			want:     "# Acceptance\n\n- first criterion\n- third criterion\n\n## Next\n",
		},
		{
			name:     "every item removed removes the list",
			markdown: list,
			ops: []model.EditOp{
				{Op: "delete", Find: "first criterion"},
				{Op: "delete", Find: "second criterion"},
				{Op: "delete", Find: "third criterion"},
			},
			want: "# Acceptance\n\n## Next\n",
		},
		{
			name:     "partial text keeps the item",
			markdown: list,
			ops:      []model.EditOp{{Op: "delete", Find: " criterion", Occurrence: new(0)}},
			want:     "# Acceptance\n\n- first\n- second criterion\n- third criterion\n\n## Next\n",
		},
		{
			name:     "whole paragraph text removes the paragraph",
			markdown: "Keep.\n\nRemove me.\n\nAlso keep.\n",
			ops:      []model.EditOp{{Op: "delete", Find: "Remove me."}},
			want:     "Keep.\n\nAlso keep.\n",
		},
		{
			name:     "whole heading text removes the heading",
			markdown: "# Keep\n\n## Remove\n\nBody.\n",
			ops:      []model.EditOp{{Op: "delete", Find: "## Remove"}},
			want:     "# Keep\n\nBody.\n",
		},
		{
			name:     "table cell text is removed but the cell stays",
			markdown: "| Key | Value |\n| --- | --- |\n| A10 | old |\n",
			ops:      []model.EditOp{{Op: "delete", Find: "old"}},
			want:     "| Key | Value |\n| :--- | :--- |\n| A10 |  |\n",
		},
		{
			name:     "a parent bullet's nested list is hoisted into its place",
			markdown: "- Parent\n  - child one\n  - child two\n- Sibling\n",
			ops:      []model.EditOp{{Op: "delete", Find: "Parent"}},
			want:     "- child one\n- child two\n- Sibling\n",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			tree, err := parseInput(test.markdown)
			if err != nil {
				t.Fatal(err)
			}
			next, err := applyOperations(tree, test.ops)
			if err != nil {
				t.Fatal(err)
			}
			markdown, err := renderTree(next.tree)
			if err != nil {
				t.Fatal(err)
			}
			if markdown != test.want {
				t.Fatalf("after deletes = %q, want %q", markdown, test.want)
			}
		})
	}
}

func TestApplyOperationDeleteThatEmptiesATypedBlockNamesTheSchemaRule(t *testing.T) {
	tree, err := parseInput(askFixture)
	if err != nil {
		t.Fatal(err)
	}
	_, err = applyOperation(tree, model.EditOp{Op: "delete", Find: "Which transport?"})
	var invalid *ErrInvalidOp
	if !errors.As(err, &invalid) || invalid.Field != "find" || !strings.Contains(invalid.Reason, "typed block \"ask\"") {
		t.Fatalf("delete of an ask's only paragraph error = %v, want invalid find naming the ask content rule", err)
	}
}

func TestApplyOperationDeleteOfABulletWithOtherContentNamesTheItemBlock(t *testing.T) {
	tree, err := parseInput("- Parent\n\n  Extra paragraph.\n\n- Sibling\n")
	if err != nil {
		t.Fatal(err)
	}
	itemID, _ := tree.Children[0].Children[0].Attrs[pmdoc.BlockIDAttr].(string)
	_, err = applyOperation(tree, model.EditOp{Op: "delete", Find: "Parent"})
	var invalid *ErrInvalidOp
	if !errors.As(err, &invalid) || invalid.Field != "find" || !strings.Contains(invalid.Reason, `delete {block:"`+itemID+`"}`) {
		t.Fatalf("delete of a bullet with other content error = %v, want invalid find naming delete {block:%q}", err, itemID)
	}
	// The named escape hatch removes the item with its content.
	next, err := applyOperation(tree, model.EditOp{Op: "delete", Block: itemID})
	if err != nil {
		t.Fatal(err)
	}
	markdown, err := renderTree(next)
	if err != nil {
		t.Fatal(err)
	}
	if markdown != "- Sibling\n" {
		t.Fatalf("after deleting the item by id = %q", markdown)
	}
}

func TestApplyOperationMovesABlockToAnAnchor(t *testing.T) {
	const contextMarkdown = "# Context\n\nThe context ends with no drift.\n\n## 4\\. Design\n\nDesign body.\n"
	tree, err := parseInput(askFixture + "\n" + contextMarkdown)
	if err != nil {
		t.Fatal(err)
	}
	designID, _ := tree.Children[3].Attrs[pmdoc.BlockIDAttr].(string)
	for _, test := range []struct {
		name string
		op   model.EditOp
		want string
	}{
		{
			name: "after a quote",
			op:   model.EditOp{Op: "move", Block: "decision", After: "no drift."},
			want: "# Context\n\nThe context ends with no drift.\n\n" + askFixture + "\n## 4\\. Design\n\nDesign body.\n",
		},
		{
			name: "before a heading",
			op:   model.EditOp{Op: "move", Block: "decision", Before: "heading:4. Design"},
			want: "# Context\n\nThe context ends with no drift.\n\n" + askFixture + "\n## 4\\. Design\n\nDesign body.\n",
		},
		{
			name: "after a block id",
			op:   model.EditOp{Op: "move", Block: "decision", After: "block:" + designID},
			want: "# Context\n\nThe context ends with no drift.\n\n## 4\\. Design\n\n" + askFixture + "\nDesign body.\n",
		},
		{
			name: "to the end",
			op:   model.EditOp{Op: "move", Block: "decision", After: "end"},
			want: contextMarkdown + "\n" + askFixture,
		},
		{
			name: "insert also accepts a block id anchor",
			op:   model.EditOp{Op: "insert", Markdown: "Inserted.", Before: "block:" + designID},
			want: askFixture + "\n# Context\n\nThe context ends with no drift.\n\nInserted.\n\n## 4\\. Design\n\nDesign body.\n",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			next, err := applyOperation(tree, test.op)
			if err != nil {
				t.Fatal(err)
			}
			markdown, err := renderTree(next)
			if err != nil {
				t.Fatal(err)
			}
			if markdown != test.want {
				t.Fatalf("after %s = %q, want %q", test.name, markdown, test.want)
			}
		})
	}
}

func TestApplyOperationMoveRejectsMalformedAndSelfAnchoredMoves(t *testing.T) {
	tree, err := parseInput(askFixture + "\nAfter.\n")
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name  string
		op    model.EditOp
		field string
	}{
		{name: "no block", op: model.EditOp{Op: "move", After: "After."}, field: "block"},
		{name: "no anchor", op: model.EditOp{Op: "move", Block: "decision"}, field: "after or before"},
		{name: "both anchors", op: model.EditOp{Op: "move", Block: "decision", After: "After.", Before: "start"}, field: "after or before"},
		{name: "quote inside the moved block", op: model.EditOp{Op: "move", Block: "decision", After: "transport"}, field: "after"},
		{name: "anchored to itself", op: model.EditOp{Op: "move", Block: "decision", Before: "block:decision"}, field: "before"},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := applyOperation(tree, test.op)
			var invalid *ErrInvalidOp
			if !errors.As(err, &invalid) || invalid.Field != test.field {
				t.Fatalf("move error = %v, want invalid %s", err, test.field)
			}
		})
	}
	if _, err := applyOperation(tree, model.EditOp{Op: "move", Block: "missing", After: "After."}); !errors.Is(err, pmdoc.ErrTargetNotFound) {
		t.Fatalf("unknown block error = %v, want ErrTargetNotFound", err)
	}
}

func TestApplyOperationRetypesATypedBlock(t *testing.T) {
	tree, err := parseInput(askFixture)
	if err != nil {
		t.Fatal(err)
	}
	next, err := applyOperation(tree, model.EditOp{Op: "retype", Block: "decision", Type: "callout", Attributes: map[string]any{"kind": "warning"}})
	if err != nil {
		t.Fatalf("retype ask block: %v", err)
	}
	markdown, err := renderTree(next)
	if err != nil {
		t.Fatal(err)
	}
	if markdown != ":::callout{#decision kind=\"warning\" title=\"\"}\nWhich transport?\n:::\n" {
		t.Fatalf("retyped ask = %q", markdown)
	}

	heading, err := parseInput("# Heading\n")
	if err != nil {
		t.Fatal(err)
	}
	headingID, _ := heading.Children[0].Attrs[pmdoc.BlockIDAttr].(string)
	_, err = applyOperation(heading, model.EditOp{Op: "retype", Block: headingID, Type: "callout"})
	var invalid *ErrInvalidOp
	if !errors.As(err, &invalid) || invalid.Field != "block" {
		t.Fatalf("retype heading error = %v, want invalid block", err)
	}
}

// Replacing a heading's text with "4. Design" turned the heading into an ordered list and left
// an empty list and an empty heading behind; a quote-anchored replace is inline by contract.
func TestApplyOperationReplaceInsideATextblockIsInline(t *testing.T) {
	for _, test := range []struct {
		name, markdown, find, with, want string
	}{
		{name: "list marker in a heading", markdown: "## Design\n\nBody.\n", find: "## Design", with: "4. Design", want: "## 4\\. Design\n\nBody.\n"},
		{name: "heading marker in a paragraph", markdown: "Body.\n", find: "Body.", with: "# Not a heading", want: "\\# Not a heading\n"},
		{name: "inline markup stays markup", markdown: "Body.\n", find: "Body.", with: "**bold** `code`", want: "**bold** `code`\n"},
		{name: "soft break joins as a space", markdown: "Body.\n", find: "Body.", with: "one\ntwo", want: "one two\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			tree, err := parseInput(test.markdown)
			if err != nil {
				t.Fatal(err)
			}
			next, err := applyOperation(tree, model.EditOp{Op: "replace", Find: test.find, With: test.with})
			if err != nil {
				t.Fatal(err)
			}
			markdown, err := renderTree(next)
			if err != nil {
				t.Fatal(err)
			}
			if markdown != test.want {
				t.Fatalf("replace with %q = %q, want %q", test.with, markdown, test.want)
			}
		})
	}
}

// TESTER-AUTHORED (Accept1326, LEGION-260 acceptance item 2): a `with` whose leading marker is of
// a different kind from the matched block's own "stays literal and escaped", so the document's own
// canonical markdown still describes the document it was rendered from. The renderer escapes a
// lone "# " at a line start but not a multi-# ATX marker — pmdoc/render.go needsInlineEscape's
// '#' case requires the very next byte to be a space — so a "## " written into a paragraph reads
// back as a heading, and one written into a list item's paragraph reads back as markdown outside
// the Proof schema, which Dispatch refuses to import at all. The single-# row is the control:
// it is escaped today and must stay so.
func TestApplyOperationReplaceKeepsAHeadingMarkerLiteralInATextblock(t *testing.T) {
	for _, test := range []struct {
		name, markdown, find, with string
	}{
		{name: "one hash in a paragraph", markdown: "Body.\n", find: "Body.", with: "# Not a heading"},
		{name: "two hashes in a paragraph", markdown: "Body.\n", find: "Body.", with: "## Not a heading"},
		{name: "six hashes in a paragraph", markdown: "Body.\n", find: "Body.", with: "###### Not a heading"},
		{name: "two hashes in a list item", markdown: "- Item.\n", find: "Item.", with: "## Not a heading"},
	} {
		t.Run(test.name, func(t *testing.T) {
			tree, err := parseInput(test.markdown)
			if err != nil {
				t.Fatal(err)
			}
			next, err := applyOperation(tree, model.EditOp{Op: "replace", Find: test.find, With: test.with})
			if err != nil {
				t.Fatal(err)
			}
			markdown, err := renderTree(next)
			if err != nil {
				t.Fatal(err)
			}
			reparsed, err := parseInput(markdown)
			if err != nil {
				t.Fatalf("canonical markdown %q does not parse back: %v", markdown, err)
			}
			if !reparsed.Equal(next) {
				t.Fatalf("canonical markdown %q reads back as a different document: the %q marker was written unescaped", markdown, test.with)
			}
		})
	}
}

// TESTER-AUTHORED (Accept1326, LEGION-260 acceptance item 3): "a heading: anchor, quote anchor or
// target that doesn't resolve returns an error naming the operation's index, the anchor text, and
// the nearest existing headings ... or blocks. It never names an internal package."
// resolveEditQuote only dresses pmdoc.ErrQuoteNotFound; a quote that resolves ambiguously
// (ErrTargetAmbiguous) or across two textblocks (ErrTargetSpansBlocks) falls through raw, so an
// agent is told "pmdoc: target is ambiguous" with no operation index and no quote to act on.
func TestApplyOperationsUnresolvedQuoteErrorsNameTheQuoteNotThePackage(t *testing.T) {
	for _, test := range []struct {
		name, markdown, find string
	}{
		{name: "quote matching two blocks", markdown: "Shared line.\n\nShared line.\n", find: "Shared line."},
		{name: "quote spanning two blocks", markdown: "## Alpha\n\nBody.\n", find: "Alpha Body."},
	} {
		t.Run(test.name, func(t *testing.T) {
			tree, err := parseInput(test.markdown)
			if err != nil {
				t.Fatal(err)
			}
			_, err = applyOperations(tree, []model.EditOp{{Op: "replace", Find: test.find, With: "z"}})
			if err == nil {
				t.Fatalf("replace find=%q resolved, want a refusal", test.find)
			}
			message := err.Error()
			if strings.Contains(message, "pmdoc") {
				t.Errorf("error %q names the internal package", message)
			}
			if !strings.Contains(message, "operation 0") {
				t.Errorf("error %q does not name the operation index", message)
			}
			if !strings.Contains(message, test.find) {
				t.Errorf("error %q does not name the quote %q", message, test.find)
			}
		})
	}
}

func TestApplyOperationReplaceRejectsBlockReplacements(t *testing.T) {
	tree, err := parseInput("Body.\n")
	if err != nil {
		t.Fatal(err)
	}
	_, err = applyOperation(tree, model.EditOp{Op: "replace", Find: "Body.", With: "one\n\ntwo"})
	var invalid *ErrInvalidOp
	if !errors.As(err, &invalid) || invalid.Field != "with" {
		t.Fatalf("multi-paragraph replace error = %v, want invalid with", err)
	}
}

// AGENTC-193's spec came back with `## ##`, `7. 7\.`, `4. 4\.` and `-    - `: a `with` carrying
// the marker its own block already renders wrote that marker twice. The heading rename is the
// one shape that keeps working, because `find` carried the marker through the match.
func TestApplyOperationReplaceKeepsOneHeadingMarkerOnARename(t *testing.T) {
	tree, err := parseInput("## New since we talked (2026-09-24)\n\nBody.\n")
	if err != nil {
		t.Fatal(err)
	}
	next, err := applyOperation(tree, model.EditOp{
		Op:   "replace",
		Find: "## New since we talked (2026-09-24)",
		With: "## New since we talked (2026-09-24) - SUPERSEDED, kept as record",
	})
	if err != nil {
		t.Fatalf("rename heading: %v", err)
	}
	markdown, err := renderTree(next)
	if err != nil {
		t.Fatal(err)
	}
	const want = "## New since we talked (2026-09-24) - SUPERSEDED, kept as record\n\nBody.\n"
	if markdown != want {
		t.Fatalf("renamed heading = %q, want %q", markdown, want)
	}
}

func TestApplyOperationReplaceRefusesAWithThatRepeatsTheBlocksOwnMarker(t *testing.T) {
	for _, test := range []struct {
		name, markdown, find, with string
	}{
		{name: "heading marker find never carried", markdown: "## Design\n", find: "Design", with: "## Design notes"},
		{name: "a different heading level", markdown: "## Design\n", find: "## Design", with: "### Design"},
		{name: "ordered marker on an ordered item", markdown: "1. Launcher contract\n", find: "Launcher contract", with: "1. Launcher contract, ruled"},
		{name: "bullet marker on a bullet item", markdown: "- Retracted\n", find: "Retracted", with: "- Retracted later"},
		{name: "an indented bullet marker on a bullet item", markdown: "- Retracted\n", find: "Retracted", with: "   - Retracted later"},
	} {
		t.Run(test.name, func(t *testing.T) {
			tree, err := parseInput(test.markdown)
			if err != nil {
				t.Fatal(err)
			}
			_, err = applyOperation(tree, model.EditOp{Op: "replace", Find: test.find, With: test.with})
			var invalid *ErrInvalidOp
			if !errors.As(err, &invalid) || invalid.Field != "with" {
				t.Fatalf("replace with %q = %v, want invalid with", test.with, err)
			}
			if !strings.Contains(invalid.Reason, "omit the marker") {
				t.Fatalf("reason = %q, want the omit-the-marker guidance", invalid.Reason)
			}
		})
	}
}

// The refusal quotes the marker the reader will see in the document, not a stand-in: AGENTC-193's
// item was `7.`, and telling that reader the block renders `1.` sends them looking for a
// different bullet.
func TestApplyOperationReplaceRefusalQuotesTheBlocksRealMarker(t *testing.T) {
	tree, err := parseInput("7. Launcher contract\n8. Acceptance\n")
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct{ find, want string }{
		{find: "Launcher contract", want: `"7. "`},
		{find: "Acceptance", want: `"8. "`},
	} {
		_, err := applyOperation(tree, model.EditOp{Op: "replace", Find: test.find, With: "9. " + test.find})
		var invalid *ErrInvalidOp
		if !errors.As(err, &invalid) {
			t.Fatalf("replace in %q = %v, want invalid with", test.find, err)
		}
		if !strings.Contains(invalid.Reason, test.want) {
			t.Fatalf("reason = %q, want it to quote %s", invalid.Reason, test.want)
		}
	}
}

// A refusal is rendered from the typed error, not the wrapped chain, so the operation index has
// to survive on the error itself or a multi-op batch never says which operation was refused.
func TestApplyOperationsRefusalsCarryTheirOperationIndex(t *testing.T) {
	tree, err := parseInput("Body.\n\n- Retracted\n")
	if err != nil {
		t.Fatal(err)
	}
	_, err = applyOperations(tree, []model.EditOp{
		{Op: "replace", Find: "Body.", With: "Body!"},
		{Op: "replace", Find: "Retracted", With: "- Retracted later"},
	})
	var invalid *ErrInvalidOp
	if !errors.As(err, &invalid) || invalid.Field != "with" {
		t.Fatalf("batch error = %v, want invalid with", err)
	}
	if !strings.Contains(invalid.Error(), "operation 1") {
		t.Fatalf("refusal = %q, want it to name operation 1", invalid.Error())
	}
}

// The refusal is about a duplicated marker, so a marker landing anywhere the renderer does not
// write one stays the literal text it has always been.
func TestApplyOperationReplaceKeepsAMarkerTheRendererDoesNotRepeatLiteral(t *testing.T) {
	for _, test := range []struct {
		name, markdown, find, with, want string
	}{
		{
			name:     "mid-block, after the item's own marker",
			markdown: "- Retracted later today\n",
			find:     "later today",
			with:     "- later today",
			want:     "- Retracted - later today\n",
		},
		{
			name:     "the start of an item's second paragraph, which renders no marker",
			markdown: "- Retracted\n\n  Later today.\n",
			find:     "Later today.",
			with:     "- Later today.",
			want:     "- Retracted\n\n  \\- Later today.\n",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			tree, err := parseInput(test.markdown)
			if err != nil {
				t.Fatal(err)
			}
			next, err := applyOperation(tree, model.EditOp{Op: "replace", Find: test.find, With: test.with})
			if err != nil {
				t.Fatalf("replace with %q: %v", test.with, err)
			}
			markdown, err := renderTree(next)
			if err != nil {
				t.Fatal(err)
			}
			if markdown != test.want {
				t.Fatalf("replace with %q = %q, want %q", test.with, markdown, test.want)
			}
		})
	}
}

// AGENTC-193's anchor was a prefix of a longer heading, and all the agent was told was
// `pmdoc: target not found`.
func TestApplyOperationHeadingAnchorMissNamesTheAnchorAndNearestHeadings(t *testing.T) {
	tree, err := parseInput("## New since we talked (2026-09-24) - SUPERSEDED, kept as record\n\nBody.\n\n## Acceptance\n")
	if err != nil {
		t.Fatal(err)
	}
	_, err = applyOperation(tree, model.EditOp{
		Op:       "insert",
		Markdown: "Intro.",
		After:    "heading:New since we talked (2026-09-24)",
	})
	if !errors.Is(err, pmdoc.ErrTargetNotFound) {
		t.Fatalf("heading anchor miss = %v, want ErrTargetNotFound", err)
	}
	message := err.Error()
	for _, want := range []string{
		`"New since we talked (2026-09-24)"`,
		"New since we talked (2026-09-24) - SUPERSEDED, kept as record",
		"Acceptance",
		"nearest headings",
	} {
		if !strings.Contains(message, want) {
			t.Fatalf("heading anchor miss = %q, want it to name %q", message, want)
		}
	}
	if strings.Contains(message, "pmdoc") {
		t.Fatalf("heading anchor miss = %q, want no internal package name", message)
	}
}

func TestApplyOperationsQuoteMissNamesTheOperationTheQuoteAndNearestBlocks(t *testing.T) {
	tree, err := parseInput("Alpha block.\n\nBeta block.\n")
	if err != nil {
		t.Fatal(err)
	}
	_, err = applyOperations(tree, []model.EditOp{
		{Op: "replace", Find: "Alpha block.", With: "Alpha block!"},
		{Op: "replace", Find: "Gamma block.", With: "Gamma block!"},
	})
	if !errors.Is(err, pmdoc.ErrTargetNotFound) {
		t.Fatalf("quote miss = %v, want ErrTargetNotFound", err)
	}
	message := err.Error()
	for _, want := range []string{"operation 1", `"Gamma block."`, "nearest blocks"} {
		if !strings.Contains(message, want) {
			t.Fatalf("quote miss = %q, want it to name %q", message, want)
		}
	}
	if strings.Contains(message, "pmdoc") {
		t.Fatalf("quote miss = %q, want no internal package name", message)
	}
}

// A quote cut before its closing `**` can never match rendered text, and "quote not found"
// sends the agent looking for the wrong mistake.
func TestApplyOperationRefusesAFindWithAnUnbalancedInlineMark(t *testing.T) {
	tree, err := parseInput("1. **SUPERSEDED by the ruling** and the rest.\n")
	if err != nil {
		t.Fatal(err)
	}
	for _, find := range []string{"**SUPERSEDED by the", "and the `rest"} {
		_, err = applyOperation(tree, model.EditOp{Op: "replace", Find: find, With: "x"})
		var invalid *ErrInvalidOp
		if !errors.As(err, &invalid) || invalid.Field != "find" {
			t.Fatalf("unbalanced find %q = %v, want invalid find", find, err)
		}
		if !strings.Contains(invalid.Reason, "balanced") {
			t.Fatalf("unbalanced find %q reason = %q, want the balance rule", find, invalid.Reason)
		}
	}
}

// blockAskHarness seeds a document with the ask fixture, settles it, and returns the ask's
// id plus helpers that settle the room and count the events on that ask.
func blockAskHarness(t *testing.T) (*Service, string, string, func(), func() int) {
	t.Helper()
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	seedServiceText(t, service, artifactID, askFixture+"\nContext ends with no drift.\n")
	service.settleRoom(artifactID, 0)
	var askID string
	if err := service.store.Pool.QueryRow(context.Background(), `
		select id::text from asks where block_artifact_id = $1 and block_id = 'decision'
	`, artifactID).Scan(&askID); err != nil {
		t.Fatalf("read indexed ask: %v", err)
	}
	settle := func() { settleCurrentGeneration(t, service, artifactID) }
	askEvents := func() int {
		var count int
		if err := service.store.Pool.QueryRow(context.Background(), `
			select count(*) from events where type like 'ask.%' and payload->>'id' = $1
		`, askID).Scan(&count); err != nil {
			t.Fatalf("count ask events: %v", err)
		}
		return count
	}
	if got := askEvents(); got != 1 {
		t.Fatalf("ask events after indexing = %d, want the opened event only", got)
	}
	return service, artifactID, askID, settle, askEvents
}

func askRowState(t *testing.T, service *Service, askID string) (state string, resolutionKind *string) {
	t.Helper()
	if err := service.store.Pool.QueryRow(context.Background(), `
		select state, resolution->>'kind' from asks where id = $1
	`, askID).Scan(&state, &resolutionKind); err != nil {
		t.Fatalf("read ask row: %v", err)
	}
	return state, resolutionKind
}

var editingSession = model.Actor{Kind: "session", ID: "session-0123456789abcdef"}

func TestApplyOpsMovingAnOpenAskBlockKeepsItsAsk(t *testing.T) {
	service, artifactID, askID, settle, askEvents := blockAskHarness(t)
	if _, err := service.ApplyOps(context.Background(), artifactID, []model.EditOp{{Op: "move", Block: "decision", After: "no drift."}}, editingSession, nil); err != nil {
		t.Fatalf("move ask block: %v", err)
	}
	waitForDocumentText(t, service, artifactID, "Context ends with no drift.\n\n"+askFixture)
	settle()
	if state, _ := askRowState(t, service, askID); state != "open" {
		t.Fatalf("moved ask state = %q, want open", state)
	}
	if got := askEvents(); got != 1 {
		t.Fatalf("ask events after move = %d, want no retract or reopen", got)
	}
}

func TestApplyOpsMovingAnAnsweredAskBlockKeepsItsAnswer(t *testing.T) {
	service, artifactID, askID, settle, askEvents := blockAskHarness(t)
	// Answer the ask the way the answer handler does: the row first, then the block's
	// server-owned attributes.
	answeredAt := time.Date(2026, 9, 15, 17, 37, 34, 0, time.UTC)
	answerJSON, err := json.Marshal(model.AskAnswer{User: "alice", Selected: []string{"REST"}, At: answeredAt})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.store.Pool.Exec(context.Background(), `update asks set state = 'answered', answer = $2 where id = $1`, askID, answerJSON); err != nil {
		t.Fatalf("answer indexed ask: %v", err)
	}
	if err := service.SetBlockAttributes(context.Background(), artifactID, "decision", map[string]any{
		"state": "answered", "answered_by": "alice", "answered_at": answeredAt.Format(time.RFC3339Nano), "selected": []string{"REST"},
	}, model.Actor{Kind: "user", ID: "alice"}); err != nil {
		t.Fatalf("write answered attributes: %v", err)
	}
	const answeredAsk = ":::ask{#decision urgency=\"high\" multiple=\"false\" state=\"answered\" answered_by=\"alice\" answered_at=\"2026-09-15T17:37:34Z\" selected=\"[&#x22;REST&#x22;]\"}\nWhich transport?\n:::\n"
	waitForDocumentText(t, service, artifactID, answeredAsk+"\nContext ends with no drift.\n")
	settle()

	if _, err := service.ApplyOps(context.Background(), artifactID, []model.EditOp{{Op: "move", Block: "decision", After: "no drift."}}, editingSession, nil); err != nil {
		t.Fatalf("move ask block: %v", err)
	}
	waitForDocumentText(t, service, artifactID, "Context ends with no drift.\n\n"+answeredAsk)
	settle()
	if state, _ := askRowState(t, service, askID); state != "answered" {
		t.Fatalf("moved ask state = %q, want answered", state)
	}
	if got := askEvents(); got != 1 {
		t.Fatalf("ask events after move = %d, want no retract, reopen, or repair", got)
	}

	// Deleting the answered block leaves the answered row as the record; a resolved row
	// carrying an answer is what the asks table forbids, and settlement must not fail on it.
	if _, err := service.ApplyOps(context.Background(), artifactID, []model.EditOp{{Op: "delete", Block: "decision"}}, editingSession, nil); err != nil {
		t.Fatalf("delete answered ask block: %v", err)
	}
	waitForDocumentText(t, service, artifactID, "Context ends with no drift.\n")
	settle()
	if state, resolution := askRowState(t, service, askID); state != "answered" || resolution != nil {
		t.Fatalf("deleted answered ask state=%q resolution=%v, want answered without a resolution", state, resolution)
	}
	if got := askEvents(); got != 1 {
		t.Fatalf("ask events after deleting an answered ask = %d, want none", got)
	}
}

func TestApplyOpsDeletingAnOpenAskBlockByIDRetractsItsAsk(t *testing.T) {
	service, artifactID, askID, settle, askEvents := blockAskHarness(t)
	if _, err := service.ApplyOps(context.Background(), artifactID, []model.EditOp{{Op: "delete", Block: "decision"}}, editingSession, nil); err != nil {
		t.Fatalf("delete ask block: %v", err)
	}
	waitForDocumentText(t, service, artifactID, "Context ends with no drift.\n")
	settle()
	state, resolution := askRowState(t, service, askID)
	if state != "resolved" || resolution == nil || *resolution != "retracted" {
		t.Fatalf("deleted ask state=%q resolution=%v, want retracted", state, resolution)
	}
	if got := askEvents(); got != 2 {
		t.Fatalf("ask events after delete = %d, want opened and resolved", got)
	}
}
