package main

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// The restart report's times, in Unix ns: the listener is killed at 100 s, the start that stays up
// execs at 130 s and answers at 132 s.
const (
	testKillNs     = 100 * 1e9
	testRestartNs  = 130 * 1e9
	testAnsweredNs = 132 * 1e9
)

func testRestartTimes() restartTimes {
	return restartTimes{KillNs: testKillNs, FirstStartNs: testRestartNs, RestartNs: testRestartNs, AnsweredNs: testAnsweredNs, Starts: 1}
}

// frameAt is one relay frame: session, stream sequence, the seconds at which the stream stored it
// and at which the session got it, and its event id.
func frameAt(session string, seq uint64, storedS, recvS float64, id int64) relayFrameRow {
	return relayFrameRow{
		Session: session, Subject: "notifications.agent." + session, Seq: seq, Delivered: 1,
		StoredAt: int64(storedS * 1e9), Recv: int64(recvS * 1e9), EventID: id,
		Identity: "lgb-" + strconv.FormatInt(id, 10), Done: true,
	}
}

// TestRestartReportTimesEverySessionsBacklogFromTheRestartExec: a session's backlog is what the
// stream stored while the listener was down, plus what was in flight at the kill; its time is its
// last backlog frame counted from the exec of the listener that stayed up, and the report's
// maximum is the run's "every session has its backlog" time. A frame stored and received before
// the kill is no backlog.
func TestRestartReportTimesEverySessionsBacklogFromTheRestartExec(t *testing.T) {
	frames := []relayFrameRow{
		frameAt("ses-lg-0000", 1, 90, 95, 11),     // before the kill, received before it
		frameAt("ses-lg-0000", 2, 95, 133, 12),    // in flight at the kill
		frameAt("ses-lg-0000", 3, 110, 134, 13),   // stored during the outage
		frameAt("ses-lg-0001", 4, 120, 135.5, 14), // stored during the outage
	}
	got := buildRestartReport(frames, testRestartTimes(), backlogSummary{})
	if got.SessionsWithBacklog != 2 || got.BacklogFrames != 3 {
		t.Errorf("sessions with a backlog %d, backlog frames %d; want 2 and 3 (the frame received before the kill is no backlog)", got.SessionsWithBacklog, got.BacklogFrames)
	}
	// pct takes the lower nearest rank, so with two sessions every percentile below the max is the
	// lower of the two.
	if want := (stat{N: 2, P50: 4000, P90: 4000, P99: 4000, Max: 5500}); got.BacklogMsAfterRestart != want {
		t.Errorf("backlog ms after the restart %+v, want %+v", got.BacklogMsAfterRestart, want)
	}
	if got.BacklogMsAfterFirstAnswerMax != 3500 {
		t.Errorf("backlog ms after the first answer %v, want 3500", got.BacklogMsAfterFirstAnswerMax)
	}
	if got.BacklogFramesPerSessionMedian != 1 {
		t.Errorf("backlog frames per session median %v, want 1 (1 and 2)", got.BacklogFramesPerSessionMedian)
	}
	if got.ListenerDownS != 30 || got.RestartToFirstAnswerMs != 2000 || got.Starts != 1 {
		t.Errorf("down %v s, restart to first answer %v ms, starts %d; want 30, 2000 and 1", got.ListenerDownS, got.RestartToFirstAnswerMs, got.Starts)
	}
}

// TestRestartReportCountsEachBacklogEnvelopeOnceAndNamesTheMissing: every published envelope
// should reach its session exactly once; one that reached none is missing and named, one that
// reached two sessions is counted apart.
func TestRestartReportCountsEachBacklogEnvelopeOnceAndNamesTheMissing(t *testing.T) {
	frames := []relayFrameRow{
		frameAt("ses-lg-0000", 1, 110, 133, 1000),
		frameAt("ses-lg-0000", 2, 110, 133, 1001),
		frameAt("ses-lg-0001", 3, 110, 133, 1001),
		frameAt("ses-lg-0001", 4, 110, 133, 7), // not a backlog envelope's id
	}
	backlog := backlogSummary{IDFrom: 1000, IDTo: 1002, Published: 3, PublishErrors: 1}
	got := buildRestartReport(frames, testRestartTimes(), backlog)
	if got.BacklogReceivedOnce != 1 || got.BacklogReceivedTwice != 1 || got.BacklogMissing != 1 {
		t.Errorf("received once %d, more than once %d, missing %d; want 1, 1 and 1", got.BacklogReceivedOnce, got.BacklogReceivedTwice, got.BacklogMissing)
	}
	if len(got.BacklogMissingIDs) != 1 || got.BacklogMissingIDs[0] != 1002 {
		t.Errorf("missing ids %v, want [1002]", got.BacklogMissingIDs)
	}
	if got.BacklogPublished != 3 || got.BacklogPublishErrors != 1 {
		t.Errorf("published %d, publish errors %d; want the backlog run's own 3 and 1", got.BacklogPublished, got.BacklogPublishErrors)
	}
}

// TestRestartReportSaysTheLedgerHidesACopyOfAWrittenFrame: a copy of a stream sequence the session
// received and answered done is one the plugin's ledger answers done without showing, and the
// report says when it arrived.
func TestRestartReportSaysTheLedgerHidesACopyOfAWrittenFrame(t *testing.T) {
	first := frameAt("ses-lg-0000", 7, 90, 95, 21)
	copied := frameAt("ses-lg-0000", 7, 90, 134, 21)
	copied.Delivered = 2
	got := buildRestartReport([]relayFrameRow{first, copied}, testRestartTimes(), backlogSummary{})
	if len(got.Duplicates) != 1 {
		t.Fatalf("duplicates %+v, want the one copy", got.Duplicates)
	}
	d := got.Duplicates[0]
	if d.Verdict != verdictLedgerDedupe || !d.EarlierDone || d.When != whenAfterKill || d.StreamSeq != 7 || d.EventID != 21 {
		t.Errorf("duplicate %+v, want the ledger's verdict on a done-answered frame, after the kill, seq 7, event 21", d)
	}
	if got.DuplicateCounts != (duplicateCounts{Total: 1, AfterKill: 1, LedgerDedupe: 1}) {
		t.Errorf("duplicate counts %+v, want one after the kill, hidden by the ledger", got.DuplicateCounts)
	}
	if len(got.shownTwice()) != 0 {
		t.Errorf("shown twice %+v, want none: the plugin's ledger holds this copy", got.shownTwice())
	}
	if got.Frames != 2 || got.SessionsWithBacklog != 0 {
		t.Errorf("frames %d, sessions with a backlog %d; want 2 and 0: a copy is no backlog frame", got.Frames, got.SessionsWithBacklog)
	}
}

// TestRestartReportSaysACopyOfAnUnwrittenFrameIsNotShownTwice: a copy of a frame the session never
// answered done is a redelivery of a frame the plugin had not written, so the transcript holds it
// once however often the relay sends it.
func TestRestartReportSaysACopyOfAnUnwrittenFrameIsNotShownTwice(t *testing.T) {
	first := frameAt("ses-lg-0000", 7, 90, 95, 21)
	first.Done = false
	copied := frameAt("ses-lg-0000", 7, 90, 96, 21)
	got := buildRestartReport([]relayFrameRow{first, copied}, testRestartTimes(), backlogSummary{})
	if len(got.Duplicates) != 1 || got.Duplicates[0].Verdict != verdictUnwrittenRedelivery {
		t.Fatalf("duplicates %+v, want one redelivery of an unwritten frame", got.Duplicates)
	}
	if d := got.Duplicates[0]; d.EarlierDone || d.When != whenBeforeKill {
		t.Errorf("duplicate %+v, want the earlier copy unanswered and a copy from before the kill", d)
	}
	if got.DuplicateCounts != (duplicateCounts{Total: 1, BeforeKill: 1, UnwrittenRedelivery: 1}) {
		t.Errorf("duplicate counts %+v, want one before the kill, a redelivery of an unwritten frame", got.DuplicateCounts)
	}
	if len(got.shownTwice()) != 0 {
		t.Errorf("shown twice %+v, want none", got.shownTwice())
	}
}

// TestRestartReportCallsACopyWithoutADedupeIdentityADefect: a copy of a frame the session answered
// done whose envelope carries neither event_id nor dedupe_key is one the plugin's ledger could not
// recognise, so it would be written twice. The report calls it out.
func TestRestartReportCallsACopyWithoutADedupeIdentityADefect(t *testing.T) {
	first := frameAt("ses-lg-0000", 7, 90, 95, 21)
	first.Identity = ""
	copied := frameAt("ses-lg-0000", 7, 90, 134, 21)
	copied.Identity = ""
	got := buildRestartReport([]relayFrameRow{first, copied}, testRestartTimes(), backlogSummary{})
	if len(got.Duplicates) != 1 || got.Duplicates[0].Verdict != verdictShownTwice {
		t.Fatalf("duplicates %+v, want one that would be shown twice", got.Duplicates)
	}
	if got.DuplicateCounts.ShownTwice != 1 || len(got.shownTwice()) != 1 {
		t.Errorf("shown twice %d / %+v, want the one copy", got.DuplicateCounts.ShownTwice, got.shownTwice())
	}
}

// TestRestartReportReadsTheRunsFilesByColumnName: the report reads relay-frames.csv by its column
// names, so a column added or moved does not change what it reads; a file without the columns it
// needs is refused by name.
func TestRestartReportReadsTheRunsFilesByColumnName(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "relay-frames.csv")
	csv := "done,event_id,stream_seq,recv_ns,stored_at_ns,identity,delivered,subject,session\n" +
		"true,21,7,134000000000,90000000000,lgb-21,2,notifications.agent.ses-lg-0000,ses-lg-0000\n"
	if err := os.WriteFile(path, []byte(csv), 0o644); err != nil {
		t.Fatal(err)
	}
	rows, err := readRelayFrames(path)
	if err != nil {
		t.Fatalf("read the frames: %v", err)
	}
	want := relayFrameRow{Session: "ses-lg-0000", Subject: "notifications.agent.ses-lg-0000", Seq: 7, Delivered: 2,
		StoredAt: 90 * 1e9, Recv: 134 * 1e9, EventID: 21, Identity: "lgb-21", Done: true}
	if len(rows) != 1 || rows[0] != want {
		t.Errorf("rows %+v, want [%+v]", rows, want)
	}
	old := filepath.Join(dir, "old.csv")
	if err := os.WriteFile(old, []byte("session,stream_seq,recv_ns,stored_at_ns,event_id,delivered\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := readRelayFrames(old); err == nil {
		t.Errorf("a file without the identity and done columns was read; want a refusal naming them")
	}
}

// TestRestartTimesMustBeInOrder: a restart.json whose times are missing or out of order is refused,
// so no report times a run against a half-written file.
func TestRestartTimesMustBeInOrder(t *testing.T) {
	if err := testRestartTimes().validate(); err != nil {
		t.Errorf("the run's own times were refused: %v", err)
	}
	for name, times := range map[string]restartTimes{
		"no kill":                {RestartNs: 2, AnsweredNs: 3},
		"no answer":              {KillNs: 1, RestartNs: 2},
		"restarted before dying": {KillNs: 3, RestartNs: 2, AnsweredNs: 4},
		"answered before it ran": {KillNs: 1, RestartNs: 3, AnsweredNs: 2},
	} {
		if err := times.validate(); err == nil {
			t.Errorf("%s was accepted; want a refusal", name)
		}
	}
}

// TestBacklogPlanGivesEverySessionItsShareInRounds: the publisher spreads its envelopes evenly
// over the time it is given, in rounds, so every session has its first envelope before any has its
// second, and the event ids run from the base in publish order.
func TestBacklogPlanGivesEverySessionItsShareInRounds(t *testing.T) {
	plans := planBacklog(3, 2, 10*time.Second, 900)
	if len(plans) != 6 {
		t.Fatalf("plans %+v, want 6", plans)
	}
	for i, want := range []string{"ses-lg-0000", "ses-lg-0001", "ses-lg-0002", "ses-lg-0000", "ses-lg-0001", "ses-lg-0002"} {
		if plans[i].Session != want || plans[i].ID != int64(900+i) {
			t.Errorf("plan %d is %+v, want session %s and id %d", i, plans[i], want, 900+i)
		}
	}
	if plans[0].At != 0 || plans[5].At != 10*time.Second {
		t.Errorf("the first goes out at %v and the last at %v, want 0 and the whole 10s", plans[0].At, plans[5].At)
	}
	env := backlogEnvelope(plans[0])
	if env.Topic != "notifications.agent.ses-lg-0000" || env.Payload != `{"id":900,"type":"loadtest.backlog"}` {
		t.Errorf("envelope topic %q payload %q, want the session's agent subject and the event id in the payload", env.Topic, env.Payload)
	}
	if err := env.Validate(); err != nil {
		t.Errorf("the backlog envelope is not a valid envelope: %v", err)
	}
	if backlogMsgID(plans[0]) == backlogMsgID(plans[3]) {
		t.Errorf("two envelopes of one session share a Nats-Msg-Id, so the stream would store one of them")
	}
}
