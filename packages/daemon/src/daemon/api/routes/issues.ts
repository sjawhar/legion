import {
  ARCHITECT_MUTABLE_LABELS,
  formatIssueKey,
  type IssueKey,
  isArchitectMutableLabel,
  LegionDaemonApi,
  parseIssueKey,
} from "@legion/contracts";
import { matchingHeldEvent, type RouteContext, treeContains } from "../context";
import { issueUrl } from "../github";
import {
  asRecord,
  HttpError,
  issueKey,
  optionalStrings,
  requiredString,
  validateContractResponse,
} from "../http";

async function createIssueComment(
  ctx: RouteContext,
  issue: IssueKey,
  commentBody: string
): Promise<{ commentId: number; url: string }> {
  const result = asRecord(
    JSON.parse(
      await ctx.github.gh(issue, [
        "gh",
        "api",
        `${issueUrl(issue)}/comments`,
        "-f",
        `body=${commentBody}`,
      ])
    )
  );
  const commentId = result.id;
  const url = result.html_url;
  if (typeof commentId !== "number" || typeof url !== "string") {
    throw new Error("GitHub comment create returned invalid response");
  }
  return { commentId, url };
}

export async function handleIssueCreate(
  ctx: RouteContext,
  body: Record<string, unknown>
): Promise<Response> {
  const tree = ctx.requireTree(body);
  ctx.auth.requireArchitectCapability(body, tree);
  const title = requiredString(body, "title");
  const issueBody = requiredString(body, "body");
  const labels = optionalStrings(body, "labels");
  if (labels.some((label) => !isArchitectMutableLabel(label))) {
    throw new HttpError(
      400,
      `Architect issue creation only accepts ${ARCHITECT_MUTABLE_LABELS.join(", ")}`
    );
  }
  const childLabels = [...new Set([...labels, "legion-child"])];
  const parsedTree = parseIssueKey(tree);
  if (!parsedTree) {
    throw new Error(`Invalid issue key in Legion state: ${tree}`);
  }
  const createCommand = [
    "gh",
    "api",
    `repos/${parsedTree.owner}/${parsedTree.repo}/issues`,
    "-f",
    `title=${title}`,
    "-f",
    `body=${issueBody}`,
    ...childLabels.flatMap((label) => ["-f", `labels[]=${label}`]),
  ];
  const created = asRecord(JSON.parse(await ctx.github.gh(tree, createCommand)));
  const number = created.number;
  const createdUrl = created.html_url;
  const childNodeId = created.node_id;
  if (
    typeof number !== "number" ||
    !Number.isInteger(number) ||
    typeof createdUrl !== "string" ||
    typeof childNodeId !== "string"
  ) {
    throw new Error("GitHub issue create returned invalid response");
  }
  const child = formatIssueKey(parsedTree.owner, parsedTree.repo, number);
  const parent = asRecord(JSON.parse(await ctx.github.gh(tree, ["gh", "api", issueUrl(tree)])));
  const parentNodeId = parent.node_id;
  if (typeof parentNodeId !== "string") {
    throw new Error("GitHub parent issue returned invalid response");
  }
  const mutation =
    "mutation($parentId: ID!, $childId: ID!) { addSubIssue(input: {issueId: $parentId, subIssueId: $childId}) { issue { id } } }";
  await ctx.github.gh(tree, [
    "gh",
    "api",
    "graphql",
    "-f",
    `query=${mutation}`,
    "-F",
    `parentId=${parentNodeId}`,
    "-F",
    `childId=${childNodeId}`,
  ]);
  ctx.deps.state.issues[child] = {
    key: child,
    title,
    parent: tree,
    state: "open",
    children: [],
    released: false,
    labels: childLabels,
  };
  ctx.deps.state.issues[tree]?.children.push(child);
  await ctx.save();
  return Response.json(
    validateContractResponse(LegionDaemonApi.IssueCreate.response, {
      issue: child,
      url: createdUrl,
    })
  );
}

export async function handleIssueComment(
  ctx: RouteContext,
  body: Record<string, unknown>
): Promise<Response> {
  const { tree, issue } = ctx.requireTreeIssue(body);
  ctx.auth.requireArchitectCapability(body, tree);
  const { commentId, url } = await createIssueComment(
    ctx,
    issue,
    ctx.appendFooter(tree, issue, requiredString(body, "body"))
  );
  await ctx.save();
  return Response.json(
    validateContractResponse(LegionDaemonApi.Comment.response, {
      commentId,
      url,
    })
  );
}

export async function handleIssueBody(
  ctx: RouteContext,
  body: Record<string, unknown>
): Promise<Response> {
  const { tree, issue } = ctx.requireTreeIssue(body);
  ctx.auth.requireArchitectCapability(body, tree);
  await ctx.github.gh(issue, [
    "gh",
    "api",
    "-X",
    "PATCH",
    issueUrl(issue),
    "-f",
    `body=${requiredString(body, "body")}`,
  ]);
  await ctx.save();
  return Response.json(validateContractResponse(LegionDaemonApi.PostBody.response, {}));
}

export async function handleIssueLabels(
  ctx: RouteContext,
  body: Record<string, unknown>
): Promise<Response> {
  const { tree, issue } = ctx.requireTreeIssue(body);
  ctx.auth.requireArchitectCapability(body, tree);
  const add = optionalStrings(body, "add");
  if (add.some((label) => !isArchitectMutableLabel(label))) {
    throw new HttpError(
      400,
      `Architect label changes only add ${ARCHITECT_MUTABLE_LABELS.join(", ")}`
    );
  }
  if (add.length > 0) {
    await ctx.github.gh(issue, [
      "gh",
      "api",
      "-X",
      "POST",
      `${issueUrl(issue)}/labels`,
      ...add.flatMap((label) => ["-f", `labels[]=${label}`]),
    ]);
  }
  const node = ctx.deps.state.issues[issue];
  if (!node) {
    throw new HttpError(404, "Unknown issue");
  }
  node.labels = [...new Set([...node.labels, ...add])];
  await ctx.save();
  return Response.json(
    validateContractResponse(LegionDaemonApi.Labels.response, {
      labels: node.labels,
    })
  );
}

export async function handleIssueClose(
  ctx: RouteContext,
  body: Record<string, unknown>
): Promise<Response> {
  const { tree, issue } = ctx.requireTreeIssue(body);
  ctx.auth.requireArchitectCapability(body, tree);
  const comment = body.comment;
  if (comment !== undefined && typeof comment !== "string") {
    throw new HttpError(400, "Expected string comment");
  }
  let finalCommentRef: string | undefined;
  if (comment) {
    const created = await createIssueComment(ctx, issue, ctx.appendFooter(tree, issue, comment));
    finalCommentRef = created.url;
  }
  await ctx.github.gh(issue, ["gh", "api", "-X", "PATCH", issueUrl(issue), "-f", "state=closed"]);
  const node = ctx.deps.state.issues[issue];
  if (node) {
    node.state = "closed";
    node.finalCommentRef = finalCommentRef;
  }
  ctx.deps.state.dispatchThreads = ctx.deps.state.dispatchThreads.filter(
    (thread) => thread.issue !== issue
  );
  if (issue === tree) ctx.deps.processManager.beginLinger(tree);
  await ctx.save();
  return Response.json(validateContractResponse(LegionDaemonApi.IssueClose.response, {}));
}

export async function handleWaveRelease(
  ctx: RouteContext,
  body: Record<string, unknown>
): Promise<Response> {
  const tree = ctx.requireTree(body);
  ctx.auth.requireArchitectCapability(body, tree);
  const children = optionalStrings(body, "children").map((child) => issueKey({ child }, "child"));
  for (const child of children) {
    if (!treeContains(ctx.deps.state, tree, child)) {
      throw new HttpError(403, "Issue is outside tree");
    }
  }
  const treeState = ctx.deps.state.trees[tree];
  if (!treeState) {
    throw new HttpError(404, "Unknown tree");
  }
  for (const child of children) {
    const node = ctx.deps.state.issues[child];
    if (node) {
      node.released = true;
    }
  }
  const retained = [];
  for (const held of treeState.heldEvents) {
    const matchingChild = children.find((child) =>
      matchingHeldEvent(ctx.deps.state, child, held.role)
    );
    if (!matchingChild) {
      retained.push(held);
      continue;
    }
    try {
      await ctx.deps.envoyPublish(`notifications.role.${held.role}`, held.payloadJson);
    } catch {
      retained.push(held);
    }
  }
  treeState.heldEvents = retained;
  await ctx.save();
  return Response.json(
    validateContractResponse(LegionDaemonApi.WaveRelease.response, {
      released: children,
    })
  );
}

export async function handleEscalate(
  ctx: RouteContext,
  body: Record<string, unknown>
): Promise<Response> {
  const tree = ctx.requireTree(body);
  ctx.auth.requireArchitectCapability(body, tree);
  const kind = requiredString(body, "kind");
  if (kind !== "re-file" && kind !== "capacity" && kind !== "cross-tree") {
    throw new HttpError(400, "Unknown escalation kind");
  }
  if (!("context" in body)) {
    throw new HttpError(400, "Expected context");
  }
  const controllerEvent = {
    type: "escalate",
    tree,
    kind,
    context: body.context,
  };
  await ctx.deps.onControllerEvent(controllerEvent);
  return Response.json(validateContractResponse(LegionDaemonApi.Escalate.response, {}));
}
