import {
  answerFor,
  answerTargets,
  type ResolvedAnswer,
  type ThreadAnswer,
  type ThreadAsk,
} from "../asks";
import { escapeHtml, renderMarkdownLite, timeAgo } from "../html";
import {
  type ParsedAskMarker,
  parseAskMarker,
  parseThreadMarker,
  parseUrgencyMarker,
  stripMarker,
} from "../markers";
import type { Comment, Issue, Origin, Thread, Urgency } from "../types";
import { type AskFormInput, askHeader, renderAskForm } from "./ask-form";
import { type ReplyFormInput, renderReplyForm } from "./reply-form";
import { renderUrgencyControls } from "./urgency-controls";

export interface ThreadWriteState {
  replyPending: boolean;
  replyError?: string;
  /** askId of the answer being posted. */
  askPending?: string;
  askError?: { askId: string; message: string };
  urgencyPending: boolean;
  urgencyError?: string;
  closePending: boolean;
  closeError?: string;
  addressedPending: boolean;
  addressedError?: string;
}

export interface ThreadDetailInput {
  issue: Issue;
  urgency: Urgency;
  comments: Comment[];
  asks: ThreadAsk[];
  answers: ThreadAnswer[];
  openAsks: ThreadAsk[];
  subThreads: Thread[];
  repo: string;
  addressed: boolean;
  writeState?: ThreadWriteState;
}

export const EMPTY_WRITE_STATE: ThreadWriteState = {
  replyPending: false,
  urgencyPending: false,
  closePending: false,
  addressedPending: false,
};

function renderAnswerValues(values: readonly string[]): string {
  if (values.length === 0) return `<em class="answer-empty">no answer</em>`;
  return values.map((value) => `<span class="answer-pill">${escapeHtml(value)}</span>`).join(" ");
}

// Beneath a question: its answer, or the fact that it is still waiting
// (linking down to its form), or that the thread closed without one.
function renderAskAnswer(
  ask: ThreadAsk,
  resolved: ResolvedAnswer | null,
  issueOpen: boolean
): string {
  if (resolved) {
    return `<div class="ask-answer">${renderAnswerValues(resolved.values)}<span class="ask-answer-meta"> — ${escapeHtml(resolved.answer.authorLogin)} · ${escapeHtml(timeAgo(resolved.answer.createdAt))}</span></div>`;
  }
  if (issueOpen) {
    return `<a class="ask-waiting" href="#ask-form-${escapeHtml(ask.askId)}">waiting for an answer — answer below ↓</a>`;
  }
  return `<em class="ask-waiting">never answered</em>`;
}

// A question and, directly beneath it, its answer. Used for body asks and for
// the asks of every follow-up turn.
function renderAskHistory(
  ask: ThreadAsk,
  resolved: ResolvedAnswer | null,
  issueOpen: boolean
): string {
  const header = escapeHtml(askHeader(ask.question, ask.index));
  const prompt = escapeHtml(ask.question.question || "");
  const options = (ask.question.options ?? [])
    .map((option) => `<span class="ask-history-option">${escapeHtml(option.label)}</span>`)
    .join(" ");
  return `<div class="ask-history" data-ask-id="${escapeHtml(ask.askId)}">
    <div class="ask-history-question"><strong class="ask-history-header">${header}</strong>${prompt ? `<span class="ask-history-prompt">${prompt}</span>` : ""}${options ? `<span class="ask-history-options">${options}</span>` : ""}</div>
    ${renderAskAnswer(ask, resolved, issueOpen)}
  </div>`;
}

// The session that asked, as the header origin line and the turn cards both
// show it; these two own the class names the stylesheet targets.
function sessionTitleSpan(origin: Origin): string {
  if (!origin.sessionTitle) return "";
  return `<span class="origin-session-title">${escapeHtml(origin.sessionTitle)}</span>`;
}

function sessionIdCode(origin: Origin): string {
  if (!origin.sessionId) return "";
  return `<code class="origin-session-id">${escapeHtml(origin.sessionId)}</code>`;
}

function renderCompactOrigin(origin: Origin | undefined): string {
  if (!origin) return "";
  const parts = [
    sessionTitleSpan(origin),
    sessionIdCode(origin),
    origin.tmux ? `tmux ${escapeHtml(origin.tmux)}` : "",
  ].filter(Boolean);
  return parts.length ? `<span class="turn-origin">${parts.join(" · ")}</span>` : "";
}

function renderTurnCard(
  comment: Comment,
  marker: ParsedAskMarker,
  input: ThreadDetailInput
): string {
  const asks = input.asks.filter(
    (ask) => ask.source.kind === "comment" && ask.source.commentId === comment.id
  );
  const history = asks
    .map((ask) =>
      renderAskHistory(ask, answerFor(ask, input.answers), input.issue.state === "OPEN")
    )
    .join("");
  return `<article class="comment turn-card" id="turn-${comment.id}" data-comment-id="${comment.id}">
    <header><strong>${escapeHtml(comment.authorLogin)}</strong><span class="comment-tag">follow-up</span><span>${escapeHtml(timeAgo(comment.createdAt))}</span>${renderCompactOrigin(marker.origin)}</header>
    <div class="comment-body turn-body">${renderMarkdownLite(stripMarker(comment.body))}</div>
    ${history}
  </article>`;
}

// An answer comment that is not the one shown beneath a question: it named
// no ask on the thread, or a different answer to the same ask is the one the
// question shows. It stays at its own position so nothing on GitHub is hidden.
function renderStandaloneAnswer(comment: Comment, answer: ThreadAnswer, tag: string): string {
  return `<article class="comment comment-answer" data-comment-id="${comment.id}">
    <header><strong>${escapeHtml(comment.authorLogin)}</strong><span>${escapeHtml(timeAgo(comment.createdAt))}</span><span class="comment-tag">${tag}</span></header>
    <div class="comment-body">${renderAnswerValues(answer.answers.flat())}</div>
  </article>`;
}

// The urgency and ask markers are read here; answers were classified when
// input.answers was collected from these same comments, so an answer comment
// is the one input.answers names.
function renderComment(comment: Comment, input: ThreadDetailInput): string {
  const urgency = parseUrgencyMarker(comment.body);
  if (urgency) {
    return `<div class="activity-row" data-comment-id="${comment.id}">
      <span class="urgency-dot urgency-${urgency}"></span>
      urgency set to <strong>${urgency}</strong> by ${escapeHtml(comment.authorLogin)} · ${escapeHtml(timeAgo(comment.createdAt))}
    </div>`;
  }
  const ask = parseAskMarker(comment.body);
  if (ask) return renderTurnCard(comment, ask, input);
  const answer = input.answers.find((candidate) => candidate.commentId === comment.id);
  if (answer) {
    // The answer a question shows renders beneath it (renderAskHistory) and
    // nowhere else. Any other answer stays at its own position.
    const targets = answerTargets(answer, input.asks);
    if (targets.length === 0) {
      return renderStandaloneAnswer(
        comment,
        answer,
        "answer to a question no longer on this thread"
      );
    }
    const shown = targets.some(
      (ask) => answerFor(ask, input.answers)?.answer.commentId === comment.id
    );
    return shown ? "" : renderStandaloneAnswer(comment, answer, "later answer");
  }
  return `<article class="comment" data-comment-id="${comment.id}">
    <header><strong>${escapeHtml(comment.authorLogin)}</strong><span>${escapeHtml(timeAgo(comment.createdAt))}</span></header>
    <div class="comment-body">${renderMarkdownLite(comment.body)}</div>
  </article>`;
}

export function renderSubThreads(subThreads: Thread[]): string {
  if (subThreads.length === 0) return "";
  const rows = subThreads
    .map(
      (thread) =>
        `<button type="button" class="sub-thread-row" data-thread-repo="${escapeHtml(thread.repo)}" data-thread-number="${thread.number}">
        <span class="sub-thread-title">${escapeHtml(thread.title)}</span>
        <span class="sub-thread-meta">
          <span class="urgency-dot urgency-${thread.urgency}"></span>
          <span class="thread-number">#${thread.number}</span>
        </span>
      </button>`
    )
    .join("");
  return `<h3>Sub-threads</h3><div class="sub-thread-list">${rows}</div>`;
}

function statusText(issue: Issue): string {
  if (issue.state !== "CLOSED") return "open";
  if (issue.stateReason === "not_planned") return "cancelled";
  if (issue.stateReason === "completed") return "resolved";
  return issue.stateReason?.toLowerCase() || "resolved";
}

function renderCloseActions(
  issue: Issue,
  addressed: boolean,
  writeState: ThreadWriteState
): string {
  if (issue.state === "CLOSED") return "";
  const closeDisabled = writeState.closePending ? "disabled" : "";
  const addressedDisabled = writeState.addressedPending ? "disabled" : "";
  const addressedButton = addressed
    ? `<button type="button" class="btn-secondary" data-action="unmark-addressed" ${addressedDisabled}>Bring back</button>`
    : `<button type="button" class="btn-secondary" data-action="mark-addressed" ${addressedDisabled}>Mark addressed</button>`;
  return `<div class="close-actions">
    ${addressedButton}
    <div class="resolve-split">
      <button type="button" class="btn-primary resolve-main" data-action="close" data-state-reason="completed" ${closeDisabled}>Resolve thread</button>
      <details class="resolve-menu-wrap">
        <summary class="btn-primary resolve-toggle" aria-label="More close options">▾</summary>
        <div class="resolve-menu" role="menu">
          <button type="button" class="resolve-menu-item" data-action="close" data-state-reason="not_planned" ${closeDisabled}>Close as not planned</button>
        </div>
      </details>
    </div>
    ${writeState.closeError ? `<span class="form-error">${escapeHtml(writeState.closeError)}</span>` : ""}
    ${writeState.addressedError ? `<span class="form-error">${escapeHtml(writeState.addressedError)}</span>` : ""}
  </div>`;
}

// The copy buttons turn marker values into pasteable text. The marker is
// issue-body content, so any collaborator on the repo can write it; only a
// literal tmux pane id (`%N`) becomes a command, and the target is still
// shown as text otherwise. `switch-client -t <pane>` moves the human's own
// client to that pane's session, window, and pane, which is what a
// `session:window.pane` name cannot do reliably inside a tmux session group.
const TMUX_PANE_ID = /^%\d+$/;

function renderSessionIdentity(origin: Origin): string {
  if (!origin.sessionTitle && !origin.sessionId) return "";
  const id = origin.sessionId
    ? `${sessionIdCode(origin)}<button type="button" class="origin-copy" data-action="copy-session-id" data-copy-text="${escapeHtml(origin.sessionId)}" title="Copy session id" aria-label="Copy session id">⧉</button>`
    : "";
  return `<span class="origin-session">${sessionTitleSpan(origin)}${id}</span>`;
}

function renderOriginLine(origin: Origin | undefined): string {
  if (!origin) return "";
  const where: string[] = [];
  if (origin.host && origin.machine) {
    where.push(`From ${escapeHtml(origin.host)} on ${escapeHtml(origin.machine)}`);
  } else if (origin.host) {
    where.push(`From ${escapeHtml(origin.host)}`);
  } else if (origin.machine) {
    where.push(`on ${escapeHtml(origin.machine)}`);
  }
  if (origin.cwd) where.push(escapeHtml(origin.cwd));
  if (origin.tmux) where.push(`tmux ${escapeHtml(origin.tmux)}`);
  const session = renderSessionIdentity(origin);
  if (where.length === 0 && !session) return "";
  const tmuxCopy =
    origin.pane && TMUX_PANE_ID.test(origin.pane)
      ? `<button type="button" class="origin-copy" data-action="copy-origin" data-copy-text="${escapeHtml(`tmux switch-client -t ${origin.pane}`)}" title="Copy tmux command" aria-label="Copy tmux command">⧉</button>`
      : "";
  return `<p class="origin-line">${session}<span class="origin-text">${where.join(" · ")}</span>${tmuxCopy}</p>`;
}

export function renderDetailHeader(input: ThreadDetailInput): string {
  const { issue, urgency, repo, addressed } = input;
  const writeState = input.writeState ?? EMPTY_WRITE_STATE;
  const meta = parseThreadMarker(issue.body);
  const status = statusText(issue);
  return `<div class="detail-header-row">
      <div class="detail-identity">
        <a class="thread-number-link" href="https://github.com/${escapeHtml(repo)}/issues/${issue.number}" target="_blank" rel="noreferrer">#${issue.number}</a>
        <span class="badge state-badge state-${status}">${escapeHtml(status)}</span>
        ${
          issue.state === "OPEN"
            ? renderUrgencyControls({
                urgency,
                pending: writeState.urgencyPending,
                error: writeState.urgencyError,
              })
            : `<span class="badge urgency-badge urgency-badge-${urgency}"><span class="urgency-dot urgency-${urgency}"></span>${urgency}</span>`
        }
      </div>
      ${renderCloseActions(issue, addressed, writeState)}
    </div>
    <h1>${escapeHtml(issue.title)}</h1>
    ${renderOriginLine(meta?.origin)}
    <p class="detail-subtitle">Opened by ${escapeHtml(issue.authorLogin)} · ${escapeHtml(timeAgo(issue.createdAt))}</p>`;
}

export function renderOpeningBody(input: ThreadDetailInput): string {
  return renderMarkdownLite(stripMarker(input.issue.body));
}

export function renderOpeningAsks(input: ThreadDetailInput): string {
  const bodyAsks = input.asks.filter((ask) => ask.source.kind === "body");
  if (bodyAsks.length === 0) return "";
  return `<h2>Question${bodyAsks.length > 1 ? "s" : ""}</h2>${bodyAsks
    .map((ask) =>
      renderAskHistory(ask, answerFor(ask, input.answers), input.issue.state === "OPEN")
    )
    .join("")}`;
}

export function renderConversation(input: ThreadDetailInput): string {
  // Answers shown beneath their questions render "" here on purpose, so an
  // empty join is not an empty conversation.
  if (input.comments.length === 0) return `<div class="empty-state">No comments yet.</div>`;
  return input.comments.map((comment) => renderComment(comment, input)).join("");
}

/**
 * The asks with a form in the pane: every open ask, plus the ask whose answer
 * is still posting. That ask reads as answered — its optimistic comment is on
 * the thread — but its form stays, disabled, so a failed post gives it back
 * with the human's choice intact; it goes once the real comment has replaced
 * the placeholder. List order is the asks' order.
 */
export function askFormAsks(input: ThreadDetailInput): ThreadAsk[] {
  if (input.issue.state !== "OPEN") return [];
  const pending = (input.writeState ?? EMPTY_WRITE_STATE).askPending;
  return input.asks.filter((ask) => ask.askId === pending || input.openAsks.includes(ask));
}

/** One ask's form input: when it was asked, and this ask's slice of the write state. */
export function askFormInput(ask: ThreadAsk, input: ThreadDetailInput): AskFormInput {
  const writeState = input.writeState ?? EMPTY_WRITE_STATE;
  const source = ask.source;
  return {
    ask,
    pending: writeState.askPending === ask.askId,
    error: writeState.askError?.askId === ask.askId ? writeState.askError.message : undefined,
    askedAt:
      source.kind === "comment"
        ? input.comments.find((comment) => comment.id === source.commentId)?.createdAt
        : undefined,
  };
}

/** The reply form's slice of the write state. */
export function replyFormInput(input: ThreadDetailInput): ReplyFormInput {
  const writeState = input.writeState ?? EMPTY_WRITE_STATE;
  return { pending: writeState.replyPending, error: writeState.replyError };
}

export function renderAskForms(input: ThreadDetailInput): string {
  return askFormAsks(input)
    .map((ask) => renderAskForm(askFormInput(ask, input)))
    .join("");
}

/** The patchable regions of the detail pane, by element id. The forms are not regions: they are reconciled in place. */
export type DetailRegionId =
  | "detail-header"
  | "detail-opening"
  | "detail-opening-asks"
  | "detail-subthreads"
  | "detail-conversation";
export type DetailRegions = Readonly<Record<DetailRegionId, string>>;

export function renderDetailRegions(input: ThreadDetailInput): DetailRegions {
  return {
    "detail-header": renderDetailHeader(input),
    "detail-opening": renderOpeningBody(input),
    "detail-opening-asks": renderOpeningAsks(input),
    "detail-subthreads": renderSubThreads(input.subThreads),
    "detail-conversation": renderConversation(input),
  };
}

/** The whole pane. A caller that already rendered the regions (to remember what it painted) passes them in. */
export function renderThreadDetail(
  input: ThreadDetailInput | null,
  regions: DetailRegions | undefined = input ? renderDetailRegions(input) : undefined
): string {
  if (!input || !regions) {
    return `<main class="dispatch-detail empty-detail"><p>Select a thread to read the conversation.</p></main>`;
  }
  return `<main class="dispatch-detail" data-thread-number="${input.issue.number}">
    <header id="detail-header" class="detail-header">${regions["detail-header"]}</header>
    <section id="detail-opening" class="opening-body">${regions["detail-opening"]}</section>
    <section id="detail-opening-asks" class="ask-context" aria-label="Questions">${regions["detail-opening-asks"]}</section>
    <section id="detail-subthreads" class="sub-threads" aria-label="Sub-threads">${regions["detail-subthreads"]}</section>
    <section id="detail-conversation" class="conversation" aria-label="Conversation">${regions["detail-conversation"]}</section>
    <section id="detail-ask-forms" class="ask-forms" aria-label="Open questions">${renderAskForms(input)}</section>
    ${input.issue.state === "OPEN" ? renderReplyForm(replyFormInput(input)) : ""}
  </main>`;
}
