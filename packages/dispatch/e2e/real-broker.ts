// The real secrets broker and its dev issuer, both started by e2e/run-broker.sh, as
// e2e/broker-approval.e2e.ts reaches them: the broker's origin, the people and the secret its fake
// secrets file holds, the seeding step that puts pending requests on it as pods would, and the
// issuer's control of who the next sign-in is.
import { execFile } from "node:child_process";
import { fileURLToPath } from "node:url";
import { promisify } from "node:util";

import { harnessPorts } from "./harness-ports";

const execFileAsync = promisify(execFile);

/** The real broker's public URL, the origin of every record's page. */
export const realBrokerOrigin = `http://127.0.0.1:${harnessPorts.realBroker.port}`;
/** The dev issuer: the broker's token issuer and its sign-in provider. */
export const devIssuerOrigin = `http://127.0.0.1:${harnessPorts.devIssuer.port}`;

/** The broker logins of the suite's people (every one in the issuer's `example.com`), and the
 *  human-tier secret run-broker.sh's fake secrets file gives alice, which a pod's request waits on
 *  her to approve. */
export const aliceBrokerLogin = "alice@example.com";
export const bobBrokerLogin = "bob@example.com";
export const aliceSecret = "ALICE_DEPLOY_KEY";

/** The seeding command run-broker.sh builds beside the broker
 *  (packages/envoy/internal/broker/brokertest/requestseed). */
const requestseed = fileURLToPath(
  new URL("../../envoy/out/dispatch-e2e-broker/requestseed", import.meta.url)
);

/** A pending request the seeding step made, with the record it waits on and that record's page. */
export interface SeededRequest {
  readonly request_id: string;
  readonly record_id: string;
  readonly approve_url: string;
}

/** A seeded request's state, read by the pod that made it. */
export interface SeededRequestState {
  readonly request_id: string;
  readonly state: string;
  readonly grant_id: string | null;
}

/** Runs requestseed's `command` on the real broker with `options` and returns what it prints. A
 *  failure's message carries the command's stderr, which logs every exchange with the broker. */
async function requestSeed(
  command: "ask" | "state" | "withdraw",
  options: readonly string[]
): Promise<string> {
  const { stdout } = await execFileAsync(requestseed, [
    command,
    "--broker",
    realBrokerOrigin,
    ...options,
  ]);
  return stdout;
}

/** The `requests` requestseed printed, refusing any other output. */
function printedRequests(printed: string): readonly unknown[] {
  const parsed: unknown = JSON.parse(printed);
  if (parsed && typeof parsed === "object" && "requests" in parsed) {
    const { requests } = parsed;
    if (Array.isArray(requests)) return requests;
  }
  throw new Error(`requestseed printed no requests: ${printed}`);
}

/** `row`'s field `name` when it is a string, null when it is null, else a refusal. */
function field(row: unknown, name: string): string | null {
  const value: unknown =
    row && typeof row === "object" ? Object.getOwnPropertyDescriptor(row, name)?.value : undefined;
  if (typeof value === "string" || value === null) return value;
  throw new Error(
    `requestseed printed a request whose ${name} is not a string: ${JSON.stringify(row)}`
  );
}

/** `row`'s field `name`, which must be a string. */
function stringField(row: unknown, name: string): string {
  const value = field(row, name);
  if (value === null) throw new Error(`requestseed printed a request with no ${name}`);
  return value;
}

/** Enrolls `count` pods on the real broker, each asking once for `secret`, so each request waits
 *  on a record of its own; `state` is the file that keeps the pods' keys for the reads below. */
export async function seedPendingRequests(
  state: string,
  secret: string,
  count = 1
): Promise<readonly SeededRequest[]> {
  const options = ["--issuer", devIssuerOrigin, "--state", state, "--secret", secret];
  const printed = await requestSeed("ask", [...options, "--count", String(count)]);
  return printedRequests(printed).map((row) => ({
    approve_url: stringField(row, "approve_url"),
    record_id: stringField(row, "record_id"),
    request_id: stringField(row, "request_id"),
  }));
}

/** The state of every request `state` names, each read by the pod that made it. */
export async function seededRequestStates(state: string): Promise<readonly SeededRequestState[]> {
  const printed = await requestSeed("state", ["--state", state]);
  return printedRequests(printed).map((row) => ({
    grant_id: field(row, "grant_id"),
    request_id: stringField(row, "request_id"),
    state: stringField(row, "state"),
  }));
}

/** Cancels every request `state` names that is still pending, so the approver's pending list on
 *  the broker, which every page of theirs prints in full, holds nothing a later row did not seed. */
export async function withdrawSeededRequests(state: string): Promise<void> {
  await requestSeed("withdraw", ["--state", state]);
}

/** Makes the issuer sign in `email` at the next sign-in, whatever its login_hint names. */
export async function queueSignIn(email: string): Promise<void> {
  const response = await fetch(`${devIssuerOrigin}/dev/sign-in-as`, {
    body: JSON.stringify({ email }),
    headers: { "Content-Type": "application/json" },
    method: "POST",
  });
  if (response.status !== 204) {
    throw new Error(`dev issuer /dev/sign-in-as ${email}: ${response.status}`);
  }
}
