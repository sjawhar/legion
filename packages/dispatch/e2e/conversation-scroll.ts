import type { Page } from "@playwright/test";

/**
 * Where a reader is in the issue's conversation, and how a test moves them.
 *
 * The conversation is drawn in the page's one right-hand panel, which is a scrollport of its own
 * at every width the margin keeps a column at (`features/margin/MarginSheet.tsx`'s `h-dvh
 * overflow-y-auto`), and in the page's own column below that. So where the reader is is that
 * box's scroll, and scrolling the window instead moves the turns not at all. The page answers
 * the same question the same way (`features/conversation/use-follow-latest.ts`'s `scrollportOf`,
 * read by the follow, by the list's `ViewportAnchor` and by the page's tab-scroll record).
 */
const FIND_SCROLLPORT = `(() => {
  const newest = document.querySelector("[data-turn]");
  for (let node = newest?.parentElement; node != null; node = node.parentElement) {
    const overflow = getComputedStyle(node).overflowY;
    if ((overflow === "auto" || overflow === "scroll") && node.scrollHeight > node.clientHeight) {
      return node;
    }
  }
  return null;
})()`;

/** The conversation's own scroll offset, which is the window's where the page is that box. */
export function conversationScrollTop(page: Page): Promise<number> {
  return page.evaluate(`(() => {
    const port = ${FIND_SCROLLPORT};
    return port === null ? window.scrollY : port.scrollTop;
  })()`) as Promise<number>;
}

/** Scrolls the conversation to `top`, in whichever box its turns scroll in. */
export async function scrollConversationTo(page: Page, top: number): Promise<void> {
  await page.evaluate(`(() => {
    const port = ${FIND_SCROLLPORT};
    if (port === null) window.scrollTo(0, ${top});
    else port.scrollTo({ top: ${top} });
  })()`);
}

/** Moves the reader `delta` pixels down the conversation, in whichever box its turns scroll in. */
export async function scrollConversationBy(page: Page, delta: number): Promise<void> {
  await page.evaluate(`(() => {
    const port = ${FIND_SCROLLPORT};
    if (port === null) window.scrollBy(0, ${delta});
    else port.scrollBy({ top: ${delta} });
  })()`);
}

/** The box the turns are clipped to: the scrollport's own, or the window's where the page
 *  itself scrolls them. It is what the page measures the reader's place against
 *  (`features/shell/ViewportAnchor.tsx`'s `rowAtCenter`), so a test that reads where the
 *  reader is reads the same box. */
export function conversationViewport(page: Page): Promise<{ bottom: number; top: number }> {
  return page.evaluate(`(() => {
    const port = ${FIND_SCROLLPORT};
    if (port === null) return { bottom: window.innerHeight, top: 0 };
    const box = port.getBoundingClientRect();
    return { bottom: box.bottom, top: box.top };
  })()`) as Promise<{ bottom: number; top: number }>;
}

/** Takes the reader away from the newest card, which is the end of the turns furthest from it. */
export async function scrollAwayFromLatest(page: Page): Promise<void> {
  await page.evaluate(`(() => {
    const port = ${FIND_SCROLLPORT};
    if (port === null) window.scrollTo(0, document.documentElement.scrollHeight);
    else port.scrollTo({ top: port.scrollHeight });
  })()`);
}
