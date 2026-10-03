import { test } from "@playwright/test";

import { usesFakeBroker } from "./harness-broker";
import { harnessPorts } from "./harness-ports";

async function fixtureRequest(path: string, body: unknown): Promise<void> {
  const response = await fetch(`http://127.0.0.1:${harnessPorts.fakeBroker.port}${path}`, {
    body: JSON.stringify(body),
    headers: { "Content-Type": "application/json" },
    method: "PUT",
  });
  if (!response.ok) {
    throw new Error(`fake broker ${path}: ${response.status}`);
  }
}

/** Replaces the fake broker's pending credential requests wholesale; each waits on `approver`.
 *  The test that seeds them is skipped where this run's server reads no fake broker: a deployed
 *  server, or one the harness switch points at no broker or another (e2e/harness-broker.ts). */
export async function setPendingCredentialRequests(
  requests: readonly {
    approver: string;
    identifiers: readonly string[];
    kind: "agent_secret" | "launcher_credential";
    record_id: string;
    requested_at: string;
  }[]
): Promise<void> {
  test.skip(!usesFakeBroker, "this run's server reads no fake broker (e2e/harness-broker.ts)");
  await fixtureRequest("/__fixture/pending", requests);
}

/** Clears the fake broker between rows, so a request one row seeded never reaches the next. A run
 *  whose server reads no fake broker started none, and has nothing to clear. */
export async function resetFakeBroker(): Promise<void> {
  if (!usesFakeBroker) return;
  await fixtureRequest("/__fixture/reset", {});
}
