package daemon

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
)

// IntakeHandlerLimitLine is the operator's sentence for the intake handler limit the daemon runs
// with: how long one fact's handling may run before it is cut, which of the two decided that —
// the window (`worker_rpc_timeout_seconds`, which intake takes as its AckWait) or intake's
// ten-minute floor — the reads that window bounds either way, and what is left for the part
// nothing bounds. derived is HandlerLimitFor's second answer: true only when the window lifted
// the limit above that floor.
//
// Unlike ProbeBudgetLine, which answers "" when nothing is derived, this returns the sentence for
// every configuration, because the boot writes it at every boot (logIntakeHandlerLimit) — a
// running daemon says its limit whether the floor or the window decided it. Only
// `legion start --check-config` is selective, and it reads derived to decide.
func IntakeHandlerLimitLine(cfg config.Config) (line string, derived bool) {
	line, _, derived = intakeHandlerLimitLine(cfg)
	return line, derived
}

// intakeHandlerLimitLine is the sentence and the two facts its readers decide on, derived once:
// whether the window lifted the limit above the floor (derived, which is what
// `legion start --check-config` prints on) and whether it lifted it above
// intake.HandlerLimitCeiling (aboveCeiling, which adds the sentence's last clause and puts the
// boot line at Warn). One condition decides the clause and the level, so a change to the ceiling
// rule cannot leave a line that warns without saying why, or says why without warning.
//
// Above the ceiling the cut is still made and nothing about the configuration is refused; what
// the clause says is that it no longer returns a wedged handler's message to the stream while an
// operator is still looking at the stall.
//
// The sentence has two shapes, because two different things decide the limit and an operator who
// wants to move it has to know which. A derived limit is the window's own arithmetic; the floor's
// is ten minutes whatever the window is, so the floor's sentence says so and says what the
// derivation actually came to. Only the derived shape can carry the ceiling clause: the floor's
// limit is ten minutes, well under intake.HandlerLimitCeiling.
func intakeHandlerLimitLine(cfg config.Config) (line string, aboveCeiling, derived bool) {
	limit, derived := intake.HandlerLimitFor(cfg.WorkerRPCTimeout)
	aboveCeiling = limit > intake.HandlerLimitCeiling
	// Both shapes name the same three reads in the same words, from one string, so an operator
	// who has read one sentence has read the other's middle.
	reads := fmt.Sprintf("worker_rpc_timeout_seconds (%d) bounds each read a review makes — whether its pull request is recorded, its truncated body and its author's repository permission",
		int(cfg.WorkerRPCTimeout.Seconds()))
	if !derived {
		// Four windows is intake's derivation, one for each of the three reads just named and one
		// for the part nothing bounds; here they come to no more than the floor, which is why the
		// floor is the limit. Naming the total is what keeps the line honest at the default
		// window, where it would otherwise read as a limit the window earned.
		return fmt.Sprintf("intake: one fact's handling is cut after %ds, intake's ten-minute floor and not this window: %s — and four of those windows come to %ds, which the floor covers, so the rest of the limit is left for the part nothing bounds, the fact's transaction and its wait on the apply lock",
			int(limit.Seconds()), reads, 4*int(cfg.WorkerRPCTimeout.Seconds())), aboveCeiling, derived
	}
	above := ""
	if aboveCeiling {
		above = fmt.Sprintf("; above %ds the cut no longer returns a wedged handler's message to the stream within one operator's look, and nothing is refused",
			int(intake.HandlerLimitCeiling.Seconds()))
	}
	return fmt.Sprintf("intake: one fact's handling is cut after %ds: %s — and one window more is left for the part nothing bounds, the fact's transaction and its wait on the apply lock%s",
		int(limit.Seconds()), reads, above), aboveCeiling, derived
}

// logIntakeHandlerLimit writes that sentence at every boot: at Info, or at Warn when the limit is
// above the ceiling, where the cut is too late to return a wedged handler's message while an
// operator is still looking. The sentence already begins `intake: `, as the package's other lines
// about intake do, so nothing is prefixed to it.
//
// The sentence is the whole message, with no attribute repeating the seconds it already names, so
// that this line and `legion start --check-config` say the same thing word for word; an operator
// who reads one has read the other. It is also the only place a running daemon says what the
// limit is — `legion state` does not carry it — so the line goes out whether the floor or the
// window decided the limit.
func logIntakeHandlerLimit(ctx context.Context, log *slog.Logger, cfg config.Config) {
	line, aboveCeiling, _ := intakeHandlerLimitLine(cfg)
	level := slog.LevelInfo
	if aboveCeiling {
		level = slog.LevelWarn
	}
	log.Log(ctx, level, line)
}
