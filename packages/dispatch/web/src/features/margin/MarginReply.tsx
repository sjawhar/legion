import type { MutationKey } from "@tanstack/react-query";
import { type ReactNode, useLayoutEffect, useMemo, useRef } from "react";
import { createPortal } from "react-dom";

import { MentionComposer } from "../conversation/MentionComposer";
import type { MarginOwner } from "./useMarginItems";

/** The name of the reply send of the margin thread `key`: a phone thread's Back and Escape hold on
 *  it while it is out. */
export function marginReplySendKey(key: string): MutationKey {
  return ["margin-thread-reply", key];
}

/**
 * A margin thread's reply composer, which the margin keeps (`MarginSheet`) above everything that
 * can unmount the thread's card: collapsing it for another thread, the Pinned tab, the rail the
 * desktop margin collapses to, a phone thread's Back, a move between the open and resolved lists,
 * and leaving the document. It renders into its own element, which the card showing the thread
 * takes into itself (`MarginReplySlot`), so its draft, a send it has out and that send's refusal
 * are wherever the thread shows again.
 */
export interface MarginReply {
  /** The document the thread is on. */
  artifact: string;
  /** The thread it answers: its root comment's id. */
  key: string;
  /** The element the composer renders into. */
  node: HTMLElement;
  /** Where its sends go: the owner the thread was shown under, wherever the margin is now. */
  owner: MarginOwner;
}

/** A kept reply as the margin renders it now. */
export interface MarginReplyEntry extends MarginReply {
  /** Its thread is a decided suggestion's, which offers no new reply (`finishing`). */
  finishing: boolean;
  /** Its document is the one open. */
  shown: boolean;
}

/** A kept reply's composer, rendered into the reply's own element wherever that element is. */
export function MarginReplyComposer({
  closed,
  frame,
  onClose,
  onHoldingChange,
  reply,
}: {
  closed: boolean;
  frame: string | undefined;
  onClose: (key: string) => void;
  onHoldingChange: (key: string, holding: boolean) => void;
  reply: MarginReplyEntry;
}): ReactNode {
  const { finishing, key, node, owner } = reply;
  const mutationKey = useMemo(() => marginReplySendKey(key), [key]);
  return createPortal(
    <MentionComposer
      closed={closed}
      finishing={finishing}
      frame={frame}
      inline
      kind="comment"
      mutationKey={mutationKey}
      onCancelReply={() => onClose(key)}
      onClose={() => onClose(key)}
      onHoldingChange={(holding) => onHoldingChange(key, holding)}
      onSent={() => {}}
      owner={
        owner.kind === "issue"
          ? { issueKey: owner.key, kind: "issue" }
          : { artifactId: owner.artifactId, kind: "artifact", project: owner.project }
      }
      replyTo={{ author: "", excerpt: "", id: key, parentKind: "comment" }}
    />,
    node
  );
}

/** Where a margin card shows its thread's reply composer: it takes the reply's element into
 *  itself for as long as it is mounted (`attach`, which keeps the reply from then on). */
export function MarginReplySlot({
  attach,
  threadKey,
}: {
  attach: (key: string, slot: HTMLElement) => () => void;
  threadKey: string;
}): ReactNode {
  const slot = useRef<HTMLDivElement>(null);
  useLayoutEffect(() => {
    const element = slot.current;
    if (element === null) return;
    return attach(threadKey, element);
  }, [attach, threadKey]);
  return <div className="contents" ref={slot} />;
}
