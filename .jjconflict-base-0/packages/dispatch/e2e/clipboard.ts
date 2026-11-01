import type { Page } from "@playwright/test";

declare global {
  interface Window {
    __dispatchCopied?: string[];
  }
}

/** Replaces the page's async clipboard with a recorder (before any navigation) so a test can
 *  assert what a copy button actually wrote, not just that it reported "Copied". */
export async function recordClipboard(page: Page): Promise<() => Promise<string[]>> {
  await page.addInitScript(() => {
    const copied: string[] = [];
    window.__dispatchCopied = copied;
    Object.defineProperty(navigator, "clipboard", {
      configurable: true,
      value: {
        writeText: async (text: string) => {
          copied.push(text);
        },
      },
    });
  });
  return () => page.evaluate(() => window.__dispatchCopied ?? []);
}
