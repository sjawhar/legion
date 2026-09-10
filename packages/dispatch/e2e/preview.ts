import type { Page } from "@playwright/test";

/** Selects the first rendered occurrence of `quote` inside the document article and fires the
 *  mouseup the app listens to, the way a drag selection arrives. */
export async function selectPreviewText(page: Page, quote: string): Promise<void> {
  const article = page.getByRole("tabpanel", { name: "Spec" }).getByRole("article").first();
  await article.waitFor();
  await article.evaluate((root, quote) => {
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
      root.dispatchEvent(new MouseEvent("mouseup", { bubbles: true }));
      return;
    }
    throw new Error(`quote is not rendered: ${quote}`);
  }, quote);
}
