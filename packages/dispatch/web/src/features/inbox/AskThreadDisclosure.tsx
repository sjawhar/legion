import { type ReactNode, useState } from "react";

import type { Ask, Comment, CreateCommentInput } from "../../api/types";
import { linkHoverText, linkText } from "../../theme/classes";
import { AskThread, type AskThreadQuery } from "./AskThread";

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

  return (
    <>
      {replyCount > 0 || composerOpen ? (
        <AskThread
          ask={ask}
          createReply={createReply}
          embedded={embedded}
          showComposer={composerOpen}
          thread={thread}
        />
      ) : null}
      {ask.state === "answered" ? (
        <button
          aria-expanded={composerOpen}
          className={`mt-3 min-h-11 text-sm font-medium ${linkText} ${linkHoverText}`}
          onClick={() => setComposerOpen((open) => !open)}
          type="button"
        >
          Reply
        </button>
      ) : null}
    </>
  );
}
