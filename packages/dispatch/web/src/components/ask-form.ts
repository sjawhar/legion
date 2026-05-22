import type { QuestionAnswer, QuestionInfo } from "@opencode-ai/sdk/v2";

import { escapeHtml } from "../html";

export interface AskFormInput {
  ask: QuestionInfo[];
  pending: boolean;
  error?: string;
}

type QuestionOption = { label: string; description?: string };

function optionsFor(question: QuestionInfo): QuestionOption[] {
  return (question.options ?? []) as QuestionOption[];
}

function renderOption(
  questionIndex: number,
  question: QuestionInfo,
  option: QuestionOption
): string {
  const type = question.multiple ? "checkbox" : "radio";
  const name = `answer-${questionIndex}`;
  const label = escapeHtml(option.label);
  const description = option.description
    ? `<span class="ask-option-description">${escapeHtml(option.description)}</span>`
    : "";
  return `<label class="ask-option">
    <input type="${type}" name="${name}" value="${label}">
    <span>${label}</span>${description}
  </label>`;
}

export function summarizeAnswers(ask: QuestionInfo[], answers: QuestionAnswer[]): string {
  return ask
    .map((question, index) => {
      const header = question.header || `Question ${index + 1}`;
      const prompt = question.question?.trim();
      const values = answers[index] ?? [];
      const answerText = values.join(", ") || "No answer";
      // Each question renders as a two-line block so the GitHub comment
      // body has the full context (header + prompt + answer) even if no
      // dashboard is rendering the structured Q&A view.
      const head = prompt ? `**${header}** — ${prompt}` : `**${header}**`;
      return `${head}\n${answerText}`;
    })
    .join("\n\n");
}

export function renderAskForm(input: AskFormInput): string {
  const questions = input.ask
    .map((question, index) => {
      const options = optionsFor(question)
        .map((option) => renderOption(index, question, option))
        .join("");
      // Free-response is always offered. Agents can't opt out; humans may
      // have an answer that doesn't fit any of the canned options.
      const customSlot = `<label class="ask-custom-toggle"><input type="checkbox" name="custom-enabled-${index}"> Other (specify)</label>
        <textarea class="ask-custom-text" name="custom-${index}" rows="3" placeholder="Type your answer"></textarea>`;
      return `<fieldset class="ask-question" data-question-index="${index}">
        <legend>${escapeHtml(question.header || `Question ${index + 1}`)}</legend>
        <p>${escapeHtml(question.question)}</p>
        <div class="ask-options">${options}</div>
        ${customSlot}
      </fieldset>`;
    })
    .join("");

  return `<form class="ask-form" data-action="ask-answer" data-question-count="${input.ask.length}">
    <h2>Answer requested</h2>
    ${questions}
    <div class="form-actions">
      <button type="submit" ${input.pending ? "disabled" : ""}>Submit answer</button>
      ${input.error ? `<span class="form-error">${escapeHtml(input.error)}</span>` : ""}
    </div>
  </form>`;
}

// Read-only echo of the ask block used after an answer has been submitted
// (or the thread has been closed). The interactive form disappears, but the
// question context must remain visible so readers can interpret the answer
// without scrolling through the conversation to find the original prompt.
export function renderAskContext(ask: QuestionInfo[]): string {
  if (!ask.length) return "";
  const questions = ask
    .map((question, index) => {
      const header = escapeHtml(question.header || `Question ${index + 1}`);
      const prompt = escapeHtml(question.question || "");
      return `<div class="ask-context-question" data-question-index="${index}">
        <strong class="ask-context-header">${header}</strong>
        ${prompt ? `<span class="ask-context-prompt">${prompt}</span>` : ""}
      </div>`;
    })
    .join("");
  return `<section class="ask-context" aria-label="Question context">
    <h2>Question</h2>
    ${questions}
  </section>`;
}
