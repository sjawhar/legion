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
import { SendInterruptedError, SendStoppedError, sameKeyRetryNote } from "./keyed-write";
import { StopTrying } from "./StopTrying";
import { useWriteSelection, useWrites, type Write } from "./write-store";

/** Why the refused write was not saved, in the reader's words. */
function refusalReason(refusal: Error): string {
  if (refusal instanceof SendInterruptedError) {
    return `this page was closed before Dispatch confirmed it. ${sameKeyRetryNote("saved")}`;
  }
  if (refusal instanceof SendStoppedError) {
    return `Dispatch has not confirmed it. ${sameKeyRetryNote("saved")}`;
  }
  return apiErrorMessage(refusal, "Dispatch could not be reached");
}

/** The writes the indicator shows: every one shown there that Dispatch has not yet made. */
function shown(rows: readonly Write[]): readonly Write[] {
  return rows.filter((row) => row.presentation === "indicator" && row.status !== "made");
}

/**
 * The shell's one account of the writes no composer shows (`write-store.ts`'s `indicator`
 * writes): chrome, out of the page's flow in the corner the connection pill shares, and gone while
 * nothing is pending. Its status line counts the writes still out - `Saving 2 changes…`, then,
 * once one is past its deadline, `Dispatch has not confirmed 2 changes — still trying` - and it
 * names each write the reader can act on: a late one with Stop trying, a refused one with why,
 * Retry and Discard. A write still within its deadline, or waiting its turn, is only counted, since
 * there is nothing to do about it yet.
 */
export function PendingWritesIndicator(): ReactNode {
  const writes = useWrites();
  const rows = useWriteSelection("indicator", shown);
  if (rows.length === 0) return null;
  const out = rows.filter((row) => row.status !== "refused");
  const late = out.some((row) => row.status === "late");
  const named = rows.filter((row) => row.status === "late" || row.status === "refused");
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
            <li
              className="flex min-h-8 flex-wrap items-center gap-x-2 gap-y-1"
              data-pending-write={row.id}
              key={row.id}
            >
              {row.status === "refused" ? (
                <>
                  <span className={`min-w-0 flex-1 ${dangerText}`}>
                    Not saved: {row.label} — {refusalReason(row.refusal)}
                  </span>
                  <button
                    className={secondaryButtonCompact}
                    onClick={() => writes.retry(row.id)}
                    type="button"
                  >
                    Retry
                  </button>
                  <button
                    className={secondaryButtonCompact}
                    onClick={() => writes.discard(row.id)}
                    type="button"
                  >
                    Discard
                  </button>
                </>
              ) : row.status === "late" ? (
                <>
                  <span className={`min-w-0 flex-1 ${textSecondaryOnSurface}`}>{row.label}</span>
                  <StopTrying onStop={() => writes.stop(row.id)} stopping={row.stopping} />
                </>
              ) : null}
            </li>
          ))}
        </ul>
      )}
    </section>
  );
}
