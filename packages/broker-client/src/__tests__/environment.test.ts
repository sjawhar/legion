// packages/broker-client/src/__tests__/environment.test.ts
//
// What a deployment's variables name, and what a deployment that names half of one is told.

import { expect, test } from "bun:test";
import {
  BROKER_URL_VAR,
  brokerTokenClient,
  IDENTITY_VAR,
  MACHINE_AUDIENCE_VAR,
  MACHINE_TOKEN_FILE_VAR,
  machineIdentity,
} from "../environment";

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
