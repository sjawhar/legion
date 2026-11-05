package docs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

const approvedUTF8 = "Café ☕ approved\r\nline two\n"
const changedUTF8 = "Changed after approval.\n"

func reviewTextHash(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

func reviewHashFixture(t *testing.T) (*store.Store, model.Artifact) {
	t.Helper()
	database := storetest.Open(t)
	id := createDocument(t, database, "initial")
	ctx := context.Background()
	if _, err := database.Pool.Exec(ctx, `update artifact_versions set markdown = $2 where artifact_id = $1 and number = 1`, id, approvedUTF8); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Pool.Exec(ctx, `insert into artifact_versions (artifact_id, number, markdown, authors) values ($1, 2, $2, '[]')`, id, changedUTF8); err != nil {
		t.Fatal(err)
	}
	artifact, err := reviewedArtifact(ctx, database.Pool, id)
	if err != nil {
		t.Fatal(err)
	}
	return database, artifact
}

func writeHashReview(t *testing.T, database *store.Store, artifact model.Artifact, version int) (model.ArtifactReview, model.Event) {
	t.Helper()
	ctx := context.Background()
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	review, event, err := WriteReview(ctx, tx, events.NewBroker(), artifact, version, "approved", ReviewKindPerson, model.Actor{Kind: "user", ID: "alice"}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	return review, event
}

func requireReviewPin(t *testing.T, review model.ArtifactReview, text, source string) {
	t.Helper()
	want := reviewTextHash(text)
	if review.ContentSHA256 == nil || *review.ContentSHA256 != want || review.ContentSHA256Source == nil || *review.ContentSHA256Source != source {
		t.Fatalf("review pin = %v/%v, want %s/%s", review.ContentSHA256, review.ContentSHA256Source, want, source)
	}
}

// Hashing the latest version instead of the reviewed version breaks this test.
func TestTheReviewHashIsTheVersionsUTF8Bytes(t *testing.T) {
	database, artifact := reviewHashFixture(t)
	review, event := writeHashReview(t, database, artifact, 1)
	requireReviewPin(t, review, approvedUTF8, "insert")
	if reviewTextHash(approvedUTF8) == reviewTextHash(changedUTF8) {
		t.Fatal("fixture texts collide")
	}
	var payload model.ArtifactReviewEventPayload
	raw, err := json.Marshal(event.Payload)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.ContentSHA256 == nil || *payload.ContentSHA256 != reviewTextHash(approvedUTF8) || payload.ContentSHA256Source == nil || *payload.ContentSHA256Source != "insert" {
		t.Fatalf("event pin = %#v", payload)
	}
}

// Removing the insert trigger lets both old-release and forged inserts escape the pin.
func TestAReviewInsertCannotChooseItsHashOrItsSource(t *testing.T) {
	database, artifact := reviewHashFixture(t)
	ctx := context.Background()
	for _, columns := range []string{"", ", content_sha256, content_sha256_source"} {
		values := ""
		if columns != "" {
			values = ", repeat('0', 64), 'backfill'"
		}
		review, err := ScanReview(database.Pool.QueryRow(ctx, `insert into artifact_reviews (artifact_id, version, state, actor`+columns+`) values ($1, 1, 'approved', '{"kind":"user","id":"alice"}'`+values+`) returning `+ReviewColumns, artifact.ID))
		if err != nil {
			t.Fatal(err)
		}
		requireReviewPin(t, review, approvedUTF8, "insert")
	}
}

// Removing either update protection lets a pin or its provenance be rewritten.
func TestAReviewsHashNeverChanges(t *testing.T) {
	database, artifact := reviewHashFixture(t)
	review, _ := writeHashReview(t, database, artifact, 1)
	ctx := context.Background()
	for _, assignment := range []string{"content_sha256 = repeat('a', 64)", "content_sha256 = null", "content_sha256_source = 'backfill'"} {
		if _, err := database.Pool.Exec(ctx, `update artifact_reviews set `+assignment+` where id = $1`, review.ID); err == nil || !strings.Contains(err.Error(), "never changes") {
			t.Fatalf("%s: error = %v", assignment, err)
		}
	}
	if _, err := database.Pool.Exec(ctx, `update artifact_reviews set event_seq = event_seq where id = $1`, review.ID); err != nil {
		t.Fatal(err)
	}
}

// Filling the latest version, relabeling insert rows, or refilling pins breaks this test.
func TestFillReviewContentHashesFillsOldRowsOnceAsBackfill(t *testing.T) {
	database, artifact := reviewHashFixture(t)
	inserted, _ := writeHashReview(t, database, artifact, 1)
	ctx := context.Background()
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `alter table artifact_reviews disable trigger artifact_reviews_content_sha256_insert`); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `insert into artifact_reviews (artifact_id, version, state, actor) select $1, n, 'approved', '{"kind":"user","id":"alice"}' from unnest(array[1,2,9]) n`, artifact.ID); err != nil {
		t.Fatal(err)
	}
	// The existing review-sequence constraint trigger is deferred; drain its events
	// before changing the insert trigger's enabled state in this transaction.
	if _, err := tx.Exec(ctx, `set constraints all immediate`); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `alter table artifact_reviews enable trigger artifact_reviews_content_sha256_insert`); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	got, err := FillReviewContentHashes(ctx, database.Pool)
	if err != nil || got != (ReviewHashFill{Filled: 2, Unhashable: 1}) {
		t.Fatalf("fill = %+v, %v", got, err)
	}
	rows, err := database.Pool.Query(ctx, `select `+ReviewColumns+` from artifact_reviews order by version`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		review, err := ScanReview(rows)
		if err != nil {
			t.Fatal(err)
		}
		switch {
		case review.ID == inserted.ID:
			requireReviewPin(t, review, approvedUTF8, "insert")
		case review.Version == 1:
			requireReviewPin(t, review, approvedUTF8, "backfill")
		case review.Version == 2:
			requireReviewPin(t, review, changedUTF8, "backfill")
		default:
			if review.ContentSHA256 != nil || review.ContentSHA256Source != nil {
				t.Fatalf("unhashable review = %#v", review)
			}
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	rows.Close()
	again, err := FillReviewContentHashes(ctx, database.Pool)
	if err != nil || again != (ReviewHashFill{Filled: 0, Unhashable: 1}) {
		t.Fatalf("second fill = %+v, %v", again, err)
	}
}

// R8: missing stored text is not a new refusal on the existing review path.
func TestAReviewOfAVersionWithNoTextIsRecordedWithANullHash(t *testing.T) {
	database, artifact := reviewHashFixture(t)
	review, event := writeHashReview(t, database, artifact, 9)
	if review.ContentSHA256 != nil || review.ContentSHA256Source != nil {
		t.Fatalf("missing version pin = %#v", review)
	}
	var payload map[string]json.RawMessage
	raw, err := json.Marshal(event.Payload)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatal(err)
	}
	if _, ok := payload["content_sha256"]; ok {
		t.Fatalf("missing version event has hash: %s", event.Payload)
	}
	if _, ok := payload["content_sha256_source"]; ok {
		t.Fatalf("missing version event has source: %s", event.Payload)
	}
	stored, err := NewestReview(context.Background(), database.Pool, artifact.ID)
	if err != nil || stored == nil || stored.ID != review.ID || event.Type != "artifact.approved" {
		t.Fatalf("stored=%#v event=%#v error=%v", stored, event, err)
	}
	present, _ := writeHashReview(t, database, artifact, 1)
	requireReviewPin(t, present, approvedUTF8, "insert")
}
