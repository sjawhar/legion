import { expect, test } from "bun:test";

import { keptTab } from "../../__tests__/kept-sends-harness";
import { ApiError } from "../../api/client";
import { SendInterruptedError, SendStoppedError } from "./keyed-write";
import {
  KEPT_SEND_RESUME_MS,
  type KeptWrite,
  keptRefusal,
  keptRequest,
  refusalFrom,
  requestFrom,
  resumesOnItsOwn,
  WriteStorage,
} from "./write-storage";

/** One write as the store keeps it, with every field an entry carries. */
const kept = (key: string, fields: Partial<KeptWrite> = {}): KeptWrite => ({
  detail: null,
  key,
  kind: "write",
  label: "CORE-1 set to done",
  login: "alice",
  presentation: "indicator",
  pressedAt: 1_700_000_000_000,
  request: { body: { status: "done" }, method: "PATCH", path: "/api/v1/issues/CORE-1" },
  state: { status: "sending" },
  target: null,
  ...fields,
});

// Every write is kept in one format, one entry per key, versioned and stamped with the login of
// the person who pressed it, and a page takes up only that person's. An entry the page cannot
// read - another version's, damaged JSON, one filed under another key, one another person kept,
// a method no write goes as - is dropped as the page loads, never read. Every write method a page
// write goes as is one the format accepts, a PUT and a DELETE included (W29).
test("one versioned format, every write method kept, and an entry that does not parse dropped", () => {
  const tab = keptTab();
  const page = tab.page();
  const entry = (write: KeptWrite, fields: Record<string, unknown> = {}) =>
    JSON.stringify({ ...write, version: 1, ...fields });
  try {
    page.keep(kept("patch", { state: { status: "late", stopping: false } }));
    page.keep(kept("put", { request: { body: { until: null }, method: "PUT", path: "/x" } }));
    page.keep(kept("delete", { request: { method: "DELETE", path: "/y" } }));
    expect(tab.kept("patch")).toEqual({
      ...kept("patch", { state: { status: "late", stopping: false } }),
      version: 1,
    });
    tab.storage.setItem("dispatch.write:newer", entry(kept("newer"), { version: 2 }));
    tab.storage.setItem("dispatch.write:damaged", "{");
    tab.storage.setItem("dispatch.write:moved", entry(kept("elsewhere")));
    tab.storage.setItem("dispatch.write:bobs", entry(kept("bobs", { login: "bob" })));
    tab.storage.setItem(
      "dispatch.write:get",
      entry(kept("get"), { request: { method: "GET", path: "/z" } })
    );
    tab.storage.setItem(
      "dispatch.write:unsaid",
      entry(kept("unsaid"), { state: { status: "refused" } })
    );
    tab.storage.setItem("someone-else", "{");

    const later = tab.page();
    expect(later.load("alice").map((write) => write.key)).toEqual(["patch", "put", "delete"]);
    expect(
      Array.from({ length: tab.storage.length }, (_, index) => tab.storage.key(index)).sort()
    ).toEqual([
      "dispatch.write:delete",
      "dispatch.write:patch",
      "dispatch.write:put",
      "someone-else",
    ]);
  } finally {
    tab.close();
  }
});

// An upload's form is kept by its fields, its file's place among them, and made whole again with
// the file a reload found - or not at all without one.
test("an upload's request is kept by its fields and made whole with its file", () => {
  const file = new File(["spec"], "spec.md");
  const body = new FormData();
  body.set("name", "spec.md");
  body.set("file", file);
  const request = keptRequest({ body, method: "POST", path: "/api/v1/issues/CORE-1/artifacts" });
  expect(request).toEqual({
    form: [
      { name: "name", value: "spec.md" },
      { file: true, name: "file" },
    ],
    method: "POST",
    path: "/api/v1/issues/CORE-1/artifacts",
  });
  expect(requestFrom(request)).toBeUndefined();
  const whole = requestFrom(request, file)?.body as FormData;
  expect(whole.get("name")).toBe("spec.md");
  expect((whole.get("file") as File).name).toBe("spec.md");
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

// Signing out ends the kept writes with the session that made them: nothing of the person who has
// gone is left for the next one to resume, and the page stops asking to confirm leaving.
test("clearing drops every kept write and the leave prompt with it", () => {
  const tab = keptTab();
  const unload = new EventTarget();
  const page = new WriteStorage(tab.storage, unload);
  const asks = () => {
    const leaving = new Event("beforeunload", { cancelable: true });
    unload.dispatchEvent(leaving);
    return leaving.defaultPrevented;
  };
  try {
    page.keep(kept("out"));
    expect(asks()).toBe(true);
    page.clear();
    expect(tab.storage.length).toBe(0);
    expect(asks()).toBe(false);
  } finally {
    tab.close();
  }
});

// A write the storage refuses - it is full, or the browser refuses the page a quota - must never
// leave an older state standing for the write: the entry goes instead, so a reload resumes nothing
// rather than resuming a write the reader has since stopped.
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
      if (value.includes('"stopping":true')) throw new Error("QuotaExceededError");
      tab.storage.setItem(name, value);
    },
  };
  const page = new WriteStorage(refusing, new EventTarget());
  try {
    page.keep(kept("out", { state: { status: "late", stopping: false } }));
    expect(tab.kept("out")).toMatchObject({ state: { status: "late" } });
    page.keep(kept("out", { state: { status: "late", stopping: true } }));
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
  page.keep(kept("out"));
  expect(asks()).toBe(true);
  expect(page.load("alice")).toEqual([]);
  page.forget("out");
  expect(asks()).toBe(false);
});
