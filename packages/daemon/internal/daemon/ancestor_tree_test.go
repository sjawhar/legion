package daemon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/sjawhar/legion/daemon/internal/admit"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/ghbranch"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// chainDispatch answers ListIssues with listed and GetIssue with each issue's detail as Dispatch
// shows it in issues, refusing a key it shows none of, or one failing names, with Dispatch's
// trouble. reads counts each key's GetIssue calls.
type chainDispatch struct {
	outboxDispatch
	listed  []dispatch.IssueSummary
	issues  map[string]dispatch.Issue
	failing string
	reads   map[string]int
}

func (d *chainDispatch) ListIssues(context.Context, string, []string) ([]dispatch.IssueSummary, error) {
	return d.listed, nil
}

func (d *chainDispatch) GetIssue(_ context.Context, key string) (dispatch.Issue, error) {
	if d.reads == nil {
		d.reads = map[string]int{}
	}
	d.reads[key]++
	issue, ok := d.issues[key]
	if !ok || key == d.failing {
		return dispatch.Issue{}, errors.New("Dispatch is restarting")
	}
	return issue, nil
}

// shownUnder is key as Dispatch shows it at status under parent ("" for none), on acme/widgets.
func shownUnder(key, status, parent string, handed bool) dispatch.Issue {
	issue := dispatch.Issue{Key: key, Status: status, Title: key, Rank: "B", HandedOver: handed, ResolvedRepo: "acme/widgets", LastSeq: 1}
	if parent != "" {
		issue.Parent = &parent
	}
	return issue
}

// recordedOn is a record of key in tree, on acme/widgets at main.
func recordedOn(key, tree, status string) record.Issue {
	return record.Issue{Key: key, Tree: tree, Project: "LEGION", Title: key, Phase: phase.Planning, Generation: 1, Status: status, Rank: "A",
		HandedOver: true, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets"}
}

// parkedIn is a record of key parked in tree under tree itself: a child its run finished, at
// backlog.
func parkedIn(key, tree string) record.Issue {
	issue := recordedOn(key, tree, "backlog")
	issue.Phase, issue.Parent, issue.HandedOver = phase.Done, &tree, false
	return issue
}

// A labelled todo issue under a parent is prepared with the keys of its Dispatch parent chain, from
// its parent upward to the top, through every ancestor, recorded or not, whether the issue is
// unrecorded or recorded as a root of its own and whether its parent is recorded or not; the walk
// stops only at a repeated key or back at the issue. A child already in its tree and an unlabelled
// todo carry none, and an unlabelled todo reads nothing up its chain. A failed read of the chain
// retries the event rather than applying it without the chain.
func TestPrepareReadsTheDispatchChainAboveAParent(t *testing.T) {
	const tree, parent, child, other = "LEGION-168", "LEGION-169", "LEGION-214", "LEGION-170"
	labelled := intake.DispatchIssue{Key: child, Seq: 1, Type: "issue.updated", Status: "todo", Title: child, Parent: parent, Rank: "B", HandedOver: true, ResolvedRepo: "acme/widgets"}
	unlabelled := labelled
	unlabelled.HandedOver = false
	top := shownUnder(tree, "in_progress", "", true)
	for _, tc := range []struct {
		name       string
		recorded   []record.Issue
		shown      []dispatch.Issue
		unlabelled bool
		failing    string
		want       []string
		fails      bool
	}{
		{name: "an unrecorded child under an unrecorded parent of a recorded tree", recorded: []record.Issue{recordedOn(tree, tree, "in_progress")},
			shown: []dispatch.Issue{shownUnder(child, "todo", parent, true), shownUnder(parent, "backlog", tree, false), top}, want: []string{parent, tree}},
		{name: "a recorded root under an unrecorded parent of a recorded tree", recorded: []record.Issue{recordedOn(tree, tree, "in_progress"), recordedOn(child, child, "todo")},
			shown: []dispatch.Issue{shownUnder(child, "todo", parent, true), shownUnder(parent, "backlog", tree, false), top}, want: []string{parent, tree}},
		{name: "a tree two unrecorded ancestors up", recorded: []record.Issue{recordedOn(tree, tree, "in_progress")},
			shown: []dispatch.Issue{shownUnder(child, "todo", parent, true), shownUnder(parent, "backlog", other, false), shownUnder(other, "backlog", tree, false), top}, want: []string{parent, other, tree}},
		{name: "a chain through a recorded member of a tree to its root", recorded: []record.Issue{recordedOn(tree, tree, "in_progress"), parkedIn(other, tree)},
			shown: []dispatch.Issue{shownUnder(child, "todo", parent, true), shownUnder(parent, "backlog", other, false), shownUnder(other, "backlog", tree, false), top}, want: []string{parent, other, tree}},
		{name: "a recorded member of a tree moved out from under it", recorded: []record.Issue{recordedOn(tree, tree, "in_progress"), parkedIn(other, tree)},
			shown: []dispatch.Issue{shownUnder(child, "todo", parent, true), shownUnder(parent, "backlog", other, false), shownUnder(other, "backlog", "", false)}, want: []string{parent, other}},
		{name: "a recorded parent", recorded: []record.Issue{recordedOn(tree, tree, "in_progress"), recordedOn(parent, tree, "in_progress")},
			shown: []dispatch.Issue{shownUnder(child, "todo", parent, true), shownUnder(parent, "in_progress", tree, true), top}, want: []string{parent, tree}},
		{name: "a child already in its tree", recorded: []record.Issue{recordedOn(tree, tree, "in_progress"), recordedOn(child, tree, "todo")},
			shown: []dispatch.Issue{shownUnder(parent, "backlog", tree, false), top}},
		{name: "an unlabelled unrecorded todo", recorded: []record.Issue{recordedOn(tree, tree, "in_progress")}, unlabelled: true,
			shown: []dispatch.Issue{shownUnder(child, "todo", parent, false), shownUnder(parent, "backlog", tree, false), top}},
		{name: "an unlabelled todo recorded as a root", recorded: []record.Issue{recordedOn(tree, tree, "in_progress"), recordedOn(child, child, "todo")}, unlabelled: true,
			shown: []dispatch.Issue{shownUnder(child, "todo", parent, false), shownUnder(parent, "backlog", tree, false), top}},
		{name: "a chain that ends with nothing recorded",
			shown: []dispatch.Issue{shownUnder(child, "todo", parent, true), shownUnder(parent, "backlog", "", false)}, want: []string{parent}},
		{name: "a chain that loops", recorded: []record.Issue{recordedOn(tree, tree, "in_progress")},
			shown: []dispatch.Issue{shownUnder(child, "todo", parent, true), shownUnder(parent, "backlog", other, false), shownUnder(other, "backlog", parent, false)}, want: []string{parent, other}},
		{name: "a chain back to the issue", recorded: []record.Issue{recordedOn(tree, tree, "in_progress")},
			shown: []dispatch.Issue{shownUnder(child, "todo", parent, true), shownUnder(parent, "backlog", child, false)}, want: []string{parent}},
		{name: "a failed read of the chain", recorded: []record.Issue{recordedOn(tree, tree, "in_progress")},
			shown: []dispatch.Issue{shownUnder(child, "todo", parent, true), shownUnder(parent, "backlog", tree, false), top}, failing: parent, fails: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := isolatedOutboxPool(t)
			records := record.NewStore()
			for _, issue := range tc.recorded {
				putOutboxIssue(t, pool, records, issue)
			}
			repos := newRepositories("LEGION")
			repos.set([]mappedRepository{{Repo: ghrepo.MustParse("acme/widgets"), Default: true}}, nil)
			branches := newDefaultBranches(roleTokens{}, "", defaultBranchTTL, time.Now)
			branches.read["acme/widgets"] = defaultBranchRead{repository: ghbranch.Repository{Name: ghrepo.MustParse("acme/widgets"), DefaultBranch: "main"}, at: time.Now()}
			client := &chainDispatch{issues: map[string]dispatch.Issue{}, failing: tc.failing}
			for _, issue := range tc.shown {
				client.issues[issue.Key] = issue
			}
			w := &workflowRuntime{pool: pool, records: records, dispatch: client, repositories: repos, defaultBranches: branches, log: quietLogger()}
			event := labelled
			if tc.unlabelled {
				event = unlabelled
			}

			got, err := w.prepareRepository(context.Background(), event)
			if tc.fails {
				if err == nil {
					t.Fatalf("prepareRepository = %#v, want the failed read to fail the preparation", got)
				}
				return
			}
			if err != nil || !slices.Equal(got.Ancestry, tc.want) {
				t.Fatalf("prepareRepository = %#v, %v; want Ancestry %q", got, err, tc.want)
			}
			if read := client.reads[parent] + client.reads[other] + client.reads[tree]; tc.unlabelled && read != 0 {
				t.Fatalf("an unlabelled todo read its ancestors from Dispatch %d times, want none: it never queues", read)
			}
		})
	}
}

// The boot read keeps the same issue out of admission's line: a labelled todo issue whose parent is
// a backlog child of a running tree is recorded as a root of its own and takes no slot or architect
// with one free. The boot walks the chain as its listing shows it, so a listed ancestor costs no
// read of Dispatch.
func TestBootReconcileHoldsATodoUnderAnUnreleasedChildOfALiveTree(t *testing.T) {
	const tree, parent, child = "CAPTURE-168", "CAPTURE-169", "CAPTURE-214"
	ctx := context.Background()
	pool := isolatedOutboxPool(t)
	conn, err := nats.Connect(workflowNATS(t), nats.Timeout(time.Second))
	if err != nil {
		t.Fatalf("connect NATS: %v", err)
	}
	t.Cleanup(conn.Close)
	js, err := jetstream.New(conn)
	if err != nil {
		t.Fatalf("open JetStream: %v", err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	consumers, err := intake.OpenConsumers(ctx, js, intake.ConsumerSpec{
		Project: "CAPTURE", Repositories: []ghrepo.Repository{ghrepo.MustParse("sjawhar/legion")}, AckWait: time.Second, NakDelay: time.Millisecond, Logger: log,
	})
	if err != nil {
		t.Fatalf("OpenConsumers: %v", err)
	}
	records := record.NewStore()
	running := record.Issue{Key: tree, Tree: tree, Project: "CAPTURE", Title: tree, Phase: phase.Planning, Generation: 1, Status: "in_progress", Rank: "A",
		HandedOver: true, Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion"}
	if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		if err := records.PutIssue(ctx, tx, running); err != nil {
			return err
		}
		return records.PutSlot(ctx, tx, record.Slot{Issue: tree, Index: 0, AdmittedAt: time.Now()})
	}); err != nil {
		t.Fatalf("seed the running tree: %v", err)
	}
	under := func(key string) *string { return &key }
	client := &chainDispatch{
		listed: []dispatch.IssueSummary{
			{Key: parent, ResolvedRepo: "sjawhar/legion", Title: parent, Status: "backlog", Parent: under(tree), Rank: "B"},
			{Key: tree, ResolvedRepo: "sjawhar/legion", Title: tree, Status: "in_progress", Rank: "A", HandedOver: true},
			{Key: child, ResolvedRepo: "sjawhar/legion", Title: child, Status: "todo", Parent: under(parent), Rank: "C", HandedOver: true},
		},
		issues: map[string]dispatch.Issue{
			parent: {Key: parent, ResolvedRepo: "sjawhar/legion", Title: parent, Status: "backlog", Parent: under(tree), Rank: "B"},
			child:  {Key: child, ResolvedRepo: "sjawhar/legion", Title: child, Status: "todo", Parent: under(parent), Rank: "C", HandedOver: true},
		},
	}
	engine := workflow.New(records, workflow.Config{Project: "CAPTURE", Repositories: testRepositories}, log)
	admission := admit.New(records, engine, 2, "CAPTURE", log)
	w := &workflowRuntime{
		pool: pool, records: records, admission: admission, engine: engine, handlers: []intake.Handler{engine, admission}, repositories: newRepositories("CAPTURE"),
		dispatch: client, dispatchProject: "CAPTURE", bootID: "ancestor-boot", log: log, consumers: consumers,
	}
	if err := w.reconcile(ctx); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(client.reads) != 0 {
		t.Fatalf("the boot read %v from Dispatch, want nothing: its listing shows every ancestor it walks", client.reads)
	}

	var waiting []string
	var slotted []string
	var got *record.Issue
	if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		issues, err := records.Issues(ctx, tx)
		if err != nil {
			return err
		}
		slots, err := records.Slots(ctx, tx)
		if err != nil {
			return err
		}
		for _, issue := range record.Waiting(issues, slots) {
			waiting = append(waiting, issue.Key)
		}
		for _, slot := range slots {
			slotted = append(slotted, slot.Issue)
		}
		got, err = records.Issue(ctx, tx, child)
		return err
	}); err != nil {
		t.Fatalf("read the records: %v", err)
	}
	if got == nil || got.Tree != child || got.Status != "todo" {
		t.Fatalf("%s = %#v, want it recorded todo as a root of its own", child, got)
	}
	if len(waiting) != 0 || len(slotted) != 1 || slotted[0] != tree {
		t.Fatalf("waiting %q, slots %q; want nothing waiting and only %s slotted", waiting, slotted, tree)
	}
	var starts int
	if err := pool.QueryRow(ctx, `select count(*) from outbox where kind = 'supervise' and payload->>'op' = 'start' and payload->>'role' = $1 and payload->>'tree' = $2`,
		string(claim.RoleArchitect), child).Scan(&starts); err != nil {
		t.Fatalf("count architect starts: %v", err)
	}
	if starts != 0 {
		t.Fatalf("%s has %d architect starts queued, want none while %s runs", child, starts, tree)
	}
}

// A labelled todo issue held out of admission's line under an unrecorded parent of a running tree
// is held only while that parent is under the tree: once a person moves the parent out of it, the
// parent's own event leaves the issue a root of its own under no live tree, and it queues and takes
// the free slot as any labelled todo root does. Its own next event must not be what releases it.
func TestATodoHeldUnderAnUnreleasedChildQueuesOnceItsParentLeavesTheLiveTree(t *testing.T) {
	const tree, parent, child = "LEGION-168", "LEGION-169", "LEGION-214"
	ctx := context.Background()
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	putOutboxIssue(t, pool, records, recordedOn(tree, tree, "in_progress"))
	if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		return records.PutSlot(ctx, tx, record.Slot{Issue: tree, Index: 0, AdmittedAt: time.Now()})
	}); err != nil {
		t.Fatalf("seed the running tree's slot: %v", err)
	}
	repos := newRepositories("LEGION")
	repos.set([]mappedRepository{{Repo: ghrepo.MustParse("acme/widgets"), Default: true}}, nil)
	branches := newDefaultBranches(roleTokens{}, "", defaultBranchTTL, time.Now)
	branches.read["acme/widgets"] = defaultBranchRead{repository: ghbranch.Repository{Name: ghrepo.MustParse("acme/widgets"), DefaultBranch: "main"}, at: time.Now()}
	client := &chainDispatch{issues: map[string]dispatch.Issue{
		tree:   shownUnder(tree, "in_progress", "", true),
		parent: shownUnder(parent, "backlog", tree, false),
		child:  shownUnder(child, "todo", parent, true),
	}}
	log := quietLogger()
	engine := workflow.New(records, workflow.Config{Project: "LEGION", Repositories: testRepositories}, log)
	admission := admit.New(records, engine, 2, "LEGION", log)
	w := &workflowRuntime{pool: pool, records: records, dispatch: client, repositories: repos, defaultBranches: branches, log: log}
	// deliver is the consumer's handling of one Dispatch event: its preparation, then its fact.
	deliver := func(event intake.DispatchIssue) {
		t.Helper()
		prepared, err := w.prepareRepository(ctx, event)
		if err != nil {
			t.Fatalf("prepare the event of %s: %v", event.Key, err)
		}
		if _, err := intake.ApplyFact(ctx, pool, "dispatch", fmt.Sprintf("%s-%d", event.Key, event.Seq), prepared, engine, admission); err != nil {
			t.Fatalf("apply the event of %s: %v", event.Key, err)
		}
	}
	slotOf := func(key string) bool {
		t.Helper()
		var slotted bool
		if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
			slots, err := records.Slots(ctx, tx)
			slotted = slices.ContainsFunc(slots, func(slot record.Slot) bool { return slot.Issue == key })
			return err
		}); err != nil {
			t.Fatalf("read the slots: %v", err)
		}
		return slotted
	}

	deliver(intake.DispatchIssue{Key: child, Seq: 2, Type: "issue.updated", Status: "todo", Title: child, Parent: parent, Rank: "B", HandedOver: true, ResolvedRepo: "acme/widgets"})
	if slotOf(child) {
		t.Fatalf("%s took a slot while its parent %s is an unreleased child of %s", child, parent, tree)
	}

	client.issues[parent] = shownUnder(parent, "backlog", "", false)
	deliver(intake.DispatchIssue{Key: parent, Seq: 3, Type: "issue.updated", Status: "backlog", Title: parent, Rank: "B", ResolvedRepo: "acme/widgets"})
	if !slotOf(child) {
		t.Fatalf("%s holds no slot with one free after its parent %s left the tree of %s: it is held under a tree it is no longer under", child, parent, tree)
	}
}

// ancestryLine is a running tree, recorded with its slot, under an admission of two slots, and the
// consumer's handling of Dispatch's events against it, Dispatch showing client's issues: the tree
// among them, at the top of its chain.
type ancestryLine struct {
	t       *testing.T
	pool    *pgxpool.Pool
	records *record.Postgres
	client  *chainDispatch
	w       *workflowRuntime
	engine  *workflow.Engine
	admit   *admit.Admission
}

func newAncestryLine(t *testing.T, tree string, recorded ...record.Issue) *ancestryLine {
	t.Helper()
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	putOutboxIssue(t, pool, records, recordedOn(tree, tree, "in_progress"))
	for _, issue := range recorded {
		putOutboxIssue(t, pool, records, issue)
	}
	if err := pgx.BeginFunc(context.Background(), pool, func(tx pgx.Tx) error {
		return records.PutSlot(context.Background(), tx, record.Slot{Issue: tree, Index: 0, AdmittedAt: time.Now()})
	}); err != nil {
		t.Fatalf("seed the running tree's slot: %v", err)
	}
	repos := newRepositories("LEGION")
	repos.set([]mappedRepository{{Repo: ghrepo.MustParse("acme/widgets"), Default: true}}, nil)
	branches := newDefaultBranches(roleTokens{}, "", defaultBranchTTL, time.Now)
	branches.read["acme/widgets"] = defaultBranchRead{repository: ghbranch.Repository{Name: ghrepo.MustParse("acme/widgets"), DefaultBranch: "main"}, at: time.Now()}
	client := &chainDispatch{issues: map[string]dispatch.Issue{tree: shownUnder(tree, "in_progress", "", true)}}
	log := quietLogger()
	engine := workflow.New(records, workflow.Config{Project: "LEGION", Repositories: testRepositories}, log)
	return &ancestryLine{t: t, pool: pool, records: records, client: client, engine: engine, admit: admit.New(records, engine, 2, "LEGION", log),
		w: &workflowRuntime{pool: pool, records: records, dispatch: client, repositories: repos, defaultBranches: branches, log: log}}
}

// deliver prepares event and applies it as its fact, as the consumer does.
func (l *ancestryLine) deliver(event intake.DispatchIssue) {
	l.t.Helper()
	prepared, err := l.w.prepareRepository(context.Background(), event)
	if err != nil {
		l.t.Fatalf("prepare the event of %s: %v", event.Key, err)
	}
	if _, err := intake.ApplyFact(context.Background(), l.pool, "dispatch", fmt.Sprintf("%s-%d", event.Key, event.Seq), prepared, l.engine, l.admit); err != nil {
		l.t.Fatalf("apply the event of %s: %v", event.Key, err)
	}
}

// queue delivers the todo event of key, labelled under parent, as Dispatch shows it.
func (l *ancestryLine) queue(key, parent string) {
	l.t.Helper()
	l.client.issues[key] = shownUnder(key, "todo", parent, true)
	l.deliver(intake.DispatchIssue{Key: key, Seq: 2, Type: "issue.updated", Status: "todo", Title: key, Parent: parent, Rank: "B", HandedOver: true, ResolvedRepo: "acme/widgets"})
}

// slotted is whether key holds a slot, and its record.
func (l *ancestryLine) slotted(key string) (bool, *record.Issue) {
	l.t.Helper()
	var slotted bool
	var issue *record.Issue
	if err := pgx.BeginFunc(context.Background(), l.pool, func(tx pgx.Tx) error {
		slots, err := l.records.Slots(context.Background(), tx)
		if err != nil {
			return err
		}
		slotted = slices.ContainsFunc(slots, func(slot record.Slot) bool { return slot.Issue == key })
		issue, err = l.records.Issue(context.Background(), tx, key)
		return err
	}); err != nil {
		l.t.Fatalf("read the slots and %s: %v", key, err)
	}
	return slotted, issue
}

// waiting is whether key waits in admission's line (record.Waiting), held by no live root.
func (l *ancestryLine) waiting(key string) bool {
	l.t.Helper()
	var waiting bool
	if err := pgx.BeginFunc(context.Background(), l.pool, func(tx pgx.Tx) error {
		issues, err := l.records.Issues(context.Background(), tx)
		if err != nil {
			return err
		}
		slots, err := l.records.Slots(context.Background(), tx)
		waiting = slices.ContainsFunc(record.Waiting(issues, slots), func(issue record.Issue) bool { return issue.Key == key })
		return err
	}); err != nil {
		l.t.Fatalf("read the waiting line: %v", err)
	}
	return waiting
}

// A labelled todo issue whose chain reaches a running tree through a recorded member of it is held
// under that tree's root, as release_children's walk of the chain reaches the root. Once a person
// moves that member out from under the tree, the member's own event, though the workflow records
// it, reads the held issue's chain again, and the issue takes the free slot: release_children
// would no longer reach it from the tree, so nothing would ever release it.
func TestAnAncestryHeldRootQueuesOnceARecordedAncestorLeavesTheLiveTree(t *testing.T) {
	const tree, member, parent, child = "LEGION-168", "LEGION-170", "LEGION-169", "LEGION-214"
	line := newAncestryLine(t, tree, parkedIn(member, tree))
	line.client.issues[member] = shownUnder(member, "backlog", tree, false)
	line.client.issues[parent] = shownUnder(parent, "backlog", member, false)

	line.queue(child, parent)
	if slotted, issue := line.slotted(child); slotted || issue == nil || !slices.Equal(issue.Ancestry, []string{parent, member, tree}) {
		t.Fatalf("%s slotted %v, recorded %#v; want it held under %s, the root its chain reaches through %s", child, slotted, issue, tree, member)
	}

	line.client.issues[member] = shownUnder(member, "backlog", "", false)
	line.deliver(intake.DispatchIssue{Key: member, Seq: 3, Type: "issue.updated", Status: "backlog", Title: member, Rank: "A", ResolvedRepo: "acme/widgets"})
	if slotted, issue := line.slotted(child); !slotted || slices.Contains(issue.Ancestry, tree) {
		t.Fatalf("%s slotted %v, recorded %#v once %s left the tree of %s; want it in the free slot under no tree", child, slotted, issue, member, tree)
	}
}

// An event that reads the held roots' chains again applies even when one root's chain cannot be
// read: that root keeps the chain it has, and every other root under the event's issue is read
// again, so one failing read of Dispatch holds no event back.
func TestARereadSkipsARootWhoseChainItCannotRead(t *testing.T) {
	const tree, shared, stuckParent, stuck, movedParent, moved = "LEGION-168", "LEGION-172", "LEGION-169", "LEGION-214", "LEGION-171", "LEGION-215"
	line := newAncestryLine(t, tree)
	line.client.issues[shared] = shownUnder(shared, "backlog", tree, false)
	line.client.issues[stuckParent] = shownUnder(stuckParent, "backlog", shared, false)
	line.client.issues[movedParent] = shownUnder(movedParent, "backlog", shared, false)
	line.queue(stuck, stuckParent)
	line.queue(moved, movedParent)
	for _, key := range []string{stuck, moved} {
		if slotted, issue := line.slotted(key); slotted || issue == nil || !slices.Contains(issue.Ancestry, tree) {
			t.Fatalf("%s slotted %v, recorded %#v; want it held under %s", key, slotted, issue, tree)
		}
	}

	line.client.failing = stuckParent
	line.client.issues[shared] = shownUnder(shared, "backlog", "", false)
	line.deliver(intake.DispatchIssue{Key: shared, Seq: 3, Type: "issue.updated", Status: "backlog", Title: shared, Rank: "B", ResolvedRepo: "acme/widgets"})
	if slotted, issue := line.slotted(moved); !slotted || slices.Contains(issue.Ancestry, tree) {
		t.Fatalf("%s slotted %v, recorded %#v once %s left the tree of %s; want it in the free slot", moved, slotted, issue, shared, tree)
	}
	if slotted, issue := line.slotted(stuck); slotted || !slices.Contains(issue.Ancestry, tree) {
		t.Fatalf("%s slotted %v, recorded %#v; want it still held under %s, its chain unread", stuck, slotted, issue, tree)
	}
}

// A labelled todo issue whose Dispatch chain reaches a running tree past a recorded root parked out
// of the workflow (INFRA-214's state: a once-queued root a person moved under the tree and did not
// release) is held under the running tree, whose release would reach it: the parked root, not
// live, is passed over rather than ending the chain, whether it is the issue's parent or further up.
func TestARootHeldPastAParkedRootOnItsChainTakesNoSlot(t *testing.T) {
	const tree, parked, parent, child = "LEGION-168", "LEGION-180", "LEGION-169", "LEGION-214"
	for _, tc := range []struct {
		name, under string
		want        []string
	}{
		{name: "an unrecorded parent under the parked root", under: parent, want: []string{parent, parked, tree}},
		{name: "the parked root as its parent", under: parked, want: []string{parked, tree}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := recordedOn(parked, parked, "backlog")
			root.Phase, root.Parent = phase.Done, record.ParentOf(tree)
			line := newAncestryLine(t, tree, root)
			line.client.issues[parked] = shownUnder(parked, "backlog", tree, false)
			line.client.issues[parent] = shownUnder(parent, "backlog", parked, false)

			line.queue(child, tc.under)
			if slotted, issue := line.slotted(child); slotted || issue == nil || issue.Status != "todo" || !slices.Equal(issue.Ancestry, tc.want) || line.waiting(child) {
				t.Fatalf("%s slotted %v, waiting %v, recorded %#v; want it held out of the line with chain %v, under %s past %s", child, slotted, line.waiting(child), issue, tc.want, tree, parked)
			}
		})
	}
}

// An event of an issue on no held root's chain reads nothing of Dispatch and reads no chain again;
// an event of a key on one held root's chain reads that root's chain again, and no other's; and
// release_children's view of a child (prepareObserved) reads no chain again at all, though the
// same issue's event would.
func TestARereadReadsOnlyTheRootsWhoseChainHoldsTheEventsIssue(t *testing.T) {
	const tree, movedParent, moved, keptParent, kept, unrelated = "LEGION-168", "LEGION-171", "LEGION-215", "LEGION-169", "LEGION-214", "LEGION-300"
	ctx := context.Background()
	line := newAncestryLine(t, tree)
	line.client.issues[movedParent] = shownUnder(movedParent, "backlog", tree, false)
	line.client.issues[keptParent] = shownUnder(keptParent, "backlog", tree, false)
	line.queue(moved, movedParent)
	line.queue(kept, keptParent)

	line.client.reads = nil
	prepared, err := line.w.prepareRepository(ctx, intake.DispatchIssue{Key: unrelated, Seq: 4, Type: "issue.updated", Status: "backlog", Title: unrelated, Parent: tree, Rank: "C", ResolvedRepo: "acme/widgets"})
	if err != nil || prepared.Reread != nil || len(line.client.reads) != 0 {
		t.Fatalf("an unrelated issue's event prepared as %#v, %v, reading %v from Dispatch; want no Reread and no read", prepared, err, line.client.reads)
	}

	line.client.issues[movedParent] = shownUnder(movedParent, "todo", "", true)
	line.client.reads = nil
	observed, err := line.w.prepareObserved(ctx, intake.ObservedIssue(line.client.issues[movedParent]))
	if err != nil || observed.Reread != nil || line.client.reads[movedParent] != 0 {
		t.Fatalf("release_children's view of %s prepared as %#v, %v, reading %v from Dispatch; want no chain read again", movedParent, observed, err, line.client.reads)
	}

	line.client.reads = nil
	prepared, err = line.w.prepareRepository(ctx, intake.DispatchIssue{Key: movedParent, Seq: 3, Type: "issue.updated", Status: "todo", Title: movedParent, Rank: "B", HandedOver: true, ResolvedRepo: "acme/widgets"})
	if err != nil || len(prepared.Reread) != 1 || !slices.Equal(prepared.Reread[moved], []string{movedParent}) {
		t.Fatalf("%s's event prepared as %#v, %v; want only %s read again, under %s alone", movedParent, prepared, err, moved, movedParent)
	}
	if line.client.reads[keptParent] != 0 {
		t.Fatalf("%s's event read %v from Dispatch; want nothing of %s, whose chain does not hold it", movedParent, line.client.reads, kept)
	}
}

// A recorded issue's event older than its record, arriving after its newer re-parent out of a
// running tree, reads no held root's chain again: the root its re-parent freed stays out from
// under the tree, as Dispatch now shows it, rather than being held there again by the stale parent.
func TestAStaleEventOfAMovedAncestorBringsBackNoAncestryHold(t *testing.T) {
	const tree, other, member, parent, child = "LEGION-168", "LEGION-190", "LEGION-170", "LEGION-169", "LEGION-214"
	line := newAncestryLine(t, tree, parkedIn(member, tree), recordedOn(other, other, "in_progress"))
	if err := pgx.BeginFunc(context.Background(), line.pool, func(tx pgx.Tx) error {
		return line.records.PutSlot(context.Background(), tx, record.Slot{Issue: other, Index: 1, AdmittedAt: time.Now()})
	}); err != nil {
		t.Fatalf("fill the second slot: %v", err)
	}
	line.client.issues[member] = shownUnder(member, "backlog", tree, false)
	line.client.issues[parent] = shownUnder(parent, "backlog", member, false)
	line.queue(child, parent)
	if _, issue := line.slotted(child); issue == nil || !slices.Contains(issue.Ancestry, tree) || line.waiting(child) {
		t.Fatalf("%s recorded %#v; want it held under %s", child, issue, tree)
	}

	line.client.issues[member] = shownUnder(member, "backlog", "", false)
	line.deliver(intake.DispatchIssue{Key: member, Seq: 5, Type: "issue.updated", Status: "backlog", Title: member, Rank: "A", ResolvedRepo: "acme/widgets"})
	if _, issue := line.slotted(child); slices.Contains(issue.Ancestry, tree) || !line.waiting(child) {
		t.Fatalf("%s recorded %#v once %s left the tree of %s; want it waiting under no live root", child, issue, member, tree)
	}

	line.deliver(intake.DispatchIssue{Key: member, Seq: 4, Type: "issue.updated", Status: "backlog", Title: member, Parent: tree, Rank: "A", ResolvedRepo: "acme/widgets"})
	if _, issue := line.slotted(child); slices.Contains(issue.Ancestry, tree) || !line.waiting(child) {
		t.Fatalf("%s recorded %#v after %s's stale event at sequence 4; want it still waiting under no live root", child, issue, member)
	}
}
