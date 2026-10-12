import { beforeEach, describe, expect, test } from "bun:test";
import {
  dropInjectedUserTurn,
  endInjectedUserTurns,
  forgetInjectedUserTurns,
  matchInjectedUserTurn,
  noteInjectedUserTurn,
  noteTypedUserTurn,
} from "./injected-user-turns";
import { envoyPluginInterface, resetEnvoyPluginInterfaceForTests } from "./interface";

/** A user message as the host records one: a prompt's text part, stamped once. */
function user(timestamp: number, text: string) {
  return { content: [{ text, type: "text" }], role: "user", timestamp };
}

beforeEach(() => {
  resetEnvoyPluginInterfaceForTests();
});

/**
 * The record of the turns envoy.ts sent in, which the stream's tag and Legion's phase-stall check
 * read the same answer from: a turn sent through `pi.sendUserMessage` is found by its text, one
 * sent through `pi.sendUserInput` by the tag the host kept.
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

  // A Send that lands after the run's last steer poll runs as a turn of its own, after the run's
  // `agent_end`. Its tag can only ever name its own message, so the run's end keeps it: otherwise
  // the page shows that Send twice and Legion's phase-stall check counts it as the daemon's
  // assignment. What a text match would find, and the answers already given, still go.
  test("sent as typed input are still found by their tag after the session's run ends", () => {
    noteTypedUserTurn("s1", "m-1");
    noteInjectedUserTurn("s1", "ship it", "m-2");
    endInjectedUserTurns("s1");

    expect(envoyPluginInterface().injectedUserTurns.get("s1")).toEqual({
      found: new Map(),
      sent: [{ messageId: "m-1", via: "typed" }],
    });
    expect(matchInjectedUserTurn("s1", user(20, "ship it"))).toBeUndefined();
    expect(matchInjectedUserTurn("s1", { ...user(30, "/sdd plan"), tag: "m-1" })).toBe("m-1");
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
    noteTypedUserTurn("s1", "m-1");
    noteTypedUserTurn("s1", "m-2");
    noteTypedUserTurn("s1", "m-3");

    const expanded = { ...user(20, "Review the code under src for bugs."), tag: "m-1" };
    expect(matchInjectedUserTurn("s1", expanded)).toBe("m-1");
    expect(matchInjectedUserTurn("s1", { ...user(30, "ship it"), tag: "m-3" })).toBe("m-3");
    expect(matchInjectedUserTurn("s1", { ...user(40, "ship it"), tag: "m-2" })).toBe("m-2");
  });

  // Two Sends can be submitted in one host millisecond, so two tagged messages can share a
  // timestamp. Each is its own Dispatch message, and asking about either again gives its own answer.
  test("tell apart two tagged messages the host stamped in the same millisecond", () => {
    noteTypedUserTurn("s1", "m-1");
    noteTypedUserTurn("s1", "m-2");
    const first = { ...user(20, "ship it"), tag: "m-1" };
    const second = { ...user(20, "ship it"), tag: "m-2" };

    expect(matchInjectedUserTurn("s1", first)).toBe("m-1");
    expect(matchInjectedUserTurn("s1", second)).toBe("m-2");
    expect(matchInjectedUserTurn("s1", first)).toBe("m-1");
    expect(matchInjectedUserTurn("s1", second)).toBe("m-2");
  });

  // A message's identity is its host timestamp and its tag, the identity the stream keys it by:
  // the memo answers by it, so a message at another timestamp is matched against the record rather
  // than taken from an earlier message's answer.
  test("answer a message by its timestamp and tag, never by an earlier message's tag alone", () => {
    noteTypedUserTurn("s1", "m-1");
    const first = { ...user(10, "ship it"), tag: "m-1" };
    expect(matchInjectedUserTurn("s1", first)).toBe("m-1");
    expect(matchInjectedUserTurn("s1", first)).toBe("m-1");
    expect(matchInjectedUserTurn("s1", { ...user(20, "ship it"), tag: "m-1" })).toBeUndefined();
  });

  // A typed command that submits nothing (`/compact`, `/session`) leaves its turn noted until the
  // host answers. A user message the tag does not name (the daemon's assignment, text typed at the
  // terminal) is never that turn, whatever its text: otherwise Legion's phase-stall check counts
  // the daemon's assignment as a person's message and leaves the phase closed.
  test("sent as typed input are found by their tag alone, never by a message's text", () => {
    noteTypedUserTurn("s1", "m-1");

    expect(matchInjectedUserTurn("s1", user(10, "/compact"))).toBeUndefined();
    expect(matchInjectedUserTurn("s1", { ...user(20, "/compact"), tag: "m-1" })).toBe("m-1");
  });

  // A person's `/skill:<name>` becomes the host's `skill-prompt` custom message, tagged like a
  // user message; Legion's phase-stall check counts it as that person's message only if the record
  // finds it. A custom message without the tag (an Envoy card) is nobody's turn.
  test("include the skill prompt a typed /skill: became, found by its tag", () => {
    noteTypedUserTurn("s1", "m-1");
    noteInjectedUserTurn("s1", "Run the review skill on src.", "m-2");
    const skill = (timestamp: number, tag?: string) => ({
      attribution: "user",
      content: "Run the review skill on src.",
      customType: "skill-prompt",
      display: true,
      role: "custom",
      timestamp,
      ...(tag !== undefined && { tag }),
    });

    expect(matchInjectedUserTurn("s1", skill(10))).toBeUndefined();
    expect(matchInjectedUserTurn("s1", user(15, "Run the review skill on src."))).toBe("m-2");
    expect(matchInjectedUserTurn("s1", skill(20, "m-1"))).toBe("m-1");
  });

  // The record is the interface both plugins read (`interface.ts`); a turn the host answered as
  // submitting nothing is no longer one of the session's sent turns.
  test("sent as typed input leave the record once the host answers that nothing was submitted", () => {
    noteTypedUserTurn("s1", "m-1");
    noteTypedUserTurn("s1", "m-2");
    dropInjectedUserTurn("s1", "m-1");

    expect(envoyPluginInterface().injectedUserTurns.get("s1")?.sent).toEqual([
      { messageId: "m-2", via: "typed" },
    ]);
  });

  // Matching always asks under the live session id (envoy.ts's stream recorder, legion.ts's
  // phase-stall check), so once the session changes nothing asks under the id it left again.
  test("of a session the process no longer serves are forgotten whole", () => {
    noteTypedUserTurn("s1", "m-1");
    noteInjectedUserTurn("s1", "ship it", "m-2");
    noteTypedUserTurn("s2", "m-3");
    forgetInjectedUserTurns("s1");

    expect(envoyPluginInterface().injectedUserTurns.has("s1")).toBe(false);
    expect(envoyPluginInterface().injectedUserTurns.get("s2")?.sent).toEqual([
      { messageId: "m-3", via: "typed" },
    ]);
  });
});
