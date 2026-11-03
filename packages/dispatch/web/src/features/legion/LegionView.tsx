// The Legion view on a project's Issues page: what the project's Legion daemon is doing, as
// Dispatch's per-minute read of `GET /legion/v1/state` recorded it. Every column, every card and
// every number comes from `GET /api/v1/legion/board`; nothing is computed here but the time an
// issue has been in its phase, which is measured from the moment the board gives.
//
// Live the way the Delivery page and the Pipeline section are: the `legion` view kind over the
// page's one live socket (`LIVE` plus `subscribeLive` in `api/queries.ts`), so a card moves with
// no reload and this makes no socket call of its own.
//
// The queue's order is the daemon's own waiting line, which is Dispatch's rank order
// (`packages/daemon/internal/record/project.go`). The view never re-sorts a column: the server
// sends each one in the order it is to be drawn.
import { useQuery } from "@tanstack/react-query";
import type { ReactNode } from "react";

import { isLegionNotConfigured } from "../../api/client";
import { legionBoardQuery } from "../../api/queries";
import type { LegionBoardColumn, LegionBoardResponse } from "../../api/types";
import { Pill } from "../../components/Pill";
import { QueryError } from "../../components/QueryError";
import {
  borderDefault,
  borderStrong,
  dangerText,
  surfaceMutedBg,
  textMutedOnCanvas,
  textPrimaryOnSurface,
  textSecondaryOnCanvas,
} from "../../theme/classes";
import { LegionCard } from "./LegionCard";
import { LegionSettingsPanel } from "./LegionSettingsPanel";

/** One column of the board, with its cards in the server's order. */
function LegionColumn({
  column,
  nowMs,
}: {
  readonly column: LegionBoardColumn;
  readonly nowMs: number;
}): ReactNode {
  return (
    <section
      aria-label={column.label}
      className="w-72 shrink-0 snap-start"
      data-legion-column={column.id}
    >
      <header className={`rounded-xl border px-3 py-2 ${borderDefault} ${surfaceMutedBg}`}>
        <div
          className={`flex items-center justify-between gap-2 text-sm font-semibold ${textPrimaryOnSurface}`}
        >
          <span>{column.label}</span>
          <Pill>{column.cards.length}</Pill>
        </div>
      </header>
      {column.cards.length === 0 ? (
        <p
          className={`mt-3 min-h-20 rounded-xl border border-dashed px-3 py-6 text-center text-xs ${borderStrong} ${textMutedOnCanvas}`}
        >
          Nothing here
        </p>
      ) : (
        <div className="mt-3 space-y-3">
          {column.cards.map((issue) => (
            <LegionCard card={issue} key={issue.key} nowMs={nowMs} />
          ))}
        </div>
      )}
    </section>
  );
}

/** What the header says about the last read of the daemon: when it answered, or that it never
 *  did, or why the last read failed. A daemon that has never answered still shows the view —
 *  with its columns empty and the failure here. */
function ReadState({ board }: { readonly board: LegionBoardResponse }): ReactNode {
  const { read_at: at, read_error: error, read_ok: ok } = board.header;
  if (at === null) {
    return (
      <span className={`text-xs ${dangerText}`} data-legion-read-error role="status">
        This project&apos;s daemon has never answered
        {error === null ? "" : `: ${error}`}
      </span>
    );
  }
  const read = `Read ${new Date(at).toLocaleString()}`;
  if (ok) {
    return (
      <span className={`text-xs ${textMutedOnCanvas}`} data-legion-read-at>
        {read}
      </span>
    );
  }
  return (
    <span className={`text-xs ${dangerText}`} data-legion-read-error role="status">
      {read}, and the last read failed
      {error === null ? "" : `: ${error}`}
    </span>
  );
}

/** The board for one answer of the route: its header, its ten columns in the server's order, and
 *  the daemon's settings. `nowMs` is the moment every card's time in its phase is measured
 *  from. */
export function LegionBoardView({
  board,
  nowMs,
}: {
  readonly board: LegionBoardResponse;
  readonly nowMs: number;
}): ReactNode {
  const { queue_length: queued, slot_cap: cap, slots_in_use: used } = board.header;
  return (
    <>
      <header className="flex flex-wrap items-baseline gap-x-4 gap-y-1 text-sm" data-legion-header>
        <span className={textSecondaryOnCanvas} data-legion-slots>
          {used} of {cap === null ? "an unknown number of" : cap} slots in use
        </span>
        <span className={textSecondaryOnCanvas} data-legion-queue-length>
          {queued} waiting in the queue
        </span>
        <ReadState board={board} />
      </header>
      <div className="mt-3 overflow-x-auto pb-3 snap-x snap-mandatory" data-legion-columns>
        <div className="flex w-max items-stretch gap-4">
          {board.columns.map((column) => (
            <LegionColumn column={column} key={column.id} nowMs={nowMs} />
          ))}
        </div>
      </div>
      <LegionSettingsPanel
        project={board.project}
        settings={board.settings}
        sources={board.setting_sources}
      />
    </>
  );
}

/** The Legion view of `project`, live. A project that names no daemon is shown no view at all —
 *  the Issues page's view selector offers the button only for a project that has a Legion
 *  setting — so the route's `LEGION_NOT_CONFIGURED` here is the race where the setting was
 *  removed while the view was open, and says so rather than offering a retry that cannot help. */
export function LegionView({ project }: { readonly project: string }): ReactNode {
  const query = useQuery(legionBoardQuery({ kind: "legion", project }));
  const nowMs = Date.now();

  return (
    <section aria-label="Legion" className="min-w-0" data-legion-view={project}>
      {query.isPending ? <p className={textMutedOnCanvas}>Loading the Legion view…</p> : null}
      {isLegionNotConfigured(query.error) ? (
        <p className={`text-sm ${textSecondaryOnCanvas}`}>
          {project} no longer names a Legion daemon.
        </p>
      ) : query.isError ? (
        <QueryError
          message="Couldn't load the Legion view."
          onRetry={() => void query.refetch()}
          retrying={query.isFetching}
        />
      ) : null}
      {query.data === undefined ? null : <LegionBoardView board={query.data} nowMs={nowMs} />}
    </section>
  );
}
