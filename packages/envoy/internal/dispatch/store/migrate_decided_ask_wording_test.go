package store

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// decidedAskWordingVersion is the migration that gives each ask reworded after its decision back
// the wording it was decided on (LEGION-651).
const decidedAskWordingVersion = 91

// TestMigrationGivesADecidedAskBackItsDecidedWording runs the shipped migration and its census,
// never a constant copy of them, over five asks stored at the version before: one answered and
// then reworded by its document (the one it repairs), one answered again after the rewording (a
// decision on the new wording, which stands), one reworded while open (no decision before it), one
// reopened between its decision and the rewording (the rewording was of an open ask), and one
// answered and reworded and already back on its decided wording (nothing to write).
func TestMigrationGivesADecidedAskBackItsDecidedWording(t *testing.T) {
	ctx := context.Background()
	store := openEmptyTestStore(t)
	if err := store.migrate(ctx, migrationsThrough(t, decidedAskWordingVersion-1)); err != nil {
		t.Fatalf("migrate through %d: %v", decidedAskWordingVersion-1, err)
	}
	const (
		reworded  = "5a660655-04ad-4ce0-8a9b-93dd03c412b1"
		reanswer  = "5a660655-04ad-4ce0-8a9b-93dd03c412b2"
		openEdit  = "5a660655-04ad-4ce0-8a9b-93dd03c412b3"
		reopened  = "5a660655-04ad-4ce0-8a9b-93dd03c412b4"
		unchanged = "5a660655-04ad-4ce0-8a9b-93dd03c412b5"
	)
	monday := `{"question":"Ship on Monday?","options":[{"label":"Yes","description":"Monday."},{"label":"No","description":"Later."}],"multiple":false,"urgency":"med"}`
	tuesday := `{"question":"Ship on Tuesday?","options":[{"label":"Tuesday","description":"Ship it."},{"label":"No","description":"Later."}],"multiple":false,"urgency":"high"}`
	answer := `{"user":"alice","selected":["Yes"],"at":"2026-10-09T09:00:00Z"}`
	if _, err := store.Pool.Exec(ctx, `
		insert into projects (key, name) values ('CORE', 'Core');
		insert into issues (key, project_key, number, title, created_by, rank)
			values ('CORE-1', 'CORE', 1, 'Spec', '{"kind":"session","id":"s"}', 'U');
	`); err != nil {
		t.Fatalf("seed the issue: %v", err)
	}
	// ask stores one row as the code before the migration left it: its question, options and urgency
	// as wording says, in state with answer.
	ask := func(id, wording, urgency, state string, answer any) {
		t.Helper()
		if _, err := store.Pool.Exec(ctx, `
			insert into asks (id, issue_key, author, question, options, multiple, urgency, state, answer)
			values ($1, 'CORE-1', '{"kind":"session","id":"s"}', $2::jsonb->>'question', $2::jsonb->'options',
				false, $3, $4, $5::jsonb)
		`, id, wording, urgency, state, answer); err != nil {
			t.Fatalf("seed ask %s: %v", id, err)
		}
	}
	ask(reworded, tuesday, "high", "answered", answer)
	ask(reanswer, tuesday, "high", "answered", answer)
	ask(openEdit, tuesday, "high", "open", nil)
	ask(reopened, tuesday, "high", "open", nil)
	ask(unchanged, monday, "med", "answered", answer)
	seq := 0
	event := func(kind, payload string) {
		t.Helper()
		seq++
		if _, err := store.Pool.Exec(ctx, `
			insert into events (issue_key, seq, type, actor, payload, notify)
			values ('CORE-1', $1, $2, '{"kind":"system","id":"document-settlement"}', $3::jsonb, false)
		`, seq, kind, payload); err != nil {
			t.Fatalf("seed %s: %v", kind, err)
		}
	}
	opened := func(id string) { event("ask.opened", `{"id":"`+id+`",`+monday[1:]) }
	answered := func(id string) { event("ask.answered", `{"id":"`+id+`","answer":`+answer+`}`) }
	edited := func(id string) {
		event("ask.edited", `{"id":"`+id+`",`+tuesday[1:len(tuesday)-1]+`,"previous":`+monday+`}`)
	}
	opened(reworded)
	answered(reworded)
	edited(reworded)
	opened(reanswer)
	answered(reanswer)
	edited(reanswer)
	answered(reanswer)
	opened(openEdit)
	edited(openEdit)
	opened(reopened)
	answered(reopened)
	event("ask.opened", `{"id":"`+reopened+`",`+monday[1:])
	edited(reopened)
	opened(unchanged)
	answered(unchanged)
	edited(unchanged)

	census, err := os.ReadFile(filepath.Join("migrations", "0091_decided_ask_wording.census.sql"))
	if err != nil {
		t.Fatalf("read the census: %v", err)
	}
	var counted int
	if err := store.Pool.QueryRow(ctx, string(census)).Scan(&counted); err != nil {
		t.Fatalf("run the census: %v", err)
	}
	if counted != 1 {
		t.Errorf("the census counts %d asks, want the one reworded after its answer", counted)
	}

	if err := store.migrate(ctx, migrationsThrough(t, decidedAskWordingVersion)); err != nil {
		t.Fatalf("migrate to %d: %v", decidedAskWordingVersion, err)
	}
	wantQuestion := map[string]string{
		reworded:  "Ship on Monday?",
		reanswer:  "Ship on Tuesday?",
		openEdit:  "Ship on Tuesday?",
		reopened:  "Ship on Tuesday?",
		unchanged: "Ship on Monday?",
	}
	for id, want := range wantQuestion {
		var question, label, urgency, state string
		if err := store.Pool.QueryRow(ctx, `
			select question, options->0->>'label', urgency, state from asks where id = $1
		`, id).Scan(&question, &label, &urgency, &state); err != nil {
			t.Fatalf("read ask %s: %v", id, err)
		}
		if question != want {
			t.Errorf("ask %s asks %q after the migration, want %q", id, question, want)
		}
		if id == reworded && (label != "Yes" || urgency != "med" || state != "answered") {
			t.Errorf("the repaired ask reads %s %s %s, want its decided options and urgency, still answered", label, urgency, state)
		}
	}
	if err := store.Pool.QueryRow(ctx, string(census)).Scan(&counted); err != nil {
		t.Fatalf("run the census after the migration: %v", err)
	}
	if counted != 0 {
		t.Errorf("the census counts %d asks after the migration, want none", counted)
	}
}
