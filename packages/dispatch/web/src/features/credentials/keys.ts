import { queryOptions } from "@tanstack/react-query";

import { api } from "../../api/client";
import { b64urlToBuf, bufToB64url } from "../../lib/webauthn";

/** Every approver key registered for `login`, as `KeysPage` lists them. */
export const credentialKeysQuery = (login: string) =>
  queryOptions({
    queryKey: ["credential-keys", login],
    queryFn: () => api.getCredentialKeys(login),
  });

/** Every live grant the viewer may revoke, as `GrantsSection` lists them. */
export const credentialGrantsQuery = () =>
  queryOptions({
    queryKey: ["credential-grants"],
    queryFn: () => api.getCredentialGrants(),
  });

function toHex(bytes: ArrayBuffer): string {
  return Array.from(new Uint8Array(bytes))
    .map((byte) => byte.toString(16).padStart(2, "0"))
    .join("");
}

/**
 * Contract v9's endorse challenge is built server-side from a key hash the client must supply:
 * lowercase-hex SHA-256 of the endorsed key's raw credential id bytes. The freshly registered
 * key's `RegistrationResponseJSON.id` already carries those bytes, base64url-encoded, so this
 * decodes and hashes it with the Web Crypto API.
 */
export async function credentialKeyHash(credentialId: string): Promise<string> {
  const digest = await crypto.subtle.digest("SHA-256", b64urlToBuf(credentialId));
  return toHex(digest);
}

/**
 * Contract v9's revoke challenge — `SHA-256("agent-secrets/revoke/v1\n" + <grant id>)`,
 * base64url-encoded — is a deterministic function of public information (the grant id), so the
 * browser can compute the same digest the broker independently verifies. The grants-list
 * response carries no `challenges` field of its own to read this from.
 */
export async function revokeChallenge(grantId: string): Promise<string> {
  const bytes = new TextEncoder().encode(`agent-secrets/revoke/v1\n${grantId}`);
  const digest = await crypto.subtle.digest("SHA-256", bytes);
  return bufToB64url(digest);
}
