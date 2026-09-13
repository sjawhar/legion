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
  const [open, setOpen] = useState(false);

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

  const count = thread.data?.replies.length ?? 0;
  // With no replies there is nothing to disclose; the only reason to open the thread is the
  // answered-ask Reply composer. An open ask's composer lives in the card, a resolved ask has
  // none, so neither gets a trigger here.
  if (count === 0 && ask.state !== "answered") return null;
  return (
    <>
      <button
        aria-controls={`thread-${ask.id}`}
        aria-expanded={open}
        className={`mt-3 min-h-11 text-sm font-medium ${linkText} ${linkHoverText}`}
        onClick={() => {
          setOpen((current) => !current);
        }}
        type="button"
      >
        {count === 0 ? "Reply" : count === 1 ? "1 reply" : `${count} replies`}
      </button>
      {open ? (
        <div id={`thread-${ask.id}`}>
          <AskThread
            ask={ask}
            createReply={createReply}
            embedded={embedded}
            showResolution={false}
            thread={thread}
          />
        </div>
      ) : null}
    </>
  );
}
