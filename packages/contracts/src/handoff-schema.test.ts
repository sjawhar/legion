import { expect, test } from "bun:test";
import {
  HANDOFF_PHASES,
  LEGION_DIR_NAME,
  PHASE_FILE_NAMES,
  validatePhaseHandoff,
} from "./handoff-schema";

test("owns only file-backed Legion handoff phases and rejects retro", () => {
  expect(HANDOFF_PHASES).toEqual(["architect", "plan", "implement", "test", "review"]);
  expect(PHASE_FILE_NAMES).toEqual({
    architect: "architect.json",
    plan: "plan.json",
    implement: "implement.json",
    test: "test.json",
    review: "review.json",
  });
  expect(LEGION_DIR_NAME).toBe(".legion");
  expect(
    validatePhaseHandoff({
      schemaVersion: 1,
      phase: "implement",
      completed: "2026-08-24T00:00:00.000Z",
      filesChanged: ["src/worker.ts"],
    })
  ).toMatchObject({ phase: "implement", filesChanged: ["src/worker.ts"] });
  expect(
    validatePhaseHandoff({
      schemaVersion: 1,
      phase: "retro",
      completed: "2026-08-24T00:00:00.000Z",
    })
  ).toBeNull();
});
