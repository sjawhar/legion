package daemon

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/config"
)

// intakeCeilingClauseMark is how these rows recognise the sentence's ceiling clause: its closing
// words. They carry no number, so a row that reads them answers whether the clause is there and
// nothing else — the limit and the window are asserted on their own, from the numbers each window
// earns. Matching on the ceiling's own seconds instead would fail for a changed ceiling, which is
// a different change from a dropped clause, and reading the whole clause back would make this a
// second verbatim copy of wording `packages/daemon/cmd/legion/intakelimit_test.go` already holds.
const intakeCeilingClauseMark = "and nothing is refused"

// intakeFloorDecided and intakeWindowDecided are the two shapes the sentence takes where it says
// which of the two decided the limit: the words straight after the limit's own seconds. A limit
// the ten-minute floor decided names the floor there; one the window derived goes straight on
// into the window's own clause, so its mark is the colon alone. Both are read anchored to the
// seconds (`cut after %ds`), which is what lets a colon tell the two apart — the sentence has
// other colons — and what makes a floor limit worded as a derivation fail: a sentence that put
// the floor's 600s in front of the window's own clause matches neither mark.
const (
	intakeFloorDecided  = ", intake's ten-minute floor and not this window:"
	intakeWindowDecided = ":"
)

// intakeHandlerLimitWindows are the `worker_rpc_timeout_seconds` windows that decide every answer
// the intake handler limit's two operator surfaces give, and the answers each window earns. Both
// rows below read this one table because one condition, limit > intake.HandlerLimitCeiling,
// decides both the sentence's last clause and the boot line's level: a window that moves one of
// those answers must move the other, and a table split in two could drift.
//
// The pair that carries the row is 900 s and 901 s. Four windows of 900 s derive exactly the
// one-hour ceiling, which is not above it, so that window earns no clause and still logs at Info;
// 901 s derives 3604 s and earns both. The test in the code is `>`, and turning it into `>=`
// reddens the 900 s case of both rows, which is why the pair is here.
var intakeHandlerLimitWindows = []struct {
	name   string
	window time.Duration
	// limitSeconds is the limit the sentence names: four windows, floored at intake's ten minutes.
	limitSeconds int
	// decidedBy is what the sentence must say decided that limit, as the words it puts straight
	// after those seconds: intakeFloorDecided for the windows the floor covers, intakeWindowDecided
	// for the windows that derive a limit of their own.
	decidedBy string
	// derived is IntakeHandlerLimitLine's second answer, true only when the window lifted the
	// limit above that floor, which is what `legion start --check-config` prints on.
	derived bool
	// ceilingClause is whether the sentence ends with the clause about the cut coming too late.
	ceilingClause bool
	// level is the level logIntakeHandlerLimit writes that same sentence at.
	level slog.Level
}{
	{
		name: "the daemon's default window", window: 5 * time.Second,
		limitSeconds: 600, decidedBy: intakeFloorDecided,
		derived: false, ceilingClause: false, level: slog.LevelInfo,
	},
	{
		name: "the last window the floor covers", window: 150 * time.Second,
		limitSeconds: 600, decidedBy: intakeFloorDecided,
		derived: false, ceilingClause: false, level: slog.LevelInfo,
	},
	{
		name: "the first window that derives a limit", window: 151 * time.Second,
		limitSeconds: 604, decidedBy: intakeWindowDecided,
		derived: true, ceilingClause: false, level: slog.LevelInfo,
	},
	{
		name: "the window that derives the ceiling itself", window: 900 * time.Second,
		limitSeconds: 3600, decidedBy: intakeWindowDecided,
		derived: true, ceilingClause: false, level: slog.LevelInfo,
	},
	{
		name: "the first window above the ceiling", window: 901 * time.Second,
		limitSeconds: 3604, decidedBy: intakeWindowDecided,
		derived: true, ceilingClause: true, level: slog.LevelWarn,
	},
	{
		// The largest value a legion.yaml may carry for a duration key: config's timer bound,
		// 2147483 s, the largest whole seconds whose milliseconds fit a signed 32-bit timer.
		name: "the largest window a legion.yaml may carry", window: 2147483 * time.Second,
		limitSeconds: 8589932, decidedBy: intakeWindowDecided,
		derived: true, ceilingClause: true, level: slog.LevelWarn,
	},
}

// IntakeHandlerLimitLine names the limit one fact's handling is cut at, which of the two decided
// it, the key and value it was derived from, and — only above intake.HandlerLimitCeiling — the
// clause saying the cut no longer returns a wedged handler's message while an operator is still
// looking. This row holds all four over the windows that decide them, and `derived`, the answer
// `legion start --check-config` prints on.
//
// The sentence is not read back whole here. `packages/daemon/cmd/legion/intakelimit_test.go`
// asserts it character for character at one window, because what an operator reads is that row's
// deliverable; a second verbatim copy would have to be edited twice for one change of wording.
// It asserts the derived shape alone, though, so the floor's shape — every default deployment's,
// logged at every boot — is held here and nowhere else. What this row holds is what one window
// cannot show: both numbers, which of the two decided the limit, `derived` and the clause,
// across the windows where each of them turns over.
func TestIntakeHandlerLimitLineNamesBothNumbersAndClausesOnlyAboveTheCeiling(t *testing.T) {
	for _, row := range intakeHandlerLimitWindows {
		t.Run(row.name, func(t *testing.T) {
			// WorkerRPCTimeout is the only field the line reads, so it is the only one set.
			cfg := config.Config{WorkerRPCTimeout: row.window}

			line, derived := IntakeHandlerLimitLine(cfg)

			// The limit and what decided it are read as one string, anchored on the seconds: the
			// floor's ten minutes named as the window's own derivation is the sentence an operator
			// would act on by changing a key that moves nothing, so neither mark may match the
			// other's sentence.
			if want := fmt.Sprintf("cut after %ds%s", row.limitSeconds, row.decidedBy); !strings.Contains(line, want) {
				t.Errorf("IntakeHandlerLimitLine(worker_rpc_timeout_seconds=%s) = %q, want it to name the limit and what decided it as %q",
					row.window, line, want)
			}
			// The window is named with the key it came from, so a line that named the limit and
			// left out which key derives it — the one key an operator changes to move it — fails.
			if want := fmt.Sprintf("worker_rpc_timeout_seconds (%d)", int(row.window.Seconds())); !strings.Contains(line, want) {
				t.Errorf("IntakeHandlerLimitLine(worker_rpc_timeout_seconds=%s) = %q, want it to name the window as %q",
					row.window, line, want)
			}
			if derived != row.derived {
				t.Errorf("IntakeHandlerLimitLine(worker_rpc_timeout_seconds=%s) derived = %v, want %v",
					row.window, derived, row.derived)
			}
			if clause := strings.Contains(line, intakeCeilingClauseMark); clause != row.ceilingClause {
				t.Errorf("IntakeHandlerLimitLine(worker_rpc_timeout_seconds=%s) = %q, ceiling clause (%q) present = %v, want %v",
					row.window, line, intakeCeilingClauseMark, clause, row.ceilingClause)
			}
		})
	}
}

// intakeBootRecord is the part of a logged record the boot row reads back.
type intakeBootRecord struct {
	Level string `json:"level"`
	Msg   string `json:"msg"`
}

// logIntakeHandlerLimit writes that same sentence at every boot, whether the floor or the window
// decided the limit, and raises it to Warn only above the ceiling. This row holds the level over
// the same windows, that exactly one record goes out for each of them — the floor's windows
// included, since a running daemon says its limit either way — and that the message is
// IntakeHandlerLimitLine's own line with nothing prefixed to it. That last one is what keeps the
// boot log and `legion start --check-config` saying the same words, and it is the assertion that
// fails if someone puts `daemon: ` in front of a sentence that already begins `intake: `. The
// message is compared against IntakeHandlerLimitLine rather than written out, because the two
// surfaces agreeing is the property, not the wording, which the cmd row asserts.
//
// The records come back from a JSON handler over a buffer, as this package's other log rows read
// theirs (TestTheBootLogNamesTheDerivedProbeBudgetOnlyWhenItIsDerived): one JSON object per
// record gives the count, `level` and `msg` give both fields exactly, and there is no handler of
// this file's own to keep correct. The handler is at Debug so that nothing is dropped by level
// before the row reads it.
func TestIntakeHandlerLimitBootLineWarnsOnlyAboveTheCeiling(t *testing.T) {
	for _, row := range intakeHandlerLimitWindows {
		t.Run(row.name, func(t *testing.T) {
			cfg := config.Config{WorkerRPCTimeout: row.window}
			logged := &strings.Builder{}

			logIntakeHandlerLimit(t.Context(),
				slog.New(slog.NewJSONHandler(logged, &slog.HandlerOptions{Level: slog.LevelDebug})), cfg)

			written := logged.String()
			var records []intakeBootRecord
			for decoder := json.NewDecoder(strings.NewReader(written)); decoder.More(); {
				var rec intakeBootRecord
				if err := decoder.Decode(&rec); err != nil {
					t.Fatalf("decoding the boot log %q: %v", written, err)
				}
				records = append(records, rec)
			}

			if len(records) != 1 {
				t.Fatalf("logIntakeHandlerLimit(worker_rpc_timeout_seconds=%s) wrote %d records, want exactly one: %q",
					row.window, len(records), written)
			}
			if records[0].Level != row.level.String() {
				t.Errorf("logIntakeHandlerLimit(worker_rpc_timeout_seconds=%s) wrote at %s, want %s: %q",
					row.window, records[0].Level, row.level, written)
			}
			if want, _ := IntakeHandlerLimitLine(cfg); records[0].Msg != want {
				t.Errorf("logIntakeHandlerLimit(worker_rpc_timeout_seconds=%s) message = %q, want %q, IntakeHandlerLimitLine's line with nothing prefixed to it",
					row.window, records[0].Msg, want)
			}
		})
	}
}
