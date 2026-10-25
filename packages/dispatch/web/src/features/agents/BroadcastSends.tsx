import { useQueryClient } from "@tanstack/react-query";
import { type ReactNode, useEffect, useId, useRef, useState, useSyncExternalStore } from "react";
import { Link, useNavigate } from "react-router-dom";

import { RefusableButton } from "../../components/RefusableButton";
import { useSubmitGuard } from "../../hooks/useSubmitGuard";
import {
  borderDefault,
  card,
  dangerText,
  linkHoverText,
  linkText,
  secondaryButtonCompact,
  textMutedOnSurface,
  textSecondaryOnSurface,
} from "../../theme/classes";
import { StopTrying } from "../conversation/StopTryingButton";
import { SendInterruptedError, SendStoppedError } from "../conversation/send-request";
import { MarkdownPreview } from "../refs/MarkdownPreview";
import { type BroadcastSend, type BroadcastSendRow, broadcastQueue } from "./broadcast-queue";

/**
 * The Agents page's hold on the broadcast queue (`broadcast-queue.ts`). Send hands its request off
 * at the press (`enqueue`) - the page clears the draft and the selection with it, so the next
 * broadcast can be written at once - and the queue sends one request at a time, in press order,
 * each under its key until Dispatch decides it. A send carries its own request, so a waiting send
 * keeps the draft it was pressed with. The request is all a send orders: the server answers
 * before it delivers, and each broadcast delivers on its own, so two broadcasts can reach one
 * agent in either order.
 *
 * `rows` is every send the queue holds, newest press first, each with its own state, so a later
 * send never hides an earlier one's failure. Nothing navigates while a send is queued or on the
 * wire; once the only send the strip lists succeeds, and it was pressed on this visit, the page
 * opens its broadcast as it always has, unless the human has already started another
 * (`composing`). While the strip lists any other send, a failure kept from an earlier visit
 * included, the page stays and each row links its broadcast. A sent row leaves with the page; a
 * failed one stays until Retry or Restore draft takes it; and one still out when the page unmounts
 * is shown with its outcome on the next visit, or after a reload.
 */
export function useBroadcastQueue(composing: boolean): {
  enqueue: (send: BroadcastSend) => void;
  /** Takes a failed send off the list: for Restore draft to hand its draft back, or for Discard
   *  to drop one a reload interrupted too long ago to be sent on its own. */
  dismiss: (row: BroadcastSendRow) => void;
  /** Sends a failed send's very request again, at the back of the line and in its own row. */
  retry: (row: BroadcastSendRow) => void;
  /** Stops trying a late send once the attempt in flight settles (Stop trying). */
  stop: (row: BroadcastSendRow) => void;
  rows: readonly BroadcastSendRow[];
} {
  const navigate = useNavigate();
  const queue = broadcastQueue(useQueryClient());
  const rows = useSyncExternalStore(queue.subscribe, queue.rows);
  const [openedAt] = useState(Date.now);
  // A same-tick double press reaches the handler twice before React re-renders without the
  // composer, so the second would queue the same request; this ref drops it until the next task.
  const press = useSubmitGuard();
  const composingNow = useRef(composing);
  composingNow.current = composing;

  useEffect(() => {
    const [only, ...others] = rows;
    if (
      only === undefined ||
      others.length > 0 ||
      only.status !== "sent" ||
      only.submittedAt < openedAt ||
      composingNow.current
    ) {
      return;
    }
    // The exclusions travel with the navigation: they are a fact about this send, not about the
    // broadcast, so the server stores none and this is the only place they can be shown.
    void navigate(`/agents/broadcasts/${only.created.id}`, {
      state: { excluded: only.created.excluded },
    });
  }, [navigate, openedAt, rows]);

  useEffect(() => () => queue.clearSent(), [queue]);

  const guarded = (fn: () => void) => {
    press.guard(fn);
    setTimeout(press.release, 0);
  };
  return {
    dismiss: (row) => queue.dismiss(row),
    enqueue: (send) => guarded(() => queue.enqueue(send)),
    retry: (row) => guarded(() => queue.retry(row)),
    rows,
    stop: (row) => {
      queue.stop(row);
    },
  };
}

function agentCount(count: number): string {
  return `${count} ${count === 1 ? "agent" : "agents"}`;
}

/**
 * The page's sends, one row each, newest press first so the send just pressed is in view: queued,
 * on the wire, still being tried under its key once past its deadline (with Stop trying), sent
 * (linking its broadcast, with the send's exclusions), or refused with the server's reason - or
 * stopped - with Retry and Restore draft. One a reload interrupted too long ago to be sent on its
 * own (`SendInterruptedError`) says so in the error's own words and offers Discard beside them. A
 * retried send keeps its row's place. The strip sits above the composer, outside it, because the
 * composer goes whenever the selection empties - at every press - and a send outlives it. Restore
 * draft refuses, and says why under its row, while the host says putting a draft back would
 * replace one (`restoreRefusal`).
 */
export function BroadcastSends({
  className = "",
  onDiscard,
  onRestore,
  onRetry,
  onStop,
  restoreRefusal,
  rows,
}: {
  /** Placement only, as the host lays the strip out. */
  className?: string;
  onDiscard: (row: BroadcastSendRow) => void;
  onRestore: (row: BroadcastSendRow) => void;
  onRetry: (row: BroadcastSendRow) => void;
  onStop: (row: BroadcastSendRow) => void;
  restoreRefusal: string | null;
  rows: readonly BroadcastSendRow[];
}): ReactNode {
  const reasonId = useId();
  return (
    <section
      aria-label="Sends"
      className={`max-h-28 overflow-y-auto rounded-xl border px-3 py-2 text-sm narrow-or-short:text-xs ${card} ${borderDefault} ${className}`}
    >
      <ul className="space-y-1">
        {rows.map((row) => {
          const count = agentCount(row.send.input.session_ids.length);
          const restoreReasonId = `${reasonId}-${row.id}`;
          return (
            <li className="flex min-h-8 flex-wrap items-center gap-x-2 gap-y-1" key={row.id}>
              <span
                className={row.status === "failed" ? dangerText : textSecondaryOnSurface}
                role="status"
              >
                {row.status === "queued" ? (
                  `Queued: to ${count}`
                ) : row.status === "sending" ? (
                  `Sending to ${count}…`
                ) : row.status === "late" ? (
                  `Still sending to ${count} — Dispatch has not confirmed it yet. It keeps trying, and the broadcast is made once.`
                ) : row.status === "failed" ? (
                  row.error instanceof SendInterruptedError ? (
                    row.error.message
                  ) : row.error instanceof SendStoppedError ? (
                    `Stopped sending to ${count}: ${row.error.message}`
                  ) : (
                    `Could not send to ${count}: ${row.error.message}`
                  )
                ) : (
                  <>
                    <Link
                      className={`${linkText} ${linkHoverText}`}
                      state={{ excluded: row.created.excluded }}
                      to={`/agents/broadcasts/${row.created.id}`}
                    >
                      Sent to {agentCount(row.created.recipients.length)}
                    </Link>
                    {row.created.excluded.length === 0
                      ? null
                      : `, ${row.created.excluded.length} excluded`}
                  </>
                )}
              </span>
              <MarkdownPreview
                className={`min-w-0 flex-1 ${textMutedOnSurface}`}
                lines={1}
                markdown={row.send.input.body}
                fullTitle
              />
              {row.status === "late" ? (
                <StopTrying onStop={() => onStop(row)} stopping={row.stopping} />
              ) : null}
              {row.status === "failed" ? (
                <>
                  <button
                    className={secondaryButtonCompact}
                    onClick={() => onRetry(row)}
                    type="button"
                  >
                    Retry
                  </button>
                  <RefusableButton
                    look="secondary-compact"
                    onPress={() => onRestore(row)}
                    refusal={restoreRefusal}
                    refusalShownBy={restoreReasonId}
                  >
                    Restore draft
                  </RefusableButton>
                  {row.error instanceof SendInterruptedError ? (
                    <button
                      className={secondaryButtonCompact}
                      onClick={() => onDiscard(row)}
                      type="button"
                    >
                      Discard
                    </button>
                  ) : null}
                  {restoreRefusal === null ? null : (
                    <span
                      className={`basis-full text-xs ${textMutedOnSurface}`}
                      id={restoreReasonId}
                    >
                      {restoreRefusal}
                    </span>
                  )}
                </>
              ) : null}
            </li>
          );
        })}
      </ul>
    </section>
  );
}
