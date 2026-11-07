package embedqueue

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/processes"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// liveProcess writes the server_processes row of a server process whose embedder has identity,
// started startedAgo before the database's now() and live now, and returns its instance id.
func liveProcess(t *testing.T, database *store.Store, identity string, startedAgo time.Duration) string {
	t.Helper()
	var id string
	if err := database.Pool.QueryRow(context.Background(), `
		insert into server_processes (instance, signing_keys, schema_version, schema_creatable, embedder_identity, file_store, started_at, heartbeat_at)
		values (gen_random_uuid(), '{fingerprint}', 1, 1, $1, false, now() - make_interval(secs => $2), now())
		returning instance::text`, identity, startedAgo.Seconds()).Scan(&id); err != nil {
		t.Fatalf("add a server process embedding with %q: %v", identity, err)
	}
	return id
}

// expireProcess moves the heartbeat of the process id past the registry's live window, as a
// process that stopped without deregistering leaves it.
func expireProcess(t *testing.T, database *store.Store, id string) {
	t.Helper()
	mustExec(t, database.Pool, `update server_processes set heartbeat_at = now() - make_interval(secs => $2) where instance = $1::uuid`,
		id, (processes.LiveWindow + 5*time.Second).Seconds())
}

// activeIdentityRow is the identity embedder_active records.
func activeIdentityRow(t *testing.T, database *store.Store) string {
	t.Helper()
	var identity string
	if err := database.Pool.QueryRow(context.Background(), `select identity from embedder_active`).Scan(&identity); err != nil {
		t.Fatalf("read embedder_active: %v", err)
	}
	return identity
}

// vectorsByModel counts the rows that hold a vector, by the identity that made it.
func vectorsByModel(t *testing.T, database *store.Store) map[string]int {
	t.Helper()
	rows, err := database.Pool.Query(context.Background(), `select model, count(*) from embeddings where embedding is not null group by model`)
	if err != nil {
		t.Fatalf("count vectors: %v", err)
	}
	defer rows.Close()
	counts := map[string]int{}
	for rows.Next() {
		var model string
		var count int
		if err := rows.Scan(&model, &count); err != nil {
			t.Fatalf("count vectors: %v", err)
		}
		counts[model] = count
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("count vectors: %v", err)
	}
	return counts
}

// Two server processes whose embedders have different identities: the first to look records its
// own as active, and the other drops no vector, embeds no row and sends no canary while the active
// identity's process is live. Once that process's row lapses, the other's identity becomes active,
// and its pass drops the first identity's vectors and its queue embeds them again. When the first
// process is live again, its identity stays inactive: the active one is still carried.
func TestOnlyTheActiveEmbedderIdentityDropsAndEmbeds(t *testing.T) {
	database := storetest.Open(t)
	second := storetest.SecondProcess(t, database)
	seeded := seedEveryKind(t, database.Pool, "ACTV")
	first := liveProcess(t, database, configuredIdentity, 2*time.Minute)
	liveProcess(t, database, foreignIdentity, time.Minute)

	firstEmbedder := newPlaceEmbedder(configuredIdentity)
	firstDeps := Deps{Store: database, Embedder: firstEmbedder}
	cover(t, firstDeps)
	drain(t, firstDeps)
	if got := vectorsByModel(t, database); got[configuredIdentity] != seeded || len(got) != 1 {
		t.Fatalf("vectors after the first process drained = %v, want %d of %q", got, seeded, configuredIdentity)
	}
	// One more item, pending, which the inactive process must leave for the active one.
	seedIssue(t, database, "ACTV", "ACTV-2", "Written while both serve")

	secondEmbedder := newPlaceEmbedder(foreignIdentity)
	secondDeps := Deps{Store: second, Embedder: secondEmbedder}
	before := snapshotRows(t, database)
	cover(t, secondDeps)
	drain(t, secondDeps)
	if len(secondEmbedder.calls) != 0 {
		t.Errorf("the inactive process called its embedder %d times, want none", len(secondEmbedder.calls))
	}
	after := snapshotRows(t, database)
	for key, row := range before {
		if after[key] != row {
			t.Errorf("the inactive process changed %s: model %q -> %q, vector %t -> %t",
				key, row.model, after[key].model, row.embedding != "", after[key].embedding != "")
		}
	}
	if pendingCount(t, database) != 1 {
		t.Errorf("pending rows after the inactive process ran = %d, want the one new issue", pendingCount(t, database))
	}
	if got := activeIdentityRow(t, database); got != configuredIdentity {
		t.Fatalf("active identity after the first process looked = %q, want its own %q", got, configuredIdentity)
	}

	expireProcess(t, database, first)
	cover(t, secondDeps)
	drain(t, secondDeps)
	if got := activeIdentityRow(t, database); got != foreignIdentity {
		t.Fatalf("active identity once the first process lapsed = %q, want %q", got, foreignIdentity)
	}
	if got := vectorsByModel(t, database); got[foreignIdentity] != seeded+1 || len(got) != 1 {
		t.Fatalf("vectors once the second identity is active = %v, want %d of %q", got, seeded+1, foreignIdentity)
	}

	// The first process serves again: the second identity is still carried, so it stays active.
	mustExec(t, database.Pool, `update server_processes set heartbeat_at = now() where instance = $1::uuid`, first)
	firstEmbedder.calls = nil
	before = snapshotRows(t, database)
	cover(t, firstDeps)
	drain(t, firstDeps)
	if len(firstEmbedder.calls) != 0 || fmt.Sprint(snapshotRows(t, database)) != fmt.Sprint(before) {
		t.Errorf("the first process, back but no longer active, called its embedder %d times or changed rows", len(firstEmbedder.calls))
	}
	if got := activeIdentityRow(t, database); got != foreignIdentity {
		t.Errorf("active identity with both live again = %q, want %q", got, foreignIdentity)
	}
}

// A process of the active identity that restarts registers again with a later start than a live
// process of another identity. The active identity is carried throughout, so it stays active: the
// other process drops nothing, and the restarted process embeds.
func TestARestartKeepsItsIdentityActive(t *testing.T) {
	database := storetest.Open(t)
	second := storetest.SecondProcess(t, database)
	seedIssue(t, database, "RSTR", "RSTR-1", "Embedded before the restart")
	first := liveProcess(t, database, configuredIdentity, 2*time.Minute)
	liveProcess(t, database, foreignIdentity, time.Minute)
	drain(t, Deps{Store: database, Embedder: newPlaceEmbedder(configuredIdentity)})

	// The restart: the new process's row is written before the old one's is deleted, in one
	// transaction here, and it starts after the other identity's process.
	mustExec(t, database.Pool, `
		with gone as (delete from server_processes where instance = $1::uuid)
		insert into server_processes (instance, signing_keys, schema_version, schema_creatable, embedder_identity, file_store)
		values (gen_random_uuid(), '{fingerprint}', 1, 1, $2, false)`, first, configuredIdentity)

	secondEmbedder := newPlaceEmbedder(foreignIdentity)
	cover(t, Deps{Store: second, Embedder: secondEmbedder})
	drain(t, Deps{Store: second, Embedder: secondEmbedder})
	if len(secondEmbedder.calls) != 0 {
		t.Errorf("the other identity's process called its embedder %d times, want none", len(secondEmbedder.calls))
	}
	if got := activeIdentityRow(t, database); got != configuredIdentity {
		t.Fatalf("active identity after the restart = %q, want %q", got, configuredIdentity)
	}

	seedIssue(t, database, "RSTR", "RSTR-2", "Written after the restart")
	restarted := newPlaceEmbedder(configuredIdentity)
	drain(t, Deps{Store: database, Embedder: restarted})
	if len(restarted.calls) != 1 || len(restarted.calls[0]) != 1 {
		t.Errorf("the restarted process embedded %v, want the one new issue", restarted.calls)
	}
	if got := vectorsByModel(t, database); got[configuredIdentity] != 2 || len(got) != 1 {
		t.Errorf("vectors after the restart = %v, want 2 of %q", got, configuredIdentity)
	}
}

// Two processes whose places differ reserve against the one embeddings_rate_limit row, each with
// its own ceiling: the bucket never refills faster than the larger ceiling, and each waits out
// its debt at its own, so together they never take more than the larger ceiling per minute, and
// neither stops the other. The ceilings are small, so a "minute" of tokens passes in seconds.
func TestTwoCeilingsTogetherNeverPassTheLargerOne(t *testing.T) {
	database := storetest.Open(t)
	second := storetest.SecondProcess(t, database)
	ctx := context.Background()
	const (
		smaller = 600  // 10 tokens a second
		larger  = 1200 // 20 tokens a second
		tokens  = 5
		window  = 3 * time.Second
	)
	mustExec(t, database.Pool, `update embeddings_rate_limit set tokens_available = 0, last_refill_at = now()`)

	started := time.Now()
	deadline := started.Add(window)
	taken := make([]int, 2)
	var wg sync.WaitGroup
	for i, side := range []struct {
		database *store.Store
		ceiling  int
	}{{database, smaller}, {second, larger}} {
		wg.Go(func() {
			for {
				reserveTokens(ctx, Deps{Store: side.database}, tokens, side.ceiling, nil)
				if time.Now().After(deadline) {
					return
				}
				taken[i] += tokens
			}
		})
	}
	wg.Wait()
	elapsed := deadline.Sub(started).Seconds()
	total := taken[0] + taken[1]
	// One reservation per process may be in flight at the window's edge.
	if limit := float64(larger)/60*elapsed + 2*tokens; float64(total) > limit {
		t.Errorf("the two processes took %d tokens in %.1fs (%v), want at most %.0f: the larger ceiling's %d a minute", total, elapsed, taken, limit, larger)
	}
	if taken[0] == 0 || taken[1] == 0 {
		t.Errorf("tokens taken = %v, want each process to take some", taken)
	}
}
