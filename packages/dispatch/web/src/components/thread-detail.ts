import type { QuestionAnswer, QuestionInfo } from "@opencode-ai/sdk/v2";
import { escapeHtml, renderMarkdownLite, timeAgo } from "../html";
import {
  parseAnswerMarker,
  parseMetaMarker,
  parseUrgencyMarker,
  stripMetaMarker,
} from "../markers";
import type { Comment, Issue, Thread, Urgency } from "../types";
import { renderAskContext, renderAskForm } from "./ask-form";
import { renderReplyForm } from "./reply-form";
import { renderUrgencyControls } from "./urgency-controls";

export interface ThreadWriteState {
  replyPending: boolean;
  replyError?: string;
  askPending: boolean;
  askError?: string;
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
  subThreads: Thread[];
  repo: string;
  addressed: boolean;
  writeState?: ThreadWriteState;
}

const EMPTY_WRITE_STATE: ThreadWriteState = {
  replyPending: false,
  askPending: false,
  urgencyPending: false,
  closePending: false,
  addressedPending: false,
};

function renderComment(comment: Comment, questions: QuestionInfo[]): string {
  const urgency = parseUrgencyMarker(comment.body);
  if (urgency) {
    return `<div class="activity-row" data-comment-id="${comment.id}">
      <span class="urgency-dot urgency-${urgency}"></span>
      urgency set to <strong>${urgency}</strong> by ${escapeHtml(comment.authorLogin)} · ${escapeHtml(timeAgo(comment.createdAt))}
    </div>`;
  }
  const answer = parseAnswerMarker(comment.body);
  if (answer) {
    return renderAnswerComment(comment, answer.answers, questions);
  }
  return `<article class="comment" data-comment-id="${comment.id}">
    <header><strong>${escapeHtml(comment.authorLogin)}</strong><span>${escapeHtml(
      timeAgo(comment.createdAt)
    )}</span></header>
    <div class="comment-body">${renderMarkdownLite(comment.body)}</div>
  </article>`;
}

function renderAnswerComment(
  comment: Comment,
  answers: QuestionAnswer[],
  questions: QuestionInfo[]
): string {
  const items = questions
    .map((question, index) => {
      const header = escapeHtml(question.header || `Question ${index + 1}`);
      const prompt = escapeHtml(question.question || "");
      const values = (answers[index] ?? []).map((v) => escapeHtml(v));
      const answerHTML =
        values.length === 0
          ? `<em class="answer-empty">no answer</em>`
          : values.map((v) => `<span class="answer-pill">${v}</span>`).join(" ");
      return `<dl class="answer-qa">
        <dt><span class="answer-q-header">${header}</span>${prompt ? `<span class="answer-q-prompt">${prompt}</span>` : ""}</dt>
        <dd>${answerHTML}</dd>
      </dl>`;
    })
    .join("");
  return `<article class="comment comment-answer" data-comment-id="${comment.id}">
    <header><strong>${escapeHtml(comment.authorLogin)}</strong><span>${escapeHtml(timeAgo(comment.createdAt))}</span><span class="comment-tag">answer</span></header>
    ${items || `<div class="comment-body">${renderMarkdownLite(stripAnswerMarker(comment.body))}</div>`}
  </article>`;
}

function stripAnswerMarker(body: string): string {
  return body.replace(/^<!-- dispatch:answer [^>]+ -->\n{0,2}/, "").trim();
}

function renderSubThreads(subThreads: Thread[]): string {
  if (subThreads.length === 0) return "";
  const rows = subThreads
    .map(
      (
        thread
      ) => `<button type="button" class="sub-thread-row" data-thread-number="${thread.number}">
        <span class="sub-thread-title">${escapeHtml(thread.title)}</span>
        <span class="sub-thread-meta">
          <span class="urgency-dot urgency-${thread.urgency}"></span>
          <span class="thread-number">#${thread.number}</span>
        </span>
      </button>`
    )
    .join("");
  return `<section class="sub-threads" aria-label="Sub-threads">
    <h3>Sub-threads</h3>
    <div class="sub-thread-list">${rows}</div>
  </section>`;
}

function statusText(issue: Issue): string {
  if (issue.state !== "CLOSED") return "open";
  if (issue.stateReason === "not_planned") return "cancelled";
  if (issue.stateReason === "completed") return "resolved";
  return issue.stateReason?.toLowerCase() || "resolved";
}

function hasAnswer(comments: Comment[], threadNumber: number): boolean {
  return comments.some((comment) => parseAnswerMarker(comment.body)?.forThread === threadNumber);
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

export function renderThreadDetail(input: ThreadDetailInput | null): string {
  if (!input) {
    return `<main class="dispatch-detail empty-detail"><p>Select a thread to read the conversation.</p></main>`;
  }
  const { issue, urgency, comments, subThreads, repo, addressed } = input;
  const writeState = input.writeState ?? EMPTY_WRITE_STATE;
  const meta = parseMetaMarker(issue.body);
  const ask = meta?.ask;
  const shouldRenderAsk =
    issue.state === "OPEN" && Boolean(ask?.length) && !hasAnswer(comments, issue.number);
  const status = statusText(issue);
  return `<main class="dispatch-detail" data-thread-number="${issue.number}">
    <header class="detail-header">
      <div class="detail-header-row">
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
      <p class="detail-subtitle">Opened by ${escapeHtml(issue.authorLogin)} · ${escapeHtml(timeAgo(issue.createdAt))}</p>
    </header>
    <section class="opening-body">${renderMarkdownLite(stripMetaMarker(issue.body))}</section>
    ${
      ask?.length
        ? shouldRenderAsk
          ? renderAskForm({ ask, pending: writeState.askPending, error: writeState.askError })
          : renderAskContext(ask)
        : ""
    }
    ${renderSubThreads(subThreads)}
    <section class="conversation" aria-label="Conversation">
      ${comments.map((comment) => renderComment(comment, ask ?? [])).join("") || `<div class="empty-state">No comments yet.</div>`}
    </section>
    ${issue.state === "OPEN" ? renderReplyForm({ pending: writeState.replyPending, error: writeState.replyError }) : ""}
  </main>`;
}
