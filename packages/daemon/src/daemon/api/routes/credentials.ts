import { randomUUID } from "node:crypto";
import { LegionDaemonApi } from "@legion/contracts";
import type { RouteContext } from "../context";
import { appRoleForLegionRole } from "../github";
import { requiredString, validateContractResponse } from "../http";

export async function handleProvisioningCredential(
  ctx: RouteContext,
  body: Record<string, unknown>
): Promise<Response> {
  const { tree } = ctx.requireTreeIssue(body);
  ctx.auth.requireArchitectCapability(body, tree);
  const lease = await ctx.github.tokenForIssue("implement");
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
  const sessionId = requiredString(body, "sessionId");
  const grantId = randomUUID();
  const expiresAt = ctx.now() + ctx.grantTtlMs;
  ctx.auth.setGrant(grantId, { issue, role: capability.role, sessionId, expiresAt });
  return Response.json(
    validateContractResponse(LegionDaemonApi.Grant.response, {
      grantId,
      expiresAt: new Date(expiresAt).toISOString(),
    })
  );
}

/** Re-resolves the grant after the GitHub lease await, not merely once before it: `resolveGrant`
 * is a pure, side-effect-free lookup (see its own doc comment), so calling it twice is safe and
 * — because `deleteCapability` deletes the grant entry outright, not just the capability that
 * minted it — actually detects a session revoked while this request's own GitHub call was in
 * flight (mirrors `handleWorkerStarted`'s re-check of the live claim after its own slow GitHub
 * await). A revoked-mid-await grant 403s here instead of handing back a credential for a
 * session that no longer holds it. */
export async function handleGitCredential(
  ctx: RouteContext,
  body: Record<string, unknown>
): Promise<Response> {
  const grant = ctx.auth.resolveGrant(body);
  const lease = await ctx.github.tokenForIssue(appRoleForLegionRole(grant.role));
  ctx.auth.resolveGrant(body);
  return new Response(`username=x-access-token\npassword=${lease.token}`, {
    headers: { "content-type": "text/plain; charset=utf-8" },
  });
}

/** See `handleGitCredential`'s doc comment: the same post-await recheck applies here. */
export async function handleGhToken(
  ctx: RouteContext,
  body: Record<string, unknown>
): Promise<Response> {
  const grant = ctx.auth.resolveGrant(body);
  const lease = await ctx.github.tokenForIssue(appRoleForLegionRole(grant.role));
  ctx.auth.resolveGrant(body);
  return Response.json(
    validateContractResponse(LegionDaemonApi.GitHubToken.response, {
      token: lease.token,
      appLogin: lease.gitIdentity.name,
    })
  );
}
