import { describe, expect, test } from "bun:test";
import {
  branchHoldsNotice,
  firstTurnNoticeMessage,
  isNoticeCopy,
  withoutRepeatedNotice,
} from "./first-turn-notice";

const TYPE = "legion-first-notice";
const OTHER = "legion-other-notice";

/** A branch entry as Oh My Pi stores a sent custom message. */
const entry = (customType: string, id: string) => ({
  type: "custom_message",
  customType,
  content: "…",
  display: true,
  details: { id },
});
/** The same message as one provider request carries it. */
const copy = (customType: string, id: string) => ({
  role: "custom",
  customType,
  content: "…",
  details: { id },
});
const turn = (text: string) => ({ role: "user", content: [{ type: "text", text }] });

test("the message carries the notice's text and id under the type it is saved as", () => {
  expect(firstTurnNoticeMessage({ id: "n-1", content: "the news" }, TYPE)).toEqual({
    customType: TYPE,
    content: "the news",
    display: true,
    details: { id: "n-1" },
  });
});

describe("a copy of one notice", () => {
  test("is the entry or the request message of that type and id, in that shape alone", () => {
    expect(isNoticeCopy(entry(TYPE, "n-1"), "entry", TYPE, "n-1")).toBe(true);
    expect(isNoticeCopy(copy(TYPE, "n-1"), "message", TYPE, "n-1")).toBe(true);
    // An entry is no request message and a request message is no entry: each kind names its own.
    expect(isNoticeCopy(entry(TYPE, "n-1"), "message", TYPE, "n-1")).toBe(false);
    expect(isNoticeCopy(copy(TYPE, "n-1"), "entry", TYPE, "n-1")).toBe(false);
  });

  test("is never another notice's, another process's, or something that carries no id", () => {
    expect(isNoticeCopy(entry(OTHER, "n-1"), "entry", TYPE, "n-1")).toBe(false);
    expect(isNoticeCopy(entry(TYPE, "n-2"), "entry", TYPE, "n-1")).toBe(false);
    for (const value of [
      undefined,
      null,
      "n-1",
      turn("TURN-1"),
      { type: "custom_message", customType: TYPE },
      { type: "custom_message", customType: TYPE, details: null },
      { type: "custom_message", customType: TYPE, details: {} },
    ]) {
      expect(isNoticeCopy(value, "entry", TYPE, "n-1")).toBe(false);
    }
  });
});

describe("the branch a process asks about its notice", () => {
  test("holds it only while its own copy is on it", () => {
    const branch = [turn("TURN-1"), entry(TYPE, "n-1")];
    expect(branchHoldsNotice(branch, TYPE, "n-1")).toBe(true);
    expect(branchHoldsNotice(branch.slice(0, 1), TYPE, "n-1")).toBe(false);
  });

  test("holds neither another notice's copy nor an earlier process's", () => {
    const branch = [entry(OTHER, "n-1"), entry(TYPE, "an-earlier-process")];
    expect(branchHoldsNotice(branch, TYPE, "n-1")).toBe(false);
  });
});

describe("one provider request's messages", () => {
  test("keep the first copy of the resent notice and lose every later one", () => {
    const messages = [copy(TYPE, "n-1"), turn("TURN-2"), copy(TYPE, "n-1"), copy(TYPE, "n-1")];
    expect(withoutRepeatedNotice(messages, TYPE, "n-1")).toEqual([
      copy(TYPE, "n-1"),
      turn("TURN-2"),
    ]);
  });

  test("are left alone when they carry the notice once, or not at all", () => {
    expect(withoutRepeatedNotice([copy(TYPE, "n-1"), turn("TURN-2")], TYPE, "n-1")).toBeUndefined();
    expect(withoutRepeatedNotice([turn("TURN-2")], TYPE, "n-1")).toBeUndefined();
  });

  test("keep every copy of another notice and of another process's, wherever they sit", () => {
    const messages = [
      copy(OTHER, "n-1"),
      copy(TYPE, "an-earlier-process"),
      copy(TYPE, "n-1"),
      copy(OTHER, "n-1"),
      copy(TYPE, "n-1"),
    ];
    expect(withoutRepeatedNotice(messages, TYPE, "n-1")).toEqual([
      copy(OTHER, "n-1"),
      copy(TYPE, "an-earlier-process"),
      copy(TYPE, "n-1"),
      copy(OTHER, "n-1"),
    ]);
  });
});
