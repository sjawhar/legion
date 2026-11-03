import { expect, test } from "bun:test";

import type { ActorAct, AskResolution } from "../../api/types";
import { actorActText, askResolutionBadge, askResolutionReason } from "./actor";

test("a person who acted themselves is the person alone", () => {
  expect(actorActText("alice", undefined)).toBe("alice");
  // An act that names no agent - neither the session a broker token proved nor the personal
  // token - has no agent to show beside the person.
  expect(actorActText("alice", { scopes: ["person"] })).toBe("alice");
});

test("an act names the session it was made through, else the token", () => {
  const act: ActorAct = {
    session: "0123456789abcdef",
    token_id: "token-1",
    token_name: "architect",
    scopes: ["person", "approve"],
  };
  expect(actorActText("alice", act)).toBe("alice, acting through session 0123456789abcdef");
  expect(actorActText("alice", { token_id: "token-1", token_name: "architect" })).toBe(
    "alice, acting through token architect"
  );
});

test("a withdrawal drops the server's reason, which only repeats the actor sentence", () => {
  const retracted: AskResolution = {
    actor: { id: "0123456789abcdef", kind: "session" },
    at: "2026-10-25T00:00:00Z",
    kind: "retracted",
    reason: "Duplicate.",
  };
  const resolved: AskResolution = { ...retracted, kind: "resolved", reason: "Done." };
  const withdrawn: AskResolution = {
    ...retracted,
    actor: { id: "alice", kind: "user" },
    kind: "withdrawn",
    reason: "Withdrawn by the close of CORE-1.",
  };

  expect(askResolutionBadge(withdrawn)).toBe("Withdrawn");
  expect(askResolutionReason(withdrawn)).toBeUndefined();
  expect(askResolutionBadge(retracted)).toBe("Retracted");
  expect(askResolutionReason(retracted)).toBe("Duplicate.");
  expect(askResolutionBadge(resolved)).toBe("Resolved");
  expect(askResolutionReason(resolved)).toBe("Done.");
});
