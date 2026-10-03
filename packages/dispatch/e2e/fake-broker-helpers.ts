import { test } from "@playwright/test";

import { harnessPorts } from "./harness-ports";

/** A deployed server (`PLAYWRIGHT_BASE_URL`) reads its own broker, so this run starts no fake one. */
const deployed = Boolean(process.env.PLAYWRIGHT_BASE_URL);

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
 *  The test that seeds them is skipped against a deployed server, which talks to no fake. */
export async function setPendingCredentialRequests(
  requests: readonly {
    approver: string;
    identifiers: readonly string[];
    kind: "agent_secret" | "launcher_credential";
    record_id: string;
    requested_at: string;
  }[]
): Promise<void> {
  test.skip(deployed, "the fake broker is unavailable with PLAYWRIGHT_BASE_URL");
  await fixtureRequest("/__fixture/pending", requests);
}

/** Clears the fake broker between rows, so a request one row seeded never reaches the next. */
export async function resetFakeBroker(): Promise<void> {
  if (deployed) return;
  await fixtureRequest("/__fixture/reset", {});
}
