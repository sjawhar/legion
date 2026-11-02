import { test } from "@playwright/test";

import type { CredentialRecord } from "../web/src/api/types";
import type { FakeMachineLogin, FakePendingRequest, FakeSignIn } from "./fake-broker";
import { usesFakeBroker } from "./harness-broker";
import { harnessPorts } from "./harness-ports";

async function fixtureRequest(path: string, body: unknown): Promise<void> {
  const response = await fetch(`http://127.0.0.1:${harnessPorts().fakeBroker.port}${path}`, {
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
  requests: readonly FakePendingRequest[]
): Promise<void> {
  test.skip(!usesFakeBroker, "this run's server reads no fake broker (e2e/harness-broker.ts)");
  await fixtureRequest("/__fixture/pending", requests);
}

/** Replaces the fake broker's full credential records wholesale, keyed by their own `record_id`:
 *  what `GET /v1/credential-requests/{id}` answers for the record page. Skipped the same way
 *  `setPendingCredentialRequests` is. */
export async function setCredentialRecords(records: readonly CredentialRecord[]): Promise<void> {
  test.skip(!usesFakeBroker, "this run's server reads no fake broker (e2e/harness-broker.ts)");
  await fixtureRequest("/__fixture/records", records);
}

/** Replaces the machine logins a typed code selects on the fake broker, each the code its
 *  machine printed and the record `POST /v1/machine-logins/lookup` answers for it. Deciding one
 *  writes its decision into that record, so a second lookup reads what the broker kept. Skipped
 *  the same way `setPendingCredentialRequests` is. */
export async function setMachineLogins(logins: readonly FakeMachineLogin[]): Promise<void> {
  test.skip(!usesFakeBroker, "this run's server reads no fake broker (e2e/harness-broker.ts)");
  await fixtureRequest("/__fixture/machine-logins", logins);
}

/** Replaces the sign-ins the fake broker lists wholesale, each the person it belongs to and the
 *  machine they signed in on. Revoking one through Dispatch drops it from this list, so a second
 *  read reports what the broker kept. Skipped the same way `setPendingCredentialRequests` is. */
export async function setSignIns(signIns: readonly FakeSignIn[]): Promise<void> {
  test.skip(!usesFakeBroker, "this run's server reads no fake broker (e2e/harness-broker.ts)");
  await fixtureRequest("/__fixture/signins", signIns);
}

/** Whether the fake broker's RFC 8414 metadata names RFC 8628's device grant, which is what
 *  Dispatch reads to decide whether to serve the machine-code page. It is served until a row
 *  turns it off, and `resetFakeBroker` turns it back on. Dispatch holds its own read of that
 *  document for `deviceGrantTTL` (packages/envoy/internal/dispatch/agentsecrets/client.go), so a
 *  row that changes it waits for the page to follow rather than expecting the next load to.
 *  Skipped the same way `setPendingCredentialRequests` is. */
export async function setBrokerDeviceGrant(served: boolean): Promise<void> {
  test.skip(!usesFakeBroker, "this run's server reads no fake broker (e2e/harness-broker.ts)");
  await fixtureRequest("/__fixture/device-grant", { served });
}

/** Clears the fake broker between rows, so a request one row seeded never reaches the next. A run
 *  whose server reads no fake broker started none, and has nothing to clear. */
export async function resetFakeBroker(): Promise<void> {
  if (!usesFakeBroker) return;
  await fixtureRequest("/__fixture/reset", {});
}
