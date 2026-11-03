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
import { type DispatchEnvironment, dispatchSettings } from "../dispatch-config";
import { DispatchClient } from "../dispatch-http";
import { readEnvoyConfig } from "../envoy-config";
import { envoySettings } from "../envoy-settings";
import { createEnvoyClient } from "../transport";

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

/** The environment of a pod that has an identity to trade and no Dispatch bearer at all. It
 * names no `AGENT_SECRETS_SESSION`: nothing in a plugin's environment sets one, and the session
 * a trade writes under is the one the caller passes. */
function podEnvironment(brokerUrl: string, dispatchUrl: string): DispatchEnvironment {
  return {
    HOME: tempDir(),
    DISPATCH_URL: dispatchUrl,
    ENVOY_URL: dispatchUrl,
    AGENT_SECRETS_URL: brokerUrl,
    AGENT_SECRETS_MACHINE_IDENTITY: "kubernetes",
    AGENT_SECRETS_MACHINE_TOKEN_FILE: projectedFile("projected.service.account.token"),
  };
}

test("a configured bearer is still the credential, whatever this machine could trade", () => {
  const broker = stubBroker(600);
  const env = { ...podEnvironment(broker.url, "http://dispatch.test"), DISPATCH_TOKEN: "written" };
  const resolution = dispatchSettings(env, noFile(env));
  if (!resolution.enabled) throw new Error(`Dispatch is off: ${resolution.error}`);
  expect(resolution.credential.scheme).toBe("Bearer");
  expect(resolution.credential.secret()).toBe("written");
});

/** An envoy.json of a person's own, holding the Dispatch token that person writes Dispatch with:
 * what a box's operator has in `~/.config/legion/envoy.json`, and what a session launched on that
 * box reads as its chosen file. */
function personalFile(env: DispatchEnvironment, serverUrl: string, token: string) {
  const file = path.join(tempDir(), "envoy.json");
  writeFileSync(file, JSON.stringify({ dispatch: { serverUrl, token } }));
  return readEnvoyConfig(file, env, { cwd: tempDir() });
}

test("told to hold its credential at the broker, a session reads no person's token", () => {
  // `legion controller start` under `dispatch_token_broker` launches the controller in the
  // operator's own HOME, where an envoy.json holds that person's Dispatch token for the very
  // server the controller writes. Unset, the variable leaves that token the credential, which is
  // the controller writing Dispatch as the operator; set, the session trades for its own.
  const broker = stubBroker(600);
  const env = podEnvironment(broker.url, "http://dispatch.test");
  const config = personalFile(env, "http://dispatch.test", "a-person-s-dispatch-token");

  const shadowed = dispatchSettings(env, config, { session: "legion-demo-controller" });
  if (!shadowed.enabled) throw new Error(`Dispatch is off: ${shadowed.error}`);
  expect(shadowed.credential.secret()).toBe("a-person-s-dispatch-token");

  const forced = dispatchSettings({ ...env, DISPATCH_TOKEN_BROKER: "1" }, config, {
    session: "legion-demo-controller",
  });
  if (!forced.enabled) throw new Error(`Dispatch is off: ${forced.error}`);
  expect(forced.credential.scheme).toBe("DPoP");
  expect(forced.url).toBe("http://dispatch.test");
});

test("told to hold its credential at the broker, a session reads no bearer variable either", () => {
  const broker = stubBroker(600);
  const env = {
    ...podEnvironment(broker.url, "http://dispatch.test"),
    DISPATCH_TOKEN: "a-person-s-dispatch-token",
    DISPATCH_TOKEN_BROKER: "1",
  };
  const resolution = dispatchSettings(env, noFile(env), { session: "legion-demo-controller" });
  if (!resolution.enabled) throw new Error(`Dispatch is off: ${resolution.error}`);
  expect(resolution.credential.scheme).toBe("DPoP");
});

test("told to hold a credential it cannot trade for, a session writes Dispatch with nothing", () => {
  // The refusal is the point: a machine whose broker or identity has gone is one that writes
  // nothing, never one that falls back to the bearer the box happens to hold.
  const broker = stubBroker(600);
  const env = {
    ...podEnvironment(broker.url, "http://dispatch.test"),
    DISPATCH_TOKEN: "a-person-s-dispatch-token",
    DISPATCH_TOKEN_BROKER: "1",
  };

  const noBroker = dispatchSettings({ ...env, AGENT_SECRETS_URL: "" }, noFile(env));
  expect(noBroker.enabled).toBe(false);
  expect(noBroker.enabled === false && noBroker.error).toContain(
    "AGENT_SECRETS_URL names no secrets broker"
  );

  const noIdentity = dispatchSettings({ ...env, AGENT_SECRETS_MACHINE_IDENTITY: "" }, noFile(env));
  expect(noIdentity.enabled === false && noIdentity.error).toContain(
    "AGENT_SECRETS_MACHINE_IDENTITY names no identity this machine proves"
  );

  // A pool identity is the Go client's alone, so this process trades nothing with it; unforced it
  // is a machine with no TypeScript route, forced it is a session that cannot write.
  const pool = dispatchSettings({ ...env, AGENT_SECRETS_MACHINE_IDENTITY: "pool" }, noFile(env));
  expect(pool.enabled === false && pool.error).toContain("traded by the Go client alone");

  // A named identity missing its own variables still says which variable.
  const halfNamed = dispatchSettings({ ...env, AGENT_SECRETS_MACHINE_TOKEN_FILE: "" }, noFile(env));
  expect(halfNamed.enabled === false && halfNamed.error).toContain(
    "AGENT_SECRETS_MACHINE_TOKEN_FILE is required"
  );
});

test("a DISPATCH_TOKEN_BROKER that is neither 1 nor unset is refused", () => {
  // Read as on, a typo would turn a person's session into one that writes nothing; read as off,
  // it would put a deployment that meant the broker route back on the operator's bearer.
  const broker = stubBroker(600);
  const env = {
    ...podEnvironment(broker.url, "http://dispatch.test"),
    DISPATCH_TOKEN_BROKER: "true",
  };
  const resolution = dispatchSettings(env, noFile(env));
  expect(resolution.enabled).toBe(false);
  expect(resolution.enabled === false && resolution.error).toContain(
    'DISPATCH_TOKEN_BROKER is "true" and takes 1'
  );
});

test("with no bearer at all, the session's credential is its own identity traded at the broker", async () => {
  const broker = stubBroker(600);
  const dispatch = stubDispatch();
  const env = podEnvironment(broker.url, dispatch.url);
  const resolution = dispatchSettings(env, noFile(env), { session: "example-worker-session" });
  if (!resolution.enabled) throw new Error(`Dispatch is off: ${resolution.error}`);
  expect(resolution.credential.scheme).toBe("DPoP");

  await new DispatchClient(resolution.url, resolution.credential).listAllIssues({});

  // One trade of the pod's own token, under the session this caller named: nothing in the
  // environment does.
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
  const resolution = dispatchSettings(env, noFile(env), { session: "example-worker-session" });
  if (!resolution.enabled) throw new Error(`Dispatch is off: ${resolution.error}`);
  const client = new DispatchClient(resolution.url, resolution.credential);

  await client.listAllIssues({});
  await client.listAllIssues({});

  expect(broker.trades.length).toBe(2);
  expect(dispatch.asked.map((one) => one.authorization)).toEqual([
    "DPoP broker-access-token-1",
    "DPoP broker-access-token-2",
  ]);
});

test("each session of one process trades under its own name, read at every trade", async () => {
  // One Oh My Pi process holds several sessions over its life, and Claude Code mints a new id on
  // /clear. A token names one session, and the listener answers 403 to a session presenting
  // another's, so the name is read when the trade happens rather than fixed at resolution.
  const broker = stubBroker(600);
  const dispatch = stubDispatch();
  const env = podEnvironment(broker.url, dispatch.url);
  let current = "session-one";
  const resolution = dispatchSettings(env, noFile(env), { session: () => current });
  if (!resolution.enabled) throw new Error(`Dispatch is off: ${resolution.error}`);

  await new DispatchClient(resolution.url, resolution.credential).listAllIssues({});
  current = "session-two";
  await new DispatchClient(resolution.url, resolution.credential).listAllIssues({});
  // Back to the first: its client is the one this process already made, so no third trade.
  current = "session-one";
  await new DispatchClient(resolution.url, resolution.credential).listAllIssues({});

  expect(broker.trades.map((trade) => trade.form.get("session"))).toEqual([
    "session-one",
    "session-two",
  ]);
  expect(dispatch.asked.map((one) => one.authorization)).toEqual([
    "DPoP broker-access-token-1",
    "DPoP broker-access-token-2",
    "DPoP broker-access-token-1",
  ]);
});

test("the listener takes the same credential, and a configured bearer still wins", () => {
  const broker = stubBroker(600);
  const env = podEnvironment(broker.url, "http://listener.test");
  const brokered = envoySettings(env, noFile(env), { nats: false, session: "session-one" });
  if (!brokered.enabled) throw new Error("Envoy is off");
  expect(brokered.credential?.scheme).toBe("DPoP");

  const written = envoySettings({ ...env, ENVOY_TOKEN: "listener-token" }, noFile(env), {
    nats: false,
  });
  if (!written.enabled) throw new Error("Envoy is off");
  expect(written.credential?.scheme).toBe("Bearer");
  expect(written.credential?.secret()).toBe("listener-token");
});

/** A broker whose tokens name the session the trade asked to write under, as the real one's do
 *  (the broker records the minted token against that session and the listener reads it back at
 *  the introspection route). */
function sessionBroker(): { url: string; sessions: string[] } {
  const sessions: string[] = [];
  const server = Bun.serve({
    port: 0,
    hostname: "127.0.0.1",
    fetch: async (request) => {
      const session = new URLSearchParams(await request.text()).get("session") ?? "";
      sessions.push(session);
      return Response.json({
        access_token: `token-for-${session}`,
        issued_token_type: "urn:ietf:params:oauth:token-type:access_token",
        token_type: "DPoP",
        expires_in: 600,
      });
    },
  });
  servers.push(server);
  return { url: `http://127.0.0.1:${server.port}`, sessions };
}

/** One call the stub listener was sent: the session its body acts as, and the session the token
 *  it presented names. */
interface Acted {
  readonly path: string;
  readonly body: string;
  readonly token: string;
}

/** A listener that binds the session a broker token names, as the real one does
 *  (`sessionNotBound`, packages/envoy/cmd/listener/brokerauth.go): a body naming another session
 *  is 403, and nothing else about the call is judged. */
function sessionBoundListener(): { url: string; acted: Acted[] } {
  const acted: Acted[] = [];
  const server = Bun.serve({
    port: 0,
    hostname: "127.0.0.1",
    fetch: async (request) => {
      const token = (request.headers.get("Authorization") ?? "").replace(/^DPoP /, "");
      const sent: unknown = JSON.parse(await request.text());
      const named =
        typeof sent === "object" && sent !== null && "session_id" in sent ? sent.session_id : "";
      const body = typeof named === "string" ? named : "";
      acted.push({ path: new URL(request.url).pathname, body, token });
      if (token !== `token-for-${body}`) {
        return Response.json(
          { error: `this request acts as session ${body}, and its credential names another` },
          { status: 403 }
        );
      }
      return Response.json({ session_id: body, machine_id: "box", dir: "/work", topics: [] });
    },
  });
  servers.push(server);
  return { url: `http://127.0.0.1:${server.port}`, acted };
}

test("a role handed back for the session this process left presents that session's credential", async () => {
  // One Oh My Pi process switches sessions, and the session it leaves may still hold a role at
  // the listener. The hand-back acts as that old session, so it must present the old session's
  // own token: under the token of the session running now the listener answers 403, the
  // hand-back fails, and the role stays with a session whose subject this process has closed
  // until the registration's TTL lapses.
  const broker = sessionBroker();
  const listener = sessionBoundListener();
  const env = podEnvironment(broker.url, listener.url);
  let current = "session-one";
  const resolution = envoySettings(env, noFile(env), { nats: false, session: () => current });
  if (!resolution.enabled)
    throw new Error(`Envoy is off: ${resolution.error ?? resolution.notice}`);
  const client = createEnvoyClient({
    baseUrl: resolution.url,
    fetch,
    ...(resolution.credential === undefined ? {} : { credential: resolution.credential }),
  });

  await client.setRole({ sessionID: "session-one", role: "controller" });
  // The switch: `bindDispatchSession` has moved this process to the new session, and the pass
  // that follows hands the old session's role back.
  current = "session-two";
  await client.unsubscribe({
    sessionID: "session-one",
    topics: ["notifications.role.controller"],
  });
  await client.subscribe({
    sessionID: "session-two",
    directory: "/work",
    topics: [],
    port: 0,
    title: "",
    driving: false,
    plugin: { name: "@sjawhar/pi-envoy", version: "0.0.0" },
  });

  expect(listener.acted.map((one) => [one.path, one.body, one.token])).toEqual([
    ["/v1/roles/set", "session-one", "token-for-session-one"],
    ["/v1/interests/unsubscribe", "session-one", "token-for-session-one"],
    ["/v1/interests/subscribe", "session-two", "token-for-session-two"],
  ]);
  // One trade per session, and the session this process left answered out of the token it
  // already held rather than trading again.
  expect(broker.sessions).toEqual(["session-one", "session-two"]);
});

test("a pool identity is a machine with no TypeScript route, not an error", () => {
  // Only the Go client federates at a workload identity pool. Reading this as a mistake would
  // turn Envoy off on a machine whose listener asks for no credential at all.
  const broker = stubBroker(600);
  const env = {
    ...podEnvironment(broker.url, "http://listener.test"),
    AGENT_SECRETS_MACHINE_IDENTITY: "pool",
  };
  const envoy = envoySettings(env, noFile(env), { nats: false, session: "session-one" });
  if (!envoy.enabled) throw new Error(`Envoy is off: ${envoy.error ?? envoy.notice}`);
  expect(envoy.credential).toBeUndefined();

  // An identity this client cannot prove at all is still a mistake in the deployment.
  const unknown = envoySettings(
    { ...env, AGENT_SECRETS_MACHINE_IDENTITY: "nonsense" },
    noFile(env),
    { nats: false }
  );
  expect(unknown.enabled).toBe(false);
});
