import type { Mutation } from "@tanstack/react-query";
import { useMutation, useMutationState, useQueryClient } from "@tanstack/react-query";
import { type ReactNode, useEffect, useRef, useState } from "react";
import { Link, useNavigate } from "react-router-dom";

import { api } from "../../api/client";
import { agentMessagesQuery } from "../../api/queries";
import type { BroadcastCreated, CreateBroadcastInput } from "../../api/types";
import { TruncatedText } from "../../components/TruncatedText";
import { useSubmitGuard } from "../../hooks/useSubmitGuard";
import {
  borderDefault,
  card,
  dangerText,
  linkHoverText,
  linkText,
  secondaryButtonBorder,
  secondaryButtonHoverBorder,
  secondaryButtonText,
  textMutedOnSurface,
  textSecondaryOnSurface,
} from "../../theme/classes";
import { firstLine } from "../conversation/ReplyQuote";

/** Every broadcast send carries this key, so the strip lists each one, not only the newest. */
const BROADCAST_SEND = ["broadcast-send"];

/** What one press of Send handed the queue: the request, and the selection it was pressed with,
 *  excluded sessions included, so Restore draft puts back the composer the human pressed. */
export interface BroadcastSend {
  readonly input: CreateBroadcastInput;
  readonly selected: readonly string[];
}

/** One send as its row shows it. */
export interface BroadcastSendRow {
  readonly created: BroadcastCreated | undefined;
  readonly error: string | undefined;
  readonly id: number;
  readonly send: BroadcastSend;
  readonly status: "failed" | "queued" | "sending" | "sent";
  readonly submittedAt: number;
}

type BroadcastSendMutation = Mutation<BroadcastCreated, Error, BroadcastSend, unknown>;

function sendRow(mutation: BroadcastSendMutation): BroadcastSendRow {
  const { data, error, isPaused, status, submittedAt, variables } = mutation.state;
  if (variables === undefined) throw new Error("a broadcast send was queued without its request");
  return {
    created: data,
    error: error === null ? undefined : error.message,
    id: mutation.mutationId,
    send: variables,
    status:
      status === "success"
        ? "sent"
        : status === "error"
          ? "failed"
          : isPaused
            ? "queued"
            : "sending",
    submittedAt,
  };
}

/**
 * The broadcast queue. Send hands its request off at the press (`enqueue`) - the page clears the
 * draft and the selection with it, so the next broadcast can be written at once - and the sends
 * run one at a time, in press order, through one mutation scope. A queued send carries its own
 * request as the mutation's variables, because a waiting mutation would otherwise take each
 * render's options, and so the draft on screen when it finally ran. The request is all a send
 * orders: the server answers before it delivers, and each broadcast delivers on its own, so two
 * broadcasts can reach one agent in either order.
 *
 * `rows` is every send this page has made, each with its own state, read from the mutation cache
 * rather than the mutation's latest, so a later send never hides an earlier one's failure. Nothing
 * navigates while a send is queued or on the wire; once the only send of this visit succeeds, the
 * page opens its broadcast as it always has, unless the human has already started another
 * (`composing`). After more than one send the page stays and each row links its broadcast. A
 * settled send leaves the cache when the page does; one still out when the page unmounts is shown
 * with its outcome on the next visit.
 */
export function useBroadcastQueue(composing: boolean): {
  enqueue: (send: BroadcastSend) => void;
  /** Takes a failed send off the list, for Restore draft to hand its draft back. */
  dismiss: (row: BroadcastSendRow) => void;
  /** Sends a failed send's very request again, as a new send at the back of the queue. */
  retry: (row: BroadcastSendRow) => void;
  rows: readonly BroadcastSendRow[];
} {
  const navigate = useNavigate();
  const queryClient = useQueryClient();
  const [openedAt] = useState(Date.now);
  // A same-tick double press reaches the handler twice before React re-renders without the
  // composer, so the second would queue the same request; this ref drops it until the next task.
  const press = useSubmitGuard();
  const { mutate } = useMutation({
    // A row stays until the page goes, however long the human leaves it: the default five
    // minutes would drop an unread failure from the list.
    gcTime: Number.POSITIVE_INFINITY,
    mutationFn: ({ input }: BroadcastSend) => api.createBroadcast(input),
    mutationKey: BROADCAST_SEND,
    onSuccess: (created) => {
      for (const recipient of created.recipients) {
        void queryClient.invalidateQueries({
          queryKey: agentMessagesQuery(recipient.session_id).queryKey,
        });
      }
      void queryClient.invalidateQueries({ queryKey: ["broadcast"] });
    },
    scope: { id: "broadcast" },
  });
  // A send is listed once it has its request: the cache announces a mutation as it is built,
  // before `mutate` hands it the variables and marks it pending.
  const rows = useMutationState<BroadcastSendRow, BroadcastSendMutation>({
    filters: {
      mutationKey: BROADCAST_SEND,
      predicate: (mutation) => mutation.state.status !== "idle",
    },
    select: sendRow,
  });
  const composingNow = useRef(composing);
  composingNow.current = composing;

  useEffect(() => {
    const [only, ...others] = rows;
    if (
      only === undefined ||
      others.length > 0 ||
      only.created === undefined ||
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

  useEffect(
    () => () => {
      const cache = queryClient.getMutationCache();
      for (const mutation of cache.findAll({ mutationKey: BROADCAST_SEND })) {
        if (mutation.state.status !== "pending") cache.remove(mutation);
      }
    },
    [queryClient]
  );

  const guarded = (fn: () => void) => {
    press.guard(fn);
    setTimeout(press.release, 0);
  };
  const dismiss = (row: BroadcastSendRow) => {
    const cache = queryClient.getMutationCache();
    const mutation = cache.getAll().find((candidate) => candidate.mutationId === row.id);
    if (mutation !== undefined) cache.remove(mutation);
  };
  return {
    dismiss,
    enqueue: (send) => guarded(() => mutate(send)),
    retry: (row) =>
      guarded(() => {
        dismiss(row);
        mutate(row.send);
      }),
    rows,
  };
}

function agentCount(count: number): string {
  return `${count} ${count === 1 ? "agent" : "agents"}`;
}

/**
 * The page's sends, one row each, oldest first: queued, on the wire, sent (linking its broadcast,
 * with the send's exclusions), or refused with the server's reason, Retry and Restore draft. It
 * sits above the composer, outside it, because the composer goes whenever the selection empties
 * - at every press - and a send outlives it.
 */
export function BroadcastSends({
  onRestore,
  onRetry,
  rows,
}: {
  onRestore: (row: BroadcastSendRow) => void;
  onRetry: (row: BroadcastSendRow) => void;
  rows: readonly BroadcastSendRow[];
}): ReactNode {
  const action = `min-h-11 shrink-0 rounded-lg border px-2 text-xs font-medium md:min-h-7 ${secondaryButtonBorder} ${secondaryButtonText} ${secondaryButtonHoverBorder}`;
  return (
    <section
      aria-label="Sends"
      className={`max-h-28 overflow-y-auto rounded-xl border px-3 py-2 text-sm narrow-or-short:text-xs ${card} ${borderDefault}`}
    >
      <ul className="space-y-1">
        {rows.map((row) => {
          const count = agentCount(row.send.input.session_ids.length);
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
                ) : row.status === "failed" || row.created === undefined ? (
                  `Could not send to ${count}: ${row.error ?? "network error"}`
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
              <TruncatedText
                className={`min-w-0 flex-1 ${textMutedOnSurface}`}
                title={row.send.input.body}
              >
                {firstLine(row.send.input.body)}
              </TruncatedText>
              {row.status === "failed" ? (
                <>
                  <button className={action} onClick={() => onRetry(row)} type="button">
                    Retry
                  </button>
                  <button className={action} onClick={() => onRestore(row)} type="button">
                    Restore draft
                  </button>
                </>
              ) : null}
            </li>
          );
        })}
      </ul>
    </section>
  );
}
