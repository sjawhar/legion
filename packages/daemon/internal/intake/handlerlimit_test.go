package intake

import (
	"context"
	"encoding/json"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// The three rows of this file hold the relation dispatch://INFRA-1574 put in the place of the
// fixed ten-minute intake handler limit: the limit one fact's handling runs under is derived from
// the acknowledgement window that bounds each of that fact's reads, so a deployment which widens
// worker_rpc_timeout_seconds widens the limit with it, instead of leaving a review's three
// bounded reads to fill a limit that never moved. The relation is worth a test of its own because
// nothing in the daemon refuses a configuration that breaks it: a window wide enough for the
// reads to outlast a fixed limit would simply have every review cut, over and over, with the cut
// warning as its only sign.
//
// The first row is the derivation itself, over the windows a legion.yaml can carry, and it drives
// the one path the daemon's own configuration takes to that derivation, normalizedSpec. The
// second is the guard on the count the derivation multiplies: boundedReadsPerFact is a constant,
// and only a row that walks ConsumerSpec's own fields can notice when a read is added to the spec
// and the constant is left behind. The third is the witness, on a live consumer, that what the
// derivation promises is what a review's handling is actually given: one whole window for each
// bounded read, and the handling as a whole inside the derived limit.

// maxTimerWindow is the largest worker_rpc_timeout_seconds a legion.yaml may carry: the largest
// whole number of seconds whose milliseconds fit a signed 32-bit timer, the bound both daemons
// hold every duration key to (maxTimerSeconds and durationKeys in
// packages/daemon/internal/config/config.go, lines 233 and 244). The table below ends on it
// because the derivation multiplies its window by four and nothing refuses the product: a legal
// configuration must still yield a limit a caller can hold, so the largest legal window is the
// one row that says the arithmetic does not overflow or wrap.
const maxTimerWindow = 2_147_483 * time.Second

// What this row proves is the whole of HandlerLimitFor's contract, over every window a deployment
// can reach it with: both of its answers exactly, and, for every row alike, the relation those
// answers exist for — each of a fact's bounded reads has a whole window of the limit, and a whole
// window is still left over for the part nothing bounds. Asserting the relation beside the exact
// values is what keeps the row honest if the derivation is ever rewritten: a new formula that
// happens to agree with one table entry still has to leave the reads their windows for every
// other.
//
// It then drives normalizedSpec, because that, not HandlerLimitFor, is what the daemon's own
// configuration reaches: a caller of OpenConsumers that sets no HandlerLimit gets the derivation,
// and one that sets its own keeps exactly what it set, which is what the package's own rigs rely
// on when they cut a handler in under a second (ackprogress_test.go:175, :535, :601, :666).
func TestTheIntakeHandlerLimitCoversTheReadsItsWindowBounds(t *testing.T) {
	for _, row := range []struct {
		// name says which deployment the window belongs to, since the number alone does not.
		name    string
		window  time.Duration
		limit   time.Duration
		derived bool
	}{
		{"the daemon's default worker_rpc_timeout_seconds", 5 * time.Second, defaultHandlerLimit, false},
		{"the widest window any caller in this tree passes today", 30 * time.Second, defaultHandlerLimit, false},
		{"the last window the floor covers", 2*time.Minute + 30*time.Second, defaultHandlerLimit, false},
		{"the window of the issue, where a review's reads filled the old fixed limit", 200 * time.Second, 800 * time.Second, true},
		{"a window of a quarter of an hour", 15 * time.Minute, 60 * time.Minute, true},
		{"the largest window a legion.yaml may carry", maxTimerWindow, 4 * maxTimerWindow, true},
	} {
		t.Run(row.name, func(t *testing.T) {
			limit, derived := HandlerLimitFor(row.window)
			if limit != row.limit || derived != row.derived {
				t.Fatalf("HandlerLimitFor(%s) = %s, %t; want %s, %t", row.window, limit, derived, row.limit, row.derived)
			}
			// The relation itself, which is what the two numbers above are for. A fact's bounded
			// reads run one after another inside handleMessage, each cut at one window, so the
			// limit has to hold all of them end to end.
			reads := boundedReadsPerFact * row.window
			if limit < reads {
				t.Errorf("a window of %s derives a limit of %s, which is less than the %d bounded reads of %s it has to hold (%s)",
					row.window, limit, boundedReadsPerFact, row.window, reads)
			}
			// And one whole window more, for the part of the handling nothing bounds: ApplyFact's
			// wait on the one advisory lock, and the fact's own transaction. A limit that left
			// the reads no headroom would cut the transaction of every fact whose reads were slow
			// but inside their own bounds.
			if limit-reads < row.window {
				t.Errorf("a window of %s derives a limit of %s, which leaves %s above its bounded reads, less than the one whole window the unbounded part is owed",
					row.window, limit, limit-reads)
			}
			// No window makes the limit shorter than the ten minutes this daemon has always had,
			// which is what leaves every deployment on a small window exactly where it was.
			if limit < defaultHandlerLimit {
				t.Errorf("a window of %s derives a limit of %s, below the floor of %s", row.window, limit, defaultHandlerLimit)
			}
		})
	}

	// The derivation only matters where the daemon meets it, and that is normalizedSpec: every
	// caller of OpenConsumers passes through it, and the daemon passes worker_rpc_timeout_seconds
	// as AckWait and sets no HandlerLimit of its own.
	withWindow := func(ackWait, handlerLimit time.Duration) ConsumerSpec {
		spec := consumerSpec(&lockedBuffer{})
		spec.AckWait, spec.HandlerLimit = ackWait, handlerLimit
		return spec
	}
	for _, row := range []struct {
		name string
		spec ConsumerSpec
		want time.Duration
	}{
		{"a window above the floor derives the limit the daemon runs under", withWindow(200*time.Second, 0), 800 * time.Second},
		{"a window the floor covers leaves the floor in place", withWindow(5*time.Second, 0), defaultHandlerLimit},
		// A rig that cuts a handler in well under a second is the reason a caller-set limit is
		// kept rather than widened to the derivation: those rows would wait out the floor.
		{"a caller that sets a limit keeps exactly what it set", withWindow(200*time.Second, 600*time.Millisecond), 600 * time.Millisecond},
	} {
		t.Run(row.name, func(t *testing.T) {
			normalized, err := normalizedSpec(row.spec)
			if err != nil {
				t.Fatalf("normalizedSpec: %v", err)
			}
			if normalized.HandlerLimit != row.want {
				t.Errorf("a spec with AckWait %s and HandlerLimit %s normalizes to a handler limit of %s, want %s",
					row.spec.AckWait, row.spec.HandlerLimit, normalized.HandlerLimit, row.want)
			}
		})
	}
}

// reviewBoundedReads are the ConsumerSpec function fields a review fact's handling reads, by field
// name, with what each one reads: the reads boundedReadsPerFact counts, each of them cut at one
// AckWait inside handleMessage and made one after another, so they add up rather than overlap.
var reviewBoundedReads = map[string]string{
	"ReviewBody":       "a truncated review-App review's full body, read from GitHub so the workflow can read the Legion footer the cap cut off (resolveReviewBody)",
	"ReviewPermission": "whether the review's author may write to the repository, which is what lets the review decide a round (resolveReviewPermission)",
}

// otherFactBoundedReads are the ConsumerSpec function fields some fact other than a review reads,
// by field name, with the fact that makes the read and why that fact makes fewer of them than a
// review does. They are bounded reads like the two above, and they are not counted, because
// boundedReadsPerFact is the most any one fact makes and no fact here makes more than one.
var otherFactBoundedReads = map[string]string{
	"RepositoryMapping": "RepoMappingChanged reads the project's repository mapping again (readMapping); handleMessage returns the moment that read answers, so this fact reaches no other read and no transaction",
	"PrepareRepository": "a DispatchIssue fact has the repository it resolves to readied before it is applied (prepareRepository); it is no pull request's event, so recordedPullRequest answers true with no read of the pool, and neither review resolver touches it: one read in all",
	"PrepareArtifact":   "a DispatchArtifact fact has the issue its review is on readied before it is applied (prepareArtifact); it reaches the pool and GitHub through nothing else, for the same reason a DispatchIssue fact does not: one read in all",
}

// consumerSpecNotReads are the ConsumerSpec function fields that are no read at all, by field
// name, with what each is instead. Nothing bounds them and nothing in the limit answers for them.
var consumerSpecNotReads = map[string]string{
	"Now": "the clock Consumers reads when it records a consumer opened or a message handled and when Lag measures an age; it leaves neither the process nor the handling's own goroutine",
}

// recordedPullRequestRead is the one bounded read of a review's handling that reflection cannot
// find: recordedPullRequest asks the pool whether the review's pull request is recorded, under
// one AckWait of its own and before either GitHub read, and it reads the pool the Consumers were
// opened on rather than a function field of the spec, so ConsumerSpec has no field for it to be
// classified as. The row below names it here, by hand, because leaving it out would make the
// count come to one less than boundedReadsPerFact and the guard would read as a derivation that
// overcounts.
const recordedPullRequestRead = 1

// This row is the guard that keeps boundedReadsPerFact honest, and it is the one thing in the
// tree that can: the constant is the count of the bounded reads one fact's handling makes, the
// derivation multiplies it, and a read added to ConsumerSpec later compiles, runs and is bounded
// at AckWait without a single existing test noticing that the limit no longer covers it. So the
// row reads ConsumerSpec's own function fields, the way
// packages/daemon/internal/api/settings_test.go reads config.Config's
// (settingsConfigTypes, TestSettingsOfClassifiesEveryConfigField), and holds every one of them to
// exactly one of three tables: a bounded read a review makes, a bounded read some other fact
// makes, or no read at all. A field in none of them fails this row by name, so whoever adds the
// read has to say which it is and bring boundedReadsPerFact with them.
func TestEveryConsumerSpecReadIsCountedInTheHandlerLimit(t *testing.T) {
	tables := map[string]map[string]string{
		"reviewBoundedReads":    reviewBoundedReads,
		"otherFactBoundedReads": otherFactBoundedReads,
		"consumerSpecNotReads":  consumerSpecNotReads,
	}
	spec := reflect.TypeFor[ConsumerSpec]()
	seen := map[string]bool{}
	for i := range spec.NumField() {
		field := spec.Field(i)
		// Only a function field can be a read: everything else the spec carries is a value the
		// caller sets, which no handling waits on.
		if field.Type.Kind() != reflect.Func {
			continue
		}
		seen[field.Name] = true
		var in []string
		for name, table := range tables {
			if _, ok := table[field.Name]; ok {
				in = append(in, name)
			}
		}
		switch len(in) {
		case 1:
		case 0:
			t.Errorf("ConsumerSpec.%s is in none of reviewBoundedReads, otherFactBoundedReads and consumerSpecNotReads: decide whether a review's handling makes this read, whether some other fact's does, or whether it is no read at all, put it in one of the three tables, and have boundedReadsPerFact count it where it counts",
				field.Name)
		default:
			t.Errorf("ConsumerSpec.%s is in more than one table (%v): a read a review makes, a read another fact makes and no read at all are three different things for the limit", field.Name, in)
		}
	}
	for name, table := range tables {
		for field := range table {
			if !seen[field] {
				t.Errorf("%s names %s, which is no function field of ConsumerSpec", name, field)
			}
		}
	}

	// And the count itself: a review's reads are the two the spec carries plus the one it does
	// not (recordedPullRequestRead), and that is the number the derivation gives a window each.
	if counted := len(reviewBoundedReads) + recordedPullRequestRead; counted != boundedReadsPerFact {
		t.Errorf("a review's handling makes %d bounded reads (%d function fields of ConsumerSpec and recordedPullRequest), while boundedReadsPerFact is %d: HandlerLimitFor gives each read a window, so the two must agree",
			counted, len(reviewBoundedReads), boundedReadsPerFact)
	}
}

// recordedDeadline is what one step of a fact's handling saw of the context it was handed: that
// it ran at all, whether that context carried a deadline, and how much of the deadline was left
// when it was called. The row below reads these instead of waiting on anything, so it asserts the
// whole shape of the derived limit — a window for each read, the limit for the handling — while
// the fact itself is handled at once. Each step records from the consumer's own goroutine while
// the test body reads afterwards, so the three values are kept under a lock.
type recordedDeadline struct {
	mu          sync.Mutex
	called      bool
	hasDeadline bool
	remaining   time.Duration
}

func (r *recordedDeadline) record(ctx context.Context) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.called = true
	deadline, ok := ctx.Deadline()
	r.hasDeadline = ok
	if ok {
		r.remaining = time.Until(deadline)
	}
}

func (r *recordedDeadline) read() (called, hasDeadline bool, remaining time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.called, r.hasDeadline, r.remaining
}

// truncatedReviewApproval is the captured review envelope with its payload rewritten to carry a
// review id and a truncated body, the two things resolveReviewBody asks of a review before it
// reads GitHub for the full text; the capture names an author and an approval already, which is
// what makes the review decide a round and so makes resolveReviewPermission read as well. The
// payload is rewritten the way TestDecodingCarriesTheReviewOrder rewrites it: the envelope
// carries its payload as JSON text, so the text is parsed, changed and written back.
func truncatedReviewApproval(t *testing.T) []byte {
	t.Helper()
	var envelope map[string]any
	if err := json.Unmarshal(capturedGitHubEnvelope(t, "review.json"), &envelope); err != nil {
		t.Fatalf("parse the captured review: %v", err)
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(envelope["payload"].(string)), &payload); err != nil {
		t.Fatalf("parse the captured review's payload: %v", err)
	}
	payload["review_id"] = "5325101010"
	payload["body_truncated"] = "true"
	text, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("encode the review's payload: %v", err)
	}
	envelope["payload"] = string(text)
	data, err := json.Marshal(envelope)
	if err != nil {
		t.Fatalf("encode the review: %v", err)
	}
	return data
}

// readSlack is how much less than a whole acknowledgement window a bounded read may be left with
// before this row calls the window broken. A read's context is made with
// context.WithTimeout(work, spec.AckWait) at the moment the read is reached, so what it loses is
// only the work the consumer does between making the context and calling the resolver — a type
// switch and a function call, microseconds of it. Five seconds is therefore not a measurement of
// anything: it is slack far above what a loaded, shared machine can add to that path, chosen so
// the row cannot fail for the pod's load, while remaining small enough that a read bounded at
// anything other than the window (a third of it, say, or the limit itself) fails the row.
const readSlack = 5 * time.Second

// This row is the witness on a real consumer that the derived limit is not just arithmetic: a
// review fact, whose handling makes all three bounded reads, is published to the daemon's own
// GitHub durable, and each step says what deadline it was handed. The two the spec carries each
// get a whole acknowledgement window, and the handler — the last thing the handling reaches, past
// every read — still has more than all three windows left, under the derived limit. That is the
// claim the issue's change makes, and no unit test of HandlerLimitFor can make it: it depends on
// handleMessage nesting each read's bound under the work context and on consumeMessage building
// that work context from the spec's own limit.
//
// It waits for nothing. The window is wide (ten minutes derives forty) exactly so that no
// deadline here can pass while the row runs, and the fact is handled the moment it is delivered;
// what the row measures are deadlines, not elapsed time.
func TestAReviewFactsBoundedReadsFitTheDerivedLimit(t *testing.T) {
	pool := migratedPool(t)
	js, stream := testJetStream(t)
	spec := consumerSpec(&lockedBuffer{})
	// A window of ten minutes derives a limit of forty, well above the floor, so what the
	// consumer runs under is the derivation and not defaultHandlerLimit. HandlerLimit is left
	// unset for the same reason: the daemon sets none either.
	spec.AckWait = 10 * time.Minute
	limit, derived := HandlerLimitFor(spec.AckWait)
	if want := 40 * time.Minute; limit != want || !derived {
		t.Fatalf("HandlerLimitFor(%s) = %s, %t; want %s and a derived limit, which is what this row runs the consumer under", spec.AckWait, limit, derived, want)
	}

	var body, permission, handler recordedDeadline
	spec.ReviewBody = func(ctx context.Context, _ PullRequestReview) (string, error) {
		body.record(ctx)
		return "Captured review, restored from GitHub", nil
	}
	spec.ReviewPermission = func(ctx context.Context, _ PullRequestReview) (bool, error) {
		permission.record(ctx)
		return true, nil
	}

	// The workflow applies a review only to a pull request it records, and a review of any other
	// is acknowledged before either GitHub read is reached (recordedPullRequest), so the row
	// records the pull request the captured review names first.
	recordPullRequest(t, pool, "LEGION-208", 42)

	stop := startConsume(t, js, spec, pool, handlerFunc(func(ctx context.Context, _ pgx.Tx, _ Fact) (Result, error) {
		handler.record(ctx)
		return Result{}, nil
	}))
	defer stop()

	published := publishAt(t, js, "notifications.github.sjawhar.legion.pr.42.review", truncatedReviewApproval(t))
	waitAcknowledged(t, stream, githubConsumerName(spec.Project), published)

	for _, read := range []struct {
		name     string
		recorded *recordedDeadline
	}{
		{"the review's truncated body", &body},
		{"the review author's repository permission", &permission},
	} {
		called, hasDeadline, remaining := read.recorded.read()
		if !called {
			t.Fatalf("%s was never read: the row cannot say what window a read it did not make was given", read.name)
		}
		if !hasDeadline {
			t.Errorf("%s was read under a context with no deadline, where the acknowledgement window bounds nothing", read.name)
			continue
		}
		if remaining > spec.AckWait {
			t.Errorf("%s was given %s, more than the acknowledgement window of %s that is supposed to bound it", read.name, remaining, spec.AckWait)
		}
		if remaining < spec.AckWait-readSlack {
			t.Errorf("%s was given %s, less than the acknowledgement window of %s it is owed (slack %s)", read.name, remaining, spec.AckWait, readSlack)
		}
	}

	called, hasDeadline, remaining := handler.read()
	if !called {
		t.Fatalf("the fact was never applied: the row cannot say what limit a handling that did not happen ran under")
	}
	if !hasDeadline {
		t.Fatalf("the fact was applied under a context with no deadline, so nothing cuts a wedged handler")
	}
	// Past all three reads, the handler still has more than the three windows they could have
	// taken: that headroom is the whole point of the fourth window, since the transaction it runs
	// in and its wait on the apply lock are what nothing else bounds.
	if reads := boundedReadsPerFact * spec.AckWait; remaining <= reads {
		t.Errorf("the handler was given %s, which is no more than the %s a fact's %d bounded reads may take: the derived limit leaves one window above them",
			remaining, reads, boundedReadsPerFact)
	}
	if remaining > limit {
		t.Errorf("the handler was given %s, more than the derived limit of %s the consumer is supposed to run under", remaining, limit)
	}
}
