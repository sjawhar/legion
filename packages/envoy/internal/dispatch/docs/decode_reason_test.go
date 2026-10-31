package docs

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/reearth/ygo/crdt"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// A stored history that does not decode is refused naming the decoder's reason once. A history cut
// mid-structure keeps ygo's reason ("encoding: unexpected end of input"). A history cut where ygo's
// apply has no reason, which it words as "crdt: invalid update: crdt: invalid update", is refused as
// "crdt: invalid update", stated once. Every decode of a stored state words it so: the check a
// rebuild makes, the room's load, the cold read, the admission of a browser's socket (loadTree)
// and the fork a write of a document no room holds makes (coldFork). Each refusal is still an
// invalid update.
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
			storeCut := func() string {
				t.Helper()
				artifactID := createDocument(t, service.store, "cut")
				if _, err := service.store.Pool.Exec(context.Background(), `
					insert into doc_updates (artifact_id, version, update, content_changed) values ($1, 1, $2, true)
				`, artifactID, test.stored); err != nil {
					t.Fatalf("store the cut row: %v", err)
				}
				return artifactID
			}
			artifactID := storeCut()
			_, coldRead := service.persistence.(*PgVersioned).LoadDocument(context.Background(), artifactID)
			// The admission check and the cold write each take a document of their own: the
			// first refusal fails the room, and a failed room answers the next caller its
			// cause rather than decoding the history again.
			_, _, admission := service.loadTree(context.Background(), storeCut())
			_, coldWrite := joinedApplyOps(service, storeCut(),
				[]model.EditOp{{Op: "replace", Find: "first", With: "second"}},
				model.Actor{Kind: "user", ID: "alice"}, nil)
			for what, err := range map[string]error{
				"the room's load":        service.warmLiveDocument(context.Background(), artifactID),
				"the cold read":          coldRead,
				"the socket's admission": admission,
				"the cold write's fork":  coldWrite,
			} {
				if err == nil || !strings.HasSuffix(err.Error(), ": "+test.reason) || strings.Contains(err.Error(), doubledInvalidUpdate) || !errors.Is(err, crdt.ErrInvalidUpdate) {
					t.Errorf("%s = %v, want an invalid update ending %q", what, err, test.reason)
				}
			}
		})
	}
}
