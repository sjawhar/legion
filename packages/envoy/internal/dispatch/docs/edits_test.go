package docs

import (
	"context"
	"errors"
	"strings"
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
	}, actor)
	if err != nil || applied != 3 {
		t.Fatalf("applied = %d, %v", applied, err)
	}
	waitForDocumentText(t, service, artifactID, "TWO one\n\n!\n")
	version := waitForDocumentVersion(t, service.store, artifactID, 2)
	if len(version.Authors) != 1 || version.Authors[0] != actor {
		t.Fatalf("authors = %#v", version.Authors)
	}
}

func TestApplyOpsRejectsAmbiguousTargetWithoutChangingDocument(t *testing.T) {
	service, artifactID := newTestService(t)
	seedServiceText(t, service, artifactID, "same same")
	_, err := service.ApplyOps(context.Background(), artifactID, []model.EditOp{{Op: "replace", Find: "same", With: "changed"}}, model.Actor{Kind: "session", ID: "session-0123456789abcdef"})
	var ambiguous *pmdoc.ErrTargetAmbiguous
	if !errors.As(err, &ambiguous) {
		t.Fatalf("ambiguous edit error = %v, want ErrTargetAmbiguous", err)
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
		_, err := service.ApplyOps(context.Background(), artifactID, []model.EditOp{{Op: "insert", Markdown: "!", After: "end"}}, model.Actor{Kind: "session", ID: "session-0123456789abcdef"})
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
	}, model.Actor{Kind: "session", ID: "session-0123456789abcdef"})
	if err != nil {
		t.Fatal(err)
	}
	waitForDocumentText(t, service, artifactID, "- item\n\n# Title\n\nIntro.\n\nBody text. more\n")
}

func TestApplyOpsRejectsMarkdownOutsideProofSchema(t *testing.T) {
	service, artifactID := newTestService(t)
	seedServiceText(t, service, artifactID, "keep")
	_, err := service.ApplyOps(context.Background(), artifactID, []model.EditOp{{Op: "replace", Find: "keep", With: "<details>x</details>"}}, model.Actor{Kind: "session", ID: "session-0123456789abcdef"})
	var invalid *ErrInvalidOp
	if !errors.As(err, &invalid) || invalid.Field != "with" || !strings.Contains(invalid.Reason, "block HTML") {
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
