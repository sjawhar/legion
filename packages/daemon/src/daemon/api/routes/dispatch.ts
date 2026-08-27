import { LegionDaemonApi, parseIssueKey } from "@legion/contracts";
import { type RouteContext, treeContains } from "../context";
import { HttpError, issueKey, legionRole, requiredString, validateContractResponse } from "../http";

export async function handleDispatchThreads(
  ctx: RouteContext,
  body: Record<string, unknown>
): Promise<Response> {
  const { tree, issue } = ctx.requireTreeIssue(body);
  const capability = ctx.auth.requireSessionCapability(body, tree, issue);
  const role = legionRole(requiredString(body, "role"));
  if (role !== capability.role) {
    throw new HttpError(403, "Dispatch role does not match the authenticated session");
  }
  if (role !== "architect") {
    throw new HttpError(403, "Only an architect may open a Dispatch thread");
  }
  const parent = issueKey(body, "parent");
  if (!treeContains(ctx.deps.state, tree, parent)) {
    throw new HttpError(403, "Dispatch parent is outside the caller tree");
  }
  const subject = requiredString(body, "subject");
  const dispatchBody = requiredString(body, "body");
  const urgency = body.urgency;
  if (
    urgency !== undefined &&
    urgency !== "low" &&
    urgency !== "med" &&
    urgency !== "high" &&
    urgency !== "blocking"
  ) {
    throw new HttpError(400, "Invalid Dispatch urgency");
  }
  const dispatched = await ctx.dispatchThread(parent, subject, dispatchBody, body.ask, urgency);
  const parsedIssue = parseIssueKey(issue);
  if (!parsedIssue) {
    throw new Error(`Invalid issue key in Legion state: ${issue}`);
  }
  const repo = `${parsedIssue.owner}/${parsedIssue.repo}` as `${string}/${string}`;
  const existingMapping = ctx.deps.state.dispatchThreads.find(
    (entry) => entry.repo === repo && entry.thread === dispatched.thread
  );
  if (existingMapping && (existingMapping.tree !== tree || existingMapping.issue !== issue)) {
    throw new HttpError(403, "Dispatch thread is already owned by another issue");
  }
  const entry = { repo, thread: dispatched.thread, role, issue, tree };
  if (existingMapping) {
    const index = ctx.deps.state.dispatchThreads.indexOf(existingMapping);
    ctx.deps.state.dispatchThreads[index] = entry;
  } else {
    ctx.deps.state.dispatchThreads.push(entry);
  }
  await ctx.save();
  return Response.json(
    validateContractResponse(LegionDaemonApi.DispatchThread.response, dispatched)
  );
}
