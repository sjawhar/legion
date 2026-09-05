import { describe, expect, it } from "bun:test";
import {
  DISPATCH_ARGUMENTS,
  DISPATCH_TOOL_NAME,
  DISPATCH_URGENCIES,
  DispatchArgumentError,
  isContinueCall,
  parseDispatchCall,
} from "../dispatch-contract";

describe("parseDispatchCall", () => {
  it("accepts an opening call and keeps every field", () => {
    const call = parseDispatchCall({
      subject: "s",
      context: "c",
      question: "q",
      urgency: "high",
      repo: "acme-org/example-repo",
      parent: "42",
      ask: [{ question: "Color?", header: "Color", options: [{ label: "red" }] }],
    });
    expect(isContinueCall(call)).toBe(false);
    expect(call).toEqual({
      subject: "s",
      context: "c",
      question: "q",
      urgency: "high",
      repo: "acme-org/example-repo",
      parent: "42",
      ask: [{ question: "Color?", header: "Color", options: [{ label: "red" }] }],
    });
  });

  it("accepts a continuing call", () => {
    const call = parseDispatchCall({ thread: "17158", context: "c", question: "q" });
    expect(isContinueCall(call)).toBe(true);
    expect(call).toEqual({ thread: "17158", context: "c", question: "q" });
  });

  it("treats a key with an undefined value as absent", () => {
    const call = parseDispatchCall({
      thread: "7",
      subject: undefined,
      context: "c",
      question: "q",
    });
    expect(isContinueCall(call)).toBe(true);
  });

  it("rejects a call that names both subject and thread", () => {
    expect(() =>
      parseDispatchCall({ subject: "s", thread: "7", context: "c", question: "q" })
    ).toThrow("dispatch: pass either subject (open a thread) or thread (continue one), not both");
  });

  it("rejects a call that names neither", () => {
    expect(() => parseDispatchCall({ context: "c", question: "q" })).toThrow(
      "dispatch: subject or thread is required"
    );
  });

  it("rejects opening-only arguments on a continuing call", () => {
    for (const extra of [{ urgency: "high" }, { repo: "o/r" }, { parent: "1" }]) {
      expect(() =>
        parseDispatchCall({ thread: "7", context: "c", question: "q", ...extra })
      ).toThrow("dispatch: thread cannot be combined with urgency, repo, or parent");
    }
  });

  it("rejects shape errors naming the path", () => {
    const thrown = (() => {
      try {
        parseDispatchCall({ subject: "s", context: "c", question: 5 });
        return null;
      } catch (error) {
        return error;
      }
    })();
    expect(thrown).toBeInstanceOf(DispatchArgumentError);
    expect((thrown as Error).message).toStartWith("dispatch: invalid arguments — question:");
  });

  it("rejects unknown keys", () => {
    expect(() =>
      parseDispatchCall({ subject: "s", context: "c", question: "q", origin: {} })
    ).toThrow(DispatchArgumentError);
  });

  it("exports the tool name, urgencies, and a description for every argument", () => {
    expect(DISPATCH_TOOL_NAME).toBe("dispatch");
    expect(DISPATCH_URGENCIES).toEqual(["low", "med", "high", "blocking"]);
    expect(Object.keys(DISPATCH_ARGUMENTS).sort()).toEqual([
      "ask",
      "context",
      "parent",
      "question",
      "repo",
      "subject",
      "thread",
      "urgency",
    ]);
  });
});
