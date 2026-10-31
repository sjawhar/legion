package docs

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/reearth/ygo/crdt"
)

// A stored history that does not decode is refused naming the decoder's reason once. A history cut
// mid-structure keeps ygo's reason ("encoding: unexpected end of input"). A history cut where ygo's
// apply has no reason, which it words as "crdt: invalid update: crdt: invalid update", is refused as
// "crdt: invalid update", stated once. The check a rebuild makes, the room's load and the cold read
// each word it so, and each refusal is still an invalid update.
func TestAHistoryThatDoesNotDecodeIsRefusedNamingItsReasonOnce(t *testing.T) {
	row := newProofHistory(t, 101).write("A first paragraph.\n\nA second paragraph, longer than the first.\n")
	cutWhere := func(reasonless bool) []byte {
		t.Helper()
		for length := len(row) - 1; length > 0; length-- {
			err := crdt.ApplyUpdateV1(newDocumentCopy(), row[:length], nil)
			if err != nil && (err.Error() == doubledInvalidUpdate) == reasonless {
				return row[:length]
			}
		}
		t.Fatalf("no cut of the row is refused reasonless=%v by ygo's apply, so nothing here is tested", reasonless)
		return nil
	}
	service, _ := newTestService(t)
	for _, test := range []struct {
		name   string
		stored []byte
		reason string
	}{
		{"a cut ygo names no reason for", cutWhere(true), "crdt: invalid update"},
		{"a cut mid-structure", row[:8], "crdt: invalid update: encoding: unexpected end of input"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := validateUpdate(test.stored, maxUpdateItems); err == nil || err.Error() != test.reason || !errors.Is(err, crdt.ErrInvalidUpdate) {
				t.Fatalf("the history check = %v, want %q, an invalid update", err, test.reason)
			}
			artifactID := createDocument(t, service.store, "cut")
			if _, err := service.store.Pool.Exec(context.Background(), `
				insert into doc_updates (artifact_id, version, update, content_changed) values ($1, 1, $2, true)
			`, artifactID, test.stored); err != nil {
				t.Fatalf("store the cut row: %v", err)
			}
			_, coldRead := service.persistence.(*PgVersioned).LoadDocument(context.Background(), artifactID)
			for what, err := range map[string]error{
				"the room's load": service.warmLiveDocument(context.Background(), artifactID),
				"the cold read":   coldRead,
			} {
				if err == nil || !strings.HasSuffix(err.Error(), ": "+test.reason) || strings.Contains(err.Error(), doubledInvalidUpdate) || !errors.Is(err, crdt.ErrInvalidUpdate) {
					t.Errorf("%s = %v, want an invalid update ending %q", what, err, test.reason)
				}
			}
		})
	}
}
