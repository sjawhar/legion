import { type ReactNode, useId, useState } from "react";

import type { Ask, Comment, CreateCommentInput } from "../../api/types";
import { linkHoverText, linkText } from "../../theme/classes";
import { AskReplyComposer, AskThread, type AskThreadQuery } from "./AskThread";

interface AskThreadDisclosureProps {
  ask: Ask;
  thread: AskThreadQuery;
  createReply?: (issueKey: string, input: CreateCommentInput) => Promise<Comment>;
  embedded?: boolean;
}

export function AskThreadDisclosure({
  ask,
  thread,
  createReply,
  embedded = false,
}: AskThreadDisclosureProps): ReactNode {
  const [composerOpen, setComposerOpen] = useState(false);
  const composerId = useId();

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

  // The composer follows the Reply control that reveals it, so the next Tab from the control
  // lands in the field and a screen reader meets the expanded panel right after its button.
  return (
    <>
      {replyCount === 0 ? null : (
        <AskThread
          ask={ask}
          createReply={createReply}
          embedded={embedded}
          showComposer={false}
          thread={thread}
        />
      )}
      {ask.state === "answered" ? (
        <>
          <button
            aria-controls={composerId}
            aria-expanded={composerOpen}
            className={`mt-3 min-h-11 text-sm font-medium ${linkText} ${linkHoverText}`}
            onClick={() => setComposerOpen((open) => !open)}
            type="button"
          >
            Reply
          </button>
          {composerOpen ? (
            <div className="mt-2" id={composerId}>
              <AskReplyComposer ask={ask} createReply={createReply} />
            </div>
          ) : null}
        </>
      ) : null}
    </>
  );
}
