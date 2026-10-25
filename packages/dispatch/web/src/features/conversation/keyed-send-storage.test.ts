import { expect, test } from "bun:test";

import { keptTab } from "../../__tests__/kept-sends-harness";
import { ApiError } from "../../api/client";
import {
  KEPT_SEND_RESUME_MS,
  KeyedSendStorage,
  keptRefusal,
  refusalFrom,
  resumesOnItsOwn,
} from "./keyed-send-storage";
import { SendInterruptedError, SendStoppedError } from "./send-request";

/** A record `parse` takes: the store's own, here a flag. */
const accept = (send: unknown) =>
  typeof send === "object" && send !== null && Reflect.get(send, "ok") === true
    ? { ok: true as const }
    : undefined;

// W1, W6. Both stores keep their sends in one format, one entry per key, versioned and stamped
// with the login of the person who pressed it, and each takes up only its own kind and only that
// person's. An entry the page cannot read - another version's, damaged JSON, one filed under
// another key, one another person kept, or a record its store refuses - is dropped as the page
// loads, never read; the other store's entries of the same person stay for it.
test("one versioned format for both stores, and an entry that does not parse is dropped on load", () => {
  const tab = keptTab();
  const page = tab.page();
  const entry = (key: string, fields: Record<string, unknown> = {}) =>
    JSON.stringify({
      key,
      kind: "session",
      login: "alice",
      refusal: null,
      send: { ok: true },
      state: "sending",
      version: 2,
      ...fields,
    });

  try {
    page.keep(
      { key: "kept", kind: "session", refusal: null, send: { ok: true }, state: "late" },
      "alice"
    );
    expect(tab.kept("kept")).toEqual({
      key: "kept",
      kind: "session",
      login: "alice",
      refusal: null,
      send: { ok: true },
      state: "late",
      version: 2,
    });
    tab.storage.setItem("dispatch.keyed-send:broadcast", entry("broadcast", { kind: "broadcast" }));
    tab.storage.setItem("dispatch.keyed-send:newer", entry("newer", { version: 3 }));
    tab.storage.setItem(
      "dispatch.keyed-send:older",
      entry("older", { login: undefined, version: 1 })
    );
    tab.storage.setItem("dispatch.keyed-send:damaged", "{");
    tab.storage.setItem("dispatch.keyed-send:moved", entry("elsewhere"));
    tab.storage.setItem("dispatch.keyed-send:bobs", entry("bobs", { login: "bob" }));
    tab.storage.setItem("dispatch.keyed-send:refused-record", entry("refused-record", { send: 7 }));
    tab.storage.setItem(
      "dispatch.keyed-send:refused-unsaid",
      entry("refused-unsaid", { state: "refused" })
    );
    tab.storage.setItem("someone-else", "{");

    const later = tab.page();
    expect(later.load("session", "alice", accept)).toEqual([
      { key: "kept", kind: "session", refusal: null, send: { ok: true }, state: "late" },
    ]);
    expect(
      Array.from({ length: tab.storage.length }, (_, index) => tab.storage.key(index)).sort()
    ).toEqual(["dispatch.keyed-send:broadcast", "dispatch.keyed-send:kept", "someone-else"]);
    expect(later.load("broadcast", "alice", accept)).toEqual([
      { key: "broadcast", kind: "broadcast", refusal: null, send: { ok: true }, state: "sending" },
    ]);
  } finally {
    tab.close();
  }
});

// A refusal comes back as the error it was: the reader's stop, a reload that interrupted the send
// too long ago, Dispatch's verdict with its status and code, or another error by its message.
test("a kept refusal is rebuilt as the error it was", () => {
  const verdict = new ApiError(409, { code: "MESSAGE_KEY_REUSED", error: "the key is used" });
  const rebuilt = refusalFrom(keptRefusal(verdict));
  expect(rebuilt).toBeInstanceOf(ApiError);
  expect(rebuilt).toMatchObject({ code: "MESSAGE_KEY_REUSED", message: "the key is used" });
  expect((rebuilt as ApiError).status).toBe(409);
  expect(refusalFrom(keptRefusal(new SendStoppedError()))).toBeInstanceOf(SendStoppedError);
  const interrupted = refusalFrom(keptRefusal(new SendInterruptedError()));
  expect(interrupted).toBeInstanceOf(SendInterruptedError);
  expect(interrupted.message).toBe(
    "Not sent: this page was closed before Dispatch confirmed it. Retry sends it under the same key, so it is delivered at most once."
  );
  expect(refusalFrom(keptRefusal(new Error("no route")))).toMatchObject({ message: "no route" });
});

// W7. A send kept for longer than five minutes is not made again on its own: the reader who wrote
// it may no longer mean it, and the page asks them instead.
test("a send resumes on its own only within five minutes of its press", () => {
  const now = Date.parse("2026-10-25T12:00:00Z");
  expect(resumesOnItsOwn(now, now)).toBe(true);
  expect(resumesOnItsOwn(now - KEPT_SEND_RESUME_MS, now)).toBe(true);
  expect(resumesOnItsOwn(now - KEPT_SEND_RESUME_MS - 1, now)).toBe(false);
  expect(KEPT_SEND_RESUME_MS).toBe(5 * 60_000);
});

// W6. Signing out ends the kept sends with the session that pressed them: nothing of the person
// who has gone is left for the next one to resume, and the page stops asking to confirm leaving.
test("clearing drops every kept send and the leave prompt with it", () => {
  const tab = keptTab();
  const unload = new EventTarget();
  const page = new KeyedSendStorage(tab.storage, unload);

  try {
    page.keep(
      { key: "out", kind: "session", refusal: null, send: { ok: true }, state: "sending" },
      "alice"
    );
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
// never leave an older state standing for the send: the entry goes instead, so a reload resumes
// nothing rather than resuming a send the reader has since stopped.
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
  const page = new KeyedSendStorage(refusing, new EventTarget());

  try {
    page.keep(
      { key: "out", kind: "session", refusal: null, send: { ok: true }, state: "late" },
      "alice"
    );
    expect(tab.kept("out")).toMatchObject({ state: "late" });

    page.keep(
      { key: "out", kind: "session", refusal: null, send: { ok: true }, state: "stopping" },
      "alice"
    );
    expect(tab.kept("out")).toBeUndefined();
  } finally {
    tab.close();
  }
});

// A page with no storage at all - a browser that refuses the tab one - keeps nothing and still
// asks to confirm leaving while a send is unconfirmed: the send itself is unaffected.
test("a page with no storage keeps nothing and still asks before the tab is closed", () => {
  const unload = new EventTarget();
  const page = new KeyedSendStorage(undefined, unload);
  const asks = () => {
    const leaving = new Event("beforeunload", { cancelable: true });
    unload.dispatchEvent(leaving);
    return leaving.defaultPrevented;
  };

  page.keep(
    { key: "out", kind: "session", refusal: null, send: { ok: true }, state: "sending" },
    "alice"
  );
  expect(asks()).toBe(true);
  expect(page.load("session", "alice", accept)).toEqual([]);

  page.forget("out");
  expect(asks()).toBe(false);
});
