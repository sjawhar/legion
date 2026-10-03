package api

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/reearth/ygo/crdt"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/docs/docstest"
	"github.com/sjawhar/envoy/internal/dispatch/peoplemigration"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// migrationPeople maps every login this package's fixtures act under to an email.
var migrationPeople = peoplemigration.Map{"alice": "alice@example.com", "ada-example": "ada@example.com"}

// answeredUnderLogin opens an issue whose spec holds an ask Ada answered under her GitHub login,
// the way Dispatch's GitHub sign-in left it: the ask row's answer and the block's answered_by
// both name the login.
func answeredUnderLogin(t *testing.T, handler http.Handler, database *store.Store, documentService *docs.Service) string {
	t.Helper()
	issue := createInteractionIssue(t, handler, "TEST", "Spec", "Before\n")
	answerIndexedAsk(t, handler, database, documentService, issue.Key, issue.PrimaryArtifactID, "Ada-Example")
	waitForLiveText(t, documentService, issue.PrimaryArtifactID, `answered_by="ada-example"`)
	return issue.PrimaryArtifactID
}

// documentServer serves documentService's rooms to browser peers.
func documentServer(t *testing.T, documentService *docs.Service, artifactID string) (string, *servedSockets) {
	t.Helper()
	sockets := &servedSockets{finished: make(map[string]chan struct{})}
	server := httptest.NewServer(sockets.serve(documentService.ServeHTTP))
	t.Cleanup(server.Close)
	return "ws" + strings.TrimPrefix(server.URL, "http") + "/ws/doc/" + artifactID, sockets
}

func runPeopleMigration(t *testing.T, database *store.Store, documentService *docs.Service) {
	t.Helper()
	var out bytes.Buffer
	if err := peoplemigration.Run(context.Background(), database, documentService, migrationPeople, &out); err != nil {
		t.Fatalf("migrate people: %v\n%s", err, out.String())
	}
}

// renamedText is the live document as the migration should leave it: as it is now, with the
// answered ask naming Ada's email in place of her login.
func renamedText(t *testing.T, documentService *docs.Service, artifactID string) string {
	t.Helper()
	text, err := documentService.Text(context.Background(), artifactID)
	if err != nil {
		t.Fatalf("read the document: %v", err)
	}
	if strings.Count(text, `answered_by="ada-example"`) != 1 {
		t.Fatalf("the document %q does not hold the one answered ask", text)
	}
	return strings.Replace(text, `answered_by="ada-example"`, `answered_by="ada@example.com"`, 1)
}

// A browser with the document open converges on the rename the migration appends, whether its
// room is the one the migration writes through or, as when the migration runs with Dispatch
// scaled to 0, it kept the document across the outage, typed into it meanwhile, and reconnects to
// the Dispatch that comes back: the rename and what it typed both hold, on both sides.
func TestAPeopleMigrationConvergesWithABrowserHoldingTheDocument(t *testing.T) {
	t.Run("connected to the room the migration writes through", func(t *testing.T) {
		documentService, handler, database := browserDocumentService(t)
		artifactID := answeredUnderLogin(t, handler, database, documentService)
		want := renamedText(t, documentService, artifactID)
		wsURL, sockets := documentServer(t, documentService, artifactID)
		peer := &syncedPeer{wsURL: wsURL, sockets: sockets, headers: http.Header{"X-Dispatch-User": []string{"alice"}}, artifactID: artifactID, doc: crdt.New()}
		peer.connect(t)
		t.Cleanup(peer.close)

		runPeopleMigration(t, database, documentService)

		peer.barrier(t)
		if rendered := renderDocument(t, peer.doc); rendered != want {
			t.Fatalf("the browser holds %q, want %q", rendered, want)
		}
		if live, err := documentService.Text(context.Background(), artifactID); err != nil || live != want {
			t.Fatalf("the room renders %q (%v), want %q", live, err, want)
		}
	})

	t.Run("kept across the outage the migration runs in", func(t *testing.T) {
		before, handler, database := browserDocumentService(t)
		artifactID := answeredUnderLogin(t, handler, database, before)
		want := renamedText(t, before, artifactID) + "\nTyped while Dispatch was down.\n"
		wsURL, sockets := documentServer(t, before, artifactID)
		peer := &syncedPeer{wsURL: wsURL, sockets: sockets, headers: http.Header{"X-Dispatch-User": []string{"alice"}}, artifactID: artifactID, doc: crdt.New()}
		peer.connect(t)
		peer.close()
		if err := before.Shutdown(context.Background()); err != nil {
			t.Fatalf("scale Dispatch to 0: %v", err)
		}
		typed, err := pmdoc.Parse("Typed while Dispatch was down.\n")
		if err != nil {
			t.Fatal(err)
		}
		fragment := peer.doc.GetXmlFragment("prosemirror")
		var typeErr error
		update := docstest.Transact(peer.doc, func(transaction *crdt.Transaction) {
			tree, err := pmdoc.ReadInTransaction(transaction, fragment)
			if err != nil {
				typeErr = err
				return
			}
			tree.Children = append(tree.Children, typed.Children...)
			typeErr = pmdoc.Update(transaction, fragment, tree)
		})
		if typeErr != nil || update == nil {
			t.Fatalf("type into the kept document: update=%d bytes err=%v", len(update), typeErr)
		}

		migration := docs.New(docs.Deps{Store: database, Settle: time.Hour})
		runPeopleMigration(t, database, migration)
		if err := migration.Shutdown(context.Background()); err != nil {
			t.Fatalf("shut down the migration's documents: %v", err)
		}

		after := docs.New(docs.Deps{Store: database, Settle: time.Hour, Identity: headerIdentity(database)})
		t.Cleanup(func() { _ = after.Shutdown(context.Background()) })
		peer.wsURL, peer.sockets = documentServer(t, after, artifactID)
		peer.connect(t)
		t.Cleanup(peer.close)

		if rendered := renderDocument(t, peer.doc); rendered != want {
			t.Fatalf("the browser holds %q, want %q", rendered, want)
		}
		waitForLiveText(t, after, artifactID, "Typed while Dispatch was down.")
		if live, err := after.Text(context.Background(), artifactID); err != nil || live != want {
			t.Fatalf("the room renders %q (%v), want %q", live, err, want)
		}
	})
}
