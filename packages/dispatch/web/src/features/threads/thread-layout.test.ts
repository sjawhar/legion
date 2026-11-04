import { expect, test } from "bun:test";

import { earlierRepliesLabel, threadLayout } from "./thread-layout";
import type { ThreadOrder } from "./thread-order";

/** Six replies, oldest first, named by their place in the thread. */
const REPLIES = [1, 2, 3, 4, 5, 6].map((place) => ({ id: `reply-${place}` }));

function ids(replies: readonly { readonly id: string }[]): string[] {
  return replies.map((reply) => reply.id);
}

function unread(...places: number[]): ReadonlySet<string> {
  return new Set(places.map((place) => `reply-${place}`));
}

const ORDERS: readonly ThreadOrder[] = ["oldest", "newest"];

test("a collapsed thread of read replies keeps the newest two and folds the rest", () => {
  const oldest = threadLayout({ collapse: true, order: "oldest", replies: REPLIES });
  expect(ids(oldest.folded)).toEqual(["reply-1", "reply-2", "reply-3", "reply-4"]);
  expect(ids(oldest.shown)).toEqual(["reply-5", "reply-6"]);
  expect(oldest.composerFirst).toBe(false);

  const newest = threadLayout({ collapse: true, order: "newest", replies: REPLIES });
  expect(ids(newest.folded)).toEqual(["reply-4", "reply-3", "reply-2", "reply-1"]);
  expect(ids(newest.shown)).toEqual(["reply-6", "reply-5"]);
  expect(newest.composerFirst).toBe(true);
});

test("unread replies inside the newest two change nothing in either order", () => {
  for (const order of ORDERS) {
    const layout = threadLayout({
      collapse: true,
      order,
      replies: REPLIES,
      unread: unread(5, 6),
    });
    expect(layout.folded).toHaveLength(4);
    expect(earlierRepliesLabel(layout.folded.length)).toBe("Show 4 earlier replies");
    expect(ids(layout.shown).sort()).toEqual(["reply-5", "reply-6"]);
  }
});

test("an unread reply is never folded: the fold stops at the oldest unread one", () => {
  for (const order of ORDERS) {
    const layout = threadLayout({
      collapse: true,
      order,
      replies: REPLIES,
      unread: unread(3, 4, 5, 6),
    });
    expect(earlierRepliesLabel(layout.folded.length)).toBe("Show 2 earlier replies");
    expect(ids(layout.folded).sort()).toEqual(["reply-1", "reply-2"]);
    expect(ids(layout.shown)).toEqual(
      order === "oldest"
        ? ["reply-3", "reply-4", "reply-5", "reply-6"]
        : ["reply-6", "reply-5", "reply-4", "reply-3"]
    );
  }
});

// The reply read before an unread one is kept with it: the fold is a prefix of the thread, so
// what it holds is always the oldest stretch of the conversation.
test("a read reply older than an unread one stays with it", () => {
  const layout = threadLayout({
    collapse: true,
    order: "oldest",
    replies: REPLIES,
    unread: unread(2, 6),
  });
  expect(ids(layout.folded)).toEqual(["reply-1"]);
  expect(earlierRepliesLabel(layout.folded.length)).toBe("Show 1 earlier reply");
  expect(ids(layout.shown)).toEqual(["reply-2", "reply-3", "reply-4", "reply-5", "reply-6"]);
});

test("a thread of two replies or fewer folds nothing", () => {
  for (const count of [0, 1, 2]) {
    const replies = REPLIES.slice(0, count);
    for (const order of ORDERS) {
      const layout = threadLayout({ collapse: true, order, replies });
      expect(layout.folded).toEqual([]);
      expect(layout.shown).toHaveLength(count);
    }
  }
});

test("a thread with no replies draws nothing but its root and the reply box", () => {
  for (const order of ORDERS) {
    for (const collapse of [true, false]) {
      const layout = threadLayout({ collapse, order, replies: [] });
      expect(layout.folded).toEqual([]);
      expect(layout.shown).toEqual([]);
      expect(layout.composerFirst).toBe(order === "newest");
    }
  }
});

test("a thread that does not collapse shows every reply in the chosen order", () => {
  const oldest = threadLayout({
    collapse: false,
    order: "oldest",
    replies: REPLIES,
    unread: unread(3, 4, 5, 6),
  });
  expect(oldest.folded).toEqual([]);
  expect(ids(oldest.shown)).toEqual(ids(REPLIES));

  const newest = threadLayout({ collapse: false, order: "newest", replies: REPLIES });
  expect(newest.folded).toEqual([]);
  expect(ids(newest.shown)).toEqual([...ids(REPLIES)].reverse());
});
