import type { ReactNode } from "react";

import { apiErrorMessage } from "../../api/client";
import {
  borderDefault,
  calloutWarningBorder,
  card,
  dangerText,
  secondaryButtonCompact,
  textSecondaryOnSurface,
} from "../../theme/classes";
import { SendInterruptedError, SendStoppedError } from "./keyed-write";
import { type PendingWrite, usePendingWriteRows, usePendingWrites } from "./pending-writes";
import { StopTrying } from "./StopTrying";

/** Why the refused write was not saved, in the reader's words. */
function refusalReason(refusal: Error | undefined): string {
  if (refusal instanceof SendInterruptedError) {
    return "this page was closed before Dispatch confirmed it. Retry asks again under the same key, so it is saved at most once.";
  }
  if (refusal instanceof SendStoppedError) {
    return "Dispatch has not confirmed it. Retry asks again under the same key, so it is saved at most once.";
  }
  return apiErrorMessage(refusal, "Dispatch could not be reached");
}

/**
 * The shell's one account of the writes no composer shows (`pending-writes.ts`): chrome, out of
 * the page's flow in the corner the connection pill shares, and gone while nothing is pending. Its
 * status line counts the writes still out - `Saving 2 changes…`, then, once one is past its
 * deadline, `Dispatch has not confirmed 2 changes — still trying` - and it names each write the
 * reader can act on: a late one with Stop trying, a refused one with why, Retry and Discard. A
 * write still within its deadline is only counted, since there is nothing to do about it yet.
 */
export function PendingWritesIndicator(): ReactNode {
  const writes = usePendingWrites();
  const rows = usePendingWriteRows();
  if (rows.length === 0) return null;
  const out = rows.filter((row) => row.status !== "refused");
  const late = out.some((row) => row.status === "late");
  const named = rows.filter((row) => row.status !== "sending");
  const count = out.length === 1 ? "1 change" : `${out.length} changes`;
  return (
    <section
      aria-label="Pending changes"
      className={`w-full max-w-md rounded-xl border px-3 py-2 text-xs shadow-lg ${card} ${
        named.length === 0 ? borderDefault : calloutWarningBorder
      }`}
      data-testid="pending-writes"
    >
      {out.length === 0 ? null : (
        <p aria-live="polite" className={`font-medium ${textSecondaryOnSurface}`} role="status">
          {late ? `Dispatch has not confirmed ${count} — still trying` : `Saving ${count}…`}
        </p>
      )}
      {named.length === 0 ? null : (
        <ul className={`max-h-48 space-y-1 overflow-y-auto ${out.length === 0 ? "" : "mt-2"}`}>
          {named.map((row) => (
            <PendingWriteRow
              key={row.id}
              onDiscard={() => writes.discard(row.id)}
              onRetry={() => writes.retry(row.id)}
              onStop={() => writes.stop(row.id)}
              row={row}
            />
          ))}
        </ul>
      )}
    </section>
  );
}

function PendingWriteRow({
  onDiscard,
  onRetry,
  onStop,
  row,
}: {
  readonly onDiscard: () => void;
  readonly onRetry: () => void;
  readonly onStop: () => void;
  readonly row: PendingWrite;
}): ReactNode {
  return (
    <li className="flex min-h-8 flex-wrap items-center gap-x-2 gap-y-1" data-pending-write={row.id}>
      {row.status === "refused" ? (
        <>
          <span className={`min-w-0 flex-1 ${dangerText}`}>
            Not saved: {row.label} — {refusalReason(row.refusal)}
          </span>
          <button className={secondaryButtonCompact} onClick={onRetry} type="button">
            Retry
          </button>
          <button className={secondaryButtonCompact} onClick={onDiscard} type="button">
            Discard
          </button>
        </>
      ) : (
        <>
          <span className={`min-w-0 flex-1 ${textSecondaryOnSurface}`}>{row.label}</span>
          <StopTrying onStop={onStop} stopping={row.stopping} />
        </>
      )}
    </li>
  );
}
