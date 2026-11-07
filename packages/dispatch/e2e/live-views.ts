// What a browser check can read about the page's live views (INFRA-812, D15). Two owners report
// on one `window` handle, `window.__dispatchLiveViews`: the registry writes a snapshot of each
// opened view (`web/src/features/agent-view/live-views.ts`) and the live socket writes how many
// agent subscriptions it holds (`web/src/api/live-socket.ts`). The snapshot is a word and two
// numbers per session; it holds nothing anyone wrote and changes no behaviour. The switch below
// is the other half: set before the page loads, it turns the sleep rule off, which is the
// negative control of the check that counts what an open view costs.
import type { Locator, Page } from "@playwright/test";

import type { LiveViewSnapshot } from "../web/src/features/agent-view/live-views";

/** The live view a mutation observer is waiting to see restored, and the two stamps it takes:
 *  the press, and the commit the view came back in. */
declare global {
  interface Window {
    __dispatchViewRestore?: {
      pressedAt: number | null;
      restoredAt: number | null;
    };
  }
}

/** Everything the page reports about its live views now. */
export interface LiveViewsReading {
  /** How many agent subscriptions the page's one live socket holds. A page with no live socket
   *  yet reports none, which is no subscriptions. */
  agentSubscriptions: number;
  /** Each session whose view the registry still holds an entry for, by session id. A session
   *  that is not here has no entry at all: it was never opened, or it slept holding nothing. */
  views: Record<string, LiveViewSnapshot>;
}

export function readLiveViews(page: Page): Promise<LiveViewsReading> {
  return page.evaluate(() => {
    const reported = window.__dispatchLiveViews;
    return {
      agentSubscriptions: reported?.agentSubscriptions ?? 0,
      views: reported?.views ?? {},
    };
  });
}

/** How many agent subscriptions the page's one live socket holds now: one per mounted live
 *  view, and none for a view that sleeps - which is also what lets the relay's watch for that
 *  session lapse, since nothing re-arms it. */
export async function openAgentSubscriptions(page: Page): Promise<number> {
  return (await readLiveViews(page)).agentSubscriptions;
}

/** The live views the shell has mounted: one element per conversation drawn, whether it is the
 *  one in front or the one lingering behind it (`MountedLiveViews.tsx`). The lingering view is
 *  invisible and `aria-hidden`, so this is a CSS locator rather than a role query: what it
 *  counts is what the page is drawing, not what a reader sees. */
export function drawnLiveViews(page: Page): Locator {
  return page.locator('[data-testid="agent-live-view"]');
}

/**
 * The page's JavaScript heap, in bytes, where the browser gives it. Only Chromium reports
 * `performance.memory`, so a check reads `null` as "this browser does not say" rather than as
 * zero, and only a Chromium launched with `--enable-precise-memory-info` reports a number that
 * moves: without the switch it quantises `usedJSHeapSize` so heavily that reads either side of
 * a hundred megabytes of transcript come back identical (measured: a constant 131,000,000, where
 * the same page with the switch read 13.5 MB before any view and 213.6 MB with eight open). A
 * row whose claim is the heap asks for it itself:
 * `test.use({ launchOptions: { args: ["--enable-precise-memory-info"] } })`.
 */
export function heapBytes(page: Page): Promise<number | null> {
  return page.evaluate(() => {
    const reported = (performance as Performance & { memory?: { usedJSHeapSize: number } }).memory;
    return reported?.usedJSHeapSize ?? null;
  });
}

/**
 * Times a live view coming back, in the page's own clock rather than the test driver's: install
 * it before the press that opens the view, then read it once the view is in front.
 *
 * The first stamp is the first `pointerdown` the page sees after this is installed, taken in the
 * capture phase so it is before any handler of the press runs. The second is the first mutation
 * in which `sessionID`'s view is the one in front with a turn drawn in it, which is the
 * conversation restored. The reader throws while either stamp is missing, so a row never reads a
 * time for something that did not happen.
 */
export async function watchViewRestore(
  page: Page,
  sessionID: string
): Promise<() => Promise<number>> {
  await page.evaluate((id) => {
    const stamps: NonNullable<Window["__dispatchViewRestore"]> = {
      pressedAt: null,
      restoredAt: null,
    };
    window.__dispatchViewRestore = stamps;
    window.addEventListener(
      "pointerdown",
      () => {
        stamps.pressedAt = performance.now();
      },
      { capture: true, once: true }
    );
    // The view in front, with a turn drawn in it (`AgentThread` gives each turn its
    // `data-message-id`): the conversation restored, rather than the box it goes in.
    const restored =
      `[data-testid="agent-live-view"][data-session-id="${id}"][data-front="true"] ` +
      `[data-message-id]`;
    const observer = new MutationObserver(() => {
      if (stamps.restoredAt !== null || document.querySelector(restored) === null) return;
      stamps.restoredAt = performance.now();
      observer.disconnect();
    });
    observer.observe(document, { attributes: true, childList: true, subtree: true });
  }, sessionID);
  return () =>
    page.evaluate(() => {
      const stamps = window.__dispatchViewRestore;
      if (stamps === undefined) throw new Error("no restore was watched");
      if (stamps.pressedAt === null) throw new Error("nothing was pressed");
      if (stamps.restoredAt === null) throw new Error("the view was not restored");
      return stamps.restoredAt - stamps.pressedAt;
    });
}

/** Turns the sleep rule off before the page loads: every view the reader opens stays mounted,
 *  holding its subscription and its drawn conversation (`live-views.ts` reads the switch at
 *  every move). This is the negative control of the check that counts what an open view costs -
 *  with it set, the counts that check holds to must fail. */
export async function keepEveryViewAwake(page: Page): Promise<void> {
  await page.addInitScript(() => {
    window.__dispatchNoViewSleep = true;
  });
}
