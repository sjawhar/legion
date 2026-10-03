import type { MutationKey } from "@tanstack/react-query";
import { useCallback, useEffect, useMemo, useState } from "react";

import { useSending } from "../../hooks/useSending";
import type { ReplyTarget } from "./composer-model";
import { useHeldSends } from "./held-sends";

/** The name every comment thread card's own reply send in a tab sits beneath. */
export function threadRepliesSendKey(sendKey: MutationKey): MutationKey {
  return [...sendKey, "thread-card"];
}

/** The name of a comment thread card's own reply composer's send: beneath the tab's `sendKey`, so
 *  the tab's Replies hold while it is out as they do for the tab's own composers, and its own, so
 *  the thread can tell that its reply is the one out. */
export function threadReplySendKey(sendKey: MutationKey, commentId: string): MutationKey {
  return [...threadRepliesSendKey(sendKey), commentId];
}

/** The Conversation's full-screen thread view on a phone, and the composer of its own that a
 *  comment's Reply answers in there. */
export interface PhoneThread {
  /** The comment whose thread shows, or undefined. It shows on a phone, and at any width while a
   *  send from the view keeps it open, until Back. */
  readonly id: string | undefined;
  /** Whether the reader has `commentId` in hand here - its thread open, or the thread composer
   *  answering it - so the tab keeps it listed whoever resolves it. */
  readonly holds: (commentId: string) => boolean;
  /** The thread composer's reply, so the docked composer's own reply and draft stay as they were.
   *  The view stays mounted, hidden, while it is set, so the composer and an unsent draft outlive
   *  Back; a send it holds is the held-send store's, and comes back with its reply. */
  readonly replyTo: ReplyTarget | null;
  /** The thread composer's send name: beneath the tab's, so every Reply holds for it as for the
   *  docked composer's, and its own, so the view can tell its send from the docked one's. */
  readonly composerKey: MutationKey;
  /** Whether the open thread's card has its own reply out: the view's Back holds for it. */
  readonly cardReplySending: boolean;
  readonly open: (commentId: string) => void;
  /** Answers `target` in its thread, and says whether it did: a send the tab has out refuses it. */
  readonly beginReply: (target: ReplyTarget) => boolean;
  readonly endReply: () => void;
  /** Back and Escape: refused while the open thread's card has its reply out, until the send's
   *  deadline. Past it they leave the thread and the reply keeps its send; otherwise a refusal
   *  the card's reply holds goes with the thread, its draft with it. */
  readonly close: () => void;
}

export function usePhoneThread({
  isPhoneViewport,
  sendKey,
  sendingNow,
}: {
  readonly isPhoneViewport: boolean;
  /** The tab's send name; the view's composers name theirs beneath it. */
  readonly sendKey: MutationKey;
  /** Whether a send the tab names is out now, from Send's own task on. */
  readonly sendingNow: () => boolean;
}): PhoneThread {
  const store = useHeldSends();
  const [openId, setOpenId] = useState<string>();
  const composerKey = useMemo<MutationKey>(() => [...sendKey, "phone-thread"], [sendKey]);
  // The thread composer's reply is its send's address: a send it has held - out, or refused -
  // still answers that reply when the tab mounts again, so the composer shows it there.
  const [replyTo, setReplyTo] = useState<ReplyTarget | null>(
    () => store.get(composerKey)?.request.replyTo ?? null
  );
  // Set when the viewport widened past the phone layout while a send from the view was out - its
  // card's own reply, or the thread composer's: the view stays open, full-screen at any width,
  // until the reader leaves it with Back, so the send's draft and its refusal stay where the
  // reader was. A widening with nothing out closes the view.
  const [outlived, setOutlived] = useState(false);
  // No comment has the empty id, so with no thread open nothing matches the card's key.
  const cardReplyKey = useMemo(() => threadReplySendKey(sendKey, openId ?? ""), [openId, sendKey]);
  const { sending: cardReplySending, sendingNow: cardReplySendingNow } = useSending(cardReplyKey, {
    untilDeadline: true,
  });
  const { sending: composerSending } = useSending(composerKey, { untilDeadline: true });
  const replySending = composerSending && replyTo !== null;
  useEffect(() => {
    if (isPhoneViewport || outlived) return;
    if (openId !== undefined && cardReplySending) {
      setOutlived(true);
      return;
    }
    if (replySending && replyTo !== null) {
      // The view that stays is the thread the composer's send answers, even one the reader had
      // left with Back: on a wider screen nothing else would show that send's outcome.
      setOpenId(replyTo.id);
      setOutlived(true);
      return;
    }
    if (openId !== undefined) setOpenId(undefined);
  }, [cardReplySending, isPhoneViewport, openId, outlived, replySending, replyTo]);
  const holds = useCallback(
    (commentId: string) => commentId === openId || commentId === replyTo?.id,
    [openId, replyTo]
  );
  return {
    beginReply: (target) => {
      if (sendingNow()) return false;
      setReplyTo(target);
      setOpenId(target.id);
      return true;
    },
    cardReplySending,
    close: () => {
      if (cardReplySendingNow()) return;
      // Back drops a refusal the card's reply holds, its draft with it: a send still out past the
      // deadline stays held for the thread's return.
      if (openId !== undefined) store.discard(threadReplySendKey(sendKey, openId));
      setOpenId(undefined);
      setOutlived(false);
    },
    composerKey,
    endReply: () => setReplyTo(null),
    holds,
    id: isPhoneViewport || outlived || cardReplySending || replySending ? openId : undefined,
    open: setOpenId,
    replyTo,
  };
}
