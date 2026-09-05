import {
  answerFor,
  answerTargets,
  type ResolvedAnswer,
  type ThreadAnswer,
  type ThreadAsk,
} from "../asks";
import { escapeHtml, renderMarkdownLite, timeAgo } from "../html";
import {
  parseAnswerMarker,
  parseAskMarker,
  parseThreadMarker,
  parseUrgencyMarker,
  stripMarker,
} from "../markers";
import type { Comment, Issue, Origin, Thread, Urgency } from "../types";
import { askHeader, renderAskForm } from "./ask-form";
import { renderReplyForm } from "./reply-form";
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

// A question and, directly beneath it, its answer (or the fact that it is
// still waiting, linking down to its form). Used for body asks and for the
// asks of every follow-up turn.
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
  const answer = resolved
    ? `<div class="ask-answer">${renderAnswerValues(resolved.values)}<span class="ask-answer-meta"> — ${escapeHtml(resolved.answer.authorLogin)} · ${escapeHtml(timeAgo(resolved.answer.createdAt))}</span></div>`
    : issueOpen
      ? `<a class="ask-waiting" href="#ask-form-${escapeHtml(ask.askId)}">waiting for an answer — answer below ↓</a>`
      : `<em class="ask-waiting">never answered</em>`;
  return `<div class="ask-history" data-ask-id="${escapeHtml(ask.askId)}">
    <div class="ask-history-question"><strong class="ask-history-header">${header}</strong>${prompt ? `<span class="ask-history-prompt">${prompt}</span>` : ""}${options ? `<span class="ask-history-options">${options}</span>` : ""}</div>
    ${answer}
  </div>`;
}

function renderCompactOrigin(origin: Origin | undefined): string {
  if (!origin) return "";
  const parts: string[] = [];
  if (origin.sessionTitle) {
    parts.push(`<span class="origin-session-title">${escapeHtml(origin.sessionTitle)}</span>`);
  }
  if (origin.sessionId) {
    parts.push(`<code class="origin-session-id">${escapeHtml(origin.sessionId)}</code>`);
  }
  if (origin.tmux) parts.push(`tmux ${escapeHtml(origin.tmux)}`);
  return parts.length ? `<span class="turn-origin">${parts.join(" · ")}</span>` : "";
}

function renderTurnCard(comment: Comment, input: ThreadDetailInput): string {
  const marker = parseAskMarker(comment.body);
  const asks = input.asks.filter(
    (ask) => ask.source.kind === "comment" && ask.source.commentId === comment.id
  );
  const history = asks
    .map((ask) =>
      renderAskHistory(ask, answerFor(ask, input.answers), input.issue.state === "OPEN")
    )
    .join("");
  return `<article class="comment turn-card" id="turn-${comment.id}" data-comment-id="${comment.id}">
    <header><strong>${escapeHtml(comment.authorLogin)}</strong><span class="comment-tag">follow-up</span><span>${escapeHtml(timeAgo(comment.createdAt))}</span>${renderCompactOrigin(marker?.origin)}</header>
    <div class="comment-body turn-body">${renderMarkdownLite(stripMarker(comment.body))}</div>
    ${history}
  </article>`;
}

function renderOrphanAnswer(comment: Comment, answer: ThreadAnswer): string {
  return `<article class="comment comment-answer" data-comment-id="${comment.id}">
    <header><strong>${escapeHtml(comment.authorLogin)}</strong><span>${escapeHtml(timeAgo(comment.createdAt))}</span><span class="comment-tag">answer to a question no longer on this thread</span></header>
    <div class="comment-body">${renderAnswerValues(answer.answers.flat())}</div>
  </article>`;
}

function renderComment(comment: Comment, input: ThreadDetailInput): string {
  const urgency = parseUrgencyMarker(comment.body);
  if (urgency) {
    return `<div class="activity-row" data-comment-id="${comment.id}">
      <span class="urgency-dot urgency-${urgency}"></span>
      urgency set to <strong>${urgency}</strong> by ${escapeHtml(comment.authorLogin)} · ${escapeHtml(timeAgo(comment.createdAt))}
    </div>`;
  }
  if (parseAskMarker(comment.body)) return renderTurnCard(comment, input);
  if (parseAnswerMarker(comment.body)) {
    const answer = input.answers.find((candidate) => candidate.commentId === comment.id);
    if (!answer) return "";
    // Answers render beneath the question they settle (renderAskHistory); only
    // an answer that names no ask on the thread stays at its own position.
    return answerTargets(answer, input.asks).length > 0 ? "" : renderOrphanAnswer(comment, answer);
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
  const title = origin.sessionTitle
    ? `<span class="origin-session-title">${escapeHtml(origin.sessionTitle)}</span>`
    : "";
  const id = origin.sessionId
    ? `<code class="origin-session-id">${escapeHtml(origin.sessionId)}</code><button type="button" class="origin-copy" data-action="copy-session-id" data-copy-text="${escapeHtml(origin.sessionId)}" title="Copy session id" aria-label="Copy session id">⧉</button>`
    : "";
  return `<span class="origin-session">${title}${id}</span>`;
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
  const rendered = input.comments.map((comment) => renderComment(comment, input)).join("");
  return rendered || `<div class="empty-state">No comments yet.</div>`;
}

export function renderAskForms(input: ThreadDetailInput): string {
  if (input.issue.state !== "OPEN") return "";
  const writeState = input.writeState ?? EMPTY_WRITE_STATE;
  return input.openAsks
    .map((ask) => {
      const source = ask.source;
      return renderAskForm({
        ask,
        pending: writeState.askPending === ask.askId,
        error: writeState.askError?.askId === ask.askId ? writeState.askError.message : undefined,
        askedAt:
          source.kind === "comment"
            ? input.comments.find((comment) => comment.id === source.commentId)?.createdAt
            : undefined,
      });
    })
    .join("");
}

export function renderThreadDetail(input: ThreadDetailInput | null): string {
  if (!input) {
    return `<main class="dispatch-detail empty-detail"><p>Select a thread to read the conversation.</p></main>`;
  }
  const writeState = input.writeState ?? EMPTY_WRITE_STATE;
  return `<main class="dispatch-detail" data-thread-number="${input.issue.number}">
    <header id="detail-header" class="detail-header">${renderDetailHeader(input)}</header>
    <section id="detail-opening" class="opening-body">${renderOpeningBody(input)}</section>
    <section id="detail-opening-asks" class="ask-context" aria-label="Questions">${renderOpeningAsks(input)}</section>
    <section id="detail-subthreads" class="sub-threads" aria-label="Sub-threads">${renderSubThreads(input.subThreads)}</section>
    <section id="detail-conversation" class="conversation" aria-label="Conversation">${renderConversation(input)}</section>
    <section id="detail-ask-forms" class="ask-forms" aria-label="Open questions">${renderAskForms(input)}</section>
    ${input.issue.state === "OPEN" ? renderReplyForm({ pending: writeState.replyPending, error: writeState.replyError }) : ""}
  </main>`;
}
