// packages/broker-client/src/__tests__/dpop.test.ts
//
// The proof this process signs: what it carries, what it never carries, and that it is signed by
// the key whose public half it embeds.

import { expect, test } from "bun:test";
import { jwtVerify } from "jose";
import { newDpopKey } from "../dpop";

/** One part of a compact JWS as JSON. */
function part(compact: string, at: number): Record<string, unknown> {
  return JSON.parse(Buffer.from(compact.split(".")[at] as string, "base64url").toString("utf8"));
}

test("a token endpoint proof carries jti, htm, htu and iat, and nothing else", async () => {
  const key = await newDpopKey();
  const compact = await key.proof({ method: "post", url: "http://broker.test/v1/oauth/token" });
  expect(Object.keys(part(compact, 0)).sort()).toEqual(["alg", "jwk", "typ"]);
  const claims = part(compact, 1);
  expect(Object.keys(claims).sort()).toEqual(["htm", "htu", "iat", "jti"]);
  expect(claims.htm).toBe("POST");
  expect(claims.htu).toBe("http://broker.test/v1/oauth/token");
  expect(Math.abs(Date.now() / 1000 - (claims.iat as number))).toBeLessThan(60);
  expect(part(compact, 0).typ).toBe("dpop+jwt");
  expect(part(compact, 0).alg).toBe("ES256");
});

test("the proof's embedded key is the public half alone, and signs the proof", async () => {
  const key = await newDpopKey();
  const compact = await key.proof({ method: "GET", url: "https://dispatch.test/api/v1/issues" });
  const jwk = part(compact, 0).jwk as Record<string, unknown>;
  expect(Object.hasOwn(jwk, "d")).toBe(false);
  expect(Object.hasOwn(key.publicJwk, "d")).toBe(false);
  expect(jwk.kty).toBe("EC");
  expect(jwk.crv).toBe("P-256");
  // The key verifies the proof, so the embedded half is this key's and no other's.
  const verified = await jwtVerify(compact, key.publicJwk);
  expect(verified.payload.htm).toBe("GET");
});

test("the key hands out nothing but its public half and its proofs", async () => {
  const key = await newDpopKey();
  // The private half is held in the closure that signs, as a key `jose` made non-extractable, so
  // no caller of this package can reach it and `crypto.subtle` would refuse to export it.
  expect(Object.keys(key).sort()).toEqual(["proof", "publicJwk"]);
  expect(Object.hasOwn(key.publicJwk, "d")).toBe(false);
});

test("a query and a fragment are no part of the target URI", async () => {
  const key = await newDpopKey();
  const compact = await key.proof({
    method: "GET",
    url: "https://dispatch.test/api/v1/issues?limit=5#top",
  });
  expect(part(compact, 1).htu).toBe("https://dispatch.test/api/v1/issues");
});

test("each proof carries a jti of its own", async () => {
  const key = await newDpopKey();
  const first = part(await key.proof({ method: "GET", url: "https://dispatch.test/a" }), 1);
  const second = part(await key.proof({ method: "GET", url: "https://dispatch.test/a" }), 1);
  expect(first.jti).not.toBe(second.jti);
  expect(String(first.jti).length).toBeGreaterThan(0);
});

test("a request presenting an access token names it in ath, and only then", async () => {
  const key = await newDpopKey();
  const accessToken = "broker-access-token";
  const withToken = part(
    await key.proof({ method: "POST", url: "https://dispatch.test/api/v1/comments", accessToken }),
    1
  );
  const digest = await crypto.subtle.digest("SHA-256", new TextEncoder().encode(accessToken));
  expect(withToken.ath).toBe(Buffer.from(digest).toString("base64url"));
  const without = part(await key.proof({ method: "GET", url: "https://dispatch.test/a" }), 1);
  expect(Object.hasOwn(without, "ath")).toBe(false);
});

test("the server's nonce is carried once it has sent one", async () => {
  const key = await newDpopKey();
  const claims = part(
    await key.proof({ method: "POST", url: "http://broker.test/v1/oauth/token", nonce: "n-1" }),
    1
  );
  expect(claims.nonce).toBe("n-1");
  expect(Object.keys(claims).sort()).toEqual(["htm", "htu", "iat", "jti", "nonce"]);
});
