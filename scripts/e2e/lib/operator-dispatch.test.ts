import { afterAll, describe, expect, test } from "bun:test";
import { mkdirSync, mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { operatorDispatch } from "./operator-dispatch";

const dir = mkdtempSync(join(tmpdir(), "operator-dispatch-test."));
afterAll(() => rmSync(dir, { recursive: true, force: true }));

const home = join(dir, "home");
const profile = join(dir, "profile");
const cwd = join(dir, "cwd");
for (const directory of [join(home, ".config", "legion"), profile, cwd]) {
  mkdirSync(directory, { recursive: true });
}
writeFileSync(
  join(home, ".config", "legion", "envoy.json"),
  JSON.stringify({
    dispatch: {
      enabled: true,
      serverUrl: "https://dispatch.internal.example/",
      token: "neutral-1",
    },
  })
);
writeFileSync(
  join(profile, "envoy.json"),
  JSON.stringify({
    dispatch: { enabled: true, serverUrl: "https://profile.internal.example", token: "profile-2" },
  })
);

describe("operatorDispatch", () => {
  test("the neutral file's own server, with its own bearer", () => {
    expect(operatorDispatch({ HOME: home }, cwd)).toEqual({
      file: join(home, ".config", "legion", "envoy.json"),
      url: "https://dispatch.internal.example",
      token: "neutral-1",
    });
  });

  test("a named omp profile's file comes before the neutral one, as for every command", () => {
    const env = { HOME: home, OMP_PROFILE: "work", PI_CODING_AGENT_DIR: profile };
    expect(operatorDispatch(env, cwd)).toMatchObject({
      file: join(profile, "envoy.json"),
      token: "profile-2",
    });
  });

  test("a file's bearer never goes to the server DISPATCH_URL names instead", () => {
    const env = { HOME: home, DISPATCH_URL: "https://other.internal.example" };
    expect(() => operatorDispatch(env, cwd)).toThrow(
      "DISPATCH_URL names https://other.internal.example, but"
    );
    expect(() => operatorDispatch(env, cwd)).not.toThrow(/neutral-1/);
  });

  test("nothing configured is off, saying why", () => {
    expect(operatorDispatch({ HOME: join(dir, "empty-home") }, cwd)).toEqual({
      file: join(dir, "empty-home", ".config", "legion", "envoy.json"),
      notice: `no envoy.json at ${join(dir, "empty-home", ".config", "legion", "envoy.json")}: Dispatch is off`,
    });
  });
});

describe("operator_dispatch", () => {
  const script = `fail() { printf 'FAIL %s\\n' "$*"; exit 1; }
. "$1"
operator_dispatch
printf '%s|%s|%s|%s\\n' "$operator_dispatch_file" "$operator_dispatch_url" "$operator_dispatch_token" "$operator_dispatch_notice"`;
  const run = (directory: string, extra: Record<string, string> = {}) =>
    Bun.spawnSync(["bash", "-c", script, "_", join(import.meta.dir, "operator-dispatch.sh")], {
      cwd: directory,
      env: { HOME: home, PATH: process.env.PATH ?? "", ...extra },
    });
  const on = run(cwd);
  const refused = run(cwd, { DISPATCH_URL: "https://other.internal.example" });
  // A checkout holding only the OpenCode-era file, which names another server and bearer.
  const oldCheckout = join(dir, "old-checkout");
  const old = join(oldCheckout, ".opencode", "envoy.json");
  mkdirSync(join(oldCheckout, ".opencode"), { recursive: true });
  writeFileSync(
    old,
    JSON.stringify({
      dispatch: {
        enabled: true,
        serverUrl: "https://checkout.internal.example",
        token: "checkout-3",
      },
    })
  );
  const inOldCheckout = run(oldCheckout);

  test("sets the file, URL and bearer the client resolves", () => {
    expect(on?.exitCode).toBe(0);
    expect(on?.stdout.toString()).toBe(
      `${join(home, ".config", "legion", "envoy.json")}|https://dispatch.internal.example|neutral-1|\n`
    );
  });

  test("a refusal goes through the caller's fail, naming it and never the bearer", () => {
    expect(refused?.exitCode).toBe(1);
    const output = refused?.stdout.toString() ?? "";
    expect(output).toStartWith(
      "FAIL the operator's Dispatch: DISPATCH_URL names https://other.internal.example, but"
    );
    expect(output).not.toContain("neutral-1");
  });

  test("a checkout's .opencode/envoy.json is named on stderr with its rename, and the user file's server is used", () => {
    expect(inOldCheckout.exitCode).toBe(0);
    expect(inOldCheckout.stderr.toString()).toBe(
      `dispatch: ${old} is not read: mkdir -p ${join(oldCheckout, ".envoy")} && mv ${old} ${join(oldCheckout, ".envoy", "envoy.json")}\n`
    );
    expect(inOldCheckout.stdout.toString()).toBe(
      `${join(home, ".config", "legion", "envoy.json")}|https://dispatch.internal.example|neutral-1|\n`
    );
  });
});
