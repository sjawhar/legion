// packages/broker-client/src/dpop.ts
//
// The one RFC 9449 proof signer of this package, mirroring the Go signer in
// `packages/envoy/internal/brokerclient/dpop.go`. Each session makes its own key in its own
// process, and only its public half ever leaves that process: a proof carries the public JWK in
// its header, and nothing here can export the private half at all — `jose` makes it a
// non-extractable `CryptoKey`, so `exportJWK` on it fails rather than writing a `d` member.

import { exportJWK, generateKeyPair, type JWK, SignJWT } from "jose";

/** The signature algorithm every proof carries: ES256 over a P-256 key, the one the broker's
 *  exchange and its resource servers accept. */
const ALGORITHM = "ES256";

/** One proof's subject: the request it is signed for. */
export interface ProofRequest {
  /** The HTTP method, uppercased into the `htm` claim. */
  readonly method: string;
  /** The request's URL. Its query and fragment are dropped: `htu` is the target URI alone. */
  readonly url: string;
  /**
   * The access token this request presents under the DPoP scheme, which the proof names as `ath`,
   * the base64url SHA-256 of it (RFC 9449 Section 4.3 step 12), so a proof is good only for the
   * one token it was signed beside. A token endpoint request presents none — the token endpoint
   * is where one is asked for — and its proof carries no `ath`.
   */
  readonly accessToken?: string;
  /** The server's most recent `DPoP-Nonce`, carried once it has sent one. */
  readonly nonce?: string;
}

/** This process's own DPoP key: the proofs it signs, and the public half the broker records. */
export interface DpopKey {
  /** The public half, as it is embedded in every proof's header. It carries no `d` member: the
   *  private half cannot be exported from the key at all. */
  readonly publicJwk: JWK;
  /** One compact proof for `request`. */
  proof(request: ProofRequest): Promise<string>;
}

/** The `htu` claim for `url`: the target URI with no query and no fragment. */
function targetUri(url: string): string {
  const parsed = new URL(url);
  parsed.search = "";
  parsed.hash = "";
  return parsed.toString();
}

/** The `ath` claim for `accessToken`: its SHA-256, base64url with no padding. */
async function accessTokenHash(accessToken: string): Promise<string> {
  const digest = await crypto.subtle.digest("SHA-256", new TextEncoder().encode(accessToken));
  return Buffer.from(digest).toString("base64url");
}

/**
 * A key of this process's own and the proofs it signs: `typ: dpop+jwt`, `alg: ES256` and the
 * public JWK in the header, and `jti`, `htm`, `htu` and `iat` in the claims, with `nonce` and
 * `ath` added only where the request has them. The claim set is the whole of what a proof carries,
 * as `testdata/wire.json` names it.
 */
export async function newDpopKey(): Promise<DpopKey> {
  const { privateKey, publicKey } = await generateKeyPair(ALGORITHM, { extractable: false });
  const publicJwk = await exportJWK(publicKey);
  return {
    publicJwk,
    async proof(request: ProofRequest): Promise<string> {
      const claims: Record<string, string | number> = {
        jti: crypto.randomUUID(),
        htm: request.method.toUpperCase(),
        htu: targetUri(request.url),
        iat: Math.floor(Date.now() / 1000),
      };
      if (request.nonce !== undefined && request.nonce !== "") claims["nonce"] = request.nonce;
      if (request.accessToken !== undefined && request.accessToken !== "") {
        claims["ath"] = await accessTokenHash(request.accessToken);
      }
      return await new SignJWT(claims)
        .setProtectedHeader({ typ: "dpop+jwt", alg: ALGORITHM, jwk: publicJwk })
        .sign(privateKey);
    },
  };
}
