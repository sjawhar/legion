import { controllerToken, LegionDaemonApi } from "@legion/contracts";
import { isExternalControllerLocator } from "../../runtime";
import { equalSecret } from "../auth";
import type { RouteContext } from "../context";
import { HttpError, requiredString, validateContractResponse } from "../http";

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
  // Under an operator-launched runtime (kubernetes) the runtime provides the external record for
  // this session and the daemon records it — last claim wins. The `ompSessionFile` such a session
  // reports is ignored on purpose: nothing resumes it, the daemon does not own that process.
  if (!ctx.deps.processManager.recordControllerReady(sessionId)) {
    // Daemon-launched (tmux). Rule: the file recorded on `controllerLocator` is always the daemon
    // pane's own transcript. The extension reports one only from that pane's own lifecycle (its
    // session start, or a session switch typed into it); the `/legion-claim-controller` takeover
    // from a hand-started session omits it, so a takeover claim moves the role and session id
    // here but leaves the pane's recorded file untouched — a dead pane is never resumed into an
    // operator's live transcript. An older plugin that omits the field likewise leaves the
    // recorded file alone.
    const ompSessionFile = body.ompSessionFile;
    if (typeof ompSessionFile === "string" && ompSessionFile.length > 0) {
      const locator = ctx.deps.state.controllerLocator;
      if (locator !== undefined && !isExternalControllerLocator(locator)) {
        locator.ompSessionFile = ompSessionFile;
      } else if (!ctx.deps.processManager.stashControllerReady(sessionId, ompSessionFile)) {
        console.warn(
          "[legion] controller/ready reported an OMP session file but no controller pane is recorded; a later respawn cannot resume it"
        );
      }
    }
  }
  await ctx.save();
  await ctx.deps.onControllerReady();
  return Response.json(validateContractResponse(LegionDaemonApi.ControllerReady.response, {}));
}

/** `legion controller start`'s one daemon call: the operator token as `Authorization: Bearer`,
 * compared in constant time against `operator_token_file`'s hash, buys a fresh controller
 * capability — minted exactly as the daemon mints one for its own pane
 * (`mintControllerCapability`: the previous controller's secret and grants stop working; last
 * claim wins). A daemon with no operator token (tmux) has this route disabled: 403 naming that,
 * nothing minted. Every refusal is one log line and mints nothing. */
export async function handleControllerSecret(
  ctx: RouteContext,
  _body: Record<string, unknown>,
  request: Request
): Promise<Response> {
  if (ctx.operatorTokenHash === undefined) {
    console.error(
      "[legion] refused POST /legion/v1/controller/secret: this daemon has no operator_token_file (a tmux daemon launches its own controller)"
    );
    throw new HttpError(
      403,
      "This daemon has no operator_token_file configured; the controller secret route is disabled"
    );
  }
  const header = request.headers.get("authorization") ?? "";
  const supplied = header.startsWith("Bearer ") ? header.slice("Bearer ".length) : "";
  if (supplied.length === 0 || !equalSecret(ctx.operatorTokenHash, supplied)) {
    console.error(
      `[legion] refused POST /legion/v1/controller/secret: ${supplied.length === 0 ? "no bearer token" : "wrong operator token"}`
    );
    throw new HttpError(403, "Invalid operator token");
  }
  const secret = await ctx.mintControllerCapability();
  return Response.json(
    validateContractResponse(LegionDaemonApi.ControllerSecret.response, { secret })
  );
}
