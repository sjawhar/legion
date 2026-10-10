package daemon

import (
	"context"
	"log/slog"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// reportStranded logs, once the boot has re-adopted every claim and replayed the failed ones to
// the workflow, the claims nothing will wake (workflow.Stranded), read from records as the state
// route reads them: one error line, however many there are, naming each claim as `<issue> <role>`
// under the move that brings it back. `resume` lists the suspended ones, which the operator's
// `legion claims resume` brings back; `needsWorkflowStart` lists the failed ones, which ignore
// resume and deliver and run again only on a workflow start: their architect's retry of the held
// phase for a worker, their tree's re-admission for an architect. A fleet a restart did not bring
// back otherwise looks healthy — its controller registers, the state shows its trees active, and
// nothing else is logged. A boot with none logs nothing. The state route judges the same rule on
// every read (strandedClaims). A failed read is logged and stops nothing: the report is the boot's
// account of its claims, and no step after it depends on it.
func reportStranded(ctx context.Context, st *store.Store, records record.Store, sup *supervisor, project string, log *slog.Logger) {
	claims, err := sup.Claims(ctx)
	if err != nil {
		log.Error("boot: read the claims to report those nothing will wake", "error", err)
		return
	}
	var stranded []supervise.Claim
	if err := pgx.BeginTxFunc(ctx, st.Pool(), pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
		stranded, err = workflow.Stranded(ctx, tx, records, claims)
		return err
	}); err != nil {
		log.Error("boot: read the issue record to report the claims nothing will wake", "error", err)
		return
	}
	if len(stranded) == 0 {
		return
	}
	resume, workflowStart := []string{}, []string{}
	for _, c := range stranded {
		named := c.Issue + " " + string(c.Role)
		if c.State == supervise.StateFailed {
			workflowStart = append(workflowStart, named)
		} else {
			resume = append(resume, named)
		}
	}
	log.Error("legion daemon started with claims nothing will wake", "project", project, "count", len(stranded),
		"resume", resume, "needsWorkflowStart", workflowStart)
}
