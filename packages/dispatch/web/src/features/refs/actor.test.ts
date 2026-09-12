import { expect, test } from "bun:test";

import { actorLabel } from "./actor";

test("labels a personally attributed session with its human owner", () => {
  expect(
    actorLabel({
      id: "architect-session",
      kind: "session",
      origin: { session_title: "Architect" },
      owner: "alice",
    })
  ).toBe("Architect (for alice)");
});
