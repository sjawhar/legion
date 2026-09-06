// DOM patching for the dashboard. The page is never rebuilt wholesale: the
// sidebar list and the detail regions are repainted individually — and only
// when their markup changed — and the two things a human types into — the
// reply textarea and the open-ask forms — are created once per selection and
// reconciled by id, never re-created by an event.

import { renderAskForm } from "./components/ask-form";
import { type ReplyFormInput, renderReplyForm, replyButtonLabel } from "./components/reply-form";
import {
  askFormAsks,
  askFormInput,
  replyFormInput,
  type ThreadDetailInput,
} from "./components/thread-detail";

// The markup last written into each region, per root. Browsers reserialise
// innerHTML (quoting, entities), so the source string is what is compared.
const painted = new WeakMap<ParentNode, Map<string, string>>();

/**
 * Write `innerHtml` into `#id` under `root` unless it is exactly what was last
 * painted there. Returns whether the region changed, so callers run the passes
 * that only matter for new markup (linkify, unfurl) on a real change. Painting
 * the same string is a no-op: text selection, open `<details>`, and unfurled
 * titles in an unchanged region survive an unrelated event.
 */
export function paintRegion(root: ParentNode, id: string, innerHtml: string): boolean {
  let regions = painted.get(root);
  if (!regions) {
    regions = new Map();
    painted.set(root, regions);
  }
  if (regions.get(id) === innerHtml) return false;
  const region = root.querySelector<HTMLElement>(`#${id}`);
  if (!region) return false;
  region.innerHTML = innerHtml;
  regions.set(id, innerHtml);
  return true;
}

/**
 * Remember what a wholesale render put into `root`'s regions, so the next
 * paintRegion of the same markup is a no-op rather than a first rewrite.
 * Replaces whatever was remembered for `root`.
 */
export function markPainted(root: ParentNode, regions: Record<string, string>): void {
  painted.set(root, new Map(Object.entries(regions)));
}

/** Forget what was painted under `root`: call before replacing its subtree wholesale. */
export function forgetPainted(root: ParentNode): void {
  painted.delete(root);
}

/** Reflect one form's pending/error state on its existing controls: the same fields the templates render from. */
export function syncFormState(
  form: HTMLFormElement,
  { pending, error }: Pick<ReplyFormInput, "pending" | "error">
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

// The id is issue-body content anyone on the repo can write. Escaping only `"`
// leaves `\` live in the selector: a trailing one makes the lookup miss (so the
// form is re-created on every paint) and `\"` throws inside querySelector and
// aborts the paint. CSS.escape neutralises both.
function askFormSelector(askId: string): string {
  return `form[data-ask-id="${CSS.escape(askId)}"]`;
}

/**
 * Remove forms for asks that closed, add forms for asks that opened, leave the
 * rest untouched. A new form goes in list order relative to the forms already
 * present; existing forms are never moved. The form of an ask whose answer is
 * still posting stays (askFormAsks), disabled, so a failed post returns it with
 * the human's choice intact.
 */
export function reconcileAskForms(root: ParentNode, input: ThreadDetailInput): void {
  const container = root.querySelector<HTMLElement>("#detail-ask-forms");
  if (!container) return;
  const asks = askFormAsks(input);
  const wanted = new Set(asks.map((ask) => ask.askId));
  for (const form of container.querySelectorAll<HTMLFormElement>("form[data-ask-id]")) {
    if (!wanted.has(form.dataset.askId ?? "")) form.remove();
  }
  let previous: HTMLFormElement | null = null;
  for (const ask of asks) {
    const formInput = askFormInput(ask, input);
    let form = container.querySelector<HTMLFormElement>(askFormSelector(ask.askId));
    if (!form) {
      const html = renderAskForm(formInput);
      if (previous) previous.insertAdjacentHTML("afterend", html);
      else container.insertAdjacentHTML("afterbegin", html);
      form = container.querySelector<HTMLFormElement>(askFormSelector(ask.askId));
    }
    if (!form) continue;
    syncFormState(form, formInput);
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
  const formInput = replyFormInput(input);
  syncFormState(form, formInput);
  const button = form.querySelector<HTMLButtonElement>("button[type=submit]");
  if (button) button.textContent = replyButtonLabel(formInput.pending);
}
