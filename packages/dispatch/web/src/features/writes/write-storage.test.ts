import { expect, test } from "bun:test";

import { keptTab } from "../../__tests__/kept-sends-harness";
import { ApiError } from "../../api/client";
import { SendInterruptedError, SendStoppedError } from "./keyed-write";
import {
  KEPT_SEND_RESUME_MS,
  keptRefusal,
  refusalFrom,
  resumesOnItsOwn,
  WriteStorage,
} from "./write-storage";

/** A record `parse` takes: the store's own, here a flag. */
const accept = (detail: unknown) =>
  typeof detail === "object" && detail !== null && Reflect.get(detail, "ok") === true
    ? { ok: true as const }
    : undefined;

/** One write as its store keeps it, with the fields every entry carries. */
const kept = (key: string, fields: Record<string, unknown> = {}) => ({
  detail: { ok: true },
  key,
  kind: "composer" as const,
  pressedAt: 1_700_000_000_000,
  refusal: null,
  request: {
    body: { body: "hello" },
    method: "POST" as const,
    path: "/api/v1/issues/CORE-1/comments",
  },
  state: "sending" as const,
  ...fields,
});

// W1, W6. Every store keeps its writes in one format, one entry per key, versioned and stamped
// with the login of the person who pressed it, and each takes up only its own kind and only that
// person's. An entry the page cannot read - another version's, damaged JSON, one filed under
// another key, one another person kept, or a record its store refuses - is dropped as the page
// loads, never read; another store's entries of the same person stay for it. So is an entry of
// the retired `dispatch.keyed-send:` format, which nothing reads now.
test("one versioned format for every store, and an entry that does not parse is dropped on load", () => {
  const tab = keptTab();
  const page = tab.page();
  const entry = (key: string, fields: Record<string, unknown> = {}) =>
    JSON.stringify({ ...kept(key), login: "alice", version: 3, ...fields });

  try {
    page.keep(kept("kept", { state: "late" }), "alice");
    expect(tab.kept("kept")).toEqual({
      ...kept("kept", { state: "late" }),
      login: "alice",
      version: 3,
    });
    tab.storage.setItem("dispatch.write:broadcast", entry("broadcast", { kind: "broadcast" }));
    tab.storage.setItem("dispatch.write:newer", entry("newer", { version: 4 }));
    tab.storage.setItem("dispatch.write:older", entry("older", { login: undefined, version: 2 }));
    tab.storage.setItem("dispatch.write:damaged", "{");
    tab.storage.setItem("dispatch.write:moved", entry("elsewhere"));
    tab.storage.setItem("dispatch.write:bobs", entry("bobs", { login: "bob" }));
    tab.storage.setItem("dispatch.write:refused-record", entry("refused-record", { detail: 7 }));
    tab.storage.setItem(
      "dispatch.write:refused-unsaid",
      entry("refused-unsaid", { state: "refused" })
    );
    // The format this release retired: dropped whatever it holds.
    tab.storage.setItem("dispatch.keyed-send:retired", entry("retired"));
    tab.storage.setItem("someone-else", "{");

    const later = tab.page();
    expect(later.load("composer", "alice", accept)).toEqual([
      { ...kept("kept", { state: "late" }), detail: { ok: true } },
    ]);
    expect(
      Array.from({ length: tab.storage.length }, (_, index) => tab.storage.key(index)).sort()
    ).toEqual(["dispatch.write:broadcast", "dispatch.write:kept", "someone-else"]);
    expect(later.load("broadcast", "alice", accept)).toEqual([
      { ...kept("broadcast"), kind: "broadcast", detail: { ok: true } },
    ]);
  } finally {
    tab.close();
  }
});

// A write with no request to make again - an upload, whose file no entry can hold - is counted
// for the leave prompt and kept nowhere, so a reload never resumes it as an empty request.
test("a write with nothing to repeat is only held for the leave prompt", () => {
  const tab = keptTab();
  const unload = new EventTarget();
  const page = new WriteStorage(tab.storage, unload);
  const asks = () => {
    const leaving = new Event("beforeunload", { cancelable: true });
    unload.dispatchEvent(leaving);
    return leaving.defaultPrevented;
  };

  try {
    page.hold("upload-1", true);
    expect(asks()).toBe(true);
    expect(tab.storage.length).toBe(0);

    page.hold("upload-1", false);
    expect(asks()).toBe(false);
  } finally {
    tab.close();
  }
});

// A refusal comes back as the error it was: the reader's stop, a reload that interrupted the write
// too long ago, Dispatch's verdict with its status and code, or another error by its message.
test("a kept refusal is rebuilt as the error it was", () => {
  const verdict = new ApiError(422, { code: "KEY_REUSED", error: "the key is used" });
  const rebuilt = refusalFrom(keptRefusal(verdict));
  expect(rebuilt).toBeInstanceOf(ApiError);
  expect(rebuilt).toMatchObject({ code: "KEY_REUSED", message: "the key is used" });
  expect((rebuilt as ApiError).status).toBe(422);
  expect(refusalFrom(keptRefusal(new SendStoppedError()))).toBeInstanceOf(SendStoppedError);
  const interrupted = refusalFrom(keptRefusal(new SendInterruptedError()));
  expect(interrupted).toBeInstanceOf(SendInterruptedError);
  expect(interrupted.message).toBe(
    "Not sent: this page was closed before Dispatch confirmed it. Retry sends it under the same key, so it is delivered at most once."
  );
  expect(refusalFrom(keptRefusal(new Error("no route")))).toMatchObject({ message: "no route" });
});

// W7. A write kept for longer than five minutes is not made again on its own: the reader who made
// it may no longer mean it, and the page asks them instead.
test("a write resumes on its own only within five minutes of its press", () => {
  const now = Date.parse("2026-10-25T12:00:00Z");
  expect(resumesOnItsOwn(now, now)).toBe(true);
  expect(resumesOnItsOwn(now - KEPT_SEND_RESUME_MS, now)).toBe(true);
  expect(resumesOnItsOwn(now - KEPT_SEND_RESUME_MS - 1, now)).toBe(false);
  expect(KEPT_SEND_RESUME_MS).toBe(5 * 60_000);
});

// W6. Signing out ends the kept writes with the session that made them: nothing of the person
// who has gone is left for the next one to resume, and the page stops asking to confirm leaving.
test("clearing drops every kept write and the leave prompt with it", () => {
  const tab = keptTab();
  const unload = new EventTarget();
  const page = new WriteStorage(tab.storage, unload);

  try {
    page.keep(kept("out"), "alice");
    const asks = () => {
      const leaving = new Event("beforeunload", { cancelable: true });
      unload.dispatchEvent(leaving);
      return leaving.defaultPrevented;
    };
    expect(asks()).toBe(true);

    page.clear();
    expect(tab.storage.length).toBe(0);
    expect(asks()).toBe(false);
  } finally {
    tab.close();
  }
});

// W10. A write the storage refuses - it is full, or the browser refuses the page a quota - must
// never leave an older state standing for the write: the entry goes instead, so a reload resumes
// nothing rather than resuming a write the reader has since stopped.
test("a write the storage refuses removes what was kept under that key", () => {
  const tab = keptTab();
  const refusing: Storage = {
    clear: () => tab.storage.clear(),
    getItem: (name) => tab.storage.getItem(name),
    key: (index) => tab.storage.key(index),
    get length() {
      return tab.storage.length;
    },
    removeItem: (name) => tab.storage.removeItem(name),
    setItem: (name, value) => {
      if (value.includes('"state":"stopping"')) throw new Error("QuotaExceededError");
      tab.storage.setItem(name, value);
    },
  };
  const page = new WriteStorage(refusing, new EventTarget());

  try {
    page.keep(kept("out", { state: "late" }), "alice");
    expect(tab.kept("out")).toMatchObject({ state: "late" });

    page.keep(kept("out", { state: "stopping" }), "alice");
    expect(tab.kept("out")).toBeUndefined();
  } finally {
    tab.close();
  }
});

// A page with no storage at all - a browser that refuses the tab one - keeps nothing and still
// asks to confirm leaving while a write is unconfirmed: the write itself is unaffected.
test("a page with no storage keeps nothing and still asks before the tab is closed", () => {
  const unload = new EventTarget();
  const page = new WriteStorage(undefined, unload);
  const asks = () => {
    const leaving = new Event("beforeunload", { cancelable: true });
    unload.dispatchEvent(leaving);
    return leaving.defaultPrevented;
  };

  page.keep(kept("out"), "alice");
  expect(asks()).toBe(true);
  expect(page.load("composer", "alice", accept)).toEqual([]);

  page.forget("out");
  expect(asks()).toBe(false);
});
