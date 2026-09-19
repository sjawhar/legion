package docs

import (
	"context"
	"errors"
	"github.com/reearth/ygo/crdt"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

func TestConditionalDocumentEditDoesNotOverwriteWriterDuringTableAnchorCheck(t *testing.T) {
	for _, test := range []struct {
		name         string
		precondition bool
		want         string
	}{
		{name: "conditional", precondition: true, want: "HUMAN WRITE"},
		{name: "unconditional baseline", want: "keep"},
	} {
		t.Run(test.name, func(t *testing.T) {
			database := openTestStore(t)
			artifactID := createDocument(t, database, "")
			service := New(Deps{Store: database, Settle: time.Hour})
			t.Cleanup(func() {
				shutdown, cancel := context.WithTimeout(context.Background(), time.Second)
				defer cancel()
				if err := service.Shutdown(shutdown); err != nil {
					t.Errorf("shutdown service: %v", err)
				}
			})
			seedServiceText(t, service, artifactID, "| Key | Value |\n| --- | --- |\n| delete | row |\n| retain | row |\n\nkeep")
			actor := model.Actor{Kind: "user", ID: "alice"}
			if _, err := service.MarkQuote(context.Background(), artifactID, MarkSpec{
				Kind: MarkComment,
				ID:   "00000000-0000-4000-8000-000000000003",
				By:   actor,
			}, "delete", nil); err != nil {
				t.Fatalf("mark table cell: %v", err)
			}
			markdown, blocks, err := service.TextWithBlocks(context.Background(), artifactID)
			if err != nil {
				t.Fatalf("read document: %v", err)
			}
			var tableID string
			for _, block := range blocks {
				if block.Type == "table" {
					tableID = block.ID
					break
				}
			}
			if tableID == "" {
				t.Fatalf("blocks = %#v, want table", blocks)
			}

			gate, err := database.Pool.Begin(context.Background())
			if err != nil {
				t.Fatalf("begin asks gate: %v", err)
			}
			defer gate.Rollback(context.Background())
			if _, err := gate.Exec(context.Background(), "lock table asks in access exclusive mode"); err != nil {
				t.Fatalf("lock asks: %v", err)
			}
			conditionalTx, err := database.Pool.Begin(context.Background())
			if err != nil {
				t.Fatalf("begin edit transaction: %v", err)
			}
			defer conditionalTx.Rollback(context.Background())
			result := make(chan error, 1)
			var precondition *model.EditPrecondition
			if test.precondition {
				precondition = &model.EditPrecondition{Document: documentTokenForTest(t, service, artifactID, markdown)}
			}
			go func() {
				_, err := service.ApplyOps(WithTx(context.Background(), conditionalTx), artifactID, []model.EditOp{{
					Op: "delete_row", Block: tableID, Index: []byte("1"),
				}}, actor, precondition)
				result <- err
			}()
			waitForAsksQueryLock(t, database)
			browserMutated := make(chan struct{})
			browserResult := make(chan error, 1)
			go func() {
				browserResult <- browserStyleWrite(service, artifactID, replaceRun("keep", "HUMAN WRITE"), browserMutated)
			}()
			select {
			case <-browserMutated:
			case <-time.After(2 * time.Second):
				t.Fatal("browser writer did not mutate the live tree")
			}
			if err := gate.Commit(context.Background()); err != nil {
				t.Fatalf("release asks gate: %v", err)
			}
			if err := awaitRaceResult(t, browserResult); err != nil {
				t.Fatalf("browser writer: %v", err)
			}
			err = awaitRaceResult(t, result)
			if test.precondition {
				var stale *ErrPreconditionFailed
				if !errors.As(err, &stale) {
					t.Fatalf("conditional edit error = %v, want ErrPreconditionFailed", err)
				}
				if err := conditionalTx.Rollback(context.Background()); err != nil {
					t.Fatalf("rollback stale conditional edit: %v", err)
				}
			} else {
				if err != nil {
					t.Fatalf("unconditional edit: %v", err)
				}
				if err := conditionalTx.Commit(context.Background()); err != nil {
					t.Fatalf("commit unconditional edit: %v", err)
				}
			}
			if err := service.Evict(context.Background(), artifactID); err != nil {
				t.Fatalf("evict resident document: %v", err)
			}
			persisted, err := service.Text(context.Background(), artifactID)
			if err != nil {
				t.Fatalf("read durable document: %v", err)
			}
			if !strings.Contains(persisted, test.want) {
				t.Fatalf("durable document = %q, want %q", persisted, test.want)
			}
		})
	}
}

func browserStyleWrite(service *Service, artifactID string, edit func(*pmdoc.Node) *pmdoc.Node, mutated chan<- struct{}) error {
	doc := service.srv.GetDoc(artifactID)
	if doc == nil {
		return errDocUnloaded
	}
	fragment := doc.GetXmlFragment(fragmentName)
	var writeErr error
	doc.Transact(func(transaction *crdt.Transaction) {
		tree, err := pmdoc.ReadInTransaction(transaction, fragment)
		if err != nil {
			writeErr = err
			return
		}
		writeErr = pmdoc.Update(transaction, fragment, edit(tree))
		if writeErr == nil {
			close(mutated)
		}
	})
	return writeErr
}

func documentTokenForTest(t *testing.T, service *Service, artifactID, markdown string) string {
	t.Helper()
	read, token, err := service.TextWithToken(context.Background(), artifactID)
	if err != nil {
		t.Fatalf("read document token: %v", err)
	}
	if read != markdown || token == "" {
		t.Fatalf("document token read = markdown %q token %q, want markdown %q and token", read, token, markdown)
	}
	return token
}

func waitForAsksQueryLock(t *testing.T, database *store.Store) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		var waiting int
		if err := database.Pool.QueryRow(context.Background(), `
			select count(*)
			from pg_stat_activity
			where wait_event_type = 'Lock' and query like '%from asks%'
		`).Scan(&waiting); err != nil {
			t.Fatalf("inspect asks query lock: %v", err)
		}
		if waiting > 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("conditional edit did not reach the table-anchor query lock")
}

func awaitRaceResult(t *testing.T, result <-chan error) error {
	t.Helper()
	select {
	case err := <-result:
		return err
	case <-time.After(10 * time.Second):
		t.Fatal("conditional edit did not complete")
		return nil
	}
}

// The WebSocket server's update observer persists the browser-style writer.
