import type { Locator, Page } from "@playwright/test";

export function documentEditor(page: Page): Locator {
  return page.getByRole("textbox", { name: "Document editor" });
}

export function actionBar(page: Page): Locator {
  return page.locator(".dispatch-action-bar");
}

export async function barAction(page: Page, label: "Comment" | "Suggest" | "Ask"): Promise<void> {
  await actionBar(page).getByRole("button", { exact: true, name: label }).click();
}

export function markSpan(page: Page, markId: string): Locator {
  return documentEditor(page).locator(`[data-id="${markId}"]`);
}

export function cursorLabel(page: Page, name: string): Locator {
  return page.locator(".proof-collab-cursor__label", { hasText: name });
}

/** Selects the first occurrence of `quote` inside the focused ProseMirror node the way a drag
 * does: a DOM Range plus the selectionchange ProseMirror's DOMObserver listens to. */
export async function selectEditorText(page: Page, quote: string): Promise<void> {
  const editor = documentEditor(page);
  await editor.waitFor();
  await editor.focus();
  await editor.evaluate((root, quote) => {
    const walker = document.createTreeWalker(root, NodeFilter.SHOW_TEXT);
    for (let node = walker.nextNode(); node !== null; node = walker.nextNode()) {
      const index = node.textContent?.indexOf(quote) ?? -1;
      if (index < 0) continue;
      const range = document.createRange();
      range.setStart(node, index);
      range.setEnd(node, index + quote.length);
      const selection = window.getSelection();
      selection?.removeAllRanges();
      selection?.addRange(range);
      document.dispatchEvent(new Event("selectionchange"));
      return;
    }
    throw new Error(`quote is not in the editor: ${quote}`);
  }, quote);
  await actionBar(page).waitFor({ state: "visible" });
}

export async function deleteEditorText(page: Page, quote: string): Promise<void> {
  await selectEditorText(page, quote);
  await page.keyboard.press("Backspace");
}

export async function typeAtEnd(page: Page, text: string): Promise<void> {
  const editor = documentEditor(page);
  await editor.click();
  await page.keyboard.press("Control+End");
  await page.keyboard.press("Enter");
  await page.keyboard.type(text);
}

export function marginCard(page: Page, id: string): Locator {
  return page.locator(`[data-margin-item="${id}"]`);
}

export function countDocumentSockets(page: Page): () => number {
  let count = 0;
  page.on("websocket", (socket) => {
    if (new URL(socket.url()).pathname.startsWith("/ws/doc/")) count += 1;
  });
  return () => count;
}
