package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// A database a release before LEGION-651 wrote holds decided asks whose rows a document's
// rewording reworded after their decision. Dispatch's boot gives each its decided wording back,
// the previous wording of the first ask.edited after its latest decision, and logs the ask, its
// document and the restored question, then the count. Of five asks on one spec, it restores only
// the one answered and then reworded: one answered again after the rewording holds a decision on
// the new wording, one reworded while open and one reopened before its rewording were decided on
// nothing they no longer ask, and one reworded and back again already asks its decided wording.
// The boot marks the restored ask's own spec owed a settlement, whose block still asks the
// rewording beside the decision until that settlement writes the decided wording back, and a
// record copy of the spec taken while the reworded ask's row held the rewording, which showed the
// decision on the reworded question and opens its own ask at that settlement; a document holding
// the block of an ask the boot left as it was owes none.
func TestDispatchBootRestoresADecidedAskAnEarlierReleaseReworded(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	const (
		reworded  = "5a660655-04ad-4ce0-8a9b-93dd03c412b1"
		reanswer  = "5a660655-04ad-4ce0-8a9b-93dd03c412b2"
		openEdit  = "5a660655-04ad-4ce0-8a9b-93dd03c412b3"
		reopened  = "5a660655-04ad-4ce0-8a9b-93dd03c412b4"
		unchanged = "5a660655-04ad-4ce0-8a9b-93dd03c412b5"
		spec      = "5a660655-04ad-4ce0-8a9b-93dd03c41200"
		record    = "5a660655-04ad-4ce0-8a9b-93dd03c41201"
		notes     = "5a660655-04ad-4ce0-8a9b-93dd03c41202"
	)
	monday := `{"question":"Ship on Monday?","options":[{"label":"Yes","description":"Monday."},{"label":"No","description":"Later."}],"multiple":false,"urgency":"med"}`
	tuesday := `{"question":"Ship on Tuesday?","options":[{"label":"Tuesday","description":"Ship it."},{"label":"No","description":"Later."}],"multiple":false,"urgency":"high"}`
	answer := `{"user":"alice","selected":["Yes"],"at":"2026-10-09T09:00:00Z"}`
	for _, statement := range []string{
		`insert into projects (key, name) values ('CORE', 'Core')`,
		`insert into issues (key, project_key, number, title, created_by, rank)
			values ('CORE-1', 'CORE', 1, 'Spec', '{"kind":"session","id":"s"}', 'U')`,
		`insert into artifacts (id, issue_key, project_key, slug, name, kind, is_primary, created_by)
			values ('` + spec + `', 'CORE-1', 'CORE', 'spec', 'spec.md', 'doc', true, '{"kind":"session","id":"s"}')`,
		// The record copy holds the reworded ask's block as its row asked it before the boot; the
		// notes hold the block of the ask answered again after its rewording.
		`insert into artifacts (id, issue_key, project_key, slug, name, kind, created_by) values
			('` + record + `', 'CORE-1', 'CORE', 'record', 'record.md', 'doc', '{"kind":"session","id":"s"}'),
			('` + notes + `', 'CORE-1', 'CORE', 'notes', 'notes.md', 'doc', '{"kind":"session","id":"s"}')`,
		`insert into artifact_versions (artifact_id, number, markdown) values
			('` + record + `', 1, ':::ask{#block-b1 urgency="high" multiple="false" state="answered" copied_from="` + reworded + `"}
Ship on Tuesday?

- Tuesday: Ship it.
- No: Later.
:::
'),
			('` + notes + `', 1, ':::ask{#block-b2 urgency="high" multiple="false"}
Ship on Tuesday?
:::
')`,
	} {
		if _, err := database.Pool.Exec(ctx, statement); err != nil {
			t.Fatalf("seed the issue: %v", err)
		}
	}
	// ask stores one block ask of the spec as a release before LEGION-651 left it: its question,
	// options and urgency as wording says, in state with answer.
	ask := func(id, wording, urgency, state string, answer any) {
		t.Helper()
		if _, err := database.Pool.Exec(ctx, `
			insert into asks (id, issue_key, block_id, block_artifact_id, author, question, options, multiple, urgency, state, answer)
			values ($1, 'CORE-1', $2, $3, '{"kind":"session","id":"s"}', $4::jsonb->>'question', $4::jsonb->'options',
				false, $5, $6, $7::jsonb)
		`, id, "block-"+id[len(id)-2:], spec, wording, urgency, state, answer); err != nil {
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
		if _, err := database.Pool.Exec(ctx, `
			insert into events (issue_key, seq, type, actor, payload, notify, published_at)
			values ('CORE-1', $1, $2, '{"kind":"system","id":"document-settlement"}', $3::jsonb, false, now())
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

	// The settlements the boot arms wait five minutes, so the debts are still there to read.
	process := startDispatchProcessWithSettleDelay(t, database.Pool.Config().ConnString(), 5*time.Minute)
	process.waitHealthy(t)
	process.WaitForOutput(t, `msg="dispatch: restored decided asks' wordings" restored=1`)
	output := process.Output.String()
	if !strings.Contains(output, `msg="dispatch: restored decided asks' wordings" restored=1 copies=1`) {
		t.Errorf("the boot's count names no copy marked owed, want copies=1")
	}
	for line := range strings.Lines(output) {
		if !strings.Contains(line, "restored a decided ask's wording") {
			continue
		}
		if !strings.Contains(line, "ask="+reworded) || !strings.Contains(line, "artifact="+spec) || !strings.Contains(line, `question="Ship on Monday?"`) {
			t.Errorf("the boot logged %q, want only the reworded ask, its document and its restored question", line)
		}
		if strings.Contains(line, "Tuesday") {
			t.Errorf("the boot logged the wording it replaced: %q", line)
		}
	}
	if !strings.Contains(output, "ask="+reworded) {
		t.Errorf("the boot logged no line for the reworded ask:\n%s", output)
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
		if err := database.Pool.QueryRow(ctx, `
			select question, options->0->>'label', urgency, state from asks where id = $1
		`, id).Scan(&question, &label, &urgency, &state); err != nil {
			t.Fatalf("read ask %s: %v", id, err)
		}
		if question != want {
			t.Errorf("ask %s asks %q after the boot, want %q", id, question, want)
		}
		if id == reworded && (label != "Yes" || urgency != "med" || state != "answered") {
			t.Errorf("the restored ask reads %s %s %s, want its decided options and urgency, still answered", label, urgency, state)
		}
	}
	var owed []string
	rows, err := database.Pool.Query(ctx, `select artifact_id::text from doc_settlements_pending order by artifact_id`)
	if err != nil {
		t.Fatalf("read the owed settlements: %v", err)
	}
	for rows.Next() {
		var document string
		if err := rows.Scan(&document); err != nil {
			t.Fatalf("scan an owed settlement: %v", err)
		}
		owed = append(owed, document)
	}
	rows.Close()
	if len(owed) != 2 || owed[0] != spec || owed[1] != record {
		t.Errorf("the documents owing a settlement after the boot are %v, want the spec %s and the record copy %s", owed, spec, record)
	}
	if t.Failed() {
		t.Logf("Dispatch's output:\n%s", output)
	}
}
