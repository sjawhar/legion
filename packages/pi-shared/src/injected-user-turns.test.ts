import { beforeEach, describe, expect, test } from "bun:test";
import {
  endInjectedUserTurns,
  matchInjectedUserTurn,
  noteInjectedUserTurn,
} from "./injected-user-turns";
import { resetEnvoyPluginInterfaceForTests } from "./interface";

/** A user message as the host records one: a prompt's text part, stamped once. */
function user(timestamp: number, text: string) {
  return { content: [{ text, type: "text" }], role: "user", timestamp };
}

beforeEach(() => {
  resetEnvoyPluginInterfaceForTests();
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

  // A turn sent through `pi.sendUserInput` carries its Dispatch id as the host's tag: the host
  // may rewrite its text (a prompt template, a file command), and the same words sent twice are
  // still two messages, which the tag tells apart where the text cannot.
  test("are found by the tag the host kept, whatever their text became", () => {
    noteInjectedUserTurn("s1", "/review src", "m-1");
    noteInjectedUserTurn("s1", "ship it", "m-2");
    noteInjectedUserTurn("s1", "ship it", "m-3");

    const expanded = { ...user(20, "Review the code under src for bugs."), tag: "m-1" };
    expect(matchInjectedUserTurn("s1", expanded)).toBe("m-1");
    expect(matchInjectedUserTurn("s1", { ...user(30, "ship it"), tag: "m-3" })).toBe("m-3");
    expect(matchInjectedUserTurn("s1", { ...user(40, "ship it"), tag: "m-2" })).toBe("m-2");
  });
});
