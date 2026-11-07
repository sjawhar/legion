package daemon

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
)

// IntakeHandlerLimitLine is the operator's sentence for the intake handler limit the daemon runs
// with: how long one fact's handling may run before it is cut, the window it is derived from
// (`worker_rpc_timeout_seconds`, which intake takes as its AckWait), the reads that window bounds,
// and the share of the limit left for the part nothing bounds. derived is HandlerLimitFor's
// second answer: true only when the window lifted the limit above intake.DefaultHandlerLimit.
//
// Unlike ProbeBudgetLine, which answers "" when nothing is derived, this returns the sentence for
// every configuration, because the boot writes it at every boot (logIntakeHandlerLimit) — a
// running daemon says its limit whether the floor or the window decided it. Only
// `legion start --check-config` is selective, and it reads derived to decide.
//
// A limit above intake.HandlerLimitCeiling gains one clause: the cut is still made, and nothing
// about the configuration is refused, but it no longer returns a wedged handler's message to the
// stream while an operator is still looking at the stall.
func IntakeHandlerLimitLine(cfg config.Config) (line string, derived bool) {
	limit, derived := intake.HandlerLimitFor(cfg.WorkerRPCTimeout)
	above := ""
	if limit > intake.HandlerLimitCeiling {
		above = fmt.Sprintf("; above %ds the cut no longer returns a wedged handler's message to the stream within one operator's look, and nothing is refused",
			int(intake.HandlerLimitCeiling.Seconds()))
	}
	return fmt.Sprintf("intake: one fact's handling is cut after %ds: worker_rpc_timeout_seconds (%d) bounds each read a review makes — whether its pull request is recorded, its truncated body and its author's repository permission — and one window more is left for the part nothing bounds, the fact's transaction and its wait on the apply lock%s",
		int(limit.Seconds()), int(cfg.WorkerRPCTimeout.Seconds()), above), derived
}

// logIntakeHandlerLimit writes IntakeHandlerLimitLine's sentence at every boot: at Info, or at
// Warn when the limit is above intake.HandlerLimitCeiling, where the cut is too late to return a
// wedged handler's message while an operator is still looking. The sentence already begins
// `intake: `, as the package's other lines about intake do, so nothing is prefixed to it.
//
// The sentence is the whole message, with no attribute repeating the seconds it already names, so
// that this line and `legion start --check-config` say the same thing word for word; an operator
// who reads one has read the other. It is also the only place a running daemon says what the
// limit is — `legion state` does not carry it — so the line goes out whether the floor or the
// window decided the limit.
func logIntakeHandlerLimit(log *slog.Logger, cfg config.Config) {
	line, _ := IntakeHandlerLimitLine(cfg)
	limit, _ := intake.HandlerLimitFor(cfg.WorkerRPCTimeout)
	level := slog.LevelInfo
	if limit > intake.HandlerLimitCeiling {
		level = slog.LevelWarn
	}
	log.Log(context.Background(), level, line)
}
