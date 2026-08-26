import { expect, it } from "bun:test";
import { TokenManager } from "../github-apps";

function toPem(label: string, der: ArrayBuffer): string {
  const base64 = Buffer.from(new Uint8Array(der)).toString("base64");
  const chunks = base64.match(/.{1,64}/g) ?? [];
  return [`-----BEGIN ${label}-----`, ...chunks, `-----END ${label}-----`, ""].join("\n");
}

async function generatePrivateKey(): Promise<string> {
  const keyPair = await crypto.subtle.generateKey(
    {
      name: "RSASSA-PKCS1-v1_5",
      modulusLength: 2048,
      publicExponent: new Uint8Array([1, 0, 1]),
      hash: "SHA-256",
    },
    true,
    ["sign", "verify"]
  );
  return toPem("PRIVATE KEY", await crypto.subtle.exportKey("pkcs8", keyPair.privateKey));
}

it("retries a previously missing installation after the negative cache expires", async () => {
  const privateKey = await generatePrivateKey();
  let now = 0;
  let installationAdded = false;
  let installationDiscoveries = 0;
  const manager = new TokenManager(
    {
      implement: { appId: "111", privateKey, installations: {} },
    },
    {
      now: () => now,
      fetchFn: async (input) => {
        const url = new URL(String(input));
        if (url.pathname === "/app/installations") {
          installationDiscoveries += 1;
          return Response.json(
            installationAdded ? [{ id: 222, account: { login: "new-org" } }] : []
          );
        }
        if (url.pathname === "/app") return Response.json({ slug: "legion-implementer" });
        if (url.pathname === "/users/legion-implementer%5Bbot%5D") {
          return Response.json({ id: 271566630 });
        }
        if (url.pathname === "/app/installations/222/access_tokens") {
          return Response.json(
            { token: "ghs_new_org", expires_at: "2099-01-01T00:00:00Z" },
            { status: 201 }
          );
        }
        throw new Error(`Unexpected GitHub request: ${url.pathname}`);
      },
    }
  );

  await expect(manager.getToken("implement", "new-org")).rejects.toThrow(
    "github_app_not_installed"
  );
  expect(installationDiscoveries).toBe(2);

  installationAdded = true;
  now += 30_001;

  await expect(manager.getToken("implement", "new-org")).resolves.toMatchObject({
    token: "ghs_new_org",
  });
  expect(installationDiscoveries).toBe(3);
});
