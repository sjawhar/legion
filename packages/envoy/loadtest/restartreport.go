package main

import (
	"encoding/csv"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// The listener-restart report (`loadgen restart-report`): what one restart run (restart-step.sh)
// measured. It reads the run's relay-frames.csv, the restart's own times (restart.json) and the
// backlog run's summary (backlog.json), and answers two questions:
//
//   - How long after the restarted listener's exec did every session have its whole backlog?
//   - Did any frame reach a session twice in a way the real plugin would write twice?
//
// The duplicate rules are the plugin's (packages/envoy-client/src/consumer-delivery.ts, and the
// ledger pi-envoy and claude-envoy persist):
//
//   - A frame of a stream sequence the session already received, whose earlier copy the session
//     answered `done`, is one the plugin had written and recorded in its ledger. The ledger answers
//     the copy done without showing it (`alreadyWritten`), as long as the frame carries a dedupe
//     identity (`parseIdentity`: the envelope's event_id, else its dedupe_key).
//   - A copy whose earlier one was not answered done is a redelivery of a frame the plugin had not
//     written. It is not shown twice either: a copy that arrives while the first is still held is
//     attached to it by its `<stream id>:<stream sequence>` claim, and a copy that arrives after it
//     was given up on is the first write of that frame.
//   - A copy of a done-answered frame that carries no dedupe identity would be handed to the
//     session again: nothing in the ledger can recognise it. That is a defect, and the report
//     names the run and fails.

// restartTimes is restart.json: when restart-step.sh killed the listener and when the start that
// stayed up ran, in Unix ns.
type restartTimes struct {
	OldPID int `json:"old_pid"`
	NewPID int `json:"new_pid"`
	// KillNs is the SIGKILL, FirstStartNs the first start attempt, RestartNs the exec of the start
	// that stayed up and AnsweredNs when it first answered /v1/sessions.
	KillNs       int64 `json:"kill_ns"`
	FirstStartNs int64 `json:"first_start_ns"`
	RestartNs    int64 `json:"restart_ns"`
	AnsweredNs   int64 `json:"answered_ns"`
	// Starts counts the start attempts, one more for each that exited at once (on gVisor the
	// killed listener's port can still be held: "bind: address already in use").
	Starts int `json:"starts"`
}

func (t restartTimes) validate() error {
	for _, f := range []struct {
		name string
		ns   int64
	}{{"kill_ns", t.KillNs}, {"restart_ns", t.RestartNs}, {"answered_ns", t.AnsweredNs}} {
		if f.ns <= 0 {
			return fmt.Errorf("restart times: %s is missing", f.name)
		}
	}
	if t.RestartNs < t.KillNs || t.AnsweredNs < t.RestartNs {
		return fmt.Errorf("restart times: the kill (%d), the restart (%d) and the first answer (%d) are not in order", t.KillNs, t.RestartNs, t.AnsweredNs)
	}
	return nil
}

// relayFrameRow is one row of relay-frames.csv (relaySessions.writeFrames).
type relayFrameRow struct {
	Session   string
	Subject   string
	Seq       uint64
	Delivered int
	StoredAt  int64
	Recv      int64
	EventID   int64
	Identity  string
	Done      bool
}

// duplicateFrame is one relay frame whose stream sequence its session had already received.
type duplicateFrame struct {
	Session   string `json:"session"`
	EventID   int64  `json:"event_id"`
	StreamSeq uint64 `json:"stream_seq"`
	Subject   string `json:"subject"`
	// When is "before the kill" or "after the kill", by the copy's own receipt time.
	When   string `json:"when"`
	RecvNs int64  `json:"recv_ns"`
	// EarlierDone is set when the session had answered the earlier copy `done`, so the plugin had
	// written that frame and its ledger holds the identity.
	EarlierDone bool `json:"earlier_copy_done_answered"`
	// Identity is the frame's dedupe identity; empty means the plugin's ledger cannot know it.
	Identity string `json:"identity"`
	// Verdict is what the real plugin would do with this copy: verdictLedgerDedupe,
	// verdictUnwrittenRedelivery or verdictShownTwice.
	Verdict string `json:"verdict"`
}

const (
	// verdictLedgerDedupe: the earlier copy was written, so the ledger answers this one done
	// without showing it.
	verdictLedgerDedupe = "ledger_dedupe"
	// verdictUnwrittenRedelivery: the earlier copy was never written, so this one is held by the
	// stream-sequence claim or is the frame's first write; the transcript holds it once.
	verdictUnwrittenRedelivery = "unwritten_redelivery"
	// verdictShownTwice: the earlier copy was written but the frame carries no dedupe identity, so
	// nothing would stop a second write. A defect.
	verdictShownTwice = "shown_twice"
)

// duplicateCounts counts the duplicates by verdict.
type duplicateCounts struct {
	Total               int `json:"total"`
	BeforeKill          int `json:"before_the_kill"`
	AfterKill           int `json:"after_the_kill"`
	LedgerDedupe        int `json:"ledger_dedupe"`
	UnwrittenRedelivery int `json:"unwritten_redelivery"`
	ShownTwice          int `json:"shown_twice"`
}

// restartReport is what one restart run measured.
type restartReport struct {
	Run string `json:"run"`
	// ListenerDownS is from the SIGKILL to the exec of the start that stayed up; Starts counts the
	// start attempts; FirstStartToRestartMs is from the first attempt to that exec (above 0 when a
	// start exited at once and was tried again); RestartToFirstAnswerMs is from that exec to the
	// listener's first answer.
	ListenerDownS          float64 `json:"listener_down_s"`
	Starts                 int     `json:"starts"`
	FirstStartToRestartMs  float64 `json:"first_start_to_restart_ms"`
	RestartToFirstAnswerMs float64 `json:"restart_to_first_answer_ms"`
	// Frames counts every relay frame in the run.
	Frames int `json:"frames"`
	// A session's backlog is every first frame whose message the stream stored while the listener
	// was down (stored between the kill and the restarted listener's first answer), and every one
	// stored before the kill but received after it (in flight at the kill).
	SessionsWithBacklog           int     `json:"sessions_with_backlog"`
	BacklogFrames                 int     `json:"backlog_frames"`
	BacklogFramesPerSessionMedian float64 `json:"backlog_frames_per_session_median"`
	// BacklogMsAfterRestart is each session's last backlog frame, in ms after the exec of the
	// listener that stayed up. Its max is the run's "every session has its backlog" time.
	BacklogMsAfterRestart stat `json:"backlog_ms_after_restart"`
	// BacklogMsAfterFirstAnswerMax is the same maximum counted from the listener's first answer.
	BacklogMsAfterFirstAnswerMax float64 `json:"backlog_ms_after_first_answer_max"`
	// The backlog run's own counts, and what reached the sessions: each published envelope should
	// reach its session exactly once.
	BacklogPublished     int     `json:"backlog_published"`
	BacklogPublishErrors int     `json:"backlog_publish_errors"`
	BacklogReceivedOnce  int     `json:"backlog_envelopes_received_once"`
	BacklogReceivedTwice int     `json:"backlog_envelopes_received_more_than_once"`
	BacklogMissing       int     `json:"backlog_envelopes_missing"`
	BacklogMissingIDs    []int64 `json:"backlog_envelopes_missing_ids,omitempty"`
	// Duplicates lists every frame of a stream sequence its session already had, with the plugin's
	// verdict on it.
	DuplicateCounts duplicateCounts  `json:"duplicate_counts"`
	Duplicates      []duplicateFrame `json:"duplicates"`
}

// shownTwice is every duplicate the real plugin would write twice.
func (r restartReport) shownTwice() []duplicateFrame {
	var out []duplicateFrame
	for _, d := range r.Duplicates {
		if d.Verdict == verdictShownTwice {
			out = append(out, d)
		}
	}
	return out
}

// maxDuplicatesListed bounds the duplicates the report lists one by one; the counts cover them
// all. A run whose duplicates are all of one shape needs no thousand rows to show it.
const maxDuplicatesListed = 500

// buildRestartReport is the whole analysis of one run: frames in arrival order, the restart's
// times, and the backlog run's summary.
func buildRestartReport(frames []relayFrameRow, times restartTimes, backlog backlogSummary) restartReport {
	out := restartReport{
		ListenerDownS:          float64(times.RestartNs-times.KillNs) / 1e9,
		Starts:                 times.Starts,
		FirstStartToRestartMs:  float64(times.RestartNs-times.FirstStartNs) / 1e6,
		RestartToFirstAnswerMs: float64(times.AnsweredNs-times.RestartNs) / 1e6,
		Frames:                 len(frames),
		BacklogPublished:       backlog.Published,
		BacklogPublishErrors:   backlog.PublishErrors,
		Duplicates:             []duplicateFrame{},
	}
	type key struct {
		session string
		seq     uint64
	}
	first := map[key]relayFrameRow{}
	last := map[string]int64{}  // session to its last backlog frame's receipt
	count := map[string]int{}   // session to its backlog frames
	received := map[int64]int{} // backlog event id to the sessions that got it, first copies only
	for _, f := range frames {
		k := key{f.Session, f.Seq}
		if earlier, ok := first[k]; ok {
			out.Duplicates = append(out.Duplicates, classifyDuplicate(f, earlier, times.KillNs))
			continue
		}
		first[k] = f
		if backlog.IDFrom <= f.EventID && f.EventID <= backlog.IDTo {
			received[f.EventID]++
		}
		inOutage := times.KillNs <= f.StoredAt && f.StoredAt < times.AnsweredNs
		inFlight := f.StoredAt < times.KillNs && f.Recv >= times.KillNs
		if inOutage || inFlight {
			count[f.Session]++
			last[f.Session] = max(last[f.Session], f.Recv)
		}
	}
	for _, d := range out.Duplicates {
		out.DuplicateCounts.Total++
		if d.When == whenBeforeKill {
			out.DuplicateCounts.BeforeKill++
		} else {
			out.DuplicateCounts.AfterKill++
		}
		switch d.Verdict {
		case verdictLedgerDedupe:
			out.DuplicateCounts.LedgerDedupe++
		case verdictUnwrittenRedelivery:
			out.DuplicateCounts.UnwrittenRedelivery++
		case verdictShownTwice:
			out.DuplicateCounts.ShownTwice++
		}
	}
	if len(out.Duplicates) > maxDuplicatesListed {
		// Keep every defect, and the first of the rest.
		kept := out.shownTwice()
		for _, d := range out.Duplicates {
			if len(kept) >= maxDuplicatesListed {
				break
			}
			if d.Verdict != verdictShownTwice {
				kept = append(kept, d)
			}
		}
		out.Duplicates = kept
	}
	out.SessionsWithBacklog = len(last)
	var times0 []float64
	var per []float64
	var lastRecv int64
	for session, recv := range last {
		times0 = append(times0, float64(recv-times.RestartNs)/1e6)
		per = append(per, float64(count[session]))
		out.BacklogFrames += count[session]
		lastRecv = max(lastRecv, recv)
	}
	out.BacklogMsAfterRestart = mkStat(times0)
	if len(per) > 0 {
		sort.Float64s(per)
		out.BacklogFramesPerSessionMedian = pct(per, 50)
		out.BacklogMsAfterFirstAnswerMax = float64(lastRecv-times.AnsweredNs) / 1e6
	}
	for id := backlog.IDFrom; backlog.IDFrom > 0 && id <= backlog.IDTo; id++ {
		switch n := received[id]; {
		case n == 0:
			out.BacklogMissing++
			if len(out.BacklogMissingIDs) < 20 {
				out.BacklogMissingIDs = append(out.BacklogMissingIDs, id)
			}
		case n == 1:
			out.BacklogReceivedOnce++
		default:
			out.BacklogReceivedTwice++
		}
	}
	return out
}

const (
	whenBeforeKill = "before the kill"
	whenAfterKill  = "after the kill"
)

// classifyDuplicate is the plugin's verdict on a copy of a frame the session already received.
func classifyDuplicate(copied, earlier relayFrameRow, killNs int64) duplicateFrame {
	d := duplicateFrame{
		Session: copied.Session, EventID: copied.EventID, StreamSeq: copied.Seq, Subject: copied.Subject,
		When: whenAfterKill, RecvNs: copied.Recv, EarlierDone: earlier.Done, Identity: copied.Identity,
	}
	if copied.Recv < killNs {
		d.When = whenBeforeKill
	}
	switch {
	case !earlier.Done:
		d.Verdict = verdictUnwrittenRedelivery
	case d.Identity != "":
		d.Verdict = verdictLedgerDedupe
	default:
		d.Verdict = verdictShownTwice
	}
	return d
}

// readRelayFrames reads relay-frames.csv. The columns are read by name, so a file written by
// another build's column order is read the same way; a file without the columns this report needs
// is refused.
func readRelayFrames(path string) ([]relayFrameRow, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	rows, err := csv.NewReader(f).ReadAll()
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if len(rows) == 0 {
		return nil, fmt.Errorf("%s is empty", path)
	}
	column := map[string]int{}
	for i, name := range rows[0] {
		column[name] = i
	}
	for _, name := range []string{"session", "subject", "stream_seq", "delivered", "stored_at_ns", "recv_ns", "event_id", "identity", "done"} {
		if _, ok := column[name]; !ok {
			return nil, fmt.Errorf("%s has no %q column; it was written by a build before the restart report", path, name)
		}
	}
	out := make([]relayFrameRow, 0, len(rows)-1)
	for i, row := range rows[1:] {
		at := func(name string) string {
			if j := column[name]; j < len(row) {
				return row[j]
			}
			return ""
		}
		rec := relayFrameRow{Session: at("session"), Subject: at("subject"), Identity: at("identity")}
		rec.Seq, err = strconv.ParseUint(at("stream_seq"), 10, 64)
		if err != nil {
			return nil, fmt.Errorf("%s line %d: stream_seq %q: %w", path, i+2, at("stream_seq"), err)
		}
		for _, field := range []struct {
			name string
			dst  *int64
		}{{"stored_at_ns", &rec.StoredAt}, {"recv_ns", &rec.Recv}, {"event_id", &rec.EventID}} {
			if *field.dst, err = strconv.ParseInt(at(field.name), 10, 64); err != nil {
				return nil, fmt.Errorf("%s line %d: %s %q: %w", path, i+2, field.name, at(field.name), err)
			}
		}
		if rec.Delivered, err = strconv.Atoi(at("delivered")); err != nil {
			return nil, fmt.Errorf("%s line %d: delivered %q: %w", path, i+2, at("delivered"), err)
		}
		if rec.Done, err = strconv.ParseBool(at("done")); err != nil {
			return nil, fmt.Errorf("%s line %d: done %q: %w", path, i+2, at("done"), err)
		}
		out = append(out, rec)
	}
	return out, nil
}

func readJSONFile(path string, v any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(b, v); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	return nil
}

func runRestartReport(args []string) error {
	fs := flag.NewFlagSet("restart-report", flag.ExitOnError)
	run := fs.String("run", "", "the run directory (relay-frames.csv, restart.json, backlog.json)")
	framesPath := fs.String("frames", "", "relay-frames.csv path (default <run>/relay-frames.csv)")
	restartPath := fs.String("restart", "", "restart.json path (default <run>/restart.json)")
	backlogPath := fs.String("backlog", "", "backlog.json path (default <run>/backlog.json)")
	out := fs.String("out", "", "where to write the report (default <run>/restart-report.json)")
	_ = fs.Parse(args)
	if *run == "" {
		return errors.New("-run is required: the directory restart-step.sh wrote")
	}
	path := func(given, name string) string {
		if given != "" {
			return given
		}
		return filepath.Join(*run, name)
	}
	frames, err := readRelayFrames(path(*framesPath, "relay-frames.csv"))
	if err != nil {
		return err
	}
	var times restartTimes
	if err := readJSONFile(path(*restartPath, "restart.json"), &times); err != nil {
		return err
	}
	if err := times.validate(); err != nil {
		return err
	}
	var backlog backlogSummary
	if err := readJSONFile(path(*backlogPath, "backlog.json"), &backlog); err != nil {
		return err
	}
	report := buildRestartReport(frames, times, backlog)
	report.Run = *run
	b, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(path(*out, "restart-report.json"), append(b, '\n'), 0o644); err != nil {
		return err
	}
	fmt.Printf("%s\n", b)
	if defects := report.shownTwice(); len(defects) > 0 {
		named := make([]string, 0, 5)
		for _, d := range defects[:min(len(defects), 5)] {
			named = append(named, fmt.Sprintf("%s stream seq %d (event %d, %s)", d.Session, d.StreamSeq, d.EventID, d.When))
		}
		return fmt.Errorf("run %s: %d relay frames would reach a session's transcript twice under the plugin's rules (a copy of a frame the session had answered done, carrying no dedupe identity the ledger could match): %s",
			*run, len(defects), strings.Join(named, "; "))
	}
	return nil
}
