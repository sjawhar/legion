import { describe, expect, test } from "bun:test";
import { join } from "node:path";
import { scriptFunctions } from "./script-functions";

// claim_states_are ISSUE ROLE STATES OTHERS (lib/workflow.sh), the predicate Stage 4b's done,
// production-check and production-check-bound checkpoints wait on: the issue's six claims, ROLE's
// in one of the states STATES names and every other in one of OTHERS, as a close leaves an issue
// whose production check keeps the role the project's `production_check` names apart from the
// rest. An empty ROLE spares none, which is how the close of a project whose `production_check`
// is off leaves the six. It is taken from the script by name and run over a claims_list stub
// answering `legion claims list --json`'s shape. The second Stage 4b run at 4ab26ee5 failed
// `done` after 300 s with the daemon log and the checking role's transcript showing the state the
// predicate names (six suspended at the root's close, that role resumed by the look's start row
// and idle with the task text): the predicate itself must hold on that listing, and refuse one
// that differs.
const fn = scriptFunctions(join(import.meta.dir, "workflow.sh"));

function holds(
  claims: { issue: string; role: string; state: string }[],
  role: string,
  states: string,
  others: string
) {
  const result = Bun.spawnSync(
    [
      "bash",
      "-c",
      `set -uo pipefail
claims_list() { printf '%s' "$CLAIMS"; }
${fn("claim_states_are")}
claim_states_are LEGSMOKE-817 "$ROLE" "$STATES" "$OTHERS"
`,
    ],
    {
      env: {
        PATH: process.env.PATH ?? "",
        CLAIMS: JSON.stringify({ claims }),
        ROLE: role,
        STATES: states,
        OTHERS: others,
      },
    }
  );
  return { ok: result.exitCode === 0, stderr: result.stderr.toString() };
}

const closed = [
  { issue: "LEGSMOKE-817", role: "architect", state: "suspended" },
  { issue: "LEGSMOKE-817", role: "implementer", state: "suspended" },
  { issue: "LEGSMOKE-817", role: "merger", state: "suspended" },
  { issue: "LEGSMOKE-817", role: "planner", state: "suspended" },
  { issue: "LEGSMOKE-817", role: "reviewer", state: "suspended" },
  { issue: "LEGSMOKE-817", role: "tester", state: "idle" },
  // Another issue's claim is not this issue's.
  { issue: "LEGSMOKE-825", role: "planner", state: "working" },
];

const allSuspended = closed.map((c) =>
  c.issue === "LEGSMOKE-817" ? { ...c, state: "suspended" } : c
);

describe("claim_states_are", () => {
  test("holds for the close's six claims: the tester live at its look, the five others suspended", () => {
    const got = holds(closed, "tester", "ready working idle", "suspended");
    expect(got.stderr).toBe("");
    expect(got.ok).toBe(true);
  });

  test("holds for a pending report's six suspended claims and for the check's close retiring the tester", () => {
    expect(holds(allSuspended, "tester", "suspended", "suspended").ok).toBe(true);
    const retired = closed.map((c) => (c.role === "tester" ? { ...c, state: "retired" } : c));
    expect(holds(retired, "tester", "retired", "suspended").ok).toBe(true);
  });

  test("holds with no role spared for the six suspended claims an off project's close leaves", () => {
    expect(holds(allSuspended, "", "", "suspended").ok).toBe(true);
    // No claim is apart there, so a tester outside OTHERS fails like any other claim.
    expect(holds(closed, "", "", "suspended").ok).toBe(false);
  });

  test("refuses a tester outside the named states, another claim not suspended, and a listing short of six", () => {
    expect(holds(allSuspended, "tester", "ready working idle", "suspended").ok).toBe(false);
    const architectWorking = closed.map((c) =>
      c.role === "architect" ? { ...c, state: "working" } : c
    );
    expect(holds(architectWorking, "tester", "ready working idle", "suspended").ok).toBe(false);
    expect(
      holds(
        closed.filter((c) => c.role !== "merger"),
        "tester",
        "ready working idle",
        "suspended"
      ).ok
    ).toBe(false);
  });
});
