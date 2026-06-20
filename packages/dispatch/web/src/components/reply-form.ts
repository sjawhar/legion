import { escapeHtml } from "../html";

export interface ReplyFormInput {
  pending: boolean;
  error?: string;
}

/**
 * Compact reply form. Looks like a single-row text input by default,
 * grows as the textarea fills. No redundant "Reply" label — the
 * placeholder + the send button carry the affordance. The send button
 * sits inline at the right of the input on wide viewports, below on
 * narrow.
 */
export function renderReplyForm(input: ReplyFormInput): string {
  return `<form class="reply-form" data-action="reply" aria-label="Reply">
    <textarea
      id="reply-body"
      name="body"
      rows="1"
      placeholder="Write a reply…"
      ${input.pending ? "disabled" : ""}
    ></textarea>
    <div class="reply-row">
      <button type="submit" class="btn-primary" ${input.pending ? "disabled" : ""}>${input.pending ? "Sending…" : "Reply"}</button>
      ${input.error ? `<span class="form-error">${escapeHtml(input.error)}</span>` : ""}
    </div>
  </form>`;
}
