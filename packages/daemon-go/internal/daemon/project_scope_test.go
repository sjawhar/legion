package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/store"
)

// Daemons of several projects share one database, and admission is each project's own. Another
// project's slots count against none of this daemon's cap, its waiting root is not this daemon's
// to promote, its stale slot is not this daemon's to release, and its outbox row, due before any of
// this daemon's, is not this daemon's to run: none of its issues is this daemon's to touch in
// Dispatch.
func TestAnotherProjectsIssuesAreNotThisDaemonsToAdmit(t *testing.T) {
	cfg := workflowConfig(t, workflowNATS(t))
	cfg.AdmissionCap = 2
	other := "OTHER" + randomSuffix(t)
	running, stale, waiting := other+"-1", other+"-2", other+"-3"
	own := cfg.Project + "-1"

	ctx := context.Background()
	st, err := store.Open(ctx, cfg.PostgresDSN)
	if err != nil {
		t.Fatalf("open the shared store: %v", err)
	}
	t.Cleanup(st.Close)
	if _, err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate the shared store: %v", err)
	}
	records := record.NewStore()
	now := time.Now().UTC()
	// A slot's index is unique across every project's slots, so the seeded ones take indices above
	// any the shared database already holds.
	var base int
	root := func(key, status, rank string) record.Issue {
		return record.Issue{Key: key, Tree: key, Project: other, Title: "Another project's root", Phase: phase.Admitted, Generation: 1, Status: status, Rank: rank}
	}
	if err := pgx.BeginFunc(ctx, st.Pool(), func(tx pgx.Tx) error {
		taken, err := records.Slots(ctx, tx)
		if err != nil {
			return err
		}
		for _, slot := range taken {
			base = max(base, slot.Index+1)
		}
		for _, issue := range []record.Issue{root(running, "in_progress", "a"), root(stale, "backlog", "b"), root(waiting, "todo", "0")} {
			if err := records.PutIssue(ctx, tx, issue); err != nil {
				return err
			}
		}
		for index, key := range []string{running, stale} {
			if err := records.PutSlot(ctx, tx, record.Slot{Issue: key, Index: base + index, AdmittedAt: now}); err != nil {
				return err
			}
		}
		// Due a minute ago, so an outbox that claimed across projects would run it before any row
		// this daemon's admission enqueues.
		row, err := record.NewOutboxRow(running, record.StatusWrite{Status: "in_progress", ObservedStatus: "todo"}, now.Add(-time.Minute))
		if err != nil {
			return err
		}
		return records.Enqueue(ctx, tx, row)
	}); err != nil {
		t.Fatalf("seed another project's admission: %v", err)
	}
	t.Cleanup(func() {
		if _, err := st.Pool().Exec(context.Background(), `delete from outbox where issue like $1`, other+"-%"); err != nil {
			t.Errorf("remove the seeded outbox row: %v", err)
		}
	})

	// ownRun closes when the outbox first reaches Dispatch for this project's issue: its first batch
	// after admission has run, and the other project's older row would have run before it.
	ownRun := make(chan struct{})
	var ownOnce sync.Once
	dispatchServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/v1/issues/"+own) {
			ownOnce.Do(func() { close(ownRun) })
		}
		if strings.Contains(r.URL.Path, other) {
			t.Errorf("the daemon asked Dispatch about another project's issue: %s %s", r.Method, r.URL.Path)
		}
		if r.Method != http.MethodGet || r.URL.Path != "/api/v1/issues" {
			// This project's own writes after admission are not what this test is about.
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode([]map[string]any{{"key": own, "title": "This project's root", "status": "todo", "parent": nil, "rank": "m"}}); err != nil {
			t.Errorf("write Dispatch issues: %v", err)
		}
	}))
	t.Cleanup(dispatchServer.Close)
	cfg.DispatchURL = dispatchServer.URL
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() {
		done <- run(runCtx, cfg, quietLogger(), overrides{
			listen:  heldListen,
			runtime: fakeRuntime(fake.NewRuntime(), &built{}).runtime, clock: stillClock{}, workflowTokens: &workflowTokenRecorder{},
		})
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("run: %v", err)
			}
		case <-time.After(15 * time.Second):
			t.Error("daemon did not stop")
		}
	})
	awaitHealthz(t, cfg, done)

	var active []string
	deadline := time.Now().Add(60 * time.Second)
	for !slices.Equal(active, []string{own}) {
		if time.Now().After(deadline) {
			t.Fatalf("admission is %v, want %s alone: another project's slots and waiting root are not this daemon's", active, own)
		}
		time.Sleep(50 * time.Millisecond)
		var state struct {
			Admission struct {
				Active []string `json:"active"`
			} `json:"admission"`
		}
		response, err := pollClient.Get("http://127.0.0.1:" + strconv.Itoa(cfg.Port) + "/legion/v1/state")
		if err != nil {
			continue
		}
		if err := json.NewDecoder(response.Body).Decode(&state); err == nil && response.StatusCode == http.StatusOK {
			active = state.Admission.Active
		}
		response.Body.Close()
	}

	select {
	case <-ownRun:
	case <-time.After(60 * time.Second):
		t.Fatalf("the outbox never reached Dispatch for %s after admitting it", own)
	}

	var slots []string
	if err := pgx.BeginFunc(ctx, st.Pool(), func(tx pgx.Tx) error {
		all, err := records.Slots(ctx, tx)
		for _, slot := range all {
			if strings.HasPrefix(slot.Issue, other+"-") {
				slots = append(slots, fmt.Sprintf("%s@%d", slot.Issue, slot.Index))
			}
		}
		return err
	}); err != nil {
		t.Fatalf("read the slots: %v", err)
	}
	if want := []string{fmt.Sprintf("%s@%d", running, base), fmt.Sprintf("%s@%d", stale, base+1)}; !reflect.DeepEqual(slots, want) {
		t.Errorf("another project's slots = %v, want %v, untouched", slots, want)
	}
}
