import type { QuestionAnswer } from "@opencode-ai/sdk/v2";

import type { ThreadAsk } from "../asks";
import { escapeHtml, timeAgo } from "../html";
import type { MarkerQuestion, MarkerQuestionOption } from "../markers";

export interface AskFormInput {
  ask: ThreadAsk;
  pending: boolean;
  error?: string;
  /** When the ask came from a follow-up, the time it was asked (for the "↑ asked" anchor). */
  askedAt?: string;
}

function renderOption(question: MarkerQuestion, option: MarkerQuestionOption): string {
  const type = question.multiple ? "checkbox" : "radio";
  const label = escapeHtml(option.label);
  const description = option.description
    ? `<span class="ask-option-description">${escapeHtml(option.description)}</span>`
    : "";
  return `<label class="ask-option">
    <input type="${type}" name="answer" value="${label}">
    <span>${label}</span>${description}
  </label>`;
}

export function askHeader(question: MarkerQuestion, index: number): string {
  return question.header || `Question ${index + 1}`;
}

/**
 * The human-readable summary written below an answer marker: header, prompt,
 * and the chosen values, so the GitHub comment carries the full context even
 * where no dashboard renders the structured view.
 */
export function summarizeAnswer(
  question: MarkerQuestion,
  values: QuestionAnswer,
  index = 0
): string {
  const header = askHeader(question, index);
  const prompt = question.question?.trim();
  const head = prompt ? `**${header}** — ${prompt}` : `**${header}**`;
  return `${head}\n${values.join(", ") || "No answer"}`;
}

/**
 * One form per open ask. Created when the thread is selected or when the ask
 * opens; never re-created by an event, so a half-filled answer survives a
 * GitHub event arriving.
 */
export function renderAskForm(input: AskFormInput): string {
  const { ask } = input;
  const askId = escapeHtml(ask.askId);
  const options = (ask.question.options ?? [])
    .map((option) => renderOption(ask.question, option))
    .join("");
  const asked = input.askedAt ? ` · asked ${escapeHtml(timeAgo(input.askedAt))}` : "";
  // The turn that asked: the opening section or the follow-up's card.
  const anchor = ask.source.kind === "body" ? "#detail-opening" : `#turn-${ask.source.commentId}`;
  // Free-response is always offered. Agents can't opt out; humans may
  // have an answer that doesn't fit any of the canned options.
  return `<form class="ask-form" id="ask-form-${askId}" data-action="ask-answer" data-ask-id="${askId}">
    <h2>Answer: ${escapeHtml(askHeader(ask.question, ask.index))} <span class="ask-form-prompt">— ${escapeHtml(ask.question.question)}</span></h2>
    <a class="ask-form-anchor" href="${anchor}">↑ question${asked}</a>
    <div class="ask-options">${options}</div>
    <label class="ask-custom-toggle"><input type="checkbox" name="custom-enabled"> Other (specify)</label>
    <textarea class="ask-custom-text" name="custom" rows="3" placeholder="Type your answer"></textarea>
    <div class="form-actions">
      <button type="submit" ${input.pending ? "disabled" : ""}>Submit answer</button>
      ${input.error ? `<span class="form-error">${escapeHtml(input.error)}</span>` : ""}
    </div>
  </form>`;
}
