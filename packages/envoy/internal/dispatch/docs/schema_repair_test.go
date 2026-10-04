package docs

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

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
	if err := service.InjectSchemaInvalidForTest(context.Background(), artifactID); err != nil {
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
	if _, err := joinedReplaceText(second, artifactID, "repaired\n", model.Actor{Kind: "user", ID: "alice"}); err != nil {
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
	_, err := joinedReplaceText(second, artifactID, "replacement without the ask\n", model.Actor{Kind: "user", ID: "alice"})
	var invalid *ErrInvalidAskBlock
	if !errors.As(err, &invalid) || !strings.Contains(err.Error(), "keep") {
		t.Fatalf("replacement dropping the open ask: %v, want ErrInvalidAskBlock naming keep", err)
	}
	if _, err := joinedReplaceText(second, artifactID, ask+"\nRepaired.\n", model.Actor{Kind: "user", ID: "alice"}); err != nil {
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

// A repair's markdown is a whole new document, so every ask block it holds is held to the ask
// content rule, as a replacement over a readable document holds each ask it writes: a question
// given only a list is refused, and the unreadable document stays as it was rather than taking
// an ask block the browser editor would drop.
func TestSchemaRepairRefusesAnAskBlockTheContentRuleRefuses(t *testing.T) {
	const broken = ":::ask{#ask-broken}\n- only a list\n- no question\n:::\n"
	for _, test := range []struct {
		name       string
		unreadable bool
	}{
		{"over a readable document", false},
		{"repairing an unreadable document", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			database := storetest.Open(t)
			artifactID := createDocument(t, database, "")
			service := newSchemaRepairService(database)
			t.Cleanup(func() {
				if err := service.Shutdown(context.Background()); err != nil {
					t.Errorf("shutdown document service: %v", err)
				}
			})
			seedServiceText(t, service, artifactID, "before\n")
			if test.unreadable {
				writeSchemaInvalidElement(t, service, artifactID)
			}
			_, err := joinedReplaceText(service, artifactID, broken, model.Actor{Kind: "user", ID: "alice"})
			var invalid *ErrInvalidAskBlock
			if !errors.As(err, &invalid) {
				t.Fatalf("replace with an ask block holding only a list: %v, want ErrInvalidAskBlock", err)
			}
			text, err := service.Text(context.Background(), artifactID)
			if test.unreadable {
				if !errors.Is(err, ErrDocOutsideSchema) {
					t.Fatalf("read after the refused repair: %q (%v), want ErrDocOutsideSchema", text, err)
				}
			} else if err != nil || text != "before\n" {
				t.Fatalf("read after the refused replacement: %q (%v), want \"before\\n\"", text, err)
			}
		})
	}
}

// A stored task item can pass the tree validator while the renderer rejects it: a checked task
// whose empty first paragraph precedes a heading has no markdown representation the browser reads
// back as a task. It remains repairable instead of failing room creation.
func TestStoredRenderOnlySchemaViolationLoadsForRepair(t *testing.T) {
	database := storetest.Open(t)
	artifactID := createDocument(t, database, "")
	if err := AppendRenderOnlySchemaViolationForTest(context.Background(), database, artifactID); err != nil {
		t.Fatal(err)
	}
	service := newSchemaRepairService(database)
	t.Cleanup(func() {
		if err := service.Shutdown(context.Background()); err != nil {
			t.Errorf("shutdown render-only service: %v", err)
		}
	})
	if err := service.warmLiveDocument(context.Background(), artifactID); err != nil {
		t.Fatalf("load render-only violation for repair: %v", err)
	}
	if _, _, err := service.TextWithBlocks(context.Background(), artifactID); !errors.Is(err, ErrDocOutsideSchema) {
		t.Fatalf("read render-only violation: %v, want ErrDocOutsideSchema", err)
	}
}
