import { describe, expect, test } from "bun:test";
import { fileURLToPath } from "node:url";

import type { HarnessListener } from "../harness-listeners";

// The test preload (web/src/__tests__/setup.ts) puts happy-dom's Response on globalThis, so a
// child's output is read with Bun's own (e2e/__tests__/round-robin-proxy.test.ts says why).
const NativeResponse = (await Bun.fetch("data:,")).constructor as typeof Response;

const module = (name: string) => fileURLToPath(new URL(`../${name}`, import.meta.url));
const runServer = module("run-server.sh");
const runSignInIssuer = module("run-signin-issuer.sh");

async function run(argv: string[], env: Record<string, string>) {
  const child = Bun.spawn(argv, {
    env: { PATH: process.env.PATH ?? "", ...env },
    stderr: "pipe",
    stdout: "pipe",
  });
  const [code, stdout, stderr] = await Promise.all([
    child.exited,
    new NativeResponse(child.stdout).text(),
    new NativeResponse(child.stderr).text(),
  ]);
  return { code, stderr, stdout };
}

// A process of its own per import: the refusal is the module's evaluation, which runs once.
const importPorts = (env: Record<string, string>) =>
  run(["bun", "-e", `await import(${JSON.stringify(module("harness-ports.ts"))})`], env);

/** The listener table as an importer in `env` builds it: the table is built at evaluation, from
 *  the environment, so each environment needs a process of its own. */
async function listenersIn(env: Record<string, string>): Promise<HarnessListener[]> {
  const script =
    `const { harnessListeners } = await import(${JSON.stringify(module("harness-listeners.ts"))});` +
    "console.log(JSON.stringify(harnessListeners));";
  const { code, stderr, stdout } = await run(["bun", "-e", script], env);
  expect(stderr).toBe("");
  expect(code).toBe(0);
  return JSON.parse(stdout) as HarnessListener[];
}

const twoProcesses = { DISPATCH_E2E_PROCESSES: "2" };
const oidc = { ...twoProcesses, DISPATCH_E2E_SIGNIN: "oidc" };

describe("DISPATCH_E2E_SIGNIN", () => {
  test("every harness importer refuses a value other than oidc or unset, naming those", async () => {
    for (const value of ["dev", "OIDC", " oidc", "1"]) {
      const { code, stderr } = await importPorts({ ...twoProcesses, DISPATCH_E2E_SIGNIN: value });
      expect(code).not.toBe(0);
      expect(stderr).toContain(
        `DISPATCH_E2E_SIGNIN must be "oidc" or unset, not ${JSON.stringify(value)}`
      );
    }
    for (const env of [{}, { DISPATCH_E2E_SIGNIN: "" }, oidc] as Record<string, string>[]) {
      expect((await importPorts(env)).code).toBe(0);
    }
  });

  test("oidc is refused without two processes", async () => {
    for (const processes of ["", "1"]) {
      const { code, stderr } = await importPorts({
        DISPATCH_E2E_PROCESSES: processes,
        DISPATCH_E2E_SIGNIN: "oidc",
      });
      expect(code).not.toBe(0);
      expect(stderr).toContain("DISPATCH_E2E_SIGNIN=oidc needs DISPATCH_E2E_PROCESSES=2");
    }
  });

  test("run-server.sh refuses an unknown value, and oidc without its inputs", async () => {
    const base = {
      ...twoProcesses,
      DATABASE_URL: "postgres://postgres@127.0.0.1:1/never-opened",
      DISPATCH_E2E_PORT: "9031",
      FAKE_BROKER_PORT: "9032",
      FAKE_ENVOY_PORT: "9033",
      FAKE_GITHUB_PORT: "9034",
    };
    const unknown = await run(["bash", runServer], { ...base, DISPATCH_E2E_SIGNIN: "dev" });
    expect(unknown.code).toBe(1);
    expect(unknown.stderr).toContain("DISPATCH_E2E_SIGNIN must be oidc or unset, not 'dev'");
    // Under oidc the dev sign-in key is not asked for; the issuer is.
    const issuerless = await run(["bash", runServer], { ...base, DISPATCH_E2E_SIGNIN: "oidc" });
    expect(issuerless.code).not.toBe(0);
    expect(issuerless.stderr).not.toContain("DISPATCH_E2E_DEV_SIGNIN_SHARED_KEY");
    expect(issuerless.stderr).toContain(
      "DISPATCH_E2E_SIGNIN_ISSUER_PORT must name the dev OIDC issuer"
    );
  });

  test("run-signin-issuer.sh refuses a port it cannot bind, and a missing input", async () => {
    const inputs = {
      DISPATCH_E2E_SIGNIN_AS: "alice@example.com",
      DISPATCH_E2E_SIGNIN_CLIENT_ID: "dispatch-e2e",
      DISPATCH_E2E_SIGNIN_CLIENT_SECRET: "secret",
      DISPATCH_E2E_SIGNIN_GROUP: "people",
    };
    const ported = await run(["bash", runSignInIssuer], {
      ...inputs,
      DISPATCH_E2E_SIGNIN_ISSUER_PORT: "9035",
    });
    expect(ported.code).toBe(2);
    expect(ported.stderr).toContain("unset DISPATCH_E2E_SIGNIN_ISSUER_PORT");
    const { DISPATCH_E2E_SIGNIN_CLIENT_SECRET: _, ...secretless } = inputs;
    const missing = await run(["bash", runSignInIssuer], secretless);
    expect(missing.code).not.toBe(0);
    expect(missing.stderr).toContain(
      "DISPATCH_E2E_SIGNIN_CLIENT_SECRET must give the secret of that client"
    );
  });

  test("oidc starts the issuer before both servers and hands all three one client secret", async () => {
    const listeners = await listenersIn(oidc);
    const ids = listeners.map((listener) => listener.id);
    const issuer = listeners.find((listener) => listener.id === "signin-issuer");
    const servers = listeners.filter(
      (listener) => listener.key.startsWith("dispatch") && listener.env
    );
    expect(ids.indexOf("signin-issuer")).toBeGreaterThan(-1);
    expect(ids.indexOf("signin-issuer")).toBeLessThan(ids.indexOf("round-robin-proxy"));
    expect(servers.map((server) => server.id)).toEqual(["dispatch-a", "dispatch-b"]);
    const issuerEnv = issuer?.env ?? {};
    const client = (env: Readonly<Record<string, string>>) => ({
      group: env.DISPATCH_E2E_SIGNIN_GROUP,
      id: env.DISPATCH_E2E_SIGNIN_CLIENT_ID,
      secret: env.DISPATCH_E2E_SIGNIN_CLIENT_SECRET,
    });
    expect(client(issuerEnv)).toEqual({
      group: "dispatch-e2e-people",
      id: "dispatch-e2e",
      secret: expect.stringMatching(/^[A-Za-z0-9_-]{43}$/),
    });
    const [a = {}, b = {}] = servers.map((server) => server.env ?? {});
    for (const env of [a, b]) {
      expect(client(env)).toEqual(client(issuerEnv));
      expect(env).not.toHaveProperty("DISPATCH_E2E_DEV_SIGNIN_SHARED_KEY");
    }
    expect(a.DISPATCH_E2E_SIGNING_KEY).toMatch(/^[A-Za-z0-9_-]{43}$/);
    expect(b.DISPATCH_E2E_SIGNING_KEY).toEqual(a.DISPATCH_E2E_SIGNING_KEY);
    expect(a.DISPATCH_E2E_SIGNING_KEY).not.toEqual(issuerEnv.DISPATCH_E2E_SIGNIN_CLIENT_SECRET);
  });

  test("dev sign-in starts no issuer and hands both servers one dev sign-in key", async () => {
    const listeners = await listenersIn(twoProcesses);
    expect(listeners.map((listener) => listener.id)).not.toContain("signin-issuer");
    const keys = listeners
      .filter((listener) => listener.env !== undefined)
      .map((listener) => listener.env ?? {});
    expect(keys).toHaveLength(2);
    for (const env of keys) {
      expect(env.DISPATCH_E2E_DEV_SIGNIN_SHARED_KEY).toBe(
        keys[0]?.DISPATCH_E2E_DEV_SIGNIN_SHARED_KEY
      );
      expect(env).not.toHaveProperty("DISPATCH_E2E_SIGNING_KEY");
      expect(env).not.toHaveProperty("DISPATCH_E2E_SIGNIN_CLIENT_SECRET");
    }
  });

  test("the issuer's port joins the collision check under oidc only", async () => {
    const resolve = [
      "bun",
      "-e",
      `(await import(${JSON.stringify(module("harness-ports.ts"))})).harnessPorts()`,
    ];
    const clash = { DISPATCH_E2E_SIGNIN_ISSUER_PORT: "9036", FAKE_ENVOY_PORT: "9036" };
    const underOidc = await run(resolve, { ...clash, ...oidc });
    expect(underOidc.code).not.toBe(0);
    expect(underOidc.stderr).toContain(
      "FAKE_ENVOY_PORT and DISPATCH_E2E_SIGNIN_ISSUER_PORT name port 9036"
    );
    expect((await run(resolve, { ...clash, ...twoProcesses })).code).toBe(0);
  });
});
