import { type ReactNode, useId, useState } from "react";

import type { ArtifactOwner } from "../../api/client";
import type { Ask } from "../../api/types";
import { linkHoverText, linkText } from "../../theme/classes";
import type { DwellRef } from "../shell/useDwell";
import { AskReplyComposer, AskThread, type AskThreadQuery, type CreateReply } from "./AskThread";
import { type ThreadOrder, useThreadOrder } from "./preferences";

interface AskThreadDisclosureProps {
  ask: Ask;
  thread: AskThreadQuery;
  createReply?: CreateReply;
  embedded?: boolean;
  /** Where a file pasted or dropped into the reply goes (`AskReplyComposer`). */
  uploadOwner?: ArtifactOwner;
  /** The Replies page's mark for the ask, placed on its newest reply (`AskThread`). */
  markNewest?: DwellRef;
  /** The reply a deep link named (`AskThread`); only the Conversation passes one. */
  currentReplyId?: string;
  /** The order the thread reads in (`AskThread`), which also places this control and the
   *  composer it reveals: under the question in Newest first, at the foot in Oldest first. */
  order?: ThreadOrder;
  /** The ids of the replies the reader has not read (`AskThread`). */
  unread?: ReadonlySet<string>;
}

export function AskThreadDisclosure({
  ask,
  thread,
  createReply,
  embedded = false,
  uploadOwner,
  markNewest,
  currentReplyId,
  order,
  unread,
}: AskThreadDisclosureProps): ReactNode {
  const [composerOpen, setComposerOpen] = useState(false);
  const composerId = useId();
  const [storedOrder] = useThreadOrder();
  const threadOrder = order ?? storedOrder;
  if (thread.isError) {
    const message =
      thread.error instanceof Error ? thread.error.message : "Unable to load replies.";
    return (
      <button
        aria-expanded="false"
        className={`mt-3 min-h-11 text-sm font-medium ${linkText} ${linkHoverText}`}
        onClick={() => {
          void thread.refetch();
        }}
        title={message}
        type="button"
      >
        Replies unavailable — retry
      </button>
    );
  }

  const replyCount = thread.data?.replies.length ?? 0;
  if (replyCount === 0 && ask.state !== "answered") return null;

  // The composer follows the control that reveals it, so the next Tab from the control lands in
  // the field and a screen reader meets the expanded panel right after its button. The control is
  // "Write a reply" so a screen reader can tell it from the form's own "Reply" submit button.
  // The pair sits where the order puts a reply box: next to the newest message.
  const threadNode =
    replyCount === 0 ? null : (
      <AskThread
        ask={ask}
        currentReplyId={currentReplyId}
        embedded={embedded}
        markNewest={markNewest}
        order={threadOrder}
        showComposer={false}
        thread={thread}
        unread={unread}
      />
    );
  const composer =
    ask.state === "answered" ? (
      <>
        <button
          aria-controls={composerId}
          aria-expanded={composerOpen}
          className={`mt-3 min-h-11 text-sm font-medium ${linkText} ${linkHoverText}`}
          onClick={() => setComposerOpen((open) => !open)}
          type="button"
        >
          Write a reply
        </button>
        {composerOpen ? (
          <div className="mt-2" id={composerId}>
            <AskReplyComposer ask={ask} createReply={createReply} uploadOwner={uploadOwner} />
          </div>
        ) : null}
      </>
    ) : null;

  return threadOrder === "newest" ? (
    <>
      {composer}
      {threadNode}
    </>
  ) : (
    <>
      {threadNode}
      {composer}
    </>
  );
}
