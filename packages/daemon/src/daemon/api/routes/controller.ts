import { controllerToken, LegionDaemonApi } from "@legion/contracts";
import type { RouteContext } from "../context";
import { issueUrl } from "../github";
import { HttpError, issueKey, requiredString, validateContractResponse } from "../http";

// T24 controller startup contract: POST this with the session ID from envoy_whoami
// immediately after boot so held events are redelivered before any work endpoint.
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
  await ctx.controllerGate.ensureReady(ctx.deps.onControllerReady);
  return Response.json(validateContractResponse(LegionDaemonApi.ControllerReady.response, {}));
}

export async function handleGatesApprove(
  ctx: RouteContext,
  body: Record<string, unknown>
): Promise<Response> {
  await ctx.auth.requireController(ctx.deps.state, body);
  const issue = issueKey(body, "issue");
  const node = ctx.deps.state.issues[issue];
  if (!node) {
    throw new HttpError(404, "Unknown issue");
  }
  await ctx.github.gh(issue, [
    "gh",
    "api",
    "-X",
    "POST",
    `${issueUrl(issue)}/labels`,
    "-f",
    "labels[]=human-approved",
  ]);
  await ctx.github.gh(issue, [
    "gh",
    "api",
    "-X",
    "DELETE",
    `${issueUrl(issue)}/labels/needs-approval`,
  ]);
  node.labels = [
    ...new Set([...node.labels.filter((label) => label !== "needs-approval"), "human-approved"]),
  ];
  await ctx.save();
  return Response.json(validateContractResponse(LegionDaemonApi.GatesApprove.response, {}));
}

export async function handleAdmission(
  ctx: RouteContext,
  body: Record<string, unknown>
): Promise<Response> {
  await ctx.auth.requireController(ctx.deps.state, body);
  const issue = issueKey(body, "issue");
  if (!ctx.deps.state.issues[issue]) {
    throw new HttpError(404, "Unknown issue");
  }
  const result = ctx.deps.processManager.admit(issue);
  await ctx.save();
  return Response.json(
    validateContractResponse(LegionDaemonApi.Admission.response, {
      result,
    })
  );
}

export async function handleBacklog(
  ctx: RouteContext,
  body: Record<string, unknown>
): Promise<Response> {
  await ctx.auth.requireController(ctx.deps.state, body);
  const issue = issueKey(body, "issue");
  const marker = requiredString(body, "marker");
  const node = ctx.deps.state.issues[issue];
  if (!node) {
    throw new HttpError(404, "Unknown issue");
  }
  await ctx.github.gh(issue, [
    "gh",
    "api",
    "-X",
    "POST",
    `${issueUrl(issue)}/labels`,
    "-f",
    "labels[]=legion-backlog",
  ]);
  node.labels = [...new Set([...node.labels, "legion-backlog"])];
  node.backlogMarker = marker;
  await ctx.save();
  return Response.json(validateContractResponse(LegionDaemonApi.Backlog.response, {}));
}
