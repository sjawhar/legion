package main

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/reearth/ygo/crdt"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// Dispatch run with DISPATCH_NATS_DISABLED=1 compacts the documents it never opened: a log whose
// rows came of age before the process started - written by the process a restart replaced, which
// held them in a compaction queue that went with it - folds into its head within the sweep's first
// tick, which comes at the boot, and the document reads as it did.
func TestTheCompactionSweepFoldsABackdatedLogWithNATSDisabled(t *testing.T) {
	ctx := context.Background()
	database := storetest.Open(t)
	const artifactID = "5a660655-04ad-4ce0-8a9b-93dd03c41300"
	for _, statement := range []string{
		`insert into projects (key, name) values ('SWEEP', 'Sweep')`,
		`insert into issues (key, project_key, number, title, created_by, rank)
			values ('SWEEP-1', 'SWEEP', 1, 'Sweep', '{"kind":"session","id":"s"}', 'U')`,
		`insert into artifacts (id, issue_key, project_key, slug, name, kind, created_by)
			values ('` + artifactID + `', 'SWEEP-1', 'SWEEP', 'notes', 'notes.md', 'doc', '{"kind":"session","id":"s"}')`,
		`insert into artifact_versions (artifact_id, number, markdown) values ('` + artifactID + `', 1, 'First.
')`,
	} {
		if _, err := database.Pool.Exec(ctx, statement); err != nil {
			t.Fatalf("seed the document: %v", err)
		}
	}
	versioned := docs.NewPgVersioned(database)
	document := crdt.New()
	fragment := document.GetXmlFragment("prosemirror")
	for _, markdown := range []string{"First.\n", "First.\n\nSecond.\n", "Third.\n"} {
		tree, err := pmdoc.Parse(markdown)
		if err != nil {
			t.Fatalf("parse %q: %v", markdown, err)
		}
		before := document.StateVector()
		if err := document.TransactE(func(txn *crdt.Transaction) error { return pmdoc.Update(txn, fragment, tree) }); err != nil {
			t.Fatalf("write %q: %v", markdown, err)
		}
		if _, err := versioned.AppendUpdate(ctx, artifactID, crdt.EncodeStateAsUpdateV1(document, before)); err != nil {
			t.Fatalf("append %q: %v", markdown, err)
		}
	}
	// The appends owe no settlement for the boot to resume: a settlement could write the
	// document again, a young row this test would then have to tell apart from the fold.
	if _, err := database.Pool.Exec(ctx, `delete from doc_settlements_pending where artifact_id = $1`, artifactID); err != nil {
		t.Fatalf("clear the appends' settlement marker: %v", err)
	}
	if _, err := database.Pool.Exec(ctx, `update doc_updates set created_at = now() - interval '1 hour' where artifact_id = $1`, artifactID); err != nil {
		t.Fatalf("backdate the document's rows: %v", err)
	}
	var head int64
	if err := database.Pool.QueryRow(ctx, `select max(version) from doc_updates where artifact_id = $1`, artifactID).Scan(&head); err != nil {
		t.Fatalf("read the document's head: %v", err)
	}

	process := startDispatchProcess(t, database.Pool.Config().ConnString())
	process.waitHealthy(t)
	var rows, version int64
	for deadline := time.Now().Add(45 * time.Second); ; time.Sleep(200 * time.Millisecond) {
		if err := database.Pool.QueryRow(ctx,
			`select count(*), coalesce(max(version), 0) from doc_updates where artifact_id = $1`, artifactID,
		).Scan(&rows, &version); err != nil {
			t.Fatalf("count the document's rows: %v", err)
		}
		if rows == 1 || time.Now().After(deadline) {
			break
		}
	}
	if rows != 1 || version != head {
		t.Fatalf("the document holds %d rows up to version %d after 45 s, want its log folded into the head %d alone:\n%s",
			rows, version, head, process.Output.String())
	}
	answer := process.call(t, http.MethodGet, "/api/v1/artifacts/"+artifactID+"/text", "", http.StatusOK)
	var text struct {
		Markdown string `json:"markdown"`
	}
	if err := json.Unmarshal(answer, &text); err != nil {
		t.Fatalf("decode the document's text: %v: %s", err, answer)
	}
	if text.Markdown != "Third.\n" {
		t.Fatalf("the swept document reads %q, want %q", text.Markdown, "Third.\n")
	}
}
