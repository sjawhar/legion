import { expect, test } from "bun:test";

import type { DeliverySessionGroup } from "../../../api/types";
import { NO_SESSION } from "./facets";
import { parentAgentLabel } from "./parentAgent";

const group: DeliverySessionGroup = {
  created_at: "2026-10-11T00:00:00Z",
  created_by: { id: "alice@example.com", kind: "user" },
  id: "11111111-1111-1111-1111-111111111111",
  name: "Release crew",
  sessions: ["s1", "s2"],
  updated_at: "2026-10-11T00:00:00Z",
  updated_by: { id: "alice@example.com", kind: "user" },
};

test("a group's value reads as its name, a placeholder as its label and a session as itself", () => {
  expect(parentAgentLabel(`group:${group.id}`, [group])).toBe("Release crew");
  expect(parentAgentLabel(NO_SESSION, [group])).toBe("No session");
  expect(parentAgentLabel("Planner (s3)", [group])).toBe("Planner (s3)");
});

test("a group the answer no longer names reads as its value, not as a guess", () => {
  expect(parentAgentLabel("group:gone", [group])).toBe("group:gone");
});
