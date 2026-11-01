import { expect, test } from "bun:test";
import { readFileSync } from "node:fs";
import path from "node:path";
import type { LegionCapabilityReportBody } from "@legion/contracts/legion-api";
import { createLegionDaemonClient } from "./daemon-client";
import { bootLegionClaim, type LegionClaimHost } from "./legion-claim-core";
import type { ClaimCapability } from "./legion-phase-ops";

/** A response the daemon's own golden test wrote (`packages/daemon/internal/api`). */
function daemonFixture(name: string): Record<string, unknown> {
  return JSON.parse(
    readFileSync(path.resolve(import.meta.dir, "../../contracts/fixtures/daemon-api", name), "utf8")
  );
}

const registered = daemonFixture("register.json");
const claimToken = String(registered.claimToken);
const registration = {
  bootToken: "boot-token-from-the-pane",
  sessionId: "ses_claim",
  ompSessionFile: "/sessions/ses_claim.jsonl",
  agentId: "ses_claim",
  pluginContract: 29,
};
const report: LegionCapabilityReportBody = {
  measuredAt: "2026-10-18T00:00:00.000Z",
  elapsedMs: 3,
  rows: [{ name: "github", ok: true, detail: "gh viewer login: legion-implementer[bot]" }],
};

/** A host over a stub daemon whose `register` and `ready` answer as given, recording every step it
 * is asked for, in order, beside the daemon's requests. */
function claimHost(options: {
  readonly register?: () => Response;
  readonly ready?: () => Response;
  readonly overrides?: Partial<LegionClaimHost>;
}) {
  const steps: string[] = [];
  const lines: string[] = [];
  const capabilities: ClaimCapability[] = [];
  let regain: ((role: string, reason: string) => Promise<void>) | undefined;
  const stub = (async (input, init) => {
    const route = new URL(input.toString()).pathname.replace("/legion/v1/", "");
    steps.push(`${route} ${init?.body ?? ""}`);
    if (route === "claims/register") return options.register?.() ?? Response.json(registered);
    return options.ready?.() ?? new Response(null, { status: 204 });
  }) as typeof fetch;
  const host: LegionClaimHost = {
    daemon: createLegionDaemonClient("http://daemon.test", stub),
    registration,
    setCapability: (capability) => {
      steps.push("setCapability");
      capabilities.push(capability);
    },
    claimRole: async (role) => {
      steps.push(`claimRole ${role}`);
    },
    onRoleRegained: (listener) => {
      steps.push("onRoleRegained");
      regain = listener;
    },
    log: (line) => lines.push(line),
    stopping: "exiting so the daemon launches it again",
    stop: () => steps.push("stop"),
    ...options.overrides,
  };
  return { host, steps, lines, capabilities, regain: () => regain };
}

const readyBody = (capabilities?: LegionCapabilityReportBody) =>
  JSON.stringify({
    claimToken,
    sessionId: "ses_claim",
    secret: registered.secret,
    generation: registered.generation,
    ...(capabilities === undefined ? {} : { capabilities }),
  });

test("registers, runs afterRegister, records the capability, claims the role, then readies with the measured report, and readies again on a regain of its role", async () => {
  const measured = Promise.withResolvers<LegionCapabilityReportBody>();
  const { host, steps, lines, capabilities, regain } = claimHost({
    overrides: {
      measure: async (claim) => {
        steps.push(`measure ${claim.issue}`);
        return measured.promise;
      },
      afterRegister: async (claim) => {
        steps.push(`afterRegister ${claim.claimToken}`);
        // The measurement runs beside the steps after the registration: it settles only now.
        measured.resolve(report);
      },
    },
  });

  await bootLegionClaim(host);

  // The measurement starts a microtask after the registration answered, so afterRegister is
  // called first and both run side by side; the ready waits on the measurement.
  expect(steps).toEqual([
    `claims/register ${JSON.stringify(registration)}`,
    `afterRegister ${claimToken}`,
    `measure ${registered.issue}`,
    "setCapability",
    `claimRole ${claimToken}`,
    `claims/ready ${readyBody(report)}`,
    "onRoleRegained",
  ]);
  expect(capabilities).toEqual([
    {
      sessionID: "ses_claim",
      tree: String(registered.tree),
      issue: String(registered.issue),
      role: registered.role as never,
      secret: String(registered.secret),
    },
  ]);

  // Another role's regain sends nothing; this claim's sends the same ready, report included.
  await regain()?.("some-other-role", "reclaimed");
  await regain()?.(claimToken, "reregistered");
  expect(steps.slice(7)).toEqual([`claims/ready ${readyBody(report)}`]);
  expect(lines).toEqual([`re-ran claims/ready after role ${claimToken} was reregistered`]);
});

test("a measurement that rejects costs the report alone: one line, ready without capabilities", async () => {
  const { host, steps, lines } = claimHost({
    overrides: { measure: async () => Promise.reject(new Error("no capability host")) },
  });
  await bootLegionClaim(host);
  expect(steps.filter((step) => step.startsWith("claims/ready"))).toEqual([
    `claims/ready ${readyBody()}`,
  ]);
  expect(lines).toEqual([
    "capability measurement failed; ready carries no report: no capability host",
  ]);
  expect(steps).not.toContain("stop");
});

for (const status of [400, 403, 409]) {
  test(`a ${status} at claims/register is logged and stops the agent, claiming nothing`, async () => {
    const { host, steps, lines } = claimHost({
      register: () => Response.json({ error: "Invalid boot token" }, { status }),
    });
    await bootLegionClaim(host);
    expect(steps).toEqual([`claims/register ${JSON.stringify(registration)}`, "stop"]);
    expect(lines).toEqual([`claims/register registration failed (${status}): Invalid boot token`]);
  });
}

test("a 5xx at claims/register is logged and rejects without stopping; a request that never reached the daemon rejects unlogged", async () => {
  const failed = claimHost({
    register: () => Response.json({ error: "register failed" }, { status: 503 }),
  });
  await expect(bootLegionClaim(failed.host)).rejects.toThrow(
    "POST /legion/v1/claims/register failed with 503: register failed"
  );
  expect(failed.steps).not.toContain("stop");
  expect(failed.lines).toEqual(["claims/register registration failed (503): register failed"]);

  const unreachable = claimHost({
    register: () => {
      throw new TypeError("fetch failed");
    },
  });
  await expect(bootLegionClaim(unreachable.host)).rejects.toThrow("fetch failed");
  expect(unreachable.steps).not.toContain("stop");
  expect(unreachable.lines).toEqual([]);
});

test("a failure after the registration is logged naming the claim token and what stop does, then stops the agent", async () => {
  const { host, steps, lines } = claimHost({
    overrides: {
      claimRole: async (role) => {
        throw new Error(`Envoy role ${role} is held by live session ses_other`);
      },
    },
  });
  await bootLegionClaim(host);
  expect(steps).toEqual([
    `claims/register ${JSON.stringify(registration)}`,
    "setCapability",
    "stop",
  ]);
  expect(lines).toEqual([
    `claim boot failed after claims/register registered ${claimToken}; exiting so the daemon launches it again: Envoy role ${claimToken} is held by live session ses_other`,
  ]);
});

test("a 4xx at claims/ready is not retried: one ready, then the agent stops", async () => {
  const { host, steps } = claimHost({
    ready: () => Response.json({ error: "Invalid session secret" }, { status: 403 }),
  });
  await bootLegionClaim(host);
  expect(steps.filter((step) => step.startsWith("claims/ready"))).toHaveLength(1);
  expect(steps.at(-1)).toBe("stop");
  expect(steps).not.toContain("onRoleRegained");
});
