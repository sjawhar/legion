import { afterEach, expect, jest, test } from "bun:test";
import { AGENT_STREAM_WATCH_TTL_MS } from "@legion/contracts";

import { LiveViews } from "./live-views";

const PLANNER = "01a0e090-4848-7473-acc5-fc96e6a646d3";
const REVIEWER = "01a0e090-4848-7473-acc5-fc96e6a646d4";
const TESTER = "01a0e090-4848-7473-acc5-fc96e6a646d5";

afterEach(() => {
  jest.useRealTimers();
  window.__dispatchNoViewSleep = undefined;
  window.__dispatchLiveViews = undefined;
});

/** A store on a clock the test drives, as every view-state rule is a rule about time. */
function onFakeClock(): LiveViews {
  jest.useFakeTimers();
  return new LiveViews();
}

test("the view in front is live and the view left last lingers", () => {
  const views = onFakeClock();

  views.show(PLANNER);
  expect(views.state(PLANNER)).toBe("live");
  expect(views.mounted()).toEqual([PLANNER]);

  views.show(REVIEWER);
  expect(views.state(REVIEWER)).toBe("live");
  expect(views.state(PLANNER)).toBe("live 30s");
  expect(views.mounted()).toEqual([PLANNER, REVIEWER]);
});

// The linger is the window a session keeps publishing for after its last watch, so a view that
// comes back inside it finds the session still on the wire. A linger of one second would pass
// every browser check there is: this is the row that holds it to thirty.
test("the lingering view is still mounted at 29 seconds and asleep at 31", () => {
  const views = onFakeClock();
  views.setPinned([PLANNER]);
  views.show(PLANNER);
  views.show(REVIEWER);

  jest.advanceTimersByTime(29_000);
  expect(views.state(PLANNER)).toBe("live 30s");
  expect(views.mounted()).toEqual([PLANNER, REVIEWER]);

  jest.advanceTimersByTime(2_000);
  expect(views.state(PLANNER)).toBe("asleep");
  expect(views.mounted()).toEqual([REVIEWER]);
  expect(AGENT_STREAM_WATCH_TTL_MS).toBe(30_000);
});

test("opening a third view puts the first to sleep at once", () => {
  const views = onFakeClock();
  views.setPinned([PLANNER, REVIEWER, TESTER]);
  views.show(PLANNER);
  views.show(REVIEWER);
  jest.advanceTimersByTime(1_000);

  views.show(TESTER);

  // No clock moves here: the first view is asleep in the same task that opened the third.
  expect(views.state(PLANNER)).toBe("asleep");
  expect(views.state(REVIEWER)).toBe("live 30s");
  expect(views.state(TESTER)).toBe("live");
  expect(views.mounted()).toEqual([REVIEWER, TESTER]);
});

test("leaving every live view lets the last one linger and then sleep", () => {
  const views = onFakeClock();
  views.setPinned([PLANNER]);
  views.show(PLANNER);

  views.show(null);
  expect(views.state(PLANNER)).toBe("live 30s");

  jest.advanceTimersByTime(AGENT_STREAM_WATCH_TTL_MS);
  expect(views.state(PLANNER)).toBe("asleep");
  expect(views.mounted()).toEqual([]);
});

test("a sleeping view keeps its draft, its place and its unread count at open", () => {
  const views = onFakeClock();
  views.show(PLANNER);
  views.setDraft(PLANNER, "half a question");
  views.setPlace(PLANNER, { id: "dispatch:m-4", top: -120 });
  views.setUnreadAtOpen(PLANNER, 3);

  views.show(REVIEWER);
  views.show(TESTER);
  expect(views.state(PLANNER)).toBe("asleep");

  expect(views.draft(PLANNER)).toBe("half a question");
  expect(views.place(PLANNER)).toEqual({ id: "dispatch:m-4", top: -120 });
  expect(views.unreadAtOpen(PLANNER)).toBe(3);
});

// D8: every link into a live view makes an entry, pinned or not, and an unpinned one that falls
// out of the front and lingering pair is discarded - unless it holds an unsent draft, which is
// never thrown away.
test("an unpinned sleeping view is discarded, and one holding a draft is kept", () => {
  const views = onFakeClock();
  views.show(PLANNER);
  views.setUnreadAtOpen(PLANNER, 2);
  views.setPlace(PLANNER, { id: "dispatch:m-4", top: -120 });
  views.show(REVIEWER);
  views.setDraft(REVIEWER, "the half-written message");

  views.show(TESTER);
  jest.advanceTimersByTime(AGENT_STREAM_WATCH_TTL_MS);

  // The unpinned view with nothing written is gone; opening its link makes it again from the
  // session's replay.
  expect(views.place(PLANNER)).toBeNull();
  expect(views.unreadAtOpen(PLANNER)).toBe(0);
  expect(window.__dispatchLiveViews?.views?.[PLANNER]).toBeUndefined();

  expect(views.draft(REVIEWER)).toBe("the half-written message");
  expect(views.state(REVIEWER)).toBe("asleep");
});

test("a pinned sleeping view is kept, and unpinning one that holds nothing discards it", () => {
  const views = onFakeClock();
  views.setPinned([PLANNER]);
  views.show(PLANNER);
  views.setUnreadAtOpen(PLANNER, 4);
  views.show(REVIEWER);
  views.show(TESTER);

  expect(views.state(PLANNER)).toBe("asleep");
  expect(views.unreadAtOpen(PLANNER)).toBe(4);

  views.setPinned([]);
  expect(views.unreadAtOpen(PLANNER)).toBe(0);
  expect(window.__dispatchLiveViews?.views?.[PLANNER]).toBeUndefined();
});

test("the snapshot names each view's state, draft length and unread count", () => {
  const views = onFakeClock();
  // Another owner's reading on the same handle - the live socket's open subscriptions - is left
  // where it is.
  window.__dispatchLiveViews = { agentSubscriptions: 2 };
  views.show(PLANNER);
  views.setUnreadAtOpen(PLANNER, 5);
  views.setDraft(PLANNER, "four");
  views.show(REVIEWER);

  expect(window.__dispatchLiveViews).toEqual({
    agentSubscriptions: 2,
    views: {
      [PLANNER]: { draftChars: 4, state: "live 30s", unreadAtOpen: 5 },
      [REVIEWER]: { draftChars: 0, state: "live", unreadAtOpen: 0 },
    },
  });
});

// The negative control of the browser check that counts what a sleeping view holds (D15).
test("the no-sleep switch leaves every opened view mounted", () => {
  window.__dispatchNoViewSleep = true;
  const views = onFakeClock();

  views.show(PLANNER);
  views.show(REVIEWER);
  views.show(TESTER);
  jest.advanceTimersByTime(10 * AGENT_STREAM_WATCH_TTL_MS);

  expect(views.mounted()).toEqual([PLANNER, REVIEWER, TESTER]);
  expect(views.state(PLANNER)).toBe("live 30s");
  expect(views.state(TESTER)).toBe("live");
});

test("every change tells the subscribers, and letting go stops them", () => {
  const views = onFakeClock();
  let told = 0;
  const unsubscribe = views.subscribe(() => {
    told += 1;
  });

  views.show(PLANNER);
  views.show(REVIEWER);
  expect(told).toBe(2);

  jest.advanceTimersByTime(AGENT_STREAM_WATCH_TTL_MS);
  expect(told).toBe(3);

  unsubscribe();
  views.show(TESTER);
  expect(told).toBe(3);
});
