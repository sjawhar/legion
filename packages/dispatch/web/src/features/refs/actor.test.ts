import { expect, test } from "bun:test";

import type { AskResolution } from "../../api/types";
import { askResolutionBadge, askResolutionReason } from "./actor";

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
