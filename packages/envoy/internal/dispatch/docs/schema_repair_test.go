package docs

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/reearth/ygo/crdt"

	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

func newSchemaRepairService(database *store.Store) *Service {
	return New(Deps{
		Store:     database,
		Events:    events.NewBroker(),
		ServerURL: "https://dispatch.example",
		Settle:    20 * time.Millisecond,
	})
}

func writeSchemaInvalidElement(t *testing.T, service *Service, artifactID string) {
	t.Helper()
	if err := service.srv.Apply(context.Background(), artifactID, func(doc *crdt.Doc, transact func(func(*crdt.Transaction))) {
		fragment := doc.GetXmlFragment(fragmentName)
		transact(func(transaction *crdt.Transaction) {
			fragment.InsertElement(transaction, 0, crdt.NewYXmlElement("callout"))
		})
	}); err != nil {
		t.Fatalf("write crafted element: %v", err)
	}
}

// A crafted client can persist a tree outside the Proof schema. After a restart, replacing that
// document from markdown repairs it instead of leaving every replacement permanently refused.
func TestStoredTreeOutsideSchemaIsRepairedByReplacementAfterRestart(t *testing.T) {
	database := storetest.Open(t)
	artifactID := createDocument(t, database, "# First\n")
	first := newSchemaRepairService(database)
	seedServiceText(t, first, artifactID, "before\n")
	writeSchemaInvalidElement(t, first, artifactID)
	if _, err := first.Text(context.Background(), artifactID); !errors.Is(err, ErrDocSchema) {
		t.Fatalf("read crafted tree: %v, want ErrDocSchema", err)
	}
	if err := first.Shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown first service: %v", err)
	}

	second := newSchemaRepairService(database)
	t.Cleanup(func() {
		if err := second.Shutdown(context.Background()); err != nil {
			t.Errorf("shutdown second service: %v", err)
		}
	})
	if _, err := second.Text(context.Background(), artifactID); !errors.Is(err, ErrDocOutsideSchema) {
		t.Fatalf("read crafted tree after restart: %v, want ErrDocOutsideSchema", err)
	}
	if _, err := second.ReplaceText(context.Background(), artifactID, "repaired\n", model.Actor{Kind: "user", ID: "alice"}); err != nil {
		t.Fatalf("replace unreadable document after restart: %v", err)
	}
	if text, err := second.Text(context.Background(), artifactID); err != nil || text != "repaired\n" {
		t.Fatalf("read repaired document: %q (%v), want repaired markdown", text, err)
	}
	if err := second.Shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown repaired service: %v", err)
	}

	third := newSchemaRepairService(database)
	t.Cleanup(func() {
		if err := third.Shutdown(context.Background()); err != nil {
			t.Errorf("shutdown third service: %v", err)
		}
	})
	if text, err := third.Text(context.Background(), artifactID); err != nil || text != "repaired\n" {
		t.Fatalf("read repaired document after second restart: %q (%v), want repaired markdown", text, err)
	}
}

// A repair has no readable previous tree to compare, so it preserves every open ask by its stored
// block ID and refuses a replacement that drops one.
func TestSchemaRepairKeepsOpenAskBlocks(t *testing.T) {
	database := storetest.Open(t)
	artifactID := createDocument(t, database, "")
	first := newSchemaRepairService(database)
	const ask = ":::ask{#keep urgency=\"med\" multiple=\"false\" state=\"open\"}\nKeep this decision?\n:::\n"
	seedServiceText(t, first, artifactID, ask)
	first.settleRoom(artifactID, 0)
	var askID string
	if err := database.Pool.QueryRow(context.Background(), `
		select id::text from asks where block_artifact_id = $1 and block_id = 'keep' and state = 'open'
	`, artifactID).Scan(&askID); err != nil {
		t.Fatalf("read open ask: %v", err)
	}
	writeSchemaInvalidElement(t, first, artifactID)
	if err := first.Shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown first service: %v", err)
	}

	second := newSchemaRepairService(database)
	t.Cleanup(func() {
		if err := second.Shutdown(context.Background()); err != nil {
			t.Errorf("shutdown second service: %v", err)
		}
	})
	_, err := second.ReplaceText(context.Background(), artifactID, "replacement without the ask\n", model.Actor{Kind: "user", ID: "alice"})
	var invalid *ErrInvalidAskBlock
	if !errors.As(err, &invalid) || !strings.Contains(err.Error(), "keep") {
		t.Fatalf("replacement dropping the open ask: %v, want ErrInvalidAskBlock naming keep", err)
	}
	if _, err := second.ReplaceText(context.Background(), artifactID, ask+"\nRepaired.\n", model.Actor{Kind: "user", ID: "alice"}); err != nil {
		t.Fatalf("replacement retaining the open ask: %v", err)
	}
	var state string
	if err := database.Pool.QueryRow(context.Background(), `select state from asks where id = $1`, askID).Scan(&state); err != nil {
		t.Fatalf("read retained ask: %v", err)
	}
	if state != "open" {
		t.Fatalf("retained ask state = %q, want open", state)
	}
}
