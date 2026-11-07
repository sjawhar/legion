import { expect, test } from "bun:test";
import { readFileSync } from "node:fs";
import path from "node:path";
import { LEGION_ROLES } from "@legion/contracts";
import { DEFAULT_STALL_LIMITS, defaultCallBoundMs, stallLimits } from "./stall-limits";

/** Every role word a session's `LEGION_ROLE` can hold: the six issue roles and the controller,
 * which is no `LegionRole` and whose bound is every other role's. */
const ROLE_WORDS: readonly (string | undefined)[] = [...LEGION_ROLES, "controller", undefined];

/** The log `stallLimits` warns to, with the warnings it was given. */
function log(): { readonly warn: (message: string) => void; readonly warnings: string[] } {
  const warnings: string[] = [];
  return { warn: (message) => warnings.push(message), warnings };
}

const HOUR = 3_600_000;
const FOUR_HOURS = 14_400_000;

// The negative control of the whole file: an environment that names nothing leaves every number
// at its default, and in particular at a default and not at zero — a bound of 0 is the call watch
// switched off, which an unset variable must never mean.
test("an unset environment leaves every number at its default, none of them zero", () => {
  for (const role of ROLE_WORDS) {
    const warn = log();
    const limits = stallLimits({}, warn, role);
    expect(limits, `${role ?? "no role"} on an empty environment`).toEqual({
      ...DEFAULT_STALL_LIMITS,
      callBoundMs: defaultCallBoundMs(role),
    });
    for (const [name, value] of Object.entries(limits)) {
      expect(value, `${name} for ${role ?? "no role"}`).toBeGreaterThan(0);
    }
    expect(warn.warnings).toEqual([]);
  }
});

test("the bound's default is four hours for an implementer and a reviewer and an hour for every other role", () => {
  const bounds = new Map(
    ROLE_WORDS.map((role) => [role, stallLimits({}, log(), role).callBoundMs])
  );

  expect(bounds.get("implementer")).toBe(FOUR_HOURS);
  expect(bounds.get("reviewer")).toBe(FOUR_HOURS);
  for (const role of ROLE_WORDS) {
    if (role === "implementer" || role === "reviewer") continue;
    expect(bounds.get(role), `${role ?? "no role"}`).toBe(HOUR);
  }
  // The exported resolver is the one the daemon's fixture is checked against below, so it answers
  // exactly what the function reads.
  for (const [role, bound] of bounds) {
    expect(defaultCallBoundMs(role), `${role ?? "no role"}`).toBe(bound);
  }
});

test("the environment's value overrides every number, and the bound for every role alike", () => {
  const env = {
    LEGION_STALL_CALL_GRACE_MS: "11000",
    LEGION_STALL_CALL_BOUND_MS: "22000",
    LEGION_STALL_STEP_MS: "33000",
  };
  for (const role of ROLE_WORDS) {
    const warn = log();
    const limits = stallLimits(env, warn, role);
    expect(
      [limits.callGraceMs, limits.callBoundMs, limits.stepMs],
      `${role ?? "no role"} on the deployment's values`
    ).toEqual([11_000, 22_000, 33_000]);
    expect(warn.warnings).toEqual([]);
  }
});

test("a value that is not a whole number of zero or more leaves one warning and the default", () => {
  for (const bad of ["", " ", "nine", "1.5", "-1", "1e3ms"]) {
    const warn = log();
    const limits = stallLimits(
      {
        LEGION_STALL_CALL_GRACE_MS: bad,
        LEGION_STALL_CALL_BOUND_MS: bad,
        LEGION_STALL_STEP_MS: bad,
      },
      warn,
      "implementer"
    );
    expect([limits.callGraceMs, limits.callBoundMs, limits.stepMs], bad).toEqual([
      DEFAULT_STALL_LIMITS.callGraceMs,
      FOUR_HOURS,
      DEFAULT_STALL_LIMITS.stepMs,
    ]);
    // One warning per variable, each naming the variable and the default it fell back to: the
    // bound's is the role's, so an implementer is told four hours and a tester an hour.
    expect(warn.warnings, bad).toEqual([
      `legion: LEGION_STALL_CALL_GRACE_MS is not an integer of 0 or more; using ${DEFAULT_STALL_LIMITS.callGraceMs}`,
      `legion: LEGION_STALL_CALL_BOUND_MS is not an integer of 0 or more; using ${FOUR_HOURS}`,
      `legion: LEGION_STALL_STEP_MS is not an integer of 0 or more; using ${DEFAULT_STALL_LIMITS.stepMs}`,
    ]);
    const tester = stallLimits({ LEGION_STALL_CALL_BOUND_MS: bad }, log(), "tester");
    expect(tester.callBoundMs, bad).toBe(HOUR);
  }
});

// 0 is the call watch off for this worker, for every role alike: it is a value the reader honours,
// not a refusal and not a warning.
test("a bound of 0 switches the watch off for that worker", () => {
  for (const role of ROLE_WORDS) {
    const warn = log();
    const limits = stallLimits({ LEGION_STALL_CALL_BOUND_MS: "0" }, warn, role);
    expect(limits.callBoundMs, `${role ?? "no role"}`).toBe(0);
    expect(warn.warnings).toEqual([]);
  }
});

// The daemon holds its own copy of these numbers, because a later refusal of a `legion.yaml`
// whose per-role backstop bound sits below the worker's whole ladder has to compute that ladder
// (`packages/daemon/internal/config/stalldefaults.go`). `go test -run StallDefaults
// ./internal/config/ -update` writes this fixture from that copy; this is where the two are held
// together, so neither can move alone.
test("the daemon's fixture carries the same defaults and the same per-role bounds", () => {
  const fixture = JSON.parse(
    readFileSync(
      path.resolve(import.meta.dir, "../../contracts/fixtures/daemon-api/stall-defaults.json"),
      "utf8"
    )
  ) as {
    readonly callGraceMs: number;
    readonly callBoundMs: number;
    readonly stepMs: number;
    readonly callBoundMsByRole: Record<string, number>;
  };

  expect({
    callGraceMs: fixture.callGraceMs,
    callBoundMs: fixture.callBoundMs,
    stepMs: fixture.stepMs,
  }).toEqual({
    callGraceMs: DEFAULT_STALL_LIMITS.callGraceMs,
    callBoundMs: DEFAULT_STALL_LIMITS.callBoundMs,
    stepMs: DEFAULT_STALL_LIMITS.stepMs,
  });

  // Every role word the daemon can launch a session as, and no other: a role the daemon forgot
  // would leave its floor computed from the wrong bound.
  expect(Object.fromEntries(Object.entries(fixture.callBoundMsByRole).sort())).toEqual(
    Object.fromEntries(
      [...LEGION_ROLES, "controller"].sort().map((role) => [role, defaultCallBoundMs(role)])
    )
  );
});
