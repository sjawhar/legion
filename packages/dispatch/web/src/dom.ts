// DOM patching for the dashboard. The page is never rebuilt wholesale: the
// sidebar list and the detail regions are repainted individually, and the two
// things a human types into — the reply textarea and the open-ask forms — are
// created once per selection and reconciled by id, never re-created by an
// event.

import { renderAskForm } from "./components/ask-form";
import { renderReplyForm } from "./components/reply-form";
import { EMPTY_WRITE_STATE, type ThreadDetailInput } from "./components/thread-detail";

export function paintRegion(root: ParentNode, id: string, innerHtml: string): void {
  const region = root.querySelector<HTMLElement>(`#${id}`);
  if (region) region.innerHTML = innerHtml;
}

export function syncFormState(
  form: HTMLFormElement,
  pending: boolean,
  error: string | undefined
): void {
  for (const control of form.querySelectorAll<
    HTMLButtonElement | HTMLInputElement | HTMLTextAreaElement
  >("button[type=submit], input, textarea")) {
    control.disabled = pending;
  }
  let slot = form.querySelector<HTMLElement>(".form-error");
  if (error) {
    if (!slot) {
      slot = form.ownerDocument.createElement("span");
      slot.className = "form-error";
      (form.querySelector(".form-actions") ?? form.querySelector(".reply-row") ?? form).append(
        slot
      );
    }
    slot.textContent = error;
  } else {
    slot?.remove();
  }
}

function askFormSelector(askId: string): string {
  return `form[data-ask-id="${askId.replaceAll('"', '\\"')}"]`;
}

/**
 * Remove forms for asks that closed, add forms for asks that opened, leave the
 * rest untouched. A new form goes in list order relative to the forms already
 * present; existing forms are never moved.
 */
export function reconcileAskForms(root: ParentNode, input: ThreadDetailInput): void {
  const container = root.querySelector<HTMLElement>("#detail-ask-forms");
  if (!container) return;
  const open = input.issue.state === "OPEN" ? input.openAsks : [];
  const wanted = new Set(open.map((ask) => ask.askId));
  for (const form of container.querySelectorAll<HTMLFormElement>("form[data-ask-id]")) {
    if (!wanted.has(form.dataset.askId ?? "")) form.remove();
  }
  const writeState = input.writeState ?? EMPTY_WRITE_STATE;
  let previous: HTMLFormElement | null = null;
  for (const ask of open) {
    let form = container.querySelector<HTMLFormElement>(askFormSelector(ask.askId));
    if (!form) {
      const source = ask.source;
      const askedAt =
        source.kind === "comment"
          ? input.comments.find((comment) => comment.id === source.commentId)?.createdAt
          : undefined;
      const html = renderAskForm({ ask, pending: false, askedAt });
      if (previous) previous.insertAdjacentHTML("afterend", html);
      else container.insertAdjacentHTML("afterbegin", html);
      form = container.querySelector<HTMLFormElement>(askFormSelector(ask.askId));
    }
    if (!form) continue;
    syncFormState(
      form,
      writeState.askPending === ask.askId,
      writeState.askError?.askId === ask.askId ? writeState.askError.message : undefined
    );
    previous = form;
  }
}

/**
 * Reflect pending/error state on the existing reply form without touching its
 * textarea. A closed thread has no reply form; a reopened one gets a fresh one.
 */
export function syncReplyForm(root: ParentNode, input: ThreadDetailInput): void {
  let form = root.querySelector<HTMLFormElement>("#detail-reply");
  if (input.issue.state !== "OPEN") {
    form?.remove();
    return;
  }
  if (!form) {
    const main = root.querySelector<HTMLElement>("main.dispatch-detail");
    if (!main) return;
    main.insertAdjacentHTML("beforeend", renderReplyForm({ pending: false }));
    form = root.querySelector<HTMLFormElement>("#detail-reply");
    if (!form) return;
  }
  const writeState = input.writeState ?? EMPTY_WRITE_STATE;
  syncFormState(form, writeState.replyPending, writeState.replyError);
  const button = form.querySelector<HTMLButtonElement>("button[type=submit]");
  if (button) button.textContent = writeState.replyPending ? "Sending…" : "Reply";
}
