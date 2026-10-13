package daemon

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// strandedReportTimeout bounds an advisory boot report's transaction. The API is already serving
// when it starts, and a report that cannot read promptly must not retain a database connection
// through the daemon's normal work or shutdown.
const strandedReportTimeout = 5 * time.Second

// reportStranded logs, once the boot has re-adopted every claim and replayed the failed ones to
// the workflow, the claims nothing will wake (workflow.Stranded), read from records as the state
// route reads them: one error line, however many there are, naming each claim as `<issue> <role>`
// under the move that brings it back, keyed by that move's value (api.StrandedClaim.Move, the one
// statement of which move each claim takes, which `legion state` groups by too). A fleet a restart
// did not bring back otherwise looks healthy — its controller registers, the state shows its trees
// active, and nothing else is logged. A boot with none logs nothing. The state route judges the
// same rule on every read (strandedClaims). A failed read is logged and stops nothing: the report is
// the boot's account of its claims, and no step after it depends on it. A read the daemon's stop
// cancels is not a failure and logs nothing; one that outlives strandedReportTimeout is. project is
// the configured project the line names, as `legion daemon started` does; projectToken selects the
// project's claims (ofProject).
func reportStranded(ctx context.Context, st *store.Store, records record.Store, project, projectToken string, log *slog.Logger) {
	var stranded []supervise.Claim
	if err := pgx.BeginTxFunc(ctx, st.Pool(), pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
		claims, err := st.ClaimsTx(ctx, tx)
		if err != nil {
			return err
		}
		claims = ofProject(claims, projectToken)
		stranded, err = workflow.Stranded(ctx, tx, records, claims)
		return err
	}); err != nil {
		if errors.Is(ctx.Err(), context.Canceled) {
			return
		}
		log.Error("boot: read the issue record to report the claims nothing will wake", "error", err)
		return
	}
	if len(stranded) == 0 {
		return
	}
	attrs := []any{"project", project, "count", len(stranded)}
	grouped := api.StrandedByMove(api.StrandedClaimsOf(stranded))
	for _, move := range api.StrandedMoves() {
		attrs = append(attrs, string(move), grouped[move])
	}
	log.Error("legion daemon started with claims nothing will wake", attrs...)
}
