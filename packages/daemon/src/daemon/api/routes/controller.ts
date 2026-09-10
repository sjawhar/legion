import { controllerToken, LegionDaemonApi } from "@legion/contracts";
import type { RouteContext } from "../context";
import { requiredString, validateContractResponse } from "../http";

// Controller sessions POST their Envoy session ID immediately after boot. The controller is a
// role holder like any other: its catch-up on ready is the existing resync triage re-emission
// for unadmitted tracked roots (see index.ts's onControllerReady wiring), which also drains any
// Slack mention (or other irreplaceable payload) recorded while no controller held the role —
// the one narrow exception to "no held events", since a mention's text has no other source of
// truth to recover it from.
export async function handleControllerReady(
  ctx: RouteContext,
  body: Record<string, unknown>
): Promise<Response> {
  await ctx.auth.requireController(ctx.deps.state, body);
  const sessionId = requiredString(body, "sessionId");
  ctx.deps.state.roles[controllerToken(ctx.deps.state.project)] = {
    role: "controller",
    sessionId,
  };
  await ctx.save();
  // Same deadlock shape as /process/ready — see handleProcessReady. markControllerReady
  // already never rejects (its own internal try/catch), but it must still never be awaited
  // here: even a caught failure costs the full RPC timeout before this call would return.
  Promise.resolve(ctx.deps.processManager.markControllerReady()).catch(() => {});
  await ctx.deps.onControllerReady();
  return Response.json(validateContractResponse(LegionDaemonApi.ControllerReady.response, {}));
}
