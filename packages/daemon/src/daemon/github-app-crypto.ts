import { createPrivateKey } from "node:crypto";

function toBase64Url(buffer: ArrayBuffer | Uint8Array): string {
  const bytes = buffer instanceof Uint8Array ? buffer : new Uint8Array(buffer);
  return Buffer.from(bytes)
    .toString("base64")
    .replace(/\+/g, "-")
    .replace(/\//g, "_")
    .replace(/=+$/, "");
}

function toPkcs8Pem(privateKeyPem: string): string {
  if (privateKeyPem.includes("BEGIN PRIVATE KEY")) {
    return privateKeyPem;
  }
  const pkcs8 = createPrivateKey(privateKeyPem).export({ type: "pkcs8", format: "pem" });
  if (typeof pkcs8 !== "string") {
    throw new Error("Expected PKCS#8 private key PEM output");
  }
  return pkcs8;
}

function pemToDer(pem: string): ArrayBuffer {
  const base64 = pem
    .replace(/-----BEGIN [^-]+-----/, "")
    .replace(/-----END [^-]+-----/, "")
    .replace(/\s/g, "");
  const binary = Buffer.from(base64, "base64");
  return binary.buffer.slice(binary.byteOffset, binary.byteOffset + binary.byteLength);
}

export async function generateJwt(appId: string, privateKeyPem: string): Promise<string> {
  const pkcs8Pem = toPkcs8Pem(privateKeyPem);
  const der = pemToDer(pkcs8Pem);
  const key = await crypto.subtle.importKey(
    "pkcs8",
    der,
    { name: "RSASSA-PKCS1-v1_5", hash: "SHA-256" },
    false,
    ["sign"]
  );
  const now = Math.floor(Date.now() / 1000);
  const header = toBase64Url(
    new TextEncoder().encode(JSON.stringify({ alg: "RS256", typ: "JWT" }))
  );
  const payload = toBase64Url(
    new TextEncoder().encode(JSON.stringify({ iss: appId, iat: now - 60, exp: now + 600 }))
  );
  const signingInput = `${header}.${payload}`;
  const signature = await crypto.subtle.sign(
    "RSASSA-PKCS1-v1_5",
    key,
    new TextEncoder().encode(signingInput)
  );
  return `${signingInput}.${toBase64Url(signature)}`;
}
