import type { Mutation } from "@tanstack/react-query";
import { useMutation, useMutationState, useQueryClient } from "@tanstack/react-query";
import { type ReactNode, useEffect, useId, useRef, useState } from "react";
import { Link, useNavigate } from "react-router-dom";

import { api } from "../../api/client";
import { agentMessagesQuery } from "../../api/queries";
import type { BroadcastCreated, CreateBroadcastInput } from "../../api/types";
import { RefusableButton } from "../../components/RefusableButton";
import { TruncatedText } from "../../components/TruncatedText";
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
import { firstLine } from "../conversation/ReplyQuote";

/** Every broadcast send carries this key, so the strip lists each one, not only the newest. */
const BROADCAST_SEND = ["broadcast-send"];

/** What one press of Send handed the queue: the request, and the selection it was pressed with,
 *  excluded sessions included, so Restore draft puts back the composer the human pressed. */
export interface BroadcastSend {
  readonly input: CreateBroadcastInput;
  readonly selected: readonly string[];
}

/** A send as the queue holds it: `pressed` is its press's place in the strip, which Retry keeps,
 *  so a retried row stays where it was and nothing moves under the pointer. */
interface QueuedSend extends BroadcastSend {
  readonly pressed: number;
}

/** One send as its row shows it: waiting its turn or on the wire, sent with the broadcast it
 *  made, or failed with the reason its request was refused. */
export type BroadcastSendRow = {
  readonly id: number;
  readonly send: QueuedSend;
  readonly submittedAt: number;
} & (
  | { readonly status: "queued" }
  | { readonly status: "sending" }
  | { readonly created: BroadcastCreated; readonly status: "sent" }
  | { readonly error: string; readonly status: "failed" }
);

type BroadcastSendMutation = Mutation<BroadcastCreated, Error, QueuedSend, unknown>;

/** Presses this page load has made, so each new send takes the next place in the strip. */
let presses = 0;
/** The last broadcast request handed to the server, settled either way. Each request waits for
 *  the one before it here, in the mutation function, rather than in a TanStack mutation `scope`:
 *  a scoped mutation behind another is paused, and a paused mutation resumes only while the tab
 *  is visible (`focusManager`), so a queued send would wait for the reader to come back. It is
 *  module state so the order holds across the page unmounting and mounting again. */
let lastRequest: Promise<unknown> = Promise.resolve();

function sendRow(mutation: BroadcastSendMutation): BroadcastSendRow {
  const { data, error, isPaused, status, submittedAt, variables } = mutation.state;
  if (variables === undefined) throw new Error("a broadcast send was queued without its request");
  const row = { id: mutation.mutationId, send: variables, submittedAt };
  switch (status) {
    case "idle":
      throw new Error("a broadcast send was listed before it started");
    case "pending":
      // Offline, TanStack holds the send before it joins the line; online, `useBroadcastQueue`
      // tells the one on the wire from those behind it.
      return { ...row, status: isPaused ? "queued" : "sending" };
    case "success":
      if (data === undefined) throw new Error("a broadcast send succeeded without its broadcast");
      return { ...row, created: data, status: "sent" };
    case "error":
      if (error === null) throw new Error("a broadcast send failed without its error");
      return { ...row, error: error.message, status: "failed" };
  }
}

/**
 * The broadcast queue. Send hands its request off at the press (`enqueue`) - the page clears the
 * draft and the selection with it, so the next broadcast can be written at once - and the
 * requests go out one at a time, in the order they were handed over: each waits for the one
 * before it to be answered, either way (`lastRequest`). A send carries its own request as the
 * mutation's variables, so a waiting send keeps the draft it was pressed with. The request is all
 * a send orders: the server answers before it delivers, and each broadcast delivers on its own,
 * so two broadcasts can reach one agent in either order.
 *
 * `rows` is every send this page holds, newest press first, each with its own state, read from
 * the mutation cache rather than the mutation's latest, so a later send never hides an earlier
 * one's failure; a send still waiting for an earlier one is `queued`. Nothing navigates while a
 * send is queued or on the wire; once the only send of this visit succeeds, the page opens its
 * broadcast as it always has, unless the human has already started another (`composing`). After
 * more than one send the page stays and each row links its broadcast. A sent row leaves the
 * cache when the page does; a failed one stays until Retry or Restore draft takes it, since its
 * row is the only copy of a message Send cleared from the composer; and one still out when the
 * page unmounts is shown with its outcome on the next visit.
 */
export function useBroadcastQueue(composing: boolean): {
  enqueue: (send: BroadcastSend) => void;
  /** Takes a failed send off the list, for Restore draft to hand its draft back. */
  dismiss: (row: BroadcastSendRow) => void;
  /** Sends a failed send's very request again, at the back of the line and in its own row. */
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
    mutationFn: ({ input }: QueuedSend) => {
      const request = lastRequest.then(() => api.createBroadcast(input));
      lastRequest = request.catch(() => undefined);
      return request;
    },
    mutationKey: BROADCAST_SEND,
    onSuccess: (created) => {
      for (const recipient of created.recipients) {
        void queryClient.invalidateQueries({
          queryKey: agentMessagesQuery(recipient.session_id).queryKey,
        });
      }
      void queryClient.invalidateQueries({ queryKey: ["broadcast"] });
    },
  });
  // A send is listed once it has its request: the cache announces a mutation as it is built,
  // before `mutate` hands it the variables and marks it pending.
  const sends = useMutationState<BroadcastSendRow, BroadcastSendMutation>({
    filters: {
      mutationKey: BROADCAST_SEND,
      predicate: (mutation) => mutation.state.status !== "idle",
    },
    select: sendRow,
  });
  // Mutation ids follow the order the requests joined the line, so of the sends still out, the
  // lowest id is on the wire and the rest wait behind it.
  const onTheWire = Math.min(...sends.flatMap((row) => (row.status === "sending" ? [row.id] : [])));
  const rows = sends
    .map(
      (row): BroadcastSendRow =>
        row.status === "sending" && row.id !== onTheWire ? { ...row, status: "queued" } : row
    )
    .sort((a, b) => b.send.pressed - a.send.pressed);
  const composingNow = useRef(composing);
  composingNow.current = composing;

  useEffect(() => {
    const [only, ...others] = sends;
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
  }, [navigate, openedAt, sends]);

  useEffect(
    () => () => {
      const cache = queryClient.getMutationCache();
      for (const mutation of cache.findAll({ mutationKey: BROADCAST_SEND })) {
        if (mutation.state.status === "success") cache.remove(mutation);
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
    const mutation = cache.find({ predicate: (candidate) => candidate.mutationId === row.id });
    if (mutation !== undefined) cache.remove(mutation);
  };
  return {
    dismiss,
    enqueue: (send) => {
      presses += 1;
      const pressed = presses;
      guarded(() => mutate({ ...send, pressed }));
    },
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
 * The page's sends, one row each, newest press first so the send just pressed is in view: queued,
 * on the wire, sent (linking its broadcast, with the send's exclusions), or refused with the
 * server's reason, Retry and Restore draft. A retried send keeps its row's place. The strip sits
 * above the composer, outside it, because the composer goes whenever the selection empties - at
 * every press - and a send outlives it. Restore draft refuses, and says why under its row, while
 * the host says putting a draft back would replace one (`restoreRefusal`).
 */
export function BroadcastSends({
  onRestore,
  onRetry,
  restoreRefusal,
  rows,
}: {
  onRestore: (row: BroadcastSendRow) => void;
  onRetry: (row: BroadcastSendRow) => void;
  restoreRefusal: string | null;
  rows: readonly BroadcastSendRow[];
}): ReactNode {
  const reasonId = useId();
  return (
    <section
      aria-label="Sends"
      className={`max-h-28 overflow-y-auto rounded-xl border px-3 py-2 text-sm narrow-or-short:mt-3 narrow-or-short:text-xs ${card} ${borderDefault}`}
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
                ) : row.status === "failed" ? (
                  `Could not send to ${count}: ${row.error}`
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
