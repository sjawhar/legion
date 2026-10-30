/**
 * INFRA-484 throwaway cross-language proof — the agent's own half.
 *
 * Runs the real plugin client against a real daemon listener: `executePhaseOp({op:
 * "ready_for_update"})` from `@legion/pi-shared/legion-phase-ops`, which calls
 * `createLegionDaemonClient(base).readyForUpdate(...)` over the global `fetch`. Nothing here
 * writes a request by hand: the only thing this file does to `fetch` is wrap the global so the
 * request body, its shape under the route's Zod request schema, and the status are printed.
 *
 * argv[2] is one JSON object: base, claimToken, sessionId, secret, generation, tree, issue, role,
 * head. Exit 0 is the op's answer, exit 3 a refusal — the negative control's expected end.
 */
import { LegionReadyForUpdateRequest } from "@legion/contracts/legion-api";
import { createLegionDaemonClient } from "@legion/pi-shared/daemon-client";
import { executePhaseOp, type PhaseOpRuntime } from "@legion/pi-shared/legion-phase-ops";

const input = JSON.parse(process.argv[2] ?? "{}") as {
  base: string;
  claimToken: string;
  sessionId: string;
  secret: string;
  generation: number;
  tree: string;
  issue: string;
  role: string;
  head: string;
};

const realFetch = globalThis.fetch;
globalThis.fetch = (async (resource: Parameters<typeof realFetch>[0], init?: RequestInit) => {
  const body = typeof init?.body === "string" ? init.body : "";
  console.log(`PROOF request ${init?.method ?? "GET"} ${String(resource)}`);
  console.log(`PROOF request-body ${body}`);
  const parsed = LegionReadyForUpdateRequest.safeParse(JSON.parse(body));
  console.log(
    `PROOF request-schema LegionReadyForUpdateRequest ${
      parsed.success ? "accepts this body" : `refuses this body: ${parsed.error.message}`
    }`
  );
  const response = await realFetch(resource, init);
  console.log(`PROOF response-status ${response.status}`);
  return response;
}) as typeof fetch;

const runtime: PhaseOpRuntime = {
  daemon: createLegionDaemonClient(input.base),
  claim: {
    sessionID: input.sessionId,
    claimToken: input.claimToken,
    generation: input.generation,
    tree: input.tree,
    issue: input.issue,
    role: input.role as PhaseOpRuntime["claim"]["role"],
    secret: input.secret,
  },
  handoffCommit: async () => {
    throw new Error("ready_for_update looks up no handoff commit");
  },
  branchHead: async () => input.head,
};

try {
  const answer = await executePhaseOp(runtime, { op: "ready_for_update" });
  console.log(`PROOF op-answer ${JSON.stringify(answer)}`);
  process.exit(0);
} catch (error) {
  console.log(
    `PROOF op-refused ${error instanceof Error ? error.name : typeof error}: ${
      error instanceof Error ? error.message : String(error)
    }`
  );
  process.exit(3);
}
