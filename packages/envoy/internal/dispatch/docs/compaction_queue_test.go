package docs

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// Writes through a room and joined writes leave, once the queue has checked their document, the
// fold-point row and every row younger than the tail: the queue folds a document with at least
// compactMinRows rows older than the tail after its first, and leaves each younger row as written,
// byte for byte and under the transaction that wrote it. Each way of writing notes the document
// itself, so a document written only one way is compacted too.
func TestTheCompactionQueueFoldsTheOldRowsOfADocumentItsWritesNoted(t *testing.T) {
	agent := model.Actor{Kind: "session", ID: "agent"}
	for _, writers := range []struct {
		name  string
		write func(t *testing.T, service *Service, artifactID string, index int)
	}{
		{"browser edits", func(t *testing.T, service *Service, artifactID string, index int) {
			editLiveTree(t, service, artifactID, appendBlocks(t, fmt.Sprintf("Paragraph %d.\n", index)))
		}},
		{"joined writes", func(t *testing.T, service *Service, artifactID string, index int) {
			if _, err := joinedReplaceText(service, artifactID, fmt.Sprintf("Replaced %d.\n", index), agent); err != nil {
				t.Fatalf("joined write %d: %v", index, err)
			}
		}},
		{"browser edits and joined writes", func(t *testing.T, service *Service, artifactID string, index int) {
			if index%2 == 0 {
				editLiveTree(t, service, artifactID, appendBlocks(t, fmt.Sprintf("Paragraph %d.\n", index)))
			} else if _, err := joinedReplaceText(service, artifactID, fmt.Sprintf("Replaced %d.\n", index), agent); err != nil {
				t.Fatalf("joined write %d: %v", index, err)
			}
		}},
	} {
		t.Run(writers.name, func(t *testing.T) {
			service, artifactID := newTestService(t)
			service.settle = time.Hour
			steps := quickCompactionQueue(service, 50*time.Millisecond)
			seedServiceText(t, service, artifactID, "First.\n")
			for index := range 20 {
				writers.write(t, service, artifactID, index)
			}
			waitForLandedAppends(t, service, artifactID)
			steps.await(t, artifactID, 0, "the queue's first check of the young rows", func(step compactionStepRecord) bool {
				return step.next.kind == compactionLater
			})
			// The queue checks a noted room at once here, so the notes of the writes below would
			// let it compact before the rows it folds are read. It is held from here until they are,
			// which drops what it holds and every note meanwhile; the document is then noted as
			// the writes' commits noted it.
			resume := service.compactions.pause(context.Background())
			old := storedRows(t, service.store, artifactID)
			if len(old) < 1+compactMinRows {
				t.Fatalf("the writes stored %d rows, want at least %d", len(old), 1+compactMinRows)
			}
			ageUpdatesThrough(t, service.store, artifactID, everyVersion, 5*time.Minute)
			for index := 20; index < 22; index++ {
				writers.write(t, service, artifactID, index)
			}
			waitForLandedAppends(t, service, artifactID)
			before, cursor := storedRows(t, service.store, artifactID), settlementCursor(t, service.store, artifactID)
			reading := storedReading(t, NewPgVersioned(service.store), artifactID)
			foldPoint := old[len(old)-1].version

			from := steps.count()
			resume()
			service.compactions.note(artifactID)
			steps.await(t, artifactID, from, "the queue to compact the document", func(step compactionStepRecord) bool {
				return step.next.kind == compactionAgain
			})
			steps.await(t, artifactID, from, "the queue's check after the compaction", func(step compactionStepRecord) bool {
				return step.next.kind == compactionLater
			})
			requireCompactedFrom(t, service.store, artifactID, before, foldPoint, cursor)
			if got := storedReading(t, NewPgVersioned(service.store), artifactID); got != reading {
				t.Fatalf("the compacted document reads\n%s\nwant\n%s", got, reading)
			}
		})
	}
}

// A load notes the document it read, through a room's load or a cold read, and the queue compacts
// it exactly when it is due: at least compactMinRows rows older than the tail after its first row,
// or any row after its first older than compactMaxTailAge. A document that is not due keeps its
// rows as stored, and is checked again when it will be.
func TestTheCompactionQueueCompactsALoadedDocumentOnlyWhenItIsDue(t *testing.T) {
	database := storetest.Open(t)
	service := New(Deps{Store: database, Events: events.NewBroker(), ServerURL: "https://dispatch.example", Settle: time.Hour})
	t.Cleanup(func() { _ = service.Shutdown(context.Background()) })
	steps := quickCompactionQueue(service, 50*time.Millisecond)
	loads := map[string]func(t *testing.T, artifactID string){
		"a room load": func(t *testing.T, artifactID string) { liveTree(t, service, artifactID) },
		"a cold read": func(t *testing.T, artifactID string) {
			if _, err := service.Text(context.Background(), artifactID); err != nil {
				t.Fatalf("read the document: %v", err)
			}
		},
	}
	for _, test := range []struct {
		name string
		rows int
		age  time.Duration
		// dueIn is how long until the document is due, zero for one due now.
		dueIn time.Duration
	}{
		{name: "nine rows five minutes old after the first", rows: 10, age: 5 * time.Minute, dueIn: 10 * time.Minute},
		{name: "fifteen rows five minutes old after the first", rows: 16, age: 5 * time.Minute, dueIn: 10 * time.Minute},
		{name: "sixteen rows five minutes old after the first", rows: 17, age: 5 * time.Minute},
		{name: "twenty rows a minute old after the first", rows: 21, age: time.Minute, dueIn: time.Minute},
		{name: "two rows twenty minutes old after the first", rows: 3, age: 20 * time.Minute},
	} {
		for loadName, load := range loads {
			t.Run(test.name+" noted by "+loadName, func(t *testing.T) {
				artifactID := storeAgedRows(t, database, test.rows, test.age)
				before := storedRows(t, database, artifactID)
				reading := storedReading(t, NewPgVersioned(database), artifactID)
				from := steps.count()
				load(t, artifactID)
				if test.dueIn == 0 {
					steps.await(t, artifactID, from, "the queue to compact the due document", func(step compactionStepRecord) bool {
						return step.next.kind == compactionAgain
					})
					steps.await(t, artifactID, from, "the queue to drop the compacted document", func(step compactionStepRecord) bool {
						return step.next.kind == compactionDone
					})
					after := storedRows(t, database, artifactID)
					if len(after) != 1 || after[0].version != before[len(before)-1].version {
						t.Fatalf("the compacted document holds rows %v, want the head %d alone", rowVersions(after), before[len(before)-1].version)
					}
					if got := storedReading(t, NewPgVersioned(database), artifactID); got != reading {
						t.Fatalf("the compacted document reads\n%s\nwant\n%s", got, reading)
					}
					return
				}
				step := steps.await(t, artifactID, from, "the queue's check of the document", func(step compactionStepRecord) bool {
					return step.next.kind == compactionLater
				})
				if wait := step.next.after; wait > test.dueIn || wait < test.dueIn-30*time.Second {
					t.Fatalf("the queue checks the document again in %s, want about %s, when it is due", wait, test.dueIn)
				}
				requireRowsAsStored(t, database, artifactID, before)
			})
		}
	}
}

// A document whose rows are all younger than the tail is checked again when they come of age: the
// queue schedules it for when its oldest row after the first passes compactMaxTailAge, and that
// check compacts it.
func TestTheCompactionQueueComesBackToADocumentWhenItsRowsComeOfAge(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	steps := quickCompactionQueue(service, 50*time.Millisecond)
	seedServiceText(t, service, artifactID, "First.\n")
	for _, paragraph := range []string{"Second.\n", "Third.\n", "Fourth.\n"} {
		editLiveTree(t, service, artifactID, appendBlocks(t, paragraph))
	}
	waitForLandedAppends(t, service, artifactID)
	step := steps.await(t, artifactID, 0, "the queue's check of the young rows", func(step compactionStepRecord) bool {
		return step.next.kind == compactionLater
	})
	if wait := step.next.after; wait > compactMaxTailAge || wait < compactMaxTailAge-time.Minute {
		t.Fatalf("the queue checks the young rows again in %s, want about %s", wait, compactMaxTailAge)
	}
	before := storedRows(t, service.store, artifactID)

	ageUpdatesThrough(t, service.store, artifactID, everyVersion, compactMaxTailAge+time.Minute)
	from := steps.count()
	recheckNow(service)
	steps.await(t, artifactID, from, "the queue to compact the rows come of age", func(step compactionStepRecord) bool {
		return step.next.kind == compactionAgain
	})
	after := storedRows(t, service.store, artifactID)
	if len(after) != 1 || after[0].version != before[len(before)-1].version {
		t.Fatalf("the compacted document holds rows %v, want the head %d alone", rowVersions(after), before[len(before)-1].version)
	}
}

// A compaction another writer drops - a prune between its read and its write - is retried after the
// queue's backoff, and the retry folds the rows the prune left.
func TestTheCompactionQueueRetriesACompactionAPruneDropped(t *testing.T) {
	database := storetest.Open(t)
	persist := NewPgVersioned(database)
	service := New(Deps{Store: database, Persistence: persist, Events: events.NewBroker(), Settle: time.Hour})
	t.Cleanup(func() { _ = service.Shutdown(context.Background()) })
	backoff := 50 * time.Millisecond
	steps := quickCompactionQueue(service, backoff)
	artifactID := storeAgedRows(t, database, 6, 20*time.Minute)
	var pruned LoadedDocument
	duringCompactionFold(persist, artifactID, func() {
		if err := persist.PruneAfter(context.Background(), artifactID, 4, nil); err != nil {
			t.Errorf("prune to version 4: %v", err)
		}
		var err error
		if pruned, err = NewPgVersioned(database).LoadDocument(context.Background(), artifactID); err != nil {
			t.Errorf("load the pruned document: %v", err)
		}
	})

	if _, err := persist.Load(context.Background(), artifactID); err != nil {
		t.Fatalf("load the document: %v", err)
	}
	steps.await(t, artifactID, 0, "the queue to drop the compacted document", func(step compactionStepRecord) bool {
		return step.next.kind == compactionDone
	})
	got := steps.of(artifactID)
	want := []compactionStepRecord{
		{room: artifactID, next: compactionNext{kind: compactionRetry}},
		{room: artifactID, retry: backoff, next: compactionNext{kind: compactionAgain}},
		{room: artifactID, next: compactionNext{kind: compactionDone}},
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("the queue's steps = %v, want the dropped compaction retried after %s and then compacted: %v", got, backoff, want)
	}
	if after := storedRows(t, database, artifactID); len(after) != 1 || after[0].version != 4 {
		t.Fatalf("the document holds rows %v after the retry, want the pruned rows folded into version 4", rowVersions(after))
	}
	prunedReading := docMarkdown(pruned.Doc) + "\n" + rawReading(t, pruned.Doc)
	if reading := storedReading(t, NewPgVersioned(database), artifactID); reading != prunedReading {
		t.Fatalf("the document reads\n%s\nwant the pruned one\n%s", reading, prunedReading)
	}
}

// A document whose lock another holder has is retried, the backoff doubling to its cap, and
// compacts on the first retry after the lock is free.
func TestTheCompactionQueueRetriesABusyDocumentUntilItsLockIsFree(t *testing.T) {
	database := storetest.Open(t)
	service := New(Deps{Store: database, Events: events.NewBroker(), Settle: time.Hour})
	t.Cleanup(func() { _ = service.Shutdown(context.Background()) })
	backoff := 20 * time.Millisecond
	steps := quickCompactionQueue(service, backoff)
	artifactID := storeAgedRows(t, database, 4, 20*time.Minute)
	before := storedRows(t, database, artifactID)
	unlock := holdDocumentLock(t, database, artifactID)

	loadStored(t, service, artifactID)
	steps.await(t, artifactID, 0, "four retries of the busy document", func(step compactionStepRecord) bool {
		return step.retry == 4*backoff && step.next.kind == compactionRetry
	})
	requireRowsAsStored(t, database, artifactID, before)
	var retries []time.Duration
	for _, step := range steps.of(artifactID) {
		retries = append(retries, step.retry)
	}
	if want := []time.Duration{0, backoff, 2 * backoff, 4 * backoff}; fmt.Sprint(retries[:4]) != fmt.Sprint(want) {
		t.Fatalf("the busy document's steps ran after backoffs %v, want %v", retries, want)
	}

	from := steps.count()
	unlock()
	steps.await(t, artifactID, from, "the retry after the lock is free to compact", func(step compactionStepRecord) bool {
		return step.retry > 0 && step.next.kind == compactionAgain
	})
	if after := storedRows(t, database, artifactID); len(after) != 1 || after[0].version != before[len(before)-1].version {
		t.Fatalf("the document holds rows %v, want the head %d alone", rowVersions(after), before[len(before)-1].version)
	}
}

// A retry finds the document has no row older than the tail after its first, and the queue stops
// retrying it but does not drop it: the young rows stay as stored, and the queue checks the
// document again for when they come due, which compacts it.
func TestTheCompactionQueueComesBackToARetriedDocumentWhenItsYoungRowsComeDue(t *testing.T) {
	database := storetest.Open(t)
	service := New(Deps{Store: database, Events: events.NewBroker(), Settle: time.Hour})
	t.Cleanup(func() { _ = service.Shutdown(context.Background()) })
	steps := quickCompactionQueue(service, 20*time.Millisecond)
	artifactID := storeAgedRows(t, database, 4, 20*time.Minute)
	unlock := holdDocumentLock(t, database, artifactID)

	loadStored(t, service, artifactID)
	steps.await(t, artifactID, 0, "a retry of the busy document", func(step compactionStepRecord) bool {
		return step.retry > 0 && step.next.kind == compactionRetry
	})
	ageUpdatesThrough(t, database, artifactID, everyVersion, 0)
	young := storedRows(t, database, artifactID)
	step := steps.await(t, artifactID, 0, "the retry that finds no old row to check the document again later", func(step compactionStepRecord) bool {
		return step.retry > 0 && step.next.kind == compactionLater
	})
	if wait := step.next.after; wait > compactMaxTailAge || wait < compactMaxTailAge-time.Minute {
		t.Fatalf("the queue checks the young rows again in %s, want about %s, when they are due", wait, compactMaxTailAge)
	}
	waitFor(t, 10*time.Second, "the queue to schedule the document's check", func() bool {
		return queueWaitsOn(service, artifactID)
	})
	requireRowsAsStored(t, database, artifactID, young)

	unlock()
	ageUpdatesThrough(t, database, artifactID, everyVersion, 20*time.Minute)
	from := steps.count()
	recheckNow(service)
	steps.await(t, artifactID, from, "the check once the rows are due to compact them", func(step compactionStepRecord) bool {
		return step.retry == 0 && step.next.kind == compactionAgain
	})
	if after := storedRows(t, database, artifactID); len(after) != 1 || after[0].version != young[len(young)-1].version {
		t.Fatalf("the document holds rows %v, want the head %d alone", rowVersions(after), young[len(young)-1].version)
	}
}

// A note that comes while its document's step runs is kept even when that step drops the document:
// an append that commits as the check after a compaction finds no row after the first has the
// queue check the document again, and once the appended row is due that check compacts it.
func TestTheCompactionQueueKeepsANoteThatComesWhileItsDocumentsStepRuns(t *testing.T) {
	database := storetest.Open(t)
	persist := NewPgVersioned(database)
	service := New(Deps{Store: database, Persistence: persist, Events: events.NewBroker(), Settle: time.Hour})
	t.Cleanup(func() { _ = service.Shutdown(context.Background()) })
	steps := quickCompactionQueue(service, 50*time.Millisecond)
	artifactID := createDocument(t, database, "renoted")
	writer := newProofHistory(t, 101)
	appendUpdates(t, NewPgVersioned(database), artifactID, writer.write("Row 0.\n"), writer.write("Row 1.\n"), writer.write("Row 2.\n"))
	ageUpdatesThrough(t, database, artifactID, everyVersion, 20*time.Minute)
	late := writer.write("Row 3.\n")
	// The append runs on the worker after the step that drops the document has decided, before the
	// queue settles that step: the room is still in flight when the append notes it.
	appended := make(chan error, 1)
	var once sync.Once
	record := service.compactions.afterStep
	service.compactions.afterStep = func(room string, retry time.Duration, next compactionNext) {
		if room == artifactID && next.kind == compactionDone {
			once.Do(func() {
				_, err := persist.AppendUpdate(context.Background(), artifactID, late)
				appended <- err
			})
		}
		record(room, retry, next)
	}

	loadStored(t, service, artifactID)
	steps.await(t, artifactID, 0, "the queue to compact the document", func(step compactionStepRecord) bool {
		return step.next.kind == compactionAgain
	})
	steps.await(t, artifactID, 0, "the check after the compaction to find no row after the first", func(step compactionStepRecord) bool {
		return step.next.kind == compactionDone
	})
	if err := <-appended; err != nil {
		t.Fatalf("append while the step was in flight: %v", err)
	}
	step := steps.await(t, artifactID, 0, "the check the note in flight queued", func(step compactionStepRecord) bool {
		return step.next.kind == compactionLater
	})
	if wait := step.next.after; wait > compactMaxTailAge || wait < compactMaxTailAge-time.Minute {
		t.Fatalf("the queue checks the appended row again in %s, want about %s, when it is due", wait, compactMaxTailAge)
	}
	waitFor(t, 10*time.Second, "the queue to schedule the document's check", func() bool {
		return queueWaitsOn(service, artifactID)
	})
	before := storedRows(t, database, artifactID)
	if len(before) != 2 {
		t.Fatalf("the document holds rows %v, want the compacted row and the appended one", rowVersions(before))
	}

	ageUpdatesThrough(t, database, artifactID, everyVersion, 20*time.Minute)
	from := steps.count()
	recheckNow(service)
	steps.await(t, artifactID, from, "the check once the appended row is due to compact it", func(step compactionStepRecord) bool {
		return step.next.kind == compactionAgain
	})
	if after := storedRows(t, database, artifactID); len(after) != 1 || after[0].version != before[1].version {
		t.Fatalf("the document holds rows %v, want the appended head %d alone", rowVersions(after), before[1].version)
	}
}

// A burst of appends that starts after a quiet check brings the document's check forward: the
// document waits out its oldest row's compactMaxTailAge after a check that found it not due, and
// every append of the burst notes it, so once compactMinRows of the burst's rows are older than the
// tail the next note's check folds them, rather than leaving them all for the 15-minute check. The
// document then holds its fold point, the rows younger than the tail and no older row.
func TestABurstAfterAQuietCheckBringsTheDocumentsCheckForward(t *testing.T) {
	database := storetest.Open(t)
	persist := NewPgVersioned(database)
	service := New(Deps{Store: database, Persistence: persist, Events: events.NewBroker(), Settle: time.Hour})
	t.Cleanup(func() { _ = service.Shutdown(context.Background()) })
	steps := quickCompactionQueue(service, 50*time.Millisecond)
	artifactID := createDocument(t, database, "burst")
	writer := newProofHistory(t, 101)
	quiet := NewPgVersioned(database)
	for index := range 4 {
		appendUpdates(t, quiet, artifactID, writer.write(fmt.Sprintf("Quiet %d.\n", index)))
	}
	ageUpdatesThrough(t, database, artifactID, everyVersion, 5*time.Minute)

	loadStored(t, service, artifactID)
	step := steps.await(t, artifactID, 0, "the quiet check", func(step compactionStepRecord) bool {
		return step.next.kind == compactionLater
	})
	if wait := step.next.after; wait < 9*time.Minute {
		t.Fatalf("the quiet check schedules the next in %s, want about 10 minutes, the oldest row's age rule", wait)
	}
	waitFor(t, 10*time.Second, "the queue to schedule the quiet document's check", func() bool {
		return queueWaitsOn(service, artifactID)
	})

	// Every note of the burst brings the check forward, so the compaction may come from the check of
	// any of them once the burst is of age, or from the young row's; either leaves the same two rows.
	from := steps.count()
	for index := range 20 {
		appendUpdates(t, persist, artifactID, writer.write(fmt.Sprintf("Burst %d.\n", index)))
	}
	// The burst comes of age, as it does compactTail after it was written.
	ageUpdatesThrough(t, database, artifactID, everyVersion, 5*time.Minute)
	appendUpdates(t, persist, artifactID, writer.write("Young.\n"))
	steps.await(t, artifactID, from, "a check the burst's notes brought forward to compact it", func(step compactionStepRecord) bool {
		return step.next.kind == compactionAgain
	})
	if after := storedRows(t, database, artifactID); len(after) != 2 {
		t.Fatalf("the document holds rows %v, want the fold point and the young row alone", rowVersions(after))
	}
}

// The queue's compaction never runs on a request: a joined write and a browser's edit of the
// document each return while the queue's compaction of it is held inside its fold, and the
// compaction then folds the rows it read beneath theirs.
func TestWritesReturnWhileTheCompactionQueueIsInsideTheirDocumentsFold(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	steps := quickCompactionQueue(service, 50*time.Millisecond)
	persist := service.persistence.(*PgVersioned)
	held, release := holdCompactionFold(t, persist, artifactID)
	seedServiceText(t, service, artifactID, "First.\n")
	editLiveTree(t, service, artifactID, appendBlocks(t, "Second.\n"))
	waitForLandedAppends(t, service, artifactID)
	steps.await(t, artifactID, 0, "the queue's check of the young rows", func(step compactionStepRecord) bool {
		return step.next.kind == compactionLater
	})
	ageUpdatesThrough(t, service.store, artifactID, everyVersion, 20*time.Minute)
	rows := storedRows(t, service.store, artifactID)
	foldPoint := rows[len(rows)-1].version

	recheckNow(service)
	<-held
	wrote := make(chan error, 1)
	go func() {
		_, err := joinedReplaceText(service, artifactID, "Replaced.\n", model.Actor{Kind: "session", ID: "agent"})
		wrote <- err
	}()
	select {
	case err := <-wrote:
		if err != nil {
			t.Fatalf("the joined write: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the joined write did not return while the queue's compaction was inside its fold")
	}
	editLiveTree(t, service, artifactID, appendBlocks(t, "After.\n"))
	waitForLandedAppends(t, service, artifactID)
	written, cursor := storedRows(t, service.store, artifactID), settlementCursor(t, service.store, artifactID)
	if len(written) < len(rows)+2 {
		t.Fatalf("the writes stored %d rows over the %d before them, want both of theirs", len(written), len(rows))
	}

	from := steps.count()
	release()
	steps.await(t, artifactID, from, "the held compaction to compact", func(step compactionStepRecord) bool {
		return step.next.kind == compactionAgain
	})
	requireCompactedFrom(t, service.store, artifactID, written, foldPoint, cursor)
	if got := docMarkdown(loadDocument(t, persist, artifactID).Doc); got != "Replaced.\n\nAfter.\n" {
		t.Fatalf("the compacted document reads %q, want the joined write's and the browser's", got)
	}
}

// Shutdown lets the queue's compaction in flight finish and commit before it returns, takes no note
// once it has begun, and returns only once the queue's worker has.
func TestShutdownFinishesTheCompactionQueuesCompactionAndStopsItsWorker(t *testing.T) {
	database := storetest.Open(t)
	persist := NewPgVersioned(database)
	service := New(Deps{Store: database, Persistence: persist, Events: events.NewBroker(), Settle: time.Hour})
	t.Cleanup(func() { _ = service.Shutdown(context.Background()) })
	steps := quickCompactionQueue(service, 50*time.Millisecond)
	compacting := storeAgedRows(t, database, 4, 20*time.Minute)
	later := storeAgedRows(t, database, 4, 20*time.Minute)
	untouched := storedRows(t, database, later)
	held, release := holdCompactionFold(t, persist, compacting)

	if _, err := persist.Load(context.Background(), compacting); err != nil {
		t.Fatalf("load the document: %v", err)
	}
	<-held
	shutdown := make(chan error, 1)
	go func() { shutdown <- service.Shutdown(context.Background()) }()
	waitFor(t, 10*time.Second, "Shutdown to stop the queue's intake", func() bool {
		service.compactions.mu.Lock()
		defer service.compactions.mu.Unlock()
		return service.compactions.shut
	})
	if _, err := persist.Load(context.Background(), later); err != nil {
		t.Fatalf("load the other document: %v", err)
	}
	if queueHolds(service, later) {
		t.Fatal("the queue took a note after Shutdown began")
	}
	select {
	case err := <-shutdown:
		t.Fatalf("Shutdown returned (%v) while the queue's compaction was inside its fold", err)
	case <-time.After(200 * time.Millisecond):
	}

	release()
	select {
	case err := <-shutdown:
		if err != nil {
			t.Fatalf("shutdown: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("Shutdown did not return once the queue's compaction was released")
	}
	select {
	case <-service.compactions.done:
	default:
		t.Fatal("the queue's worker is still running after Shutdown returned")
	}
	if rows := storedUpdateCount(t, database, compacting); rows != 1 {
		t.Fatalf("the document held %d rows when Shutdown returned, want the compaction in flight committed", rows)
	}
	if got := steps.of(compacting); len(got) != 1 || got[0].next.kind != compactionAgain {
		t.Fatalf("the queue's steps of the compacted document = %v, want its one compaction and no check after Shutdown", got)
	}
	requireRowsAsStored(t, database, later, untouched)
}

// Quiesce waits for the queue's compaction in flight, takes no note while it runs and drops what the
// queue held, and the queue takes notes again once it returns.
func TestQuiesceWaitsForTheCompactionQueueAndReopensIt(t *testing.T) {
	database := storetest.Open(t)
	persist := NewPgVersioned(database)
	service := New(Deps{Store: database, Persistence: persist, Events: events.NewBroker(), Settle: time.Hour})
	t.Cleanup(func() { _ = service.Shutdown(context.Background()) })
	steps := quickCompactionQueue(service, 50*time.Millisecond)
	compacting := storeAgedRows(t, database, 4, 20*time.Minute)
	young := storeAgedRows(t, database, 4, 0)
	later := storeAgedRows(t, database, 4, 20*time.Minute)
	held, release := holdCompactionFold(t, persist, compacting)

	if _, err := persist.Load(context.Background(), young); err != nil {
		t.Fatalf("load the young document: %v", err)
	}
	steps.await(t, young, 0, "the queue's check of the young document", func(step compactionStepRecord) bool {
		return step.next.kind == compactionLater
	})
	if _, err := persist.Load(context.Background(), compacting); err != nil {
		t.Fatalf("load the document: %v", err)
	}
	<-held
	quiesced := make(chan error, 1)
	go func() { quiesced <- service.Quiesce(context.Background()) }()
	waitFor(t, 10*time.Second, "Quiesce to pause the queue", func() bool {
		service.compactions.mu.Lock()
		defer service.compactions.mu.Unlock()
		return service.compactions.paused
	})
	if _, err := persist.Load(context.Background(), later); err != nil {
		t.Fatalf("load the other document: %v", err)
	}
	if queueHolds(service, later) {
		t.Fatal("the queue took a note while Quiesce ran")
	}
	select {
	case err := <-quiesced:
		t.Fatalf("Quiesce returned (%v) while the queue's compaction was inside its fold", err)
	case <-time.After(200 * time.Millisecond):
	}

	release()
	select {
	case err := <-quiesced:
		if err != nil {
			t.Fatalf("quiesce: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("Quiesce did not return once the queue's compaction was released")
	}
	if rows := storedUpdateCount(t, database, compacting); rows != 1 {
		t.Fatalf("the document held %d rows when Quiesce returned, want the compaction in flight committed", rows)
	}
	service.compactions.mu.Lock()
	queued := len(service.compactions.rooms)
	service.compactions.mu.Unlock()
	if queued != 0 {
		t.Fatalf("the queue holds %d documents after Quiesce, want none", queued)
	}

	if _, err := persist.Load(context.Background(), later); err != nil {
		t.Fatalf("load the other document after Quiesce: %v", err)
	}
	waitFor(t, 30*time.Second, "the queue to compact a document noted after Quiesce", func() bool {
		return storedUpdateCount(t, database, later) == 1
	})
}

// The rooms Shutdown closes compact as they close, and leave every row younger than the tail as
// written, byte for byte and under the transaction that wrote it.
func TestShutdownsCompactionLeavesTheYoungRowsAsWritten(t *testing.T) {
	database := storetest.Open(t)
	artifactID := createDocument(t, database, "# First")
	service := New(Deps{Store: database, Events: events.NewBroker(), Settle: time.Hour})
	t.Cleanup(func() { _ = service.Shutdown(context.Background()) })
	seedServiceText(t, service, artifactID, "First.\n")
	editLiveTree(t, service, artifactID, appendBlocks(t, "Second.\n"))
	waitForLandedAppends(t, service, artifactID)
	ageUpdatesThrough(t, database, artifactID, everyVersion, 20*time.Minute)
	old := storedRows(t, database, artifactID)
	editLiveTree(t, service, artifactID, appendBlocks(t, "Third.\n"))
	editLiveTree(t, service, artifactID, appendBlocks(t, "Fourth.\n"))
	waitForLandedAppends(t, service, artifactID)
	young := storedRows(t, database, artifactID)[len(old):]
	if len(young) < 2 {
		t.Fatalf("the young edits stored %d rows, want two", len(young))
	}

	if err := service.Shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	after := storedRows(t, database, artifactID)
	if after[0].version != old[len(old)-1].version {
		t.Fatalf("the document holds rows %v after Shutdown, want the old rows folded into version %d", rowVersions(after), old[len(old)-1].version)
	}
	for _, row := range young {
		found := false
		for _, stands := range after {
			found = found || sameRow(stands, row)
		}
		if !found {
			t.Fatalf("young row %d was rewritten or deleted at Shutdown; rows now %v", row.version, rowVersions(after))
		}
	}
}

// compactionStepRecord is one step the compaction queue ran: its room, the backoff it retried
// under (zero for a check), and what it decided.
type compactionStepRecord struct {
	room  string
	retry time.Duration
	next  compactionNext
}

// compactionSteps is every step a service's compaction queue has run, in order.
type compactionSteps struct {
	mu    sync.Mutex
	steps []compactionStepRecord
}

// quickCompactionQueue makes the service's compaction queue check a noted room at once and retry
// after backoff, doubling to four times it, and returns the record of its steps. It runs before
// anything notes a room, so the worker reads none of it before it is set.
func quickCompactionQueue(service *Service, backoff time.Duration) *compactionSteps {
	steps := &compactionSteps{}
	q := service.compactions
	q.afterStep = func(room string, retry time.Duration, next compactionNext) {
		steps.mu.Lock()
		defer steps.mu.Unlock()
		steps.steps = append(steps.steps, compactionStepRecord{room: room, retry: retry, next: next})
	}
	q.mu.Lock()
	q.timing = compactionTiming{backoff: backoff, backoffCap: 4 * backoff}
	q.mu.Unlock()
	return steps
}

func (s *compactionSteps) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.steps)
}

// of is the steps the queue has run for room, in order.
func (s *compactionSteps) of(room string) []compactionStepRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	var steps []compactionStepRecord
	for _, step := range s.steps {
		if step.room == room {
			steps = append(steps, step)
		}
	}
	return steps
}

// await waits for a step of room at or after index from that want accepts, and returns it.
func (s *compactionSteps) await(t *testing.T, room string, from int, description string, want func(compactionStepRecord) bool) compactionStepRecord {
	t.Helper()
	var found compactionStepRecord
	waitFor(t, 30*time.Second, description, func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		for _, step := range s.steps[min(from, len(s.steps)):] {
			if step.room == room && want(step) {
				found = step
				return true
			}
		}
		return false
	})
	return found
}

// recheckNow brings every room the service's compaction queue waits on to its check now, as the
// time the queue scheduled it for would once it came.
func recheckNow(service *Service) {
	q := service.compactions
	q.mu.Lock()
	now := time.Now()
	for _, entry := range q.due {
		entry.at = now
	}
	q.mu.Unlock()
	q.signal()
}

// queueHolds reports whether the service's compaction queue holds room.
func queueHolds(service *Service, room string) bool {
	service.compactions.mu.Lock()
	defer service.compactions.mu.Unlock()
	return service.compactions.rooms[room] != nil
}

// queueWaitsOn reports whether the service's compaction queue has room scheduled for a step, its
// step in flight settled.
func queueWaitsOn(service *Service, room string) bool {
	q := service.compactions
	q.mu.Lock()
	defer q.mu.Unlock()
	for _, entry := range q.due {
		if entry.room == room {
			return true
		}
	}
	return false
}

// holdCompactionFold holds the first compaction of artifactID on versioned inside its fold: held is
// closed once it is there, and release lets it go on, which the test's cleanup does too.
func holdCompactionFold(t *testing.T, versioned *PgVersioned, artifactID string) (<-chan struct{}, func()) {
	t.Helper()
	held, released := make(chan struct{}), make(chan struct{})
	var once sync.Once
	release := func() { once.Do(func() { close(released) }) }
	t.Cleanup(release)
	duringCompactionFold(versioned, artifactID, func() {
		close(held)
		<-released
	})
	return held, release
}

// storeAgedRows stores rows rows of a new document through a store no service notes, each created
// age before the database's clock, and returns the document.
func storeAgedRows(t *testing.T, database *store.Store, rows int, age time.Duration) string {
	t.Helper()
	artifactID := createDocument(t, database, "aged")
	versioned := NewPgVersioned(database)
	writer := newProofHistory(t, 101)
	for index := range rows {
		appendUpdates(t, versioned, artifactID, writer.write(fmt.Sprintf("Row %d.\n", index)))
	}
	ageUpdatesThrough(t, database, artifactID, everyVersion, age)
	return artifactID
}

// loadStored loads the document as a room's load does (PgVersioned.Load), without a room.
func loadStored(t *testing.T, service *Service, artifactID string) {
	t.Helper()
	if _, err := service.persistence.Load(context.Background(), artifactID); err != nil {
		t.Fatalf("load the document: %v", err)
	}
}
