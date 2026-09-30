import { expect, test } from "bun:test";
import {
  describePhaseHandoffProblems,
  describePhaseHandoffWriteProblems,
  HANDOFF_PHASES,
  type HandoffPhase,
  LEGION_DIR_NAME,
  PHASE_FILE_NAMES,
  PLAN_REVIEW_MAX_ROUNDS,
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
});

test("a blank or whitespace-only proof field is refused by name", () => {
  const base = { schemaVersion: 1, phase: "implement", completed: "2026-09-14T00:00:00.000Z" };
  const blank = { ...proof, observed: "   " };
  expect(validatePhaseHandoff({ ...base, proof: [blank] })).toBeNull();
  expect(describePhaseHandoffProblems({ ...base, proof: [blank] }).join("; ")).toContain(
    "proof.0.observed"
  );
  expect(validatePhaseHandoff({ ...base, proof: [{ ...proof, headSha: "" }] })).toBeNull();
  expect(
    validatePhaseHandoff({ ...base, proof: [{ ...proof, observed: "  exit 1  " }] })
  ).toMatchObject({ proof: [{ observed: "exit 1" }] });
});

test("a test handoff that reports a failure count records at least one failure", () => {
  const base = {
    schemaVersion: 1,
    phase: "test",
    completed: "2026-09-14T00:00:00.000Z",
    implementerProof: { verdict: "verified", how: "re-ran its command" },
  };
  expect(validatePhaseHandoff({ ...base, failed: 1 })).toBeNull();
  expect(describePhaseHandoffProblems({ ...base, failed: 1 }).join("; ")).toContain("failures");
  expect(validatePhaseHandoff({ ...base, failed: 1, failures: [] })).toBeNull();
  expect(
    validatePhaseHandoff({
      ...base,
      failed: 1,
      failures: [{ criterion: "2", evidence: "exit 0 where 1 was expected" }],
    })
  ).not.toBeNull();
});

test("a rejected implementer proof records at least one failure", () => {
  const base = {
    schemaVersion: 1,
    phase: "test",
    completed: "2026-09-14T00:00:00.000Z",
    implementerProof: { verdict: "rejected", how: "read .legion/implement.json" },
  };
  const cleanPass = { ...base, passed: 3, failed: 0, proof: [proof] };
  expect(validatePhaseHandoff(cleanPass)).toBeNull();
  expect(describePhaseHandoffProblems(cleanPass).join("; ")).toContain("failures");
  expect(validatePhaseHandoff({ ...cleanPass, failures: [] })).toBeNull();
  expect(
    validatePhaseHandoff({
      ...base,
      failures: [{ criterion: "proof", evidence: "implement.json carried none" }],
    })
  ).toMatchObject({ implementerProof: { verdict: "rejected" } });
});

test("a missing or unknown phase is reported as the expected list, never as Invalid input", () => {
  const expected = "phase: expected one of architect|plan|implement|test|review";
  const completed = "2026-09-14T00:00:00.000Z";
  expect(describePhaseHandoffProblems({ schemaVersion: 1, phase: "retro", completed })).toEqual([
    expected,
  ]);
  expect(describePhaseHandoffProblems({ schemaVersion: 1, completed })).toEqual([expected]);
  expect(describePhaseHandoffProblems({ schemaVersion: 1, phase: 7, completed })).toEqual([
    expected,
  ]);
});

test("an undeclared field survives validation at every phase", () => {
  const completed = "2026-09-14T00:00:00.000Z";
  const minimal: Record<HandoffPhase, object> = {
    architect: {},
    plan: {},
    implement: { proof: [proof] },
    test: { implementerProof: { verdict: "verified", how: "re-ran it" }, proof: [proof] },
    review: {},
  };
  for (const phase of HANDOFF_PHASES) {
    expect(
      validatePhaseHandoff({
        schemaVersion: 1,
        phase,
        completed,
        ...minimal[phase],
        undeclared: "kept",
      })
    ).toMatchObject({ phase, undeclared: "kept" });
  }
});

// Both plan checks recorded, so a test of another write rule sees only its own problems.
const planChecks = {
  gapAnalysis: { findings: [] },
  planReview: { verdict: "approved", rounds: 1 },
};
const skills = { implement: ["using-jj"], test: ["testing"], review: ["testing"] };

test("a plan is written only with a non-empty skill list per downstream role; an explicit none counts; reading stays tolerant", () => {
  const base = { schemaVersion: 1, phase: "plan", completed: "2026-09-18T00:00:00.000Z" };
  const legacy = { ...base, taskCount: 4 };
  expect(validatePhaseHandoff(legacy)?.phase).toBe("plan");
  expect(describePhaseHandoffProblems(legacy)).toEqual([]);
  expect(describePhaseHandoffWriteProblems({ ...legacy, ...planChecks })).toEqual([
    "requiredSkills: missing or empty — name the skills this role must load, or state `none: <what you looked through and why nothing fits>`",
  ]);
  expect(
    describePhaseHandoffWriteProblems({
      ...base,
      ...planChecks,
      requiredSkills: { implement: ["using-jj"], test: [], review: [" "] },
    })
  ).toEqual([
    "requiredSkills.test: missing or empty — name the skills this role must load, or state `none: <what you looked through and why nothing fits>`",
    "requiredSkills.review.0: missing or empty — name the skills this role must load, or state `none: <what you looked through and why nothing fits>`",
  ]);
  expect(
    describePhaseHandoffWriteProblems({
      ...base,
      ...planChecks,
      requiredSkills: {
        implement: ["none: no agent skills exist here yet"],
        test: ["none: same"],
        review: ["none: same"],
      },
    })
  ).toEqual([]);
  // Other phases are untouched by the write-only rule.
  expect(describePhaseHandoffWriteProblems({ ...base, phase: "architect" })).toEqual([]);
});

test("a plan is written only with its gap analysis and its plan review recorded; a failed check is recorded, never blocking; reading stays tolerant", () => {
  const base = {
    schemaVersion: 1,
    phase: "plan",
    completed: "2026-09-30T00:00:00.000Z",
    requiredSkills: skills,
  };
  const write = (checks: object) => describePhaseHandoffWriteProblems({ ...base, ...checks });
  const gapAnalysis = {
    findings: [{ finding: "no criterion checks the refusal", answer: "task 3's check" }],
  };
  const issue = { issue: "task 2 edits a missing file", evidence: "src/gone.ts does not exist" };
  const rejected = {
    verdict: "rejected",
    rounds: PLAN_REVIEW_MAX_ROUNDS,
    remainingIssues: [issue],
  };

  // A plan committed before the checks existed still reads; it is only refused a new write.
  expect(validatePhaseHandoff(base)?.phase).toBe("plan");
  expect(write({})).toEqual([
    "gapAnalysis: missing — record the gap analyst's `findings`, each with how the plan answers it (`[]` when it found none), or its failed call's `error`",
    "planReview: missing — record the plan review's `verdict` and `rounds`, with `remainingIssues` when it was rejected or `error` when a review's call failed",
  ]);

  expect(write({ gapAnalysis, planReview: { verdict: "approved", rounds: 2 } })).toEqual([]);
  // The reviewer still rejecting after the last round: the plan proceeds with what it named.
  expect(write({ gapAnalysis, planReview: rejected })).toEqual([]);
  // Both calls failed: each is recorded as its error, and the plan still goes ahead.
  expect(
    write({
      gapAnalysis: { error: "model call failed: 503" },
      planReview: { verdict: "failed", rounds: 1, error: "model call failed: 503" },
    })
  ).toEqual([]);

  expect(write({ gapAnalysis: { ...gapAnalysis, error: "x" }, planReview: rejected })).toEqual([
    "gapAnalysis: record either `findings` or the failed call's `error`, not both",
  ]);
  expect(
    write({ gapAnalysis: { findings: [{ finding: "a gap", answer: " " }] }, planReview: rejected })
  ).toEqual([expect.stringMatching(/^gapAnalysis\.findings\.0\.answer: /)]);
  expect(write({ gapAnalysis, planReview: { ...rejected, rounds: 1 } })).toEqual([
    "planReview.rounds: a review still rejecting after 1 of 3 rounds is revised and reviewed again, not recorded",
  ]);
  expect(write({ gapAnalysis, planReview: { ...rejected, remainingIssues: [] } })).toEqual([
    "planReview.remainingIssues: a rejected review records the blocking issues its last round named",
  ]);
  expect(
    write({ gapAnalysis, planReview: { ...rejected, remainingIssues: [{ issue: "x" }] } })
  ).toEqual([expect.stringMatching(/^planReview\.remainingIssues\.0\.evidence: /)]);
  expect(
    write({ gapAnalysis, planReview: { verdict: "approved", rounds: 1, remainingIssues: [issue] } })
  ).toEqual(["planReview.remainingIssues: an approved review leaves no blocking issue standing"]);
  expect(write({ gapAnalysis, planReview: { verdict: "failed", rounds: 2 } })).toEqual([
    "planReview.error: a failed review records its call's error, and only a failed review does",
  ]);
  expect(
    write({ gapAnalysis, planReview: { verdict: "approved", rounds: 1, error: "x" } })
  ).toEqual([
    "planReview.error: a failed review records its call's error, and only a failed review does",
  ]);
  expect(
    write({ gapAnalysis, planReview: { ...rejected, rounds: PLAN_REVIEW_MAX_ROUNDS + 1 } })
  ).toEqual([expect.stringMatching(/^planReview\.rounds: /)]);
});
