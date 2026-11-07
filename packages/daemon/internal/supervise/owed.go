package supervise

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5/pgconn"
)

// owedClaimWrite is one claim write the store did not take and the machine still owes: the whole
// of what a failed write leaves behind, held on the Machine until the write is made. It carries
// nothing of the claim, because the claim the repair writes is the one the machine holds when
// the repair runs rather than a copy taken when the write failed (repair), and it names no store
// method either: which call the repair makes is decided afresh from what the claim holds then,
// and the method of each failure is in that failure's own log line. What is left is the durable
// reports those failed decisions never made.
type owedClaimWrite struct {
	// reports are the durable reports the failed decisions owed, oldest first, made when the
	// write they waited on lands — at a repair, or at any claim write of a later decision that
	// lands first, which is the repair too (settleOwedWrite, makeOwedReports). There is one per
	// decision that owed a report rather than one per mark: a second failing decision while the
	// first is still owed adds its report here instead of replacing it, since the two name
	// different transitions and the daemon acts on each.
	reports []owedReport
}

// owedReport is one report a decision would have made had its write landed, kept until the write
// lands. The reports are the daemon's own record of what a claim durably became: it stamps a
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
// machine that already owes one keeps the single mark, since the repair writes the claim as it
// stands rather than one write per failure; method names the call that failed for the log line
// alone, and each failure's line carries its own.
//
// It is split out of owes for the two callers that do not put a failure to the rule. One is the
// repair's own failure, which has already asked. The other is a retirement the store refused
// because it had already made it (retirePending), which knows from the refusal itself that the
// claim write of that same transaction was rolled back with the delete — a fact about the
// store's transaction rather than about the shape of the error. Asking writeOwed there would be
// asking the wrong question, and would answer by accident: the sentinel carries no SQLSTATE, so
// the rule owes it, but it would stop owing it the day the refusal is spelled as a server error.
func (m *Machine) oweWrite(method string, err error) {
	if m.owedWrite == nil {
		m.owedWrite = &owedClaimWrite{}
	}
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

// oweDurableState keeps the report a decision owed when its own write was left owed, made once
// that write lands, by the repair or by whatever claim write lands first (settleOwedWrite,
// makeOwedReports). Every report is made after the write of the transition it reports, so a
// write that failed skips the report with it, and the daemon never learns a claim fact its
// memory already holds — the working report of a turn that started is the one the measured
// incident lost.
//
// A claim that owes no write owes no report either: the write the server refused will be refused
// again, so no write of this claim is coming to make the report, and a report of a transition
// the row will never hold would tell the daemon more than the store can say.
func (m *Machine) oweDurableState(state ClaimState) {
	if m.owedWrite == nil {
		return
	}
	m.owedWrite.reports = append(m.owedWrite.reports, owedReport{state: state, claim: copyClaim(m.claim)})
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
// It goes through the machine's own write helpers (putClaim, putClaimAndPending), so the write
// it makes is the write every other decision makes, under the same deadline and normalized the
// same way. What follows a write that landed is theirs too — the mark dropped, the line that
// says the write was made, and the reports those failed decisions skipped (settleOwedWrite) —
// because a claim write that lands is the repair whoever made it, and this function would
// otherwise be a second place that knows what landing means. What is left here is the
// head-of-decision entry point and the handling of a repair that failed; method names the call
// for those failure lines alone, since the line of a write that landed is written where it
// landed.
//
// It returns nothing. A repair that fails must not drop the event behind it: the error would
// reach supervisor.decisionFailed in internal/daemon, which logs it and drops the event, and an
// event dropped for a write the database did not take is the fault this change exists to end. A
// repair that failed keeps the mark, logs it, and the decision goes on to its own row, which will
// be owed too if the database is still away.
//
// A failure the rule does not owe drops the mark: the server answered and refused the write, and
// it would refuse the same write again, so keeping the mark would only buy the same refusal at
// every decision from here on. The reports the mark held go with it, and the line that records
// the drop carries how many of them there were (droppedReports), because that line is the whole
// record of what was lost: R6 asks it to say that the write and the reports it held are dropped,
// and a line naming only the write would leave no trace that anything went with it — the states
// and the generations those reports carried are gone from the daemon the moment the mark is.
func (m *Machine) repair(ctx context.Context) {
	if m.owedWrite == nil {
		return
	}
	method, pending := "PutClaim", m.claim.Pending
	var err error
	if pending != nil {
		method = "PutClaimAndDelivery"
		err = m.putClaimAndPending(ctx)
	} else {
		err = m.putClaim(ctx)
	}
	// A write that landed settled the mark inside the helper that made it, reports and all.
	if err == nil {
		return
	}
	if writeOwed(err) {
		m.oweWrite(method, err)
		return
	}
	m.log.Warn(owedWriteLine, "store", method, "error", err, "owed", false,
		"droppedReports", len(m.owedWrite.reports))
	m.owedWrite = nil
}

// settleOwedWrite settles the write the machine owes, now that a claim write has landed, and
// returns at once when nothing is owed. A claim write that lands while a write is owed is the
// repair, whichever decision made it: every claim write that reaches here writes the claim as
// the machine holds it — putClaim and putClaimAndPending from m.stored(), and retirePending its
// own claim write, written in the same transaction as the delete — which is exactly what the
// repair would have written. The row is no longer behind memory, so there is nothing left for a
// later repair to do, and nothing of the order to get wrong.
//
// That order is why the owed reports are made here rather than left to the next repair. They are
// made before the landing decision's own report, which follows its own write, so the daemon is
// told what the claim durably became in the order the decisions made it: the working of a turn
// whose write failed, then the ready of the decision whose write settled it. Left to a later
// repair, the landing decision's own report would come first and the owed ones after it, which
// is the order nothing lived.
//
// It settles the claim write and nothing else. The pending delivery's own mark (unsaved) is left
// where it is, because a claim write that lands says nothing about the delivery row: a landing
// putClaim wrote the claim alone, and clearing the delivery's mark there would drop a delivery
// write that was never made. The writes that do cover both rows clear it themselves, where it is
// true of what they wrote (putClaimAndPending, retirePending).
//
// method is the store call that landed, for the line that closes the story (repairedWriteLine):
// an operator counts the pair of lines, and the one that ends it names the write that made good.
func (m *Machine) settleOwedWrite(method string) {
	if m.owedWrite == nil {
		return
	}
	// The reports are taken before the mark goes, since the mark is what holds them.
	reports := m.owedWrite.reports
	m.owedWrite = nil
	m.log.Info(repairedWriteLine, "store", method)
	m.makeOwedReports(reports)
}

// makeOwedReports makes the reports the failed decisions owed, oldest first, now that the write
// they waited on has landed. Each is handed to the daemon exactly as its own decision would have
// handed it (report), so what the daemon learns from a claim whose database was away is what it
// would have learnt had every write been taken, only later.
//
// One kind of owed report is dropped instead, because what it would tell the daemon is no longer
// true: a report of a generation the claim has left. The daemon keys the workflow fact on the
// claim's generation (applyTerminal in internal/daemon), so such a report names a fact of a run
// that is over, which no report of the generation now running stands in for and which nothing
// running wants to hear.
//
// No report made here can be one the claim has reported past, so none is dropped as superseded.
// Every report follows a claim write that landed, and a claim write that lands while a write is
// owed makes the owed reports first (settleOwedWrite): between the write that failed and the
// next one that lands, the claim reports nothing at all, and when one lands the reports it owed
// go before the report of the decision that made it. So the oldest owed report is still the
// oldest fact the daemon has not been told, and the whole of what is owed is told in the order
// the decisions made it.
func (m *Machine) makeOwedReports(reports []owedReport) {
	for _, r := range reports {
		if r.claim.Generation != m.claim.Generation {
			m.log.Info("supervise: an owed report is dropped; the claim has left the generation it was owed at",
				"state", r.state, "generation", r.claim.Generation)
			continue
		}
		m.report(r.claim, r.state)
	}
}

// writeOwed is the rule for a claim write the store refused: a write is owed unless the server
// judged the statement. A server that judged it read the statement and decided against it — a
// constraint it breaks, a column it has not got, a fence it does not pass — and it would decide
// the same way again, so that refusal is reported once and never repeated, and the write is not
// made a second time. Every other failure leaves the write unmade, or its fate unknown, and is
// owed: dropping one loses a claim fact the daemon's memory already holds and the row does not,
// and the write is the only thing that would ever put it there. A nil error is no refusal and
// owes nothing.
//
// The rule reads the error in two steps, and their order is the substance of it rather than a
// style of writing.
//
// The wrapper comes first: an error that unwraps to a *pgconn.ConnectError is owed whatever it
// carries. The connection never opened, so no statement of this write ever reached a server, and
// nothing in such an error can be a judgement of it. That covers the dial that was refused, which
// carries no SQLSTATE at all, and equally a connection PostgreSQL itself refused, which pgx
// answers as the server's own *pgconn.PgError inside that wrapper (newPerDialConnectError in
// pgconn/pgconn.go, whose Unwrap keeps the chain open) and which can carry any SQLSTATE
// whatever — a restart's 57P03, and a rotated password's 28P01, whose class the second step
// would not owe at all.
//
// The SQLSTATE comes second, and only then is it the server's answer about a statement it was
// given: an error that unwraps to a *pgconn.PgError is owed when that code's class, its first two
// characters, is one of the six a server answers with about something other than the statement:
//
//   - 08, connection exception: the connection itself is what failed, and the statement went down
//     with it.
//   - 40, transaction rollback: the server rolled its own transaction back — a serialization
//     failure, a deadlock it broke — so the statement was abandoned rather than judged.
//   - 53, insufficient resources: the memory, the disk or the connection slots the server had not
//     got for this statement, and will have again.
//   - 55, object not in prerequisite state: an object another session holds, a lock not available
//     chief among them, rather than a statement the server read and refused.
//   - 57, operator intervention: a backend terminated, a query cancelled, a database shutting down
//     or starting up — the platform's doing or an operator's, not the statement's.
//   - 58, system error: a file or an input/output error of the server's own, which says nothing
//     about what was asked of it.
//
// Any other class is the server judging the statement, and is not owed. A code shorter than two
// characters has no class to read and judges nothing, so it is owed with the rest rather than
// taken for a decision about the statement. An error that unwraps to neither type is owed too:
// the network cut, and a write the claim-write deadline cut short before any answer (R5).
//
// Read the other way round — the SQLSTATE first, the wrapper after — the rule refuses every
// connection the server itself refused, because such an error does carry a PgError and its class
// is whatever the refusal was: a rotated password's 28P01 would read as a statement judged, and
// the write of a claim fact only memory holds would be dropped for a statement no server ever
// read. That is this issue's own fault, reached through the rule written to end it. The same
// order, for the same reason, is how the outbox tells a statement Postgres refused from a
// connection it refused (refusedStatement in packages/envoy's
// internal/dispatch/outbox/lookups.go); it is another module, so the rule is kept consistent with
// it by hand rather than imported.
//
// The identities below are what pgx v5.11.0 returns, measured in this pod on 2026-11-07, and they
// are the only evidence this rule rests on:
//
// |what was done|error|Go type|SQLSTATE|pgconn.SafeToRetry|
// |---|---|---|---|---|
// |a pool at 127.0.0.1:1, then Exec|failed to connect to `user=… database=…`: … dial error: … connection refused|*pgconn.ConnectError|none|false|
// |Exec under a one second context against a listener that accepts and never answers|context deadline exceeded|context.deadlineExceededError|none|false|
// |pg_terminate_backend of the pool's one backend, then Exec|FATAL: terminating connection due to administrator command (SQLSTATE 57P01)|*pgconn.PgError|57P01|false|
// |a listener answering the startup message with an ErrorResponse, as a restarting managed instance answers a new connection|failed to connect to `user=… database=…`: … server error: FATAL: the database system is starting up (SQLSTATE 57P03)|*pgconn.PgError inside a *pgconn.ConnectError|57P03|false|
// |the same listener answering the startup message with the refusal a rotated password draws|failed to connect to `user=… database=…`: … server error: FATAL: password authentication failed for user "…" (SQLSTATE 28P01)|*pgconn.PgError inside a *pgconn.ConnectError|28P01|false|
//
// The last two rows are the ones the order is for. Both are owed by the first step, because
// neither connection opened. Read the SQLSTATE first and the 57P03 row is owed only by the
// accident of its class being one of the six, while the 28P01 row is refused outright: the
// write is dropped, and the claim fact only memory holds is lost, for a statement no server
// ever read.
//
// The rows are also why the rule cannot be a list of error types. The first is a pgx type, the
// second one the standard library returns through a path that reaches no pgx type at all, the
// third is neither, and the last two are both at once; what the owed ones share is not a type but
// the absence of a judgement of the statement, which no list can be kept in step with. They are
// also why the rule cannot be pgconn.SafeToRetry: it is false for all five — it reports whether
// pgx knows the statement never reached the server, not whether the server decided against it —
// so a rule built on it would drop the dial failure and the cut-short write, the two that are
// most plainly owed. Only asking whether the server judged the statement covers every row.
func writeOwed(err error) bool {
	if err == nil {
		return false
	}
	// The wrapper first: a connection that never opened judged no statement, whatever the server
	// said in refusing the connection itself.
	var connErr *pgconn.ConnectError
	if errors.As(err, &connErr) {
		return true
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || len(pgErr.Code) < 2 {
		return true
	}
	switch pgErr.Code[:2] {
	case "08", "40", "53", "55", "57", "58":
		return true
	default:
		return false
	}
}
