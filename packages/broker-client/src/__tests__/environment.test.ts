// packages/broker-client/src/__tests__/environment.test.ts
//
// What a deployment's variables name, and what a deployment that names half of one is told.

import { expect, test } from "bun:test";
import { mkdtemp, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import {
  BROKER_URL_VAR,
  brokerTokenClient,
  IDENTITY_VAR,
  MACHINE_AUDIENCE_VAR,
  MACHINE_TOKEN_FILE_VAR,
  machineIdentity,
  tradeSession,
} from "../environment";
import { FORM_LAUNCHER_CREDENTIAL, FORM_LAUNCHER_CREDENTIAL_DECLINED } from "../wire";

test("a machine that names no identity trades nothing, and that is no error", async () => {
  expect(machineIdentity({})).toBeUndefined();
  expect(machineIdentity({ [BROKER_URL_VAR]: "http://broker.test" })).toBeUndefined();
  expect(await brokerTokenClient({ [BROKER_URL_VAR]: "http://broker.test" })).toBeUndefined();
  // A named identity with no broker to spend it at trades nothing either.
  expect(
    await brokerTokenClient({
      [IDENTITY_VAR]: "kubernetes",
      [MACHINE_TOKEN_FILE_VAR]: "/var/run/secrets/token",
    })
  ).toBeUndefined();
});

test("each identity is read from its own variables", () => {
  const pod = machineIdentity({
    [IDENTITY_VAR]: "kubernetes",
    [MACHINE_TOKEN_FILE_VAR]: "/var/run/secrets/exchange/token",
  });
  expect(pod?.describe()).toBe(
    "the projected service-account token /var/run/secrets/exchange/token"
  );
  const machine = machineIdentity({
    [IDENTITY_VAR]: "metadata",
    [MACHINE_AUDIENCE_VAR]: "example-audience",
  });
  expect(machine?.describe()).toContain("example-audience");
});

test("an identity named with its variables missing is refused naming them", () => {
  expect(() => machineIdentity({ [IDENTITY_VAR]: "kubernetes" })).toThrow(
    `${MACHINE_TOKEN_FILE_VAR} is required by ${IDENTITY_VAR}=kubernetes`
  );
  expect(() => machineIdentity({ [IDENTITY_VAR]: "metadata" })).toThrow(
    `${MACHINE_AUDIENCE_VAR} is required by ${IDENTITY_VAR}=metadata`
  );
  expect(() => machineIdentity({ [IDENTITY_VAR]: "pool" })).toThrow("agent-secrets");
  expect(() => machineIdentity({ [IDENTITY_VAR]: "ec2" })).toThrow(
    'AGENT_SECRETS_MACHINE_IDENTITY="ec2" names no identity this machine can prove'
  );
});

test("a client is built for the broker, the identity and the session the variables name", async () => {
  const client = await brokerTokenClient({
    [BROKER_URL_VAR]: "http://broker.test/",
    [IDENTITY_VAR]: "kubernetes",
    [MACHINE_TOKEN_FILE_VAR]: "/var/run/secrets/exchange/token",
    AGENT_SECRETS_SESSION: "example-worker-session",
  });
  expect(client?.describe()).toBe(
    "the projected service-account token /var/run/secrets/exchange/token"
  );
  // The key is this process's own, made once with the client, and only its public half exists
  // outside the signer.
  expect(Object.hasOwn(client?.key.publicJwk ?? {}, "d")).toBe(false);
});

test("a client this factory builds asks the broker for no launcher credential", async () => {
  // Every TypeScript caller wants a Dispatch access token and nothing else, and a key is one
  // process's and one session's: a trade that asked would leave a launcher_credentials row and
  // an audit row behind for each `dispatch` call, hook run and session, on the operator's
  // machine-logins list and removed by nothing.
  const forms: URLSearchParams[] = [];
  const broker = Bun.serve({
    port: 0,
    hostname: "127.0.0.1",
    fetch: async (request) => {
      forms.push(new URLSearchParams(await request.text()));
      return Response.json({
        access_token: "broker-access-token",
        issued_token_type: "urn:ietf:params:oauth:token-type:access_token",
        token_type: "DPoP",
        expires_in: 600,
      });
    },
  });
  try {
    const file = join(await mkdtemp(join(tmpdir(), "broker-environment-")), "token");
    await writeFile(file, "projected.service.account.token\n");
    const client = await brokerTokenClient({
      [BROKER_URL_VAR]: `http://127.0.0.1:${broker.port}`,
      [IDENTITY_VAR]: "kubernetes",
      [MACHINE_TOKEN_FILE_VAR]: file,
      AGENT_SECRETS_SESSION: "example-worker-session",
    });
    const token = await client?.token();
    expect(token?.credentialId).toBe("");
    expect(forms).toHaveLength(1);
    expect(forms[0]?.get(FORM_LAUNCHER_CREDENTIAL)).toBe(FORM_LAUNCHER_CREDENTIAL_DECLINED);
  } finally {
    broker.stop(true);
  }
});

test("the session a trade writes under is the caller's own, and the variable's where it has none", () => {
  const deployment = { AGENT_SECRETS_SESSION: " legion-demo-controller " };
  // A caller that acts as a session: its token must name that session, since the Envoy
  // listener refuses a write acting as one whose token names another.
  expect(tradeSession(deployment, "ses_01a9")).toBe("ses_01a9");
  expect(tradeSession({}, " ses_01a9 ")).toBe("ses_01a9");
  // A caller that acts as no session: a daemon, a command, the helper.
  expect(tradeSession(deployment)).toBe("legion-demo-controller");
  expect(tradeSession(deployment, "   ")).toBe("legion-demo-controller");
  // Neither: the trade names no session, and the fleet entry's own name stands.
  expect(tradeSession({})).toBe("");
});
