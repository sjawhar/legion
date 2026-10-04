import { beforeEach, describe, expect, test } from "bun:test";
import {
  endInjectedUserTurns,
  matchInjectedUserTurn,
  noteInjectedUserTurn,
  resetInjectedUserTurnsForTests,
} from "./dispatch-user-turn";

/** A user message as the host records one: a prompt's text part, stamped once. */
function user(timestamp: number, text: string) {
  return { content: [{ text, type: "text" }], role: "user", timestamp };
}

beforeEach(() => {
  resetInjectedUserTurnsForTests();
});

/**
 * The host links a user message to nothing the extension sent, so the record of the turns
 * envoy.ts sent in finds each by its text, and the stream's tag and Legion's phase-stall check
 * read the same answer from it.
 */
describe("the user turns sent into a session", () => {
  test("are each the first user message with their text; other text and a later repeat are not", () => {
    noteInjectedUserTurn("s1", "ship it", "m-1");

    expect(matchInjectedUserTurn("s1", user(10, "typed at the terminal"))).toBeUndefined();
    expect(matchInjectedUserTurn("s1", user(20, "ship it"))).toBe("m-1");
    expect(matchInjectedUserTurn("s1", user(30, "ship it"))).toBeUndefined();
  });

  test("answer the same for one user message whichever extension asks first", () => {
    noteInjectedUserTurn("s1", "ship it", "m-1");
    const message = user(20, "ship it");

    expect(matchInjectedUserTurn("s1", message)).toBe("m-1");
    expect(matchInjectedUserTurn("s1", message)).toBe("m-1");
  });

  test("are not looked for once the session's run ends", () => {
    noteInjectedUserTurn("s1", "ship it", "m-1");
    endInjectedUserTurns("s1");

    expect(matchInjectedUserTurn("s1", user(20, "ship it"))).toBeUndefined();
  });

  test("belong to the session they were sent into", () => {
    noteInjectedUserTurn("s1", "ship it", "m-1");
    endInjectedUserTurns("s2");

    expect(matchInjectedUserTurn("s2", user(20, "ship it"))).toBeUndefined();
    expect(matchInjectedUserTurn("s1", user(20, "ship it"))).toBe("m-1");
  });

  test("are never an assistant's message", () => {
    noteInjectedUserTurn("s1", "ship it", "m-1");

    expect(
      matchInjectedUserTurn("s1", { content: "ship it", role: "assistant", timestamp: 20 })
    ).toBeUndefined();
    expect(matchInjectedUserTurn("s1", user(21, "ship it"))).toBe("m-1");
  });
});
