package docs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/reearth/ygo/crdt"
	ygws "github.com/reearth/ygo/provider/websocket"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

const (
	regionBlock    = ":::ask{#region urgency=\"med\" multiple=\"false\"}\nWhich region?\n:::\n"
	transportBlock = ":::ask{#transport urgency=\"med\" multiple=\"false\"}\nWhich transport?\n\n- REST: Matches the platform.\n- gRPC: Streams.\n:::\n"
	// otherTransportBlock is a question of its own under transportBlock's id: other options.
	otherTransportBlock = ":::ask{#transport urgency=\"med\" multiple=\"false\"}\nWhich transport?\n\n- NATS: Already deployed.\n- Kafka: Durable.\n:::\n"
	freshBlock          = ":::ask{#fresh urgency=\"med\" multiple=\"false\"}\nShould the copy ask this?\n:::\n"
)

// The close of an issue writes each withdrawn ask's state into its own block and into every copy
// of it on another document of the issue, which keeps naming its source. A copy of an answered
// ask, a block asking something else under a withdrawn ask's id, and a block no settlement has
// indexed yet are left as they are; the last is written resolved only by a write that marks
// unindexed blocks, which names it. No version is written, and the closer is credited beside the
// authors already pending.
func TestWritingIssueAskStatesReachesTheAsksBlocksAndTheirCopiesAlone(t *testing.T) {
	service, original := newTestService(t)
	service.settle = time.Hour
	ctx := context.Background()
	seedServiceText(t, service, original, "Context\n\n"+regionBlock+"\n"+transportBlock+"\n"+shipBlock)
	settleCurrentGeneration(t, service, original)
	region := blockAskOn(t, service, original, "region")
	transport := blockAskOn(t, service, original, "transport")
	ship := blockAskOn(t, service, original, "ship")
	answer, _ := json.Marshal(model.AskAnswer{User: "alice", Selected: []string{"Yes"}, At: time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)})
	if _, err := service.store.Pool.Exec(ctx, `update asks set state = 'answered', answer = $2 where id = $1`, ship.ID, answer); err != nil {
		t.Fatalf("answer the ship ask: %v", err)
	}

	copied := createIssueDocument(t, service.store, 1, "# Copy")
	seedServiceText(t, service, copied, "Context\n\n"+regionBlock+"\n"+otherTransportBlock+"\n"+shipBlock)
	settleCurrentGeneration(t, service, copied)
	if count := asksOn(t, service, copied); count != 1 {
		t.Fatalf("the copy opened %d asks, want one, its own transport question", count)
	}
	regionCopy := `:::ask{#region urgency="med" multiple="false" state="%s" ` + copiedFrom(t, service, region.ID, original) + `}`
	requireCopiedStates(t, service, copied,
		fmt.Sprintf(regionCopy, "open"),
		`:::ask{#ship urgency="med" multiple="false" state="answered" answered_by="alice" answered_at="2026-10-08T12:00:00Z" selected="[&#x22;Yes&#x22;]" `+copiedFrom(t, service, ship.ID, original)+`}`,
		`:::ask{#transport urgency="med" multiple="false" state="open"}`)
	asker := model.Actor{Kind: "session", ID: "fresh-asking-session"}
	writeThroughLedger(t, service, copied, asker, freshBlock, withoutVersion)

	originalBefore := documentText(t, service, original)
	copiedBefore := documentText(t, service, copied)
	versionsBefore := documentVersions(t, service, original, copied)
	closer := model.Actor{Kind: "user", ID: "carol"}
	withdrawn := []model.Ask{withdrawnAsk(region, closer), withdrawnAsk(transport, closer)}

	if marked := writeIssueAskStates(t, service, withdrawn, closer, false); len(marked) != 0 {
		t.Fatalf("a write that marks no unindexed block marked %+v", marked)
	}
	originalWant := strings.NewReplacer(
		`:::ask{#region urgency="med" multiple="false" state="open"}`, `:::ask{#region urgency="med" multiple="false" state="resolved"}`,
		`:::ask{#transport urgency="med" multiple="false" state="open"}`, `:::ask{#transport urgency="med" multiple="false" state="resolved"}`,
	).Replace(originalBefore)
	copiedWant := strings.Replace(copiedBefore, fmt.Sprintf(regionCopy, "open"), fmt.Sprintf(regionCopy, "resolved"), 1)
	if originalWant == originalBefore || copiedWant == copiedBefore {
		t.Fatalf("the documents lack the blocks this test withdraws:\n%s\n%s", originalBefore, copiedBefore)
	}
	requireText(t, service, original, originalWant)
	requireText(t, service, copied, copiedWant)

	marked := writeIssueAskStates(t, service, withdrawn, closer, true)
	if want := []BlockRef{{ArtifactID: copied, BlockID: "fresh"}}; !reflect.DeepEqual(marked, want) {
		t.Fatalf("the sweep marked %+v, want %+v", marked, want)
	}
	freshOpen := `:::ask{#fresh urgency="med" multiple="false" state="open"}`
	if !strings.Contains(copiedWant, freshOpen) {
		t.Fatalf("the copy lacks the unindexed block:\n%s", copiedWant)
	}
	copiedWant = strings.Replace(copiedWant, freshOpen, `:::ask{#fresh urgency="med" multiple="false" state="resolved"}`, 1)
	requireText(t, service, original, originalWant)
	requireText(t, service, copied, copiedWant)
	if marked := writeIssueAskStates(t, service, withdrawn, closer, true); len(marked) != 0 {
		t.Fatalf("a second sweep marked %+v, which the first already wrote", marked)
	}
	requireText(t, service, copied, copiedWant)

	if count := asksOn(t, service, copied); count != 1 {
		t.Fatalf("the copy holds %d asks after the writes, want its one", count)
	}
	if versions := documentVersions(t, service, original, copied); !reflect.DeepEqual(versions, versionsBefore) {
		t.Fatalf("the writes changed the stored versions:\n%v\nwant\n%v", versions, versionsBefore)
	}
	if pending := pendingAuthorRows(t, service.store, original); !slices.Contains(pending, closer) {
		t.Fatalf("the original's pending authors = %+v, want %+v among them", pending, closer)
	}
	if pending := pendingAuthorRows(t, service.store, copied); !slices.Contains(pending, closer) || !slices.Contains(pending, asker) {
		t.Fatalf("the copy's pending authors = %+v, want %+v and %+v among them", pending, closer, asker)
	}
}

// A closed issue's documents refuse every ordinary write, and the close's own still reaches them,
// since its caller holds the issue's row: a room flagged closed that a reader loaded again, and a
// document no room holds, whose write the store keeps.
func TestWritingIssueAskStatesReachesAClosedIssuesDocuments(t *testing.T) {
	for _, resident := range []bool{true, false} {
		name := "no resident room"
		if resident {
			name = "a resident room flagged closed"
		}
		t.Run(name, func(t *testing.T) {
			service, artifactID := newTestService(t)
			service.settle = time.Hour
			ctx := context.Background()
			seedServiceText(t, service, artifactID, "Context\n\n"+regionBlock)
			settleCurrentGeneration(t, service, artifactID)
			region := blockAskOn(t, service, artifactID, "region")
			before := documentText(t, service, artifactID)
			closeTestIssue(t, service)
			if resident {
				if err := service.srv.Apply(withOwnerVerified(ctx), artifactID, func(*crdt.Doc, func(func(*crdt.Transaction))) {}); err != nil && !errors.Is(err, ygws.ErrNoChanges) {
					t.Fatalf("load the closed issue's room: %v", err)
				}
				if service.srv.GetDoc(artifactID) == nil || !service.roomClosed(artifactID) {
					t.Fatal("the closed issue's room is not resident and flagged closed")
				}
			} else if service.srv.GetDoc(artifactID) != nil {
				t.Fatal("the closed issue's room is still resident")
			}
			if _, err := joinedApplyOps(service, artifactID, []model.EditOp{{Op: "replace", Find: "Context", With: "Edited"}}, model.Actor{Kind: "user", ID: "alice"}, nil); !errors.Is(err, ErrIssueClosed) {
				t.Fatalf("an ordinary write to the closed issue's document = %v, want ErrIssueClosed", err)
			}

			closer := model.Actor{Kind: "user", ID: "carol"}
			writeIssueAskStates(t, service, []model.Ask{withdrawnAsk(region, closer)}, closer, false)
			want := strings.Replace(before,
				`:::ask{#region urgency="med" multiple="false" state="open"}`, `:::ask{#region urgency="med" multiple="false" state="resolved"}`, 1)
			if want == before {
				t.Fatalf("the document lacks the withdrawn block:\n%s", before)
			}
			requireText(t, service, artifactID, want)
			closeDocumentRoom(t, service, artifactID)
			requireText(t, service, artifactID, want)
		})
	}
}

// A reopen writes the withdrawn blocks back to open, so a document that had nothing else pending
// renders as its latest version again, and the settlement the close and the reopen left owed
// writes no version.
func TestWritingAReopensAskStatesBackLeavesNoVersionToWrite(t *testing.T) {
	service, original := newTestService(t)
	service.settle = time.Hour
	ctx := context.Background()
	seedServiceText(t, service, original, "Context\n\n"+regionBlock)
	settleCurrentGeneration(t, service, original)
	region := blockAskOn(t, service, original, "region")
	copied := createIssueDocument(t, service.store, 1, "# Copy")
	seedServiceText(t, service, copied, "Copied context\n\n"+regionBlock)
	settleCurrentGeneration(t, service, copied)
	documents := []string{original, copied}
	for _, document := range documents {
		requireSettledAtLatestVersion(t, service, document)
	}
	versionsBefore := documentVersions(t, service, documents...)

	closer := model.Actor{Kind: "user", ID: "carol"}
	withdrawn := withdrawnAsk(region, closer)
	writeIssueAskStates(t, service, []model.Ask{withdrawn}, closer, false)
	resolution, _ := json.Marshal(withdrawn.Resolution)
	if _, err := service.store.Pool.Exec(ctx, `update asks set state = 'resolved', resolution = $2 where id = $1`, region.ID, resolution); err != nil {
		t.Fatalf("withdraw the region ask: %v", err)
	}
	closeTestIssue(t, service)
	for _, document := range documents {
		if text := documentText(t, service, document); !strings.Contains(text, `state="resolved"`) {
			t.Fatalf("the closed issue's document reads the withdrawn block open:\n%s", text)
		}
	}

	reopenTestIssue(t, service)
	if _, err := service.store.Pool.Exec(ctx, `update asks set state = 'open', answer = null, resolution = null where id = $1`, region.ID); err != nil {
		t.Fatalf("restore the region ask: %v", err)
	}
	reopener := model.Actor{Kind: "user", ID: "dave"}
	writeIssueAskStates(t, service, []model.Ask{blockAskOn(t, service, original, "region")}, reopener, false)
	for _, document := range documents {
		if text, latest := documentText(t, service, document), latestVersionMarkdown(t, service, document); text != latest {
			t.Fatalf("the reopened document renders\n%s\nwant its latest version\n%s", text, latest)
		}
		if pending := pendingAuthorRows(t, service.store, document); !slices.Contains(pending, closer) || !slices.Contains(pending, reopener) {
			t.Fatalf("the reopened document's pending authors = %+v, want %+v and %+v among them", pending, closer, reopener)
		}
		settleCurrentGeneration(t, service, document)
		requireSettledAtLatestVersion(t, service, document)
	}
	if versions := documentVersions(t, service, documents...); !reflect.DeepEqual(versions, versionsBefore) {
		t.Fatalf("the close and reopen left versions\n%v\nwant\n%v", versions, versionsBefore)
	}
}

// An ask block written while the settle delay holds its settlement off is an ask, credited to its
// writer, once SettleIssueDocuments returns: the settlement runs on the caller's goroutine.
func TestSettlingAnIssuesDocumentsIndexesABlockWrittenWithinTheSettleDelay(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	ctx := context.Background()
	seedServiceText(t, service, artifactID, "# Decision\n\nContext.\n")
	untouched := createIssueDocument(t, service.store, 1, "# Untouched")
	actor := model.Actor{Kind: "session", ID: "settled-asking-session"}
	writeThroughLedger(t, service, artifactID, actor, freshBlock, withoutVersion)
	if count := asksOn(t, service, artifactID); count != 0 {
		t.Fatalf("the block was indexed before the settlement: %d asks", count)
	}

	if err := service.SettleIssueDocuments(store.WithTransactionTracking(ctx), "DOC-1"); err != nil {
		t.Fatalf("settle the issue's documents: %v", err)
	}
	ask := blockAskOn(t, service, artifactID, "fresh")
	if ask.State != "open" || ask.Author != actor {
		t.Fatalf("the settled block's ask is %s by %+v, want open by %+v", ask.State, ask.Author, actor)
	}
	requireSettledAtLatestVersion(t, service, artifactID)
	if versions := documentVersions(t, service, untouched); len(versions) != 1 {
		t.Fatalf("the document that owed nothing has versions %v, want its first alone", versions)
	}
}

// SettleIssueDocuments opens a transaction of its own for each settlement, so it refuses to run
// inside one, and the block it would have indexed waits for its settlement.
func TestSettlingAnIssuesDocumentsRefusesToRunInsideATransaction(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	seedServiceText(t, service, artifactID, "# Decision\n\nContext.\n")
	writeThroughLedger(t, service, artifactID, model.Actor{Kind: "session", ID: "waiting-session"}, freshBlock, withoutVersion)

	ctx := store.WithTransactionTracking(context.Background())
	tx, err := service.store.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the caller's transaction: %v", err)
	}
	defer tx.Rollback(ctx)
	if err := service.SettleIssueDocuments(ctx, "DOC-1"); !errors.Is(err, store.ErrNestedAcquire) {
		t.Fatalf("settling inside a tracked transaction = %v, want ErrNestedAcquire", err)
	}
	joined, ledger := service.Join(ctx, tx)
	defer ledger.Discard()
	if err := service.SettleIssueDocuments(joined, "DOC-1"); !errors.Is(err, errSettleJoined) {
		t.Fatalf("settling inside a joined transaction = %v, want errSettleJoined", err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatalf("end the caller's transaction: %v", err)
	}
	if count := asksOn(t, service, artifactID); count != 0 {
		t.Fatalf("a refused settlement indexed %d asks", count)
	}
	owed, err := settlementPending(context.Background(), service.store.Pool, artifactID)
	if err != nil || !owed {
		t.Fatalf("the document owes a settlement = %t (%v), want true", owed, err)
	}
}

// WriteIssueAskStates writes only inside a transaction a caller joined.
func TestWritingIssueAskStatesNeedsAJoinedTransaction(t *testing.T) {
	service, _ := newTestService(t)
	if _, err := service.WriteIssueAskStates(context.Background(), "DOC-1", nil, model.Actor{Kind: "user", ID: "carol"}, true); !errors.Is(err, errUnjoined) {
		t.Fatalf("an unjoined write = %v, want errUnjoined", err)
	}
}

// withdrawnAsk is ask as the close of DOC-1 by closer leaves it.
func withdrawnAsk(ask model.Ask, closer model.Actor) model.Ask {
	ask.State = "resolved"
	ask.Answer = nil
	ask.Resolution = &model.AskResolution{Kind: "withdrawn", Actor: closer, Reason: "Withdrawn by the close of DOC-1.", At: time.Now().UTC()}
	return ask
}

// writeIssueAskStates runs WriteIssueAskStates for DOC-1 in a transaction of its own, as the issue
// route does, and commits it.
func writeIssueAskStates(t *testing.T, service *Service, asks []model.Ask, actor model.Actor, markUnindexed bool) []BlockRef {
	t.Helper()
	var marked []BlockRef
	err := joinedWrite(service, func(ctx context.Context) (err error) {
		tx, _ := txFromContext(ctx)
		// The issue route holds the issue's row before it writes any document.
		if _, err := tx.Exec(ctx, `select 1 from issues where key = 'DOC-1' for no key update`); err != nil {
			return fmt.Errorf("lock the issue: %w", err)
		}
		marked, err = service.WriteIssueAskStates(ctx, "DOC-1", asks, actor, markUnindexed)
		return err
	})
	if err != nil {
		t.Fatalf("write the issue's ask states: %v", err)
	}
	return marked
}

func documentText(t *testing.T, service *Service, artifactID string) string {
	t.Helper()
	text, err := service.Text(context.Background(), artifactID)
	if err != nil {
		t.Fatalf("read the document: %v", err)
	}
	return text
}

// documentVersions is every stored version of documents, as document, number and markdown.
func documentVersions(t *testing.T, service *Service, documents ...string) []string {
	t.Helper()
	rows, err := service.store.Pool.Query(context.Background(), `
		select artifact_id::text, number, markdown from artifact_versions
		where artifact_id::text = any($1) order by artifact_id, number
	`, documents)
	if err != nil {
		t.Fatalf("read the documents' versions: %v", err)
	}
	defer rows.Close()
	var versions []string
	for rows.Next() {
		var document, markdown string
		var number int
		if err := rows.Scan(&document, &number, &markdown); err != nil {
			t.Fatalf("scan a document version: %v", err)
		}
		versions = append(versions, fmt.Sprintf("%s #%d %q", document, number, markdown))
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the documents' versions: %v", err)
	}
	return versions
}

func latestVersionMarkdown(t *testing.T, service *Service, artifactID string) string {
	t.Helper()
	var markdown string
	if err := service.store.Pool.QueryRow(context.Background(), `
		select markdown from artifact_versions where artifact_id = $1 order by number desc limit 1
	`, artifactID).Scan(&markdown); err != nil {
		t.Fatalf("read the latest version: %v", err)
	}
	return markdown
}

// requireSettledAtLatestVersion fails unless the document owes no settlement and renders as its
// latest version.
func requireSettledAtLatestVersion(t *testing.T, service *Service, artifactID string) {
	t.Helper()
	owed, err := settlementPending(context.Background(), service.store.Pool, artifactID)
	if err != nil || owed {
		t.Fatalf("the document owes a settlement = %t (%v), want false", owed, err)
	}
	if text, latest := documentText(t, service, artifactID), latestVersionMarkdown(t, service, artifactID); text != latest {
		t.Fatalf("the settled document renders\n%s\nwant its latest version\n%s", text, latest)
	}
}
