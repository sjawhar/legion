import type {
  Ask,
  AskAnswerWithTrees,
  AskEditWithTrees,
  AskFollower,
  AskKind,
  AskLastReply,
  AskReach,
  AskWithTrees,
  InboxDocument,
  IssueAskRow,
  OpenAskOwner,
} from "./dispatch-api-asks";
import type { CommentWithTrees } from "./dispatch-api-comments";
import type { Actor, AskUrgency, DisplayTree } from "./dispatch-api-core";
import type { ArtifactApproval } from "./dispatch-api-documents";
import type { Issue, IssuePriority } from "./dispatch-api-issues";
import type { AskTurn, SessionAnswerRelation } from "./dispatch-api-limits";

/**
 * What a person and a session each have waiting: the Inbox's open asks, the threads of an ask,
 * the answers a person wrote, the replies they are owed, and what Dispatch delivered to one
 * session.
 */

/** An ask's thread as a list row carries it: everything a card needs before its first targeted
 *  thread refresh. */
export interface AskThread extends AskReach {
  readonly replies: CommentWithTrees[];
  /** Every rewording of the question, oldest first; empty when never edited. */
  readonly edits: AskEditWithTrees[];
  /** Every answer, oldest first: empty on an Inbox row, which is an open ask, and the answers of
   *  an answered ask on `GET /api/v1/issues/{key}/asks?thread=1`. */
  readonly answers: AskAnswerWithTrees[];
  /** The sessions this ask's answer and replies reach directly, oldest first. */
  readonly followers: AskFollower[];
}

/** An open ask as returned by the human Inbox. */
export interface InboxRow extends AskWithTrees {
  /** The owning issue's coarse priority, or null for an unset or unassigned issue. */
  readonly priority: IssuePriority | null;
  /** When the viewer's own snooze on this row ends, or null when they have not snoozed it.
   *  The server surfaces it and never filters on it: the row is listed either way, and the
   *  Inbox folds it into `Later` only while this moment is still ahead. */
  readonly snoozed_until: string | null;
  /** The card's initial thread; `["ask-thread", id]` remains its targeted live refresh. */
  readonly thread: AskThread;
}

/** One ask of `GET /api/v1/issues/{key}/asks?thread=1`: the row above, plus the thread that
 *  renders the card whole without a fetch per ask. A reader asks for this shape by asking for
 *  the thread, so the thread is always there. */
export interface IssueAskThreadRow extends IssueAskRow {
  /** The card's initial thread; `["ask-thread", id]` remains its targeted live refresh. */
  readonly thread: AskThread;
}

export interface AskRead extends AskReach {
  readonly ask: AskWithTrees;
  readonly replies: CommentWithTrees[];
  /** Every rewording of the question, oldest first; empty when never edited. */
  readonly edits: AskEditWithTrees[];
  /** Every answer, oldest first; a restored document block does not repeat its answer here. */
  readonly answers: AskAnswerWithTrees[];
  /** The sessions this ask's answer and replies reach directly, oldest first. */
  readonly followers: AskFollower[];
}

export interface AskFollowersRead {
  readonly followers: AskFollower[];
}

export interface CommentRead {
  readonly comment: CommentWithTrees;
  readonly replies: CommentWithTrees[];
}

/** What `POST /api/v1/artifacts/{id}/approval-requests` answers: the document's open request (null
 *  when its latest version is already approved), the version it names, and the document's approval
 *  as the call left it. */
export interface ApprovalRequestResponse {
  readonly ask: Ask | null;
  readonly artifact_id: string;
  readonly version: number;
  readonly approval: ArtifactApproval;
}

export interface OpenAsk {
  readonly id: string;
  /** Relative Dispatch deep link to the ask itself: `/issues/KEY/asks/ID` for an issue ask (the
   *  item route, which selects an anchored ask on its document and focuses an unanchored one's
   *  Conversation turn), `/projects/PROJECT/documents/SLUG?ask=ID` for a document ask. */
  readonly ref: string;
  readonly question: string;
  readonly kind: AskKind;
  readonly urgency: AskUrgency;
  readonly created_at: string;
  readonly age_seconds: number;
  readonly priority: IssuePriority | null;
  readonly owner: OpenAskOwner;
  /** True when any human has replied, even if the agent spoke last. */
  readonly human_replied: boolean;
  readonly last_reply: AskLastReply | null;
  /** Whose reply would move the currently open ask forward. */
  readonly waiting_on: AskTurn;
}

export interface OpenAsksResponse {
  readonly session_id: string;
  readonly as_of: string;
  /** An ask was opened by this session at or after the supplied server baseline. */
  readonly opened_since: boolean;
  readonly count: number;
  readonly waiting_on_human: number;
  readonly waiting_on_agent: number;
  readonly asks: OpenAsk[];
}

/** One answer or ask reply written by the caller on `GET /api/v1/me/answers`. */
export interface MyAnswerRow {
  readonly kind: "answer" | "reply";
  readonly at: string;
  readonly ask_id: string;
  /** Relative Dispatch deep link to the ask itself, as on `OpenAsk.ref`. */
  readonly ref: string;
  readonly question: string;
  /** The display tree of `question`, absent unless the request sent `Dispatch-Trees: 1`. */
  readonly question_tree?: DisplayTree;
  readonly ask_kind: AskKind;
  readonly ask_state: Ask["state"];
  readonly edited_at: string | null;
  readonly owner: OpenAskOwner;
  readonly answer?: AskAnswerWithTrees;
  /** True when an answer row's answer is still the ask's current answer. */
  readonly current: boolean;
  readonly reply?: {
    readonly id: string;
    readonly body: string;
    /** The display tree of `body`, absent unless the request sent `Dispatch-Trees: 1`. */
    readonly body_tree?: DisplayTree;
  };
}

export interface MyAnswersResponse {
  readonly rows: MyAnswerRow[];
  readonly total: number;
  readonly limit: number;
  readonly offset: number;
}

/** The newest write someone else made in a thread on `GET /api/v1/me/replies`: a reply (a comment
 *  or a message), or the ask's answer, whose `id` is the ask's, whose `author` is the person who
 *  answered and whose `first_line` is the chosen options and the answer's text. */
export interface MyReplyReply {
  readonly id: string;
  readonly kind: "reply" | "answer";
  readonly author: Actor;
  /** The reply's or the answer's first non-empty line, at most 160 characters. */
  readonly first_line: string;
  /** The display tree of `first_line`, absent unless the request sent `Dispatch-Trees: 1`. */
  readonly first_line_tree?: DisplayTree;
  readonly created_at: string;
}

/** One thread the caller wrote in where someone else has written since the caller last read it:
 *  an ask's thread, a comment thread by its root, or an issue message thread by its root. */
export interface MyReplyRow {
  /** The ask's id, or the comment or message thread's root id: what
   *  `PUT /api/v1/me/threads/{id}/read` marks. */
  readonly thread_id: string;
  readonly kind: "ask" | "comment" | "message";
  /** Relative Dispatch deep link to the thread's root, never to the reply. */
  readonly ref: string;
  readonly issue?: Pick<Issue, "key" | "title">;
  readonly document?: InboxDocument;
  /** The ask's question, or the root's first line, at most 160 characters. */
  readonly subject: string;
  /** The display tree of `subject`, absent unless the request sent `Dispatch-Trees: 1`. */
  readonly subject_tree?: DisplayTree;
  readonly reply: MyReplyReply;
}

export interface MyRepliesResponse {
  readonly rows: MyReplyRow[];
  readonly total: number;
  readonly limit: number;
  readonly offset: number;
}

/** One thing a person wrote that Dispatch delivered to a session, on
 *  `GET /api/v1/agents/{session_id}/answers`: an ask's current answer, a comment at its current
 *  body, or a message. The list holds each `(kind, id)` once, whichever ways it reached the
 *  session. Mirrors `model.SessionAnswer`. */
export interface SessionAnswer {
  /** `answer` is an ask's current answer, `comment` a comment (a reply in an ask's thread
   *  included), `message` a message. */
  readonly kind: "answer" | "comment" | "message";
  /** What the item is to the session. */
  readonly relation: SessionAnswerRelation;
  /** The ask's id for an answer, else the comment's or the message's id. */
  readonly id: string;
  /** When the person wrote it (RFC 3339, UTC): the current answer's time, or the comment's or the
   *  message's creation. The window and the order both read it. */
  readonly at: string;
  /** When a comment's body was last edited; absent on a comment never edited, and on an answer or
   *  a message. */
  readonly edited_at?: string;
  /** The person who wrote it: the list carries nothing an agent or the system wrote. */
  readonly author: Extract<Actor, { readonly kind: "user" }>;
  /** The person's words in full, never cut: the answer's text (`""` when the answer chose options
   *  only), the comment's current body, or the message's body. */
  readonly text: string;
  /** An answer's chosen option labels, in the order the person chose them; absent when it chose
   *  none, and on a comment or a message. */
  readonly selected?: string[];
  /** `true` on an answer when the ask has more than one answer, so this one replaced an earlier
   *  one; absent otherwise. */
  readonly replaced?: true;
  /** The ask the item answers or replies in, with the session's whole question (a reader cuts
   *  it): present on an answer and on a reply in an ask's thread, absent otherwise. */
  readonly ask?: { readonly id: string; readonly question: string };
  /** The issue (key and title) or the project document holding the item; null for a direct
   *  message, which is on no issue. */
  readonly owner: OpenAskOwner | null;
  /** The item's `dispatch://` address (`dispatch://KEY/ask/ID`, `dispatch://KEY/comment/ID`,
   *  `dispatch://KEY/message/ID`, or `dispatch://PROJECT/artifact/SLUG/ask/ID` and `/comment/ID`
   *  on a project document); null for a direct message, which has no address. */
  readonly ref: string | null;
  /** Relative Dispatch deep link to the item, as `DISPATCH_HREF_REFERENCES` lists the shapes, or
   *  `/agents/SESSION/live` for a direct message. */
  readonly href: string;
}

/** `GET /api/v1/agents/{session_id}/answers`: what people wrote to one session since `since`,
 *  newest first. Mirrors `model.SessionAnswersResponse`. */
export interface SessionAnswersResponse {
  readonly session_id: string;
  /** The window's start the server applied (RFC 3339, UTC): the request's `since`, or 72 hours
   *  before the read when it named none. */
  readonly since: string;
  /** How many items the window holds, `max_chars` notwithstanding. */
  readonly total: number;
  /** Newest first by `at`, then by `id`. With `max_chars`, the list stops after the item whose
   *  `text` takes the running sum of text lengths past it, so it can be shorter than `total`. */
  readonly items: SessionAnswer[];
}
