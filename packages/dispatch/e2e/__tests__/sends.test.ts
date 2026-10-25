import { expect, test } from "bun:test";
import type { Page } from "@playwright/test";

import { MESSAGE_REFUSED, refusePosts } from "../sends";

/** A page whose routes the test reads back: the only part of a page `refusePosts` touches. */
function routedPage(): { page: Page; patterns: string[] } {
  const patterns: string[] = [];
  const page = {
    route: async (pattern: string) => {
      patterns.push(pattern);
    },
  } as unknown as Page;
  return { page, patterns };
}

// A send to one session tries a server that is down again under its key, so a row that refused
// that route with a 5xx or a 429 would wait on it for good. The helper refuses the row instead,
// naming the refusal that does end the send.
test("refusePosts refuses a 5xx or a 429 for the agent-messages route, naming MESSAGE_REFUSED", async () => {
  for (const pattern of [
    "**/api/v1/agents/*/messages",
    "**/api/v1/agents/01a0e0c1-0000-7000-8000-00000000a002/messages",
  ]) {
    for (const status of [503, 500, 429]) {
      const { page, patterns } = routedPage();
      await expect(
        refusePosts(page, pattern, { code: "UNAVAILABLE", error: "down", status })
      ).rejects.toThrow(/MESSAGE_REFUSED/);
      expect(patterns).toEqual([]);
    }
    const { page } = routedPage();
    await expect(refusePosts(page, pattern)).rejects.toThrow(/MESSAGE_REFUSED/);
  }
});

test("refusePosts routes Dispatch's verdict on a message, and a 503 anywhere else", async () => {
  const { page, patterns } = routedPage();
  await refusePosts(page, "**/api/v1/agents/*/messages", MESSAGE_REFUSED);
  await refusePosts(page, "**/api/v1/issues/CORE-1/comments");
  await refusePosts(page, "**/api/v1/issues/*/messages");
  expect(patterns).toEqual([
    "**/api/v1/agents/*/messages",
    "**/api/v1/issues/CORE-1/comments",
    "**/api/v1/issues/*/messages",
  ]);
});
