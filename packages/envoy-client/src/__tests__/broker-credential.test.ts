// packages/envoy-client/src/__tests__/broker-credential.test.ts
//
// The credential a machine makes out of its own identity: when a session uses it (only when no
// bearer is configured at all), what it puts on a request, and that it renews mid-session with
// nothing restarted.

import { afterEach, expect, test } from "bun:test";
import { mkdtempSync, writeFileSync } from "node:fs";
import * as os from "node:os";
import * as path from "node:path";
import { forgetBrokerClients } from "../broker-credential";
import { type DispatchEnvironment, dispatchCredential, dispatchSettings } from "../dispatch-config";
import { DispatchClient } from "../dispatch-http";
import { readEnvoyConfig } from "../envoy-config";
import { envoyCredential, envoySettings } from "../envoy-settings";

afterEach(forgetBrokerClients);

const servers: { stop: (closeActive?: boolean) => void }[] = [];
afterEach(() => {
  for (const server of servers.splice(0)) server.stop(true);
});

function tempDir(): string {
  return mkdtempSync(path.join(os.tmpdir(), "broker-credential-"));
}

/** The config a caller with no envoy.json reads: the neutral file, which does not exist. */
function noFile(env: DispatchEnvironment) {
  return readEnvoyConfig(path.join(tempDir(), "envoy.json"), env, { cwd: tempDir() });
}

/** One trade the stub broker was asked. */
interface Trade {
  readonly form: URLSearchParams;
  readonly proof: Record<string, unknown>;
}

/** A broker that mints an access token good for `seconds`, numbering each one. */
function stubBroker(seconds: number): { url: string; trades: Trade[] } {
  const trades: Trade[] = [];
  const server = Bun.serve({
    port: 0,
    hostname: "127.0.0.1",
    fetch: async (request) => {
      const parts = (request.headers.get("DPoP") ?? "").split(".");
      trades.push({
        form: new URLSearchParams(await request.text()),
        proof: JSON.parse(Buffer.from(parts[1] ?? "", "base64url").toString("utf8")),
      });
      return Response.json({
        access_token: `broker-access-token-${trades.length}`,
        issued_token_type: "urn:ietf:params:oauth:token-type:access_token",
        token_type: "DPoP",
        expires_in: seconds,
        credential_id: "cred-1",
      });
    },
  });
  servers.push(server);
  return { url: `http://127.0.0.1:${server.port}`, trades };
}

/** One request the stub Dispatch was sent. */
interface Asked {
  readonly authorization: string;
  readonly proof: string;
}

/** A Dispatch that answers an empty issue list and keeps what it was sent. */
function stubDispatch(): { url: string; asked: Asked[] } {
  const asked: Asked[] = [];
  const server = Bun.serve({
    port: 0,
    hostname: "127.0.0.1",
    fetch: (request) => {
      asked.push({
        authorization: request.headers.get("Authorization") ?? "",
        proof: request.headers.get("DPoP") ?? "",
      });
      return Response.json([]);
    },
  });
  servers.push(server);
  return { url: `http://127.0.0.1:${server.port}`, asked };
}

/** A pod's projected token file. */
function projectedFile(token: string): string {
  const file = path.join(tempDir(), "token");
  writeFileSync(file, `${token}\n`);
  return file;
}

/** The environment of a pod that has an identity to trade and no Dispatch bearer at all. */
function podEnvironment(brokerUrl: string, dispatchUrl: string): DispatchEnvironment {
  return {
    HOME: tempDir(),
    DISPATCH_URL: dispatchUrl,
    ENVOY_URL: dispatchUrl,
    AGENT_SECRETS_URL: brokerUrl,
    AGENT_SECRETS_MACHINE_IDENTITY: "kubernetes",
    AGENT_SECRETS_MACHINE_TOKEN_FILE: projectedFile("projected.service.account.token"),
    AGENT_SECRETS_SESSION: "example-worker-session",
  };
}

test("a configured bearer is still the credential, whatever this machine could trade", () => {
  const broker = stubBroker(600);
  const env = { ...podEnvironment(broker.url, "http://dispatch.test"), DISPATCH_TOKEN: "written" };
  const resolution = dispatchSettings(env, noFile(env));
  expect(resolution).toEqual({ enabled: true, url: "http://dispatch.test", token: "written" });
  expect(dispatchCredential(resolution as { token: string })).toBe("written");
});

test("with no bearer at all, the session's credential is its own identity traded at the broker", async () => {
  const broker = stubBroker(600);
  const dispatch = stubDispatch();
  const env = podEnvironment(broker.url, dispatch.url);
  const resolution = dispatchSettings(env, noFile(env));
  if (!resolution.enabled) throw new Error(`Dispatch is off: ${resolution.error}`);
  expect(resolution.token).toBe("");
  expect(resolution.brokered?.scheme).toBe("DPoP");

  await new DispatchClient(resolution.url, dispatchCredential(resolution)).listAllIssues({});

  // One trade of the pod's own token, under the session the deployment named.
  expect(broker.trades.length).toBe(1);
  expect(broker.trades[0]?.form.get("subject_token")).toBe("projected.service.account.token");
  expect(broker.trades[0]?.form.get("session")).toBe("example-worker-session");
  // The access token is presented under the DPoP scheme, with a proof naming this request and
  // hashing the token it is presented beside: a bearer copy of it buys nothing.
  const asked = dispatch.asked[0];
  expect(asked?.authorization).toBe("DPoP broker-access-token-1");
  const claims = JSON.parse(
    Buffer.from(asked?.proof.split(".")[1] ?? "", "base64url").toString("utf8")
  );
  expect(claims.htm).toBe("GET");
  expect(claims.htu).toBe(`${dispatch.url}/api/v1/issues`);
  const digest = await crypto.subtle.digest(
    "SHA-256",
    new TextEncoder().encode("broker-access-token-1")
  );
  expect(claims.ath).toBe(Buffer.from(digest).toString("base64url"));
});

test("a session past its token's life trades again, with nothing restarted", async () => {
  // One second of life is the broker's own minimum (BROKER_ACCESS_TOKEN_SECONDS), so every
  // request after the first is inside the renewal window.
  const broker = stubBroker(1);
  const dispatch = stubDispatch();
  const env = podEnvironment(broker.url, dispatch.url);
  const resolution = dispatchSettings(env, noFile(env));
  if (!resolution.enabled) throw new Error(`Dispatch is off: ${resolution.error}`);
  const client = new DispatchClient(resolution.url, dispatchCredential(resolution));

  await client.listAllIssues({});
  await client.listAllIssues({});

  expect(broker.trades.length).toBe(2);
  expect(dispatch.asked.map((one) => one.authorization)).toEqual([
    "DPoP broker-access-token-1",
    "DPoP broker-access-token-2",
  ]);
});

test("the listener takes the same credential, and a configured bearer still wins", () => {
  const broker = stubBroker(600);
  const env = podEnvironment(broker.url, "http://listener.test");
  const brokered = envoySettings(env, noFile(env), { nats: false });
  if (!brokered.enabled) throw new Error("Envoy is off");
  expect(brokered.token).toBeUndefined();
  expect(envoyCredential(brokered)).toBe(brokered.brokered as never);

  const written = envoySettings({ ...env, ENVOY_TOKEN: "listener-token" }, noFile(env), {
    nats: false,
  });
  if (!written.enabled) throw new Error("Envoy is off");
  expect(written.brokered).toBeUndefined();
  expect(envoyCredential(written)).toBe("listener-token");
});

test("a client given an empty credential is refused rather than sending `Bearer `", () => {
  expect(() => new DispatchClient("http://dispatch.test", "")).toThrow("needs a credential");
});
