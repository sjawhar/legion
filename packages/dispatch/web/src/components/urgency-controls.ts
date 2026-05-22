import { escapeHtml } from "../html";
import type { Urgency } from "../types";

const LEVELS: Urgency[] = ["blocking", "high", "med", "low"];

export interface UrgencyControlsInput {
  urgency: Urgency;
  pending: boolean;
  error?: string;
}

/**
 * Renders the urgency control as a clickable chip in the header. The chip
 * itself is a `<summary>` inside a `<details>` element, so clicking it
 * toggles a small menu of urgency levels. No labeled form field, no
 * full-width `<select>` — just a tight chip that shows the current value
 * and reveals the four options on click.
 *
 * `main.ts` listens for clicks on `[data-urgency-value]` buttons and
 * dispatches via `controller.setUrgency()`. The `<details>` element also
 * closes automatically after a selection because the click handler in
 * main.ts collapses the element after firing the action.
 */
export function renderUrgencyControls(input: UrgencyControlsInput): string {
  const current = input.urgency;
  const disabled = input.pending ? "data-pending" : "";
  const options = LEVELS.map(
    (level) => `<button
      type="button"
      class="urgency-option urgency-badge-${level}"
      role="menuitem"
      data-urgency-value="${level}"
      ${level === current ? 'aria-current="true"' : ""}
    ><span class="urgency-dot urgency-${level}"></span>${level}</button>`
  ).join("");
  return `<details class="urgency-chip-wrap" ${disabled}>
    <summary
      class="urgency-chip urgency-badge-${current}"
      title="Change urgency"
      aria-haspopup="menu"
    ><span class="urgency-dot urgency-${current}"></span>${current}${input.pending ? '<span class="urgency-spinner" aria-hidden="true"></span>' : ""}</summary>
    <div class="urgency-menu" role="menu">${options}</div>
    ${input.error ? `<span class="form-error urgency-error">${escapeHtml(input.error)}</span>` : ""}
  </details>`;
}
