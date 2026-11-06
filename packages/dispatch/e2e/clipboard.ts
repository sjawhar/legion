import type { Page } from "@playwright/test";

declare global {
  interface Window {
    __dispatchCopied?: string[];
  }
}

/** Replaces the page's async clipboard with a recorder (before any navigation) so a test can
 *  assert what a copy button actually wrote, not just that it reported "Copied". It records a
 *  `write` of clipboard items (each one's plain text, once its read has finished) as it records
 *  `writeText`. */
export async function recordClipboard(page: Page): Promise<() => Promise<string[]>> {
  await page.addInitScript(() => {
    const copied: string[] = [];
    window.__dispatchCopied = copied;
    Object.defineProperty(navigator, "clipboard", {
      configurable: true,
      value: {
        write: async (items: ClipboardItem[]) => {
          for (const item of items) {
            copied.push(await (await item.getType("text/plain")).text());
          }
        },
        writeText: async (text: string) => {
          copied.push(text);
        },
      },
    });
  });
  return () => page.evaluate(() => window.__dispatchCopied ?? []);
}
