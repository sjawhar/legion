package docs

import (
	"bytes"
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

const answererOpenAsk = ":::ask{#decision urgency=\"high\" multiple=\"false\" state=\"open\"}\nWhich transport?\n:::\n"

func answererAnsweredAsk(by string) string {
	return ":::ask{#decision urgency=\"high\" multiple=\"false\" state=\"answered\" answered_by=\"" + by +
		"\" answered_at=\"2026-09-15T17:37:34Z\" selected=\"[&#x22;REST&#x22;]\"}\nWhich transport?\n:::\n"
}

// renameAdaExample is the rename a people migration hands RenameAskAnswerers: one login, in any
// case, to its person's email.
func renameAdaExample(value string) (string, bool) {
	if strings.EqualFold(value, "ada-example") {
		return "ada@example.com", true
	}
	return "", false
}

// answeredByAdaExample seeds artifactID with an ask a person answered under the GitHub login
// Dispatch knew them by, as Dispatch leaves one: settlement indexes the block as an ask, the
// answer handler writes the row's answer and then the block's server-owned attributes, and
// settlement versions the answered document.
func answeredByAdaExample(t *testing.T, service *Service, artifactID string) {
	t.Helper()
	seedServiceText(t, service, artifactID, answererOpenAsk)
	settleCurrentGeneration(t, service, artifactID)
	if tag, err := service.store.Pool.Exec(context.Background(), `
		update asks set state = 'answered', answer = '{"user":"Ada-Example","selected":["REST"],"text":null,"at":"2026-09-15T17:37:34Z"}'
		where block_artifact_id = $1 and block_id = 'decision'
	`, artifactID); err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("answer the indexed ask: %v (%d rows)", err, tag.RowsAffected())
	}
	if err := service.SetBlockAttributes(context.Background(), artifactID, "decision", map[string]any{
		"state": "answered", "answered_by": "Ada-Example", "answered_at": "2026-09-15T17:37:34Z", "selected": []string{"REST"},
	}, model.Actor{Kind: "user", ID: "Ada-Example"}); err != nil {
		t.Fatalf("answer the ask block: %v", err)
	}
	waitForPersistedProofText(t, service.store, artifactID, answererAnsweredAsk("Ada-Example"))
	settleCurrentGeneration(t, service, artifactID)
}

// moveAnswerToEmail moves the ask row's answer to Ada's email, as the people migration moves the
// database before it renames any document.
func moveAnswerToEmail(t *testing.T, database *store.Store, artifactID string) {
	t.Helper()
	if _, err := database.Pool.Exec(context.Background(), `
		update asks set answer = jsonb_set(answer, '{user}', '"ada@example.com"') where block_artifact_id = $1
	`, artifactID); err != nil {
		t.Fatalf("move the answer to email: %v", err)
	}
}

type persistedUpdate struct {
	version int64
	update  []byte
}

func persistedUpdates(t *testing.T, database *store.Store, artifactID string) []persistedUpdate {
	t.Helper()
	rows, err := database.Pool.Query(context.Background(), `
		select version, update from doc_updates where artifact_id = $1 order by version
	`, artifactID)
	if err != nil {
		t.Fatalf("read document updates: %v", err)
	}
	defer rows.Close()
	var updates []persistedUpdate
	for rows.Next() {
		var update persistedUpdate
		if err := rows.Scan(&update.version, &update.update); err != nil {
			t.Fatalf("scan document update: %v", err)
		}
		updates = append(updates, update)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate document updates: %v", err)
	}
	return updates
}

func versionMarkdowns(t *testing.T, database *store.Store, artifactID string) []string {
	t.Helper()
	rows, err := database.Pool.Query(context.Background(), `
		select markdown || ' ' || authors::text from artifact_versions where artifact_id = $1 order by number
	`, artifactID)
	if err != nil {
		t.Fatalf("read document versions: %v", err)
	}
	defer rows.Close()
	var versions []string
	for rows.Next() {
		var version string
		if err := rows.Scan(&version); err != nil {
			t.Fatalf("scan document version: %v", err)
		}
		versions = append(versions, version)
	}
	return versions
}

// The person who answered an ask is renamed by one update appended to the document's state: every
// update before it stays as it was, so a browser holding the earlier state merges the rename
// rather than meeting a replaced document. The document's versions are left alone, and settling it
// afterwards versions nothing either: a version for a server-owned attribute would stale every
// approval pinned to the version before it.
func TestRenameAskAnswerersAppendsOneUpdateAndKeepsTheHistory(t *testing.T) {
	database := storetest.Open(t)
	artifactID := createDocument(t, database, "before")
	service := New(Deps{Store: database, Events: events.NewBroker(), Settle: time.Hour})
	t.Cleanup(func() { _ = service.Shutdown(context.Background()) })
	answeredByAdaExample(t, service, artifactID)

	answerers, unreadable, err := service.AskAnswerers(context.Background())
	if err != nil || len(unreadable) != 0 {
		t.Fatalf("read answerers: %v, unreadable %#v", err, unreadable)
	}
	if want := []AskAnswerer{{ArtifactID: artifactID, BlockID: "decision", AnsweredBy: "Ada-Example"}}; !reflect.DeepEqual(answerers, want) {
		t.Fatalf("answerers = %#v, want %#v", answerers, want)
	}

	moveAnswerToEmail(t, database, artifactID)
	before := persistedUpdates(t, database, artifactID)
	versions := versionMarkdowns(t, database, artifactID)
	reports := service.RenameAskAnswerers(context.Background(), []string{artifactID}, renameAdaExample)
	if want := []AnswererRename{{ArtifactID: artifactID, Renamed: 1}}; !reflect.DeepEqual(reports, want) {
		t.Fatalf("rename reports = %#v, want %#v", reports, want)
	}

	after := persistedUpdates(t, database, artifactID)
	if len(after) != len(before)+1 {
		t.Fatalf("document updates = %d after the rename, want the %d before it and one more", len(after), len(before))
	}
	for index, update := range before {
		if after[index].version != update.version || !bytes.Equal(after[index].update, update.update) {
			t.Fatalf("document update %d changed under the rename", update.version)
		}
	}
	waitForPersistedProofText(t, database, artifactID, answererAnsweredAsk("ada@example.com"))
	settleCurrentGeneration(t, service, artifactID)
	if got := versionMarkdowns(t, database, artifactID); !reflect.DeepEqual(got, versions) {
		t.Fatalf("document versions = %q after the rename settled, want them unchanged: %q", got, versions)
	}
	waitForPersistedProofText(t, database, artifactID, answererAnsweredAsk("ada@example.com"))
	after = persistedUpdates(t, database, artifactID)

	// A second rename finds nothing to do and appends nothing.
	again := service.RenameAskAnswerers(context.Background(), []string{artifactID}, renameAdaExample)
	if want := []AnswererRename{{ArtifactID: artifactID}}; !reflect.DeepEqual(again, want) {
		t.Fatalf("second rename reports = %#v, want %#v", again, want)
	}
	if got := persistedUpdates(t, database, artifactID); len(got) != len(after) {
		t.Fatalf("a rename with nothing to rename appended %d updates", len(got)-len(after))
	}
}

// A closed issue's document is still read and renamed: its answered asks name the person too, and
// its room refuses every other write.
func TestRenameAskAnswerersRenamesAClosedIssuesDocument(t *testing.T) {
	database := storetest.Open(t)
	artifactID := createDocument(t, database, "before")
	service := New(Deps{Store: database, Events: events.NewBroker(), Settle: time.Hour})
	t.Cleanup(func() { _ = service.Shutdown(context.Background()) })
	answeredByAdaExample(t, service, artifactID)
	if _, err := database.Pool.Exec(context.Background(), `update issues set closed_at = now() where key = 'DOC-1'`); err != nil {
		t.Fatalf("close document issue: %v", err)
	}
	service.SetIssueClosed(context.Background(), "DOC-1", true)

	answerers, unreadable, err := service.AskAnswerers(context.Background())
	if err != nil || len(unreadable) != 0 || len(answerers) != 1 {
		t.Fatalf("read a closed issue's answerers = %#v, unreadable %#v, %v; want its one answered ask", answerers, unreadable, err)
	}
	moveAnswerToEmail(t, database, artifactID)
	reports := service.RenameAskAnswerers(context.Background(), []string{artifactID}, renameAdaExample)
	if want := []AnswererRename{{ArtifactID: artifactID, Renamed: 1}}; !reflect.DeepEqual(reports, want) {
		t.Fatalf("rename reports = %#v, want %#v", reports, want)
	}
	waitForPersistedProofText(t, database, artifactID, answererAnsweredAsk("ada@example.com"))
}
