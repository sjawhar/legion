/**
 * A thread of the Inbox, drawn whole.
 *
 * One element for the four shapes a Reply or an Agent row comes in — a comment thread, an issue
 * message conversation, an ask's thread, and a one-off message to an agent session — read when it
 * is shown, every message rendered whole with `MarkdownBody`, laid out by the one order-and-
 * collapse rule (`threads/thread-layout.ts`), each message the reader has not read marked
 * **New**, and ending in the composer of the thread's own page, so a reply sent from here goes
 * where one sent there goes. The reading pane draws it, below 1280 px and in the drawer the card
 * draws it, and inside a document it is drawn in the slot under the block it is anchored to
 * (`ThreadPane.tsx`).
 *
 * What is unread is the row's own answer, never a count this module works out: a Reply row
 * carries the thread's read mark (`read_through`) and an Agent row the ids of its unread replies.
 * Nothing here marks anything read.
 */

import { useQuery, useQueryClient } from "@tanstack/react-query";
import { type ReactNode, useEffect, useRef, useState } from "react";
import { Link } from "react-router-dom";

import { api } from "../../api/client";
import { agentMessagesQuery, askThreadQuery, issueQuery, myRepliesKey } from "../../api/queries";
import type {
  Actor,
  Comment,
  Message,
  MessageRead,
  MyReplyAgentRow,
  MyReplyRow,
  MyReplyThreadRow,
} from "../../api/types";
import { LoadingSkeleton } from "../../components/LoadingSkeleton";
import { useSending } from "../../hooks/useSending";
import { compareTimestamps } from "../../lib/timestamps";
import {
  badgePrimary,
  borderDefault,
  dangerText,
  linkHoverText,
  linkText,
  quoteAccentBorder,
  quoteBodyText,
  surfaceMutedBg,
  textMutedOnCanvas,
  textMutedOnSurfaceMuted,
  textPrimaryOnSurfaceMuted,
} from "../../theme/classes";
import { AgentMessageComposer, agentSendKey } from "../agents/AgentMessageComposer";
import { agentReplyTo, newestAttempt } from "../agents/exchange-delivery";
import { useMarkShownRepliesRead } from "../agents/unread";
import type { ComposerOwner } from "../conversation/composer-model";
import { rootOf } from "../conversation/conversation-model";
import { MentionComposer } from "../conversation/MentionComposer";
import { useProjectArtifact } from "../document/useProjectArtifact";
import { AgentName } from "../refs/AgentName";
import { agentsQuery } from "../refs/agent-registry";
import { MarkdownBody } from "../refs/MarkdownBody";
import { Timestamp } from "../refs/Timestamp";
import { type DwellRef, useDwell } from "../shell/useDwell";
import { EarlierReplies } from "../threads/EarlierReplies";
import { threadLayout, unreadWrites } from "../threads/thread-layout";
import { useThreadOrder } from "../threads/thread-order";
import { type RepliesOwner, useMarkThreadRead } from "../threads/useMarkThreadRead";
import { AskCard } from "./AskCard";

/** One message of a thread, whatever route it was read from. */
interface ThreadTurn {
  readonly id: string;
  readonly author: Actor;
  readonly body: string;
  readonly created_at: string;
  /** Drawn under the body: an Agent reply the session took as a turn of its live conversation
   *  says so there. */
  readonly note?: ReactNode;
}

/** The text a thread in a document is anchored to, which a card and the pane's fall-back show in
 *  place of the document itself. */
function ThreadQuote({ thread }: { thread: MyReplyRow }): ReactNode {
  if (thread.kind === "agent") return null;
  const quote = thread.anchor?.quote;
  if (quote === undefined || quote === "") return null;
  return (
    <blockquote
      className={`mt-2 border-l-2 pl-2 text-sm ${quoteAccentBorder} ${quoteBodyText}`}
      data-inbox-thread-quote=""
    >
      {quote}
    </blockquote>
  );
}

function ThreadTurnView({
  first,
  turn,
  unread,
}: {
  /** Whether this is the first unread message of the thread, which the pane opens scrolled to. */
  first?: boolean;
  turn: ThreadTurn;
  unread: boolean;
}): ReactNode {
  return (
    <div
      className={`rounded-lg p-2 text-sm ${surfaceMutedBg}`}
      data-inbox-thread-first-unread={first === true ? "" : undefined}
      data-inbox-thread-turn={turn.id}
      data-inbox-thread-unread={unread ? "" : undefined}
    >
      <p className={`mb-1 flex flex-wrap items-center gap-x-2 text-xs ${textMutedOnSurfaceMuted}`}>
        <span>
          <AgentName actor={turn.author} inline /> · <Timestamp at={turn.created_at} />
        </span>
        {unread ? (
          <span
            className={`rounded-full px-1.5 py-0.5 text-[10px] font-semibold uppercase ${badgePrimary.bg} ${badgePrimary.text}`}
          >
            New
          </span>
        ) : null}
      </p>
      <div className={textPrimaryOnSurfaceMuted}>
        <MarkdownBody markdown={turn.body} />
      </div>
      {turn.note}
    </div>
  );
}

/**
 * The thread as it is drawn: the root, the replies in the reader's order with the oldest read
 * ones folded behind **Show N earlier replies**, and the composer at the end of that order, next
 * to the newest message. `threadLayout` is the only rule here; this draws what it answers.
 */
function ThreadTurns({
  composer,
  markNewest,
  openAtFirstUnread,
  replies,
  root,
  threadId,
  unread,
}: {
  composer: ReactNode;
  /** The read mark's watch, placed on the thread's newest message: once that message has been
   *  on screen long enough (`useDwell`), the thread is read. */
  markNewest: DwellRef;
  /** Whether the thread scrolls its first unread message into view when it opens, which the
   *  reading pane does under **Oldest first** and a card never does. */
  openAtFirstUnread: boolean;
  /** The thread's replies, oldest first. */
  replies: readonly ThreadTurn[];
  root: ThreadTurn | undefined;
  threadId: string;
  unread: ReadonlySet<string>;
}): ReactNode {
  const [order] = useThreadOrder();
  const [showEarlier, setShowEarlier] = useState(false);
  const firstUnread = useRef<HTMLDivElement>(null);
  const { composerFirst, folded, shown } = threadLayout({
    collapse: true,
    order,
    replies,
    unread,
  });
  // The thread opens where the reader left off: under Oldest first its first unread message is
  // at the top of the column, since everything above it has been read. Newest first already puts
  // the newest message first, so it opens where it stands.
  const openAt = openAtFirstUnread && order === "oldest" ? threadId : undefined;
  useEffect(() => {
    if (openAt === undefined) return;
    firstUnread.current?.scrollIntoView({ block: "start" });
  }, [openAt]);
  /** The first message of the thread, in drawn order, that the reader has not read. */
  const firstUnreadId = [...(root === undefined ? [] : [root]), ...shown].find((turn) =>
    unread.has(turn.id)
  )?.id;
  // The thread's newest message, whatever order it is drawn in: the one the read mark watches.
  // Every message is drawn and an unread one is never folded away, so it is always on the page.
  const newestId = (replies.at(-1) ?? root)?.id;
  const turnView = (turn: ThreadTurn): ReactNode => (
    <div key={turn.id} ref={turn.id === firstUnreadId ? firstUnread : undefined}>
      <div ref={turn.id === newestId ? markNewest : undefined}>
        <ThreadTurnView
          first={turn.id === firstUnreadId}
          turn={turn}
          unread={unread.has(turn.id)}
        />
      </div>
    </div>
  );
  // The fold and what it holds are one block, drawn furthest from the newest message: under the
  // root in Oldest first, at the foot of the thread in Newest first (`thread-layout.ts`).
  const fold = (
    <EarlierReplies
      count={folded.length}
      onToggle={() => setShowEarlier(!showEarlier)}
      open={showEarlier}
      replies={(id) => (
        <div className="space-y-2" id={id}>
          {folded.map(turnView)}
        </div>
      )}
    />
  );
  return (
    <div className="mt-2 space-y-2" data-inbox-thread={threadId}>
      {root === undefined ? null : turnView(root)}
      {composerFirst ? composer : fold}
      {shown.length === 0 ? null : <div className="space-y-2">{shown.map(turnView)}</div>}
      {composerFirst ? fold : composer}
    </div>
  );
}

/** Where a thread's reads and its composer are addressed: the issue it sits on, or the project
 *  document it was written in. */
type ThreadOwner = Extract<ComposerOwner, { kind: "issue" } | { kind: "artifact" }>;

/** The name the thread's reply is sent under: one per thread, so a reply still out or refused is
 *  shown by whichever surface mounts the composer again (`held-sends.ts`). */
function threadSendKey(threadId: string): readonly unknown[] {
  return ["inbox-thread-reply", threadId];
}

/**
 * The read mark a Reply row's thread carries: the dwell rule the thread's own pages already use
 * (`useMarkThreadRead`, `features/shell/useDwell.ts`) over the row's own newest write. The
 * returned ref goes on the newest message. Reading a thread takes it out of no list - it stays in
 * the Inbox until it is dismissed - so the mark is all this does.
 */
function useThreadRead(thread: MyReplyThreadRow, owner: RepliesOwner | undefined): DwellRef {
  return useMarkThreadRead({
    // A row the reader has already read needs no mark, and reads no unread set to decide it.
    enabled: thread.unread,
    newestShownAt: thread.reply.created_at,
    owner,
    threadId: thread.thread_id,
  }).ref;
}

/** The replies an Agent thread marks when nothing has dwelled yet. */
const NO_REPLIES: readonly Message[] = [];

/**
 * An Agent row's read mark. Its replies are read by id (`useMarkShownRepliesRead`), never by
 * moving the session's own read mark, so a reply in the live conversation or in another thread
 * of the same session stays unread. The watch is the Reply rows' dwell rule, on the thread's
 * newest message, and it marks the unread replies the row names among the ones drawn - every
 * one of them at or before that newest message, since the body draws the thread whole.
 */
function useAgentThreadRead(thread: MyReplyAgentRow, replies: readonly Message[]): DwellRef {
  const unreadIds = thread.unread_reply_ids;
  // Which set of unread replies has dwelled, as `useMarkThreadRead` keeps which reply has: a
  // reply that arrives while the thread is on screen is read only once it has dwelled too.
  const seen = `${thread.thread_id} ${unreadIds.join(" ")}`;
  const [read, setRead] = useState<string | undefined>(undefined);
  const ref = useDwell({
    enabled: thread.unread && unreadIds.length > 0,
    onDwell: () => setRead(seen),
    resetKey: seen,
  });
  useMarkShownRepliesRead(
    thread.session.id,
    read === seen ? replies.filter((reply) => unreadIds.includes(reply.id)) : NO_REPLIES
  );
  return ref;
}

/** A comment thread on an issue, or in a project document: every comment whose reply chain leads
 *  to the row's root. */
function CommentThreadBody({
  openAtFirstUnread,
  owner,
  thread,
}: {
  openAtFirstUnread: boolean;
  owner: ThreadOwner;
  thread: MyReplyThreadRow;
}): ReactNode {
  const queryClient = useQueryClient();
  const markNewest = useThreadRead(
    thread,
    owner.kind === "issue" ? { issue: owner.issueKey } : { artifact: owner.artifactId }
  );
  const issueKey = owner.kind === "issue" ? owner.issueKey : undefined;
  const issue = useQuery({ ...issueQuery(issueKey), enabled: issueKey !== undefined });
  // The reads the issue's Conversation and the document's margin already hold, under their own
  // keys: the same request, shared rather than made a second time.
  const commentsKey =
    owner.kind === "issue"
      ? ["comments", owner.issueKey]
      : ["artifact", owner.artifactId, "comments"];
  const comments = useQuery({
    queryKey: commentsKey,
    queryFn: () =>
      owner.kind === "issue"
        ? api.listComments(owner.issueKey)
        : api.listArtifactComments(owner.artifactId),
  });
  if (comments.isPending) return <LoadingSkeleton label="Opening the thread" />;
  if (comments.isError) {
    return <p className={`mt-2 text-sm ${dangerText}`}>Could not load this thread.</p>;
  }
  const byId = new Map(comments.data.map((comment) => [comment.id, comment]));
  const root = byId.get(thread.thread_id);
  const replies = comments.data
    .filter(
      (comment) => comment.id !== thread.thread_id && rootOf(byId, comment).id === thread.thread_id
    )
    .sort(
      (left, right) =>
        compareTimestamps(left.created_at, right.created_at) || left.id.localeCompare(right.id)
    )
    .map(commentTurn);
  const rootTurn = root === undefined ? undefined : commentTurn(root);
  return (
    <ThreadTurns
      composer={
        root === undefined ? null : (
          <MentionComposer
            closed={issue.data !== undefined && issue.data.closed_at !== null}
            frame="mt-2"
            inline
            kind="comment"
            mutationKey={threadSendKey(thread.thread_id)}
            onClose={noReturn}
            onSent={() => {
              void queryClient.invalidateQueries({ queryKey: commentsKey });
              void queryClient.invalidateQueries({ queryKey: myRepliesKey });
            }}
            owner={owner}
            replyTo={{ actor: root.author, excerpt: "", id: root.id, parentKind: "comment" }}
            standing
          />
        )
      }
      markNewest={markNewest}
      openAtFirstUnread={openAtFirstUnread}
      replies={replies}
      root={rootTurn}
      threadId={thread.thread_id}
      unread={unreadWrites(thread, [...(rootTurn === undefined ? [] : [rootTurn]), ...replies])}
    />
  );
}

function commentTurn(comment: Comment): ThreadTurn {
  return {
    author: comment.author,
    body: comment.body,
    created_at: comment.created_at,
    id: comment.id,
  };
}

function messageTurn(message: Message, note?: ReactNode): ThreadTurn {
  return {
    author: message.author,
    body: message.body,
    created_at: message.created_at,
    id: message.id,
    ...(note === undefined ? {} : { note }),
  };
}

/** An issue message conversation: the root message the row names and every reply under it, as the
 *  issue's Conversation reads them. */
function MessageThreadBody({
  issueKey,
  openAtFirstUnread,
  thread,
}: {
  issueKey: string;
  openAtFirstUnread: boolean;
  thread: MyReplyThreadRow;
}): ReactNode {
  const queryClient = useQueryClient();
  const markNewest = useThreadRead(thread, { issue: issueKey });
  const issue = useQuery(issueQuery(issueKey));
  const messagesKey = ["issue-messages", issueKey];
  const messages = useQuery({
    queryKey: messagesKey,
    queryFn: () => api.listIssueMessages(issueKey),
  });
  if (messages.isPending) return <LoadingSkeleton label="Opening the thread" />;
  if (messages.isError) {
    return <p className={`mt-2 text-sm ${dangerText}`}>Could not load this thread.</p>;
  }
  const read = messages.data.find((candidate) => candidate.message.id === thread.thread_id);
  const root = read?.message;
  const rootTurn = root === undefined ? undefined : messageTurn(root);
  const replies = (read?.replies ?? []).map((message) => messageTurn(message));
  // A reply into a targeted thread is delivered as the thread's newest attempt was, which is how
  // the issue's Conversation addresses one.
  const last = read === undefined ? undefined : newestAttempt([read.message, ...read.replies]);
  const target = root?.target;
  return (
    <ThreadTurns
      composer={
        root === undefined ? null : (
          <MentionComposer
            closed={issue.data !== undefined && issue.data.closed_at !== null}
            frame="mt-2"
            inline
            mutationKey={threadSendKey(thread.thread_id)}
            onClose={noReturn}
            onSent={() => {
              void queryClient.invalidateQueries({ queryKey: messagesKey });
              void queryClient.invalidateQueries({ queryKey: myRepliesKey });
            }}
            owner={{ issueKey, kind: "issue" }}
            replyTo={{
              actor: root.author,
              excerpt: "",
              id: root.id,
              parentKind: "message",
              ...(target === null || target === undefined
                ? {}
                : { thread: { delivery: last?.delivery ?? "steer", target } }),
            }}
            standing
          />
        )
      }
      markNewest={markNewest}
      openAtFirstUnread={openAtFirstUnread}
      replies={replies}
      root={rootTurn}
      threadId={thread.thread_id}
      unread={unreadWrites(thread, [...(rootTurn === undefined ? [] : [rootTurn]), ...replies])}
    />
  );
}

/** The thread of an ask: the ask's own card, which draws the question, its answer and its
 *  exchange, and answers or replies in place. The card draws the thread whole, so the mark
 *  watches the card itself rather than one message inside it: the newest write is in it, and
 *  the dwell rule reads a card taller than the screen as the screen filling with it. The card
 *  marks nothing itself, so which of the replies are unread is this row's own answer, passed
 *  down: without it the card would fold by count alone and hide replies this mark then reads. */
function AskThreadBody({
  artifactId,
  thread,
}: {
  artifactId: string | undefined;
  thread: MyReplyThreadRow;
}): ReactNode {
  const issueKey = thread.issue?.key;
  const markNewest = useThreadRead(
    thread,
    issueKey !== undefined
      ? { issue: issueKey }
      : artifactId === undefined
        ? undefined
        : { artifact: artifactId }
  );
  const read = useQuery(askThreadQuery(thread.thread_id));
  if (read.isPending) return <LoadingSkeleton label="Opening the thread" />;
  if (read.isError) {
    return <p className={`mt-2 text-sm ${dangerText}`}>Could not load this thread.</p>;
  }
  return (
    <div className="mt-2" data-inbox-thread={thread.thread_id} ref={markNewest}>
      <AskCard
        ask={read.data.ask}
        initialThread={read.data}
        owner={thread.document}
        thread="inline"
        unread={unreadWrites(thread, read.data.replies)}
        variant="full"
      />
    </div>
  );
}

/** What the live view says of a session its list no longer holds. */
const ENDED_SESSION_LINE = "Not in the live session list — it may have ended.";

/** What the live view says of a listed session that advertises no delivery mode. */
const NO_DELIVERY_LINE =
  "This session advertises no delivery mode, so a message sent here records a failed attempt. " +
  "A Claude Code session takes messages only when it was started with its Envoy channel.";

/** Said under a reply the session took as its user's own turn: the answer is streamed in the live
 *  view rather than stored under the reply. */
function AcceptedAsTurn({ sessionId }: { sessionId: string }): ReactNode {
  return (
    <p className={`mt-1 flex flex-wrap items-center gap-x-1 text-xs ${textMutedOnSurfaceMuted}`}>
      <span data-testid="agent-reply-accepted">Taken as a turn of the live conversation.</span>
      <Link className={`${linkText} ${linkHoverText}`} to={`/agents/${sessionId}/live`}>
        Live view
      </Link>
    </p>
  );
}

/**
 * A one-off message the reader sent an agent session, with every message under it: the reader's
 * own follow-ups included, and a reply the session took as a turn of its live conversation saying
 * so. The composer replies under the thread's newest message, in the mode the Agents page uses for
 * a reply in that thread (`exchangeDelivery`), so a reply sent here behaves as one sent there.
 */
function AgentThreadBody({
  openAtFirstUnread,
  thread,
}: {
  openAtFirstUnread: boolean;
  thread: MyReplyAgentRow;
}): ReactNode {
  const sessionId = thread.session.id;
  const messages = useQuery(agentMessagesQuery(sessionId));
  const live = useQuery(agentsQuery());
  const agent = live.data?.find((candidate) => candidate.session_id === sessionId);
  const sendKey = agentSendKey(sessionId);
  const hold = useSending(sendKey);
  // Read before the loading branches, because the mark below is a hook: the thread's own
  // conversation among the session's, which is undefined until the read lands.
  const read: MessageRead | undefined = messages.data?.find(
    (candidate) => candidate.message.id === thread.thread_id
  );
  const markNewest = useAgentThreadRead(thread, read?.replies ?? NO_REPLIES);
  if (messages.isPending) return <LoadingSkeleton label="Opening the conversation" />;
  if (messages.isError) {
    return <p className={`mt-2 text-sm ${dangerText}`}>Could not load this conversation.</p>;
  }
  const newest = read === undefined ? undefined : (read.replies.at(-1) ?? read.message);
  const turn = (message: Message): ThreadTurn =>
    messageTurn(
      message,
      message.deliveries.some((attempt) => attempt.accepted_as === "user_turn") ? (
        <AcceptedAsTurn sessionId={sessionId} />
      ) : undefined
    );
  const turns = read === undefined ? [] : [read.message, ...read.replies].map(turn);
  return (
    <>
      <ThreadTurns
        composer={
          agent === undefined || read === undefined || newest === undefined ? null : (
            <div data-inbox-agent-composer="">
              {agent.capabilities.length === 0 ? (
                <>
                  <p className={`mt-2 text-xs ${textMutedOnCanvas}`}>Takes no messages</p>
                  <p className={`mt-1 text-sm ${dangerText}`} data-testid="agent-takes-no-messages">
                    {NO_DELIVERY_LINE}
                  </p>
                </>
              ) : null}
              <AgentMessageComposer
                agent={agent}
                hold={hold}
                onCancelReply={noReturn}
                onClose={noReturn}
                replyTo={agentReplyTo(agent, read, newest)}
                sendKey={sendKey}
                standing
              />
            </div>
          )
        }
        markNewest={markNewest}
        openAtFirstUnread={openAtFirstUnread}
        replies={turns.slice(1)}
        root={turns[0]}
        threadId={thread.thread_id}
        unread={unreadWrites(thread, turns)}
      />
      {/* A session the live list no longer holds takes no message at all, so the thread shows
          what the live view shows and offers no composer, as the Agents page's "Replied, no
          longer connected" rows do. */}
      {agent === undefined && !live.isPending ? (
        <p className={`mt-2 text-xs ${textMutedOnCanvas}`} data-testid="agent-session-ended">
          {ENDED_SESSION_LINE}
        </p>
      ) : null}
    </>
  );
}

/** The composer has nothing to close here: the thread's reply box is the thread's, not a reply
 *  the reader opened and can leave. */
function noReturn(): void {}

export interface ThreadBodyProps {
  /** Whether the thread scrolls its first unread message into view when it opens: the reading
   *  pane under **Oldest first**, never a card. */
  openAtFirstUnread?: boolean;
  /** Whether the text the thread is anchored to is drawn above it, which a card and the pane's
   *  fall-back do in place of the document. */
  quote?: boolean;
  thread: MyReplyRow;
}

/** A Reply or an Agent row's thread, whichever shape it is. */
export function ThreadBody({
  openAtFirstUnread = false,
  quote = false,
  thread,
}: ThreadBodyProps): ReactNode {
  // An Agent row is on no issue and in no document, so it has neither of these; its own body
  // needs neither.
  const document = thread.kind === "agent" ? undefined : thread.document;
  const anchor = thread.kind === "agent" ? undefined : thread.anchor;
  // A thread in a project document is read under that document's id. An anchored root carries it;
  // an unanchored one is found by the document's own `project/slug` read, the one the document
  // page shares.
  const artifact = useProjectArtifact(
    document === undefined || anchor !== undefined
      ? undefined
      : { kind: "document", project: document.project, slug: document.slug }
  );
  const artifactId = anchor?.artifact_id ?? artifact.data?.id;
  const body = (): ReactNode => {
    if (thread.kind === "agent") {
      return <AgentThreadBody openAtFirstUnread={openAtFirstUnread} thread={thread} />;
    }
    if (thread.kind === "ask") {
      return <AskThreadBody artifactId={artifactId} thread={thread} />;
    }
    const issueKey = thread.issue?.key;
    if (thread.kind === "message") {
      return issueKey === undefined ? null : (
        <MessageThreadBody
          issueKey={issueKey}
          openAtFirstUnread={openAtFirstUnread}
          thread={thread}
        />
      );
    }
    if (issueKey !== undefined) {
      return (
        <CommentThreadBody
          openAtFirstUnread={openAtFirstUnread}
          owner={{ issueKey, kind: "issue" }}
          thread={thread}
        />
      );
    }
    if (document === undefined || artifactId === undefined) {
      return artifact.isPending ? <LoadingSkeleton label="Opening the thread" /> : null;
    }
    return (
      <CommentThreadBody
        openAtFirstUnread={openAtFirstUnread}
        owner={{ artifactId, kind: "artifact", project: document.project }}
        thread={thread}
      />
    );
  };
  return (
    <div
      className={`border-t pt-2 ${borderDefault}`}
      data-inbox-thread-body={thread.kind}
      data-testid={`inbox-thread-${thread.kind}`}
    >
      {quote ? <ThreadQuote thread={thread} /> : null}
      {body()}
    </div>
  );
}
