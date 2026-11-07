package supervise

import (
	"context"
	"errors"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"
)

// owedClaimWrite is one claim write the store did not take and the machine still owes: the whole
// of what a failed write leaves behind, held on the Machine until the write is made. The write
// itself carries nothing of the claim, because the claim the repair writes is the one the machine
// holds when the repair runs rather than a copy taken when the write failed (repair). What is
// held beside it is what a reader of the log needs to follow the write from the failure to the
// repair, and the durable reports those failed decisions never made.
type owedClaimWrite struct {
	// method is the store method whose call failed ("PutClaim" or "PutClaimAndDelivery"), for the
	// log line a reader follows.
	method string
	// reports are the durable reports the failed decisions owed, oldest first, made by the repair
	// once the write they waited on has landed (makeOwedReports). There is one per decision that
	// owed a report rather than one per mark: a second failing decision while the first is still
	// owed adds its report here instead of replacing it, since the two name different transitions
	// and the daemon acts on each.
	reports []owedReport
}

// owedReport is one report a decision would have made had its write landed, kept until the repair
// makes it. The reports are the daemon's own record of what a claim durably became: it stamps a
// woken pod's first turn on working, and ends the workflow's phase on ready and on failed
// (workflowRuntime.claimDurableState in internal/daemon). One skipped by a failed write is a fact
// no later report makes good, because the fact is keyed on the claim's generation, so it is kept
// here rather than left to whatever the claim reports next.
type owedReport struct {
	// state is the durable transition to report.
	state ClaimState
	// claim is the claim copy of the decision that owed it, as it stood when the report was owed.
	// The report keeps its own copy because applyTerminal in internal/daemon reads the issue, the
	// role and the failure from it and keys the workflow fact on its generation, where the
	// repair's write uses the claim as the machine holds it then.
	claim Claim
	// made is the value of Machine.reportsMade when this report was owed, which is how a report
	// the claim has since superseded is told from one that still stands.
	made uint64
}

// owedWriteLine is the one message a write left owed is logged under, at Warn, both where the
// write failed and where a repair of it failed again. It is pinned as a constant because an
// operator and a production check read it whole: a search for this line is the search for every
// claim whose row is behind the daemon's memory, and a message that varied by call site would
// answer that search with a part of the answer.
const owedWriteLine = "supervise: a claim write the database did not take"

// repairedWriteLine is the close of that story, at Info: the owed write was made, and the row is
// what memory holds again. It is pinned for the same reason — the pair of lines is what an
// operator counts to see an outage end — and it is the line a check reads to tell a daemon that
// caught up from one that is still behind.
const repairedWriteLine = "supervise: a claim write the database did not take was made"

// oweWrite marks the claim write owed and says so in the log: the row the store holds is behind
// what memory holds, and the claim's next decision is to make that write again (repair). A
// machine that already owes one keeps the single mark and takes the newer method's name, since
// the repair writes the claim as it stands rather than one write per failure, and the newest
// failure is the one a reader of the log is following.
//
// It is split out of owes for the one caller that does not put its failure to the rule at all: a
// retirement the store refused because it had already made it (retirePending) knows from the
// refusal itself that the claim write of that same transaction was rolled back with the delete,
// which is a fact about the store's transaction rather than about the shape of the error. Asking
// writeOwed there would be asking the wrong question, and would answer by accident: the sentinel
// carries no SQLSTATE, so the rule owes it, but it would stop owing it the day the refusal is
// spelled as a server error.
func (m *Machine) oweWrite(method string, err error) {
	if m.owedWrite == nil {
		m.owedWrite = &owedClaimWrite{}
	}
	m.owedWrite.method = method
	m.log.Warn(owedWriteLine, "store", method, "error", err, "owed", true)
}

// owes marks the claim write owed when the rule owes err, and returns err as it was: the decision
// still fails, and its event is still dropped; what the mark adds is that the write itself is not
// lost with it. A failure the rule does not owe marks nothing: the server answered and refused
// the statement, and would refuse it again.
func (m *Machine) owes(method string, err error) error {
	if writeOwed(err) {
		m.oweWrite(method, err)
	}
	return err
}

// oweDurableState keeps the report a decision owed when its own write was left owed, for the
// repair to make once that write has landed (makeOwedReports). Every report is made after the
// write of the transition it reports, so a write that failed skips the report with it, and the
// daemon never learns a claim fact its memory already holds — the working report of a turn that
// started is the one the measured incident lost.
//
// A claim that owes no write owes no report either: the write the server refused will be refused
// again, so no repair is coming to make the report, and a report of a transition the row will
// never hold would tell the daemon more than the store can say.
func (m *Machine) oweDurableState(state ClaimState) {
	if m.owedWrite == nil {
		return
	}
	m.owedWrite.reports = append(m.owedWrite.reports, owedReport{state: state, claim: copyClaim(m.claim), made: m.reportsMade})
}

// repair makes the claim write the machine owes, at the head of the decision, before anything
// reads or writes the claim again (Handle). It is the whole of what this mark is for: a write the
// database did not take is made at the claim's next decision instead of being lost with the event
// that failed.
//
// It writes the claim as the machine holds it now, never a copy taken when the write failed.
// Every state the machine holds came from the worker's own frame — a claim that reads working is
// a claim whose agent said so — and no decision that marks a write owed puts its own field back,
// so the claim standing now is the claim every failed write was trying to record, together with
// whatever the claim has decided since. A copy would write an older claim over a newer one.
//
// It writes once: the claim with its pending delivery where one is pending, the claim alone where
// none is. One call covers both rows, so a claim that owes a claim write and holds an unsaved
// delivery is made whole here, and the settle that follows this call finds nothing unsaved and
// writes nothing — where two writes would leave the row in between, and the second would have to
// be owed in its turn.
//
// It makes the reports those failed decisions skipped, after the write and after the line that
// says the write landed (makeOwedReports): the daemon is told what the row now says, in the
// order the decisions said it, and never before the store holds it.
//
// It returns nothing. A repair that fails must not drop the event behind it: the error would
// reach supervisor.decisionFailed in internal/daemon, which logs it and drops the event, and an
// event dropped for a write the database did not take is the fault this change exists to end. A
// repair that failed keeps the mark, logs it, and the decision goes on to its own row, which will
// be owed too if the database is still away.
//
// A failure the rule does not owe drops the mark: the server answered and refused the write, and
// it would refuse the same write again, so keeping the mark would only buy the same refusal at
// every decision from here on.
func (m *Machine) repair(ctx context.Context) {
	if m.owedWrite == nil {
		return
	}
	method, pending := "PutClaim", m.claim.Pending
	if pending != nil {
		method = "PutClaimAndDelivery"
	}
	c := m.stored()
	err := m.write(ctx, func(ctx context.Context) error {
		if pending != nil {
			return m.deps.Store.PutClaimAndDelivery(ctx, c, *pending)
		}
		return m.deps.Store.PutClaim(ctx, c)
	})
	if err != nil {
		owed := writeOwed(err)
		m.log.Warn(owedWriteLine, "store", method, "error", err, "owed", owed)
		if owed {
			m.owedWrite.method = method
			return
		}
		m.owedWrite = nil
		return
	}
	// The reports are taken before the mark goes, since the mark is what holds them.
	reports := m.owedWrite.reports
	m.owedWrite, m.unsaved = nil, false
	m.log.Info(repairedWriteLine, "store", method)
	m.makeOwedReports(reports)
}

// makeOwedReports makes the reports the failed decisions owed, oldest first, now that the write
// they waited on has landed. Each is handed to the daemon exactly as its own decision would have
// handed it (report), so what the daemon learns from a claim whose database was away is what it
// would have learnt had every write been taken, only later.
//
// Two kinds of owed report are dropped instead, because what each would tell the daemon is no
// longer true. A report the claim has reported past is the first: a later report has already
// said where the claim is, and the older one speaks of a state the claim has left — a working
// owed by a turn that started, made after the failure that ended the claim, would tell the
// workflow a failed claim is at work. A report of a generation the claim has left is the second:
// the daemon keys the workflow fact on the claim's generation (applyTerminal in internal/daemon),
// so the report names a fact of a run that is over, which no report of the generation now
// running stands in for and which nothing running wants to hear.
//
// Reports owed with no successful report between them all stand, however many of them there are:
// they were owed against the same count, so none of them is the report that supersedes another,
// and the working of a turn that started is still true when the ready of the turn that ended is
// reported after it. That is why the count is read once, before any of them is made: read afresh
// for each, the first report made here would raise the count and drop every report behind it.
func (m *Machine) makeOwedReports(reports []owedReport) {
	// Read once, before any of them is made: the reports below raise the count themselves, and
	// each is judged against the claim as it stood when the repair began.
	made := m.reportsMade
	for _, r := range reports {
		switch {
		case r.made < made:
			m.log.Info("supervise: an owed report is dropped; the claim has reported since it was owed",
				"state", r.state, "generation", r.claim.Generation)
		case r.claim.Generation != m.claim.Generation:
			m.log.Info("supervise: an owed report is dropped; the claim has left the generation it was owed at",
				"state", r.state, "generation", r.claim.Generation)
		default:
			m.report(r.claim, r.state)
		}
	}
}

// writeOwed is the rule for a claim write the store refused: a write is owed unless the server
// answered and refused the statement. A server that answered has read the statement and decided
// against it — a constraint it breaks, a column it has not got, a fence it does not pass — and it
// would decide the same way again, so making that write a second time only buys the same refusal.
// Every other failure leaves the write unmade, or its fate unknown: the connection never came up,
// or it died with the statement in flight, or a deadline ended the wait before any answer. Those
// are owed, because dropping one loses a claim fact the daemon's memory already holds and the row
// does not, and the write is the only thing that would ever put it there. A nil error is no
// refusal and owes nothing.
//
// Two SQLSTATEs the server does answer with are not decisions about the statement, and are owed
// with the rest: class 08, the connection exception, where the connection itself is what failed,
// and 57P01, the backend terminated under the statement — the shutdown a managed restart of the
// database sends. Judging those two by their class and code is what keeps the rule to one
// question about the server's answer.
//
// The identities below are what pgx v5.11.0 returns, measured in this pod on 2026-11-07, and they
// are the only evidence this rule rests on:
//
// |what was done|error|Go type|SQLSTATE|pgconn.SafeToRetry|
// |---|---|---|---|---|
// |a pool at 127.0.0.1:1, then Exec|failed to connect to `user=… database=…`: … dial error: … connection refused|*pgconn.ConnectError|none|false|
// |Exec under a one second context against a listener that accepts and never answers|context deadline exceeded|context.deadlineExceededError|none|false|
// |pg_terminate_backend of the pool's one backend, then Exec|FATAL: terminating connection due to administrator command (SQLSTATE 57P01)|*pgconn.PgError|57P01|false|
//
// Those rows are why the rule cannot be written as a list of error types. The first is a pgx
// type, the second one the standard library returns through a path that reaches no pgx type at
// all, and the third is neither; what they share is not a type but the absence of an answer about
// the statement, which no list can be kept in step with. They are also why the rule cannot be
// pgconn.SafeToRetry: it is false for all three — it reports whether pgx knows the statement
// never reached the server, not whether the server decided against it — so a rule built on it
// would drop the dial failure and the cut-short write, the two that are most plainly owed. Only
// asking whether the server answered covers every row.
func writeOwed(err error) bool {
	if err == nil {
		return false
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return strings.HasPrefix(pgErr.Code, "08") || pgErr.Code == "57P01"
	}
	return true
}
