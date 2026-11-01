// What a spec reads off a heavy body already in a page: the element a surface renders one into,
// the text standing loose in that element, and the state a drawn body ends in. Two specs read a
// body through the element and the settled state - `heavy-bodies.e2e.ts`, which draws the bodies
// of `heavy-bodies.ts` in the Conversation, the Inbox's ask card and a hover card, and
// `heavy-streamed-turn.e2e.ts`, which watches one grow under a paint in the live view and alone
// counts its loose text - so both read the same element and mean the same thing by a settled
// body (sjawhar/legion#2519).
import { expect, type Locator } from "@playwright/test";

/**
 * The element a surface renders a body into (`markdownClassName` in `markdown-engine.ts`), which
 * is also the element that carries `data-markdown-pending` while the worker owes its formatting
 * and `data-markdown-fallback` when the parser itself refused the body.
 */
export function markdownBody(card: Locator): Locator {
  return card.locator(".dispatch-markdown").first();
}

/** The characters standing in `element` itself rather than inside a block a paint attached:
 *  a body's own literal text, which a reader is shown while the worker owes its formatting and
 *  which a paint replaces rather than stands behind. */
export function looseTextLength(element: Locator): Promise<number> {
  return element.evaluate((node) => {
    let loose = 0;
    for (const child of node.childNodes) {
      if (child.nodeType === Node.TEXT_NODE) {
        loose += child.textContent?.length ?? 0;
      }
    }
    return loose;
  });
}

/** The card ends with its body formatted: nothing is still waiting on the worker, and the parser
 *  refused nothing. */
export async function expectSettled(card: Locator): Promise<void> {
  await expect(card.locator("[data-markdown-pending]")).toHaveCount(0);
  await expect(card.locator("[data-markdown-fallback]")).toHaveCount(0);
}
