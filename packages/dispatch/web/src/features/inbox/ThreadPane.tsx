/**
 * The Inbox's reading column for a Reply or an Agent row: one body per shape the thread comes
 * in, each marked `data-inbox-pane-body`.
 *
 * A thread that needs no document is drawn whole under its subject line (`ThreadBodyPane`); a
 * thread whose document the column can open is drawn in that document, at the block it belongs
 * to (`ThreadDocumentBody`) - the anchored comment thread below its block, a decision block's
 * ask in the block itself. A document that cannot be opened, and an anchor that cannot be
 * placed, fall back to the thread alone with the text it is anchored to.
 */

import { useQuery } from "@tanstack/react-query";
import type { ReactNode } from "react";

import { askThreadQuery } from "../../api/queries";
import type { MyReplyRow, MyReplyThreadRow } from "../../api/types";
import { textPrimaryOnCanvas } from "../../theme/classes";
import type { ProofDocumentProps } from "../doc/ProofDocument";
import { isDecisionAsk } from "./ask-decision";
import { DocumentPane, PaneFrame } from "./PaneDocument";
import { questionLine } from "./question-line";
import { ThreadPaneHead } from "./ReadingPaneHead";
import { ThreadBody } from "./ThreadBody";

/** The subject line a thread body opens with: what the thread is about, in one line. */
function ThreadSubject({ thread }: { thread: MyReplyRow }): ReactNode {
  return (
    <h2 className={`text-sm font-semibold ${textPrimaryOnCanvas}`} data-inbox-thread-subject="">
      {questionLine(thread.subject)}
    </h2>
  );
}

/** Which of the column's four thread bodies this is, as the pane marks it. */
type ThreadPaneBody = "agent" | "ask" | "document" | "issue";

/** What a thread body is drawn from, whether it is a pane of its own or the document body's
 *  fall-back. */
interface ThreadBodyProps {
  body: ThreadPaneBody;
  /** Whether this is the document body falling back to the thread alone. */
  fallback?: boolean;
  quote: boolean;
  thread: MyReplyRow;
}

/** What a thread body is: the thread's subject line and the thread itself, marked as the
 *  column's own body. The document body draws this in its place where it falls back. */
function ThreadBodyContent({ body, fallback = false, quote, thread }: ThreadBodyProps): ReactNode {
  return (
    <div data-inbox-pane-body={body} data-inbox-pane-fallback={fallback ? "" : undefined}>
      <ThreadSubject thread={thread} />
      <ThreadBody openAtFirstUnread quote={quote} thread={thread} />
    </div>
  );
}

/** A thread that needs no document: a comment thread on an issue, an issue message
 *  conversation, an ask that is not a decision block, and a thread whose document the column
 *  could not open, which shows the quote the thread is anchored to in place of it. */
function ThreadBodyPane(pane: ThreadBodyProps): ReactNode {
  return (
    <PaneFrame head={<ThreadPaneHead thread={pane.thread} />} label="Selected thread">
      <ThreadBodyContent {...pane} />
    </PaneFrame>
  );
}

/**
 * A thread drawn in its own document: the live document, opened at the section that holds the
 * thread's block, with the quote highlighted.
 *
 * The two shapes differ in what the block carries. A comment thread anchored in a project
 * document is drawn inside the document column directly below its block, through the node
 * view's own slot (`anchoredThread`); a decision block's ask needs none, because the block
 * draws the question, its answer and its thread itself (`AskBlockCard`).
 *
 * It falls back to the thread alone, with its quote, when the document cannot be opened - and,
 * for a thread the column must place itself, when the document holds no such block: an orphaned
 * anchor, or one naming a block that has gone, leaves the thread nowhere to be drawn.
 */
function ThreadDocumentBody({
  anchoredThread,
  artifactId,
  blockId,
  body,
  issueKey,
  thread,
}: {
  anchoredThread: ProofDocumentProps["anchoredThread"];
  artifactId: string;
  blockId: string;
  body: Extract<ThreadPaneBody, "ask" | "document">;
  issueKey: string | null;
  thread: MyReplyThreadRow;
}): ReactNode {
  return (
    <DocumentPane
      anchoredThread={anchoredThread}
      artifactId={artifactId}
      blockId={blockId}
      fallback={<ThreadBodyContent body={body} fallback quote thread={thread} />}
      head={(document) => (
        <ThreadPaneHead sectionBlockId={document.sectionBlockId} thread={thread} />
      )}
      issueKey={issueKey}
      label="Selected thread"
      loadingName={thread.document?.name ?? "the document"}
      onAskAnswered={noAnswer}
      paneBody={body}
    />
  );
}

/** The thread of an ask. A decision block's ask opens its document at its section, where the
 *  block hosts its own answer form and exchange, as an open question's does; every other ask
 *  shows its card, with its answer and its thread. */
function AskThreadPane({ thread }: { thread: MyReplyThreadRow }): ReactNode {
  // The card's own key, so the card below reads no second copy of the same thread.
  const read = useQuery(askThreadQuery(thread.thread_id));
  const ask = read.data?.ask;
  if (ask !== undefined && isDecisionAsk(ask)) {
    return (
      <ThreadDocumentBody
        anchoredThread={undefined}
        artifactId={ask.block_artifact.id}
        blockId={ask.block_id}
        body="ask"
        issueKey={ask.issue_key}
        thread={thread}
      />
    );
  }
  return <ThreadBodyPane body="ask" quote={false} thread={thread} />;
}

/** The reading column for a Reply or an Agent row: one body per shape the thread comes in. */
export function ThreadPane({ thread }: { thread: MyReplyRow }): ReactNode {
  if (thread.kind === "agent") {
    return <ThreadBodyPane body="agent" quote={false} thread={thread} />;
  }
  if (thread.kind === "ask") {
    return <AskThreadPane thread={thread} />;
  }
  const anchor = thread.anchor;
  const blockId = anchor?.block_id;
  const inDocument = thread.kind === "comment" && thread.document !== undefined;
  if (
    inDocument &&
    anchor !== undefined &&
    !anchor.orphaned &&
    blockId !== null &&
    blockId !== undefined
  ) {
    return (
      <ThreadDocumentBody
        anchoredThread={{
          blockId,
          children: <ThreadBody openAtFirstUnread thread={thread} />,
          markId: anchor.mark_id,
        }}
        artifactId={anchor.artifact_id}
        blockId={blockId}
        body="document"
        issueKey={null}
        thread={thread}
      />
    );
  }
  // A thread in a document whose anchor cannot be placed - orphaned, or naming no block - is the
  // document body falling back to the thread alone, with its quote.
  return (
    <ThreadBodyPane
      body={inDocument ? "document" : "issue"}
      fallback={inDocument}
      quote={inDocument}
      thread={thread}
    />
  );
}

/** A thread's document answers nothing of the column's: an ask answered in one of its blocks is
 *  the block's own business, and the list reads its rows again on the event. */
function noAnswer(): void {}
