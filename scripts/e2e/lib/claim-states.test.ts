import { describe, expect, test } from "bun:test";
import { join } from "node:path";
import { scriptFunctions } from "./script-functions";

// claim_states_are ISSUE IMPLEMENTER OTHERS (lib/workflow.sh), the predicate Stage 4b's done,
// production-check and production-check-bound checkpoints wait on: the issue's six claims, the
// implementer's in one of the states IMPLEMENTER names and every other in one of OTHERS, as a
// close leaves an issue whose production check keeps the implementer apart from the rest. It is
// taken from the script by name and run over a claims_list stub answering `legion claims list
// --json`'s shape. The second Stage 4b run at 4ab26ee5 failed `done` after 300 s with the daemon
// log and the implementer's transcript showing the state the predicate names (six suspended at the
// root's close, the implementer resumed by the look's start row and idle with the task text): the
// predicate itself must hold on that listing, and refuse one that differs.
const fn = scriptFunctions(join(import.meta.dir, "workflow.sh"));

function holds(
  claims: { issue: string; role: string; state: string }[],
  implementer: string,
  others: string
) {
  const result = Bun.spawnSync(
    [
      "bash",
      "-c",
      `set -uo pipefail
claims_list() { printf '%s' "$CLAIMS"; }
${fn("claim_states_are")}
claim_states_are LEGSMOKE-817 "$IMPLEMENTER" "$OTHERS"
`,
    ],
    {
      env: {
        PATH: process.env.PATH ?? "",
        CLAIMS: JSON.stringify({ claims }),
        IMPLEMENTER: implementer,
        OTHERS: others,
      },
    }
  );
  return { ok: result.exitCode === 0, stderr: result.stderr.toString() };
}

const closed = [
  { issue: "LEGSMOKE-817", role: "architect", state: "suspended" },
  { issue: "LEGSMOKE-817", role: "implementer", state: "idle" },
  { issue: "LEGSMOKE-817", role: "merger", state: "suspended" },
  { issue: "LEGSMOKE-817", role: "planner", state: "suspended" },
  { issue: "LEGSMOKE-817", role: "reviewer", state: "suspended" },
  { issue: "LEGSMOKE-817", role: "tester", state: "suspended" },
  // Another issue's claim is not this issue's.
  { issue: "LEGSMOKE-825", role: "planner", state: "working" },
];

describe("claim_states_are", () => {
  test("holds for the close's six claims: the implementer live at its look, the five others suspended", () => {
    const got = holds(closed, "ready working idle", "suspended");
    expect(got.stderr).toBe("");
    expect(got.ok).toBe(true);
  });

  test("holds for a pending report's six suspended claims and for the check's close retiring the implementer", () => {
    const suspended = closed.map((c) =>
      c.role === "implementer" ? { ...c, state: "suspended" } : c
    );
    expect(holds(suspended, "suspended", "suspended").ok).toBe(true);
    const retired = closed.map((c) => (c.role === "implementer" ? { ...c, state: "retired" } : c));
    expect(holds(retired, "retired", "suspended").ok).toBe(true);
  });

  test("refuses an implementer outside the named states, another claim not suspended, and a listing short of six", () => {
    const stillSuspended = closed.map((c) =>
      c.role === "implementer" ? { ...c, state: "suspended" } : c
    );
    expect(holds(stillSuspended, "ready working idle", "suspended").ok).toBe(false);
    const architectWorking = closed.map((c) =>
      c.role === "architect" ? { ...c, state: "working" } : c
    );
    expect(holds(architectWorking, "ready working idle", "suspended").ok).toBe(false);
    expect(
      holds(
        closed.filter((c) => c.role !== "merger"),
        "ready working idle",
        "suspended"
      ).ok
    ).toBe(false);
  });
});
