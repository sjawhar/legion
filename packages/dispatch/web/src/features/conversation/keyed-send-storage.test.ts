import { expect, test } from "bun:test";

import { keptTab } from "../../__tests__/kept-sends-harness";
import { ApiError } from "../../api/client";
import { keptRefusal, refusalFrom } from "./keyed-send-storage";
import { SendStoppedError } from "./send-request";

/** A record `parse` takes: the store's own, here a flag. */
const accept = (send: unknown) =>
  typeof send === "object" && send !== null && Reflect.get(send, "ok") === true
    ? { ok: true as const }
    : undefined;

// W1. Both stores keep their sends in one format, one entry per key and versioned, and each takes
// up only its own kind. An entry the page cannot read - another version's, damaged JSON, one
// filed under another key, or a record its store refuses - is dropped as the page loads, never
// read; the other store's entries stay for it.
test("one versioned format for both stores, and an entry that does not parse is dropped on load", () => {
  const tab = keptTab();
  const page = tab.page();
  const entry = (key: string, fields: Record<string, unknown> = {}) =>
    JSON.stringify({
      key,
      kind: "session",
      refusal: null,
      send: { ok: true },
      state: "sending",
      version: 1,
      ...fields,
    });

  try {
    page.keep({ key: "kept", kind: "session", refusal: null, send: { ok: true }, state: "late" });
    expect(tab.kept("kept")).toEqual({
      key: "kept",
      kind: "session",
      refusal: null,
      send: { ok: true },
      state: "late",
      version: 1,
    });
    tab.storage.setItem("dispatch.keyed-send:broadcast", entry("broadcast", { kind: "broadcast" }));
    tab.storage.setItem("dispatch.keyed-send:newer", entry("newer", { version: 2 }));
    tab.storage.setItem("dispatch.keyed-send:damaged", "{");
    tab.storage.setItem("dispatch.keyed-send:moved", entry("elsewhere"));
    tab.storage.setItem("dispatch.keyed-send:refused-record", entry("refused-record", { send: 7 }));
    tab.storage.setItem(
      "dispatch.keyed-send:refused-unsaid",
      entry("refused-unsaid", { state: "refused" })
    );
    tab.storage.setItem("someone-else", "{");

    const later = tab.page();
    expect(later.load("session", accept)).toEqual([
      { key: "kept", kind: "session", refusal: null, send: { ok: true }, state: "late" },
    ]);
    expect(
      Array.from({ length: tab.storage.length }, (_, index) => tab.storage.key(index)).sort()
    ).toEqual(["dispatch.keyed-send:broadcast", "dispatch.keyed-send:kept", "someone-else"]);
    expect(later.load("broadcast", accept)).toEqual([
      { key: "broadcast", kind: "broadcast", refusal: null, send: { ok: true }, state: "sending" },
    ]);
  } finally {
    tab.close();
  }
});

// A refusal comes back as the error it was: the reader's stop, Dispatch's verdict with its status
// and code, or another error by its message.
test("a kept refusal is rebuilt as the error it was", () => {
  const verdict = new ApiError(409, { code: "MESSAGE_KEY_REUSED", error: "the key is used" });
  const rebuilt = refusalFrom(keptRefusal(verdict));
  expect(rebuilt).toBeInstanceOf(ApiError);
  expect(rebuilt).toMatchObject({ code: "MESSAGE_KEY_REUSED", message: "the key is used" });
  expect((rebuilt as ApiError).status).toBe(409);
  expect(refusalFrom(keptRefusal(new SendStoppedError()))).toBeInstanceOf(SendStoppedError);
  expect(refusalFrom(keptRefusal(new Error("no route")))).toMatchObject({ message: "no route" });
});
