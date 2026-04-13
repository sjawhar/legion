import { describe, expect, test } from "bun:test";
import type { MachineConfig } from "../machines";
import { computeNatsUrls, computeTsnetEnvs } from "../services";

describe("computeNatsUrls", () => {
  const machines = [
    { name: "sami-agents-mx", nats: true },
    { name: "sami", nats: true },
    { name: "sami-claude", nats: true },
    { name: "ghost-wispr", nats: false },
  ];

  test("machine with local NATS peer gets 127.0.0.1 first, then remote peers", () => {
    const urls = computeNatsUrls("sami-agents-mx", true, machines);
    expect(urls).toBe("nats://127.0.0.1:4222,nats://sami:4222,nats://sami-claude:4222");
  });

  test("machine without local NATS peer gets all remote peers", () => {
    const urls = computeNatsUrls("ghost-wispr", false, machines);
    expect(urls).toBe("nats://sami-agents-mx:4222,nats://sami:4222,nats://sami-claude:4222");
  });
});

describe("computeTsnetEnvs", () => {
  const baseMachine: MachineConfig = {
    name: "test-machine",
    machineId: "test-machine",
    listener: {},
  };

  const tsnetMachine: MachineConfig = {
    ...baseMachine,
    listener: {
      tsnet: {
        hostname: "envoy-listener-test",
        stateDir: "/var/lib/envoy-tsnet/listener-test",
      },
    },
  };

  const tsnetMachineWithTags: MachineConfig = {
    ...baseMachine,
    listener: {
      tsnet: {
        hostname: "envoy-listener-test",
        stateDir: "/var/lib/envoy-tsnet/listener-test",
        tags: "tag:envoy",
      },
    },
  };

  const emptySecrets = {
    githubWebhookSecret: {} as any,
    slackSigningSecret: {} as any,
  };

  test("returns disabled when tsnet not configured", () => {
    const envs = computeTsnetEnvs(baseMachine, emptySecrets);
    expect(envs).toEqual(["ENVOY_TSNET_ENABLED=false"]);
  });

  test("returns enabled env vars when tsnet configured", () => {
    const envs = computeTsnetEnvs(tsnetMachine, emptySecrets);
    expect(envs).toContain("ENVOY_TSNET_ENABLED=true");
    expect(envs).toContain("ENVOY_TSNET_HOSTNAME=envoy-listener-test");
    expect(envs).toContain("ENVOY_TSNET_STATE_DIR=/var/lib/envoy-tsnet/listener-test");
  });

  test("includes tags when configured on machine", () => {
    const envs = computeTsnetEnvs(tsnetMachineWithTags, emptySecrets);
    expect(envs).toContain("ENVOY_TSNET_TAGS=tag:envoy");
  });

  test("omits tags when not configured on machine", () => {
    const envs = computeTsnetEnvs(tsnetMachine, emptySecrets);
    const hasTagsEnv = envs.some((e) => typeof e === "string" && e.startsWith("ENVOY_TSNET_TAGS="));
    expect(hasTagsEnv).toBe(false);
  });

  test("includes OAuth client ID when secret provided", () => {
    const secrets = {
      ...emptySecrets,
      tsnetOAuthClientId: {} as any,
    };
    const envs = computeTsnetEnvs(tsnetMachineWithTags, secrets);
    // OAuth client ID is a pulumi.interpolate output — verify extra entry
    expect(envs.length).toBe(5); // enabled + hostname + stateDir + tags + clientId
  });

  test("includes OAuth client secret when secret provided", () => {
    const secrets = {
      ...emptySecrets,
      tsnetOAuthClientSecret: {} as any,
    };
    const envs = computeTsnetEnvs(tsnetMachineWithTags, secrets);
    expect(envs.length).toBe(5); // enabled + hostname + stateDir + tags + clientSecret
  });

  test("includes both OAuth credentials when provided", () => {
    const secrets = {
      ...emptySecrets,
      tsnetOAuthClientId: {} as any,
      tsnetOAuthClientSecret: {} as any,
    };
    const envs = computeTsnetEnvs(tsnetMachineWithTags, secrets);
    // enabled + hostname + stateDir + tags + clientId + clientSecret
    expect(envs.length).toBe(6);
  });

  test("omits OAuth credentials when not provided", () => {
    const envs = computeTsnetEnvs(tsnetMachineWithTags, emptySecrets);
    // enabled + hostname + stateDir + tags (no OAuth)
    expect(envs.length).toBe(4);
  });
});
