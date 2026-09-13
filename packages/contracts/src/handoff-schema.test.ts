import { expect, test } from "bun:test";
import {
  describePhaseHandoffProblems,
  HANDOFF_PHASES,
  LEGION_DIR_NAME,
  PHASE_FILE_NAMES,
  validatePhaseHandoff,
} from "./handoff-schema";

const proof = {
  criterion: "1",
  surface: "branch CLI in a scratch workspace",
  command: "bun packages/daemon/src/cli/index.ts handoff write --phase implement",
  observed: "exit 1 naming proof",
  headSha: "0123456789abcdef0123456789abcdef01234567",
  negativeControl: "the same payload without proof -> exit 1",
};

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
      proof: [proof],
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

test("an implement handoff needs at least one complete production-like proof", () => {
  const base = { schemaVersion: 1, phase: "implement", completed: "2026-09-13T00:00:00.000Z" };
  expect(validatePhaseHandoff(base)).toBeNull();
  expect(describePhaseHandoffProblems(base).join("; ")).toContain("proof");
  expect(validatePhaseHandoff({ ...base, proof: [] })).toBeNull();
  const { negativeControl: _dropped, ...incomplete } = proof;
  expect(describePhaseHandoffProblems({ ...base, proof: [incomplete] }).join("; ")).toContain(
    "proof.0.negativeControl"
  );
  expect(validatePhaseHandoff({ ...base, proof: [proof] })).toMatchObject({ proof: [proof] });
});

test("a test handoff carries its verdict on the implementer's proof, and its own unless it reports a failure", () => {
  const base = { schemaVersion: 1, phase: "test", completed: "2026-09-13T00:00:00.000Z" };
  expect(describePhaseHandoffProblems({ ...base, passed: 3, failed: 0 }).join("; ")).toContain(
    "implementerProof"
  );
  const verified = {
    ...base,
    passed: 3,
    failed: 0,
    implementerProof: { verdict: "verified", how: "re-ran its command" },
  };
  expect(validatePhaseHandoff(verified)).toBeNull();
  expect(describePhaseHandoffProblems(verified).join("; ")).toContain("proof");
  expect(validatePhaseHandoff({ ...verified, proof: [proof] })).toMatchObject({ proof: [proof] });
  const rejected = {
    ...base,
    failed: 1,
    failures: [{ criterion: "production-like proof", evidence: "implement.json carried none" }],
    implementerProof: { verdict: "rejected", how: "read .legion/implement.json and the PR body" },
  };
  expect(validatePhaseHandoff(rejected)).toMatchObject({
    implementerProof: { verdict: "rejected" },
  });
  expect(
    validatePhaseHandoff({
      ...base,
      failed: 2,
      implementerProof: { verdict: "rejected", how: "re-ran it" },
    })
  ).not.toBeNull();
});
