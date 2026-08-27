import { randomUUID } from "node:crypto";
import { LegionDaemonApi } from "@legion/contracts";
import type { RouteContext } from "../context";
import { appRoleForLegionRole } from "../github";
import { validateContractResponse } from "../http";

export async function handleProvisioningCredential(
  ctx: RouteContext,
  body: Record<string, unknown>
): Promise<Response> {
  const { tree, issue } = ctx.requireTreeIssue(body);
  ctx.auth.requireArchitectCapability(body, tree);
  const lease = await ctx.github.tokenForIssue(issue, "implement");
  return Response.json(
    validateContractResponse(LegionDaemonApi.ProvisioningCredential.response, {
      token: lease.token,
    })
  );
}

export async function handleGrants(
  ctx: RouteContext,
  body: Record<string, unknown>
): Promise<Response> {
  const { tree, issue } = ctx.requireTreeIssue(body);
  const capability = ctx.auth.requireSessionCapability(body, tree, issue);
  const grantId = randomUUID();
  const expiresAt = ctx.now() + ctx.grantTtlMs;
  ctx.auth.setGrant(grantId, { issue, role: capability.role, expiresAt });
  return Response.json(
    validateContractResponse(LegionDaemonApi.Grant.response, {
      grantId,
      expiresAt: new Date(expiresAt).toISOString(),
    })
  );
}

export async function handleGitCredential(
  ctx: RouteContext,
  body: Record<string, unknown>
): Promise<Response> {
  const grant = ctx.auth.resolveGrant(body);
  const lease = await ctx.github.tokenForIssue(grant.issue, appRoleForLegionRole(grant.role));
  return new Response(`username=x-access-token\npassword=${lease.token}`, {
    headers: { "content-type": "text/plain; charset=utf-8" },
  });
}

export async function handleGhToken(
  ctx: RouteContext,
  body: Record<string, unknown>
): Promise<Response> {
  const grant = ctx.auth.resolveGrant(body);
  const lease = await ctx.github.tokenForIssue(grant.issue, appRoleForLegionRole(grant.role));
  return Response.json(
    validateContractResponse(LegionDaemonApi.GitHubToken.response, {
      token: lease.token,
      appLogin: lease.gitIdentity.name,
    })
  );
}
