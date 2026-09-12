import { controllerToken, LegionDaemonApi } from "@legion/contracts";
import type { RouteContext } from "../context";
import { requiredString, validateContractResponse } from "../http";

// Controller sessions POST their Envoy session ID immediately after boot. The controller is an
// interactive OMP terminal session (no shim, no socket) and a role holder like any other: its
// catch-up on ready is the existing resync triage re-emission for unadmitted tracked roots (see
// index.ts's onControllerReady wiring), which also drains any Slack mention (or other
// irreplaceable payload) recorded while no controller held the role — the one narrow exception
// to "no held events", since a mention's text has no other source of truth to recover it from.
// The optional `ompSessionFile` is the session transcript `ensureController` resumes with
// `--resume` when the pane is later found dead.
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
  // Rule: the file recorded on `controllerLocator` is always the daemon pane's own transcript.
  // The extension reports one only from that pane's own lifecycle (its session start, or a
  // session switch typed into it); the `/legion-claim-controller` takeover from a hand-started
  // session omits it, so a takeover claim moves the role and session id here but leaves the
  // pane's recorded file untouched — a dead pane is never resumed into an operator's live
  // transcript. An older plugin that omits the field likewise leaves the recorded file alone.
  // With no recorded pane at all the file is logged and dropped: the daemon never invents a
  // locator (plan decision D2).
  const ompSessionFile = body.ompSessionFile;
  if (typeof ompSessionFile === "string" && ompSessionFile.length > 0) {
    if (ctx.deps.state.controllerLocator) {
      ctx.deps.state.controllerLocator.ompSessionFile = ompSessionFile;
    } else {
      console.warn(
        "[legion] controller/ready reported an OMP session file but no controller pane is recorded; a later respawn cannot resume it"
      );
    }
  }
  await ctx.save();
  await ctx.deps.onControllerReady();
  return Response.json(validateContractResponse(LegionDaemonApi.ControllerReady.response, {}));
}
