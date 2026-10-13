import { afterAll, describe, expect, test } from "bun:test";
import {
  mkdirSync,
  mkdtempSync,
  readdirSync,
  readFileSync,
  rmSync,
  statSync,
  writeFileSync,
} from "node:fs";
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

// The bearer never reaches stdout, where an agent running the command would put it in its
// transcript: it goes to a 0600 file whose path the caller passes, and lib/operator-dispatch.sh
// reads it from there.
describe("bun lib/operator-dispatch.ts <bearer file>", () => {
  const cli = (args: readonly string[], env: Record<string, string>) =>
    Bun.spawnSync(["bun", join(import.meta.dir, "operator-dispatch.ts"), ...args], {
      cwd,
      env: { PATH: process.env.PATH ?? "", ...env },
    });

  test("writes the bearer to the file, owner-only, and prints the file and the URL alone", () => {
    const held = mkdtempSync(join(dir, "held."));
    const bearer = join(held, "bearer");
    const run = cli([bearer], { HOME: home });
    expect(run.exitCode).toBe(0);
    expect(JSON.parse(run.stdout.toString())).toEqual({
      file: join(home, ".config", "legion", "envoy.json"),
      url: "https://dispatch.internal.example",
    });
    expect(run.stdout.toString()).not.toContain("neutral-1");
    expect(readFileSync(bearer, "utf-8")).toBe("neutral-1");
    expect(statSync(bearer).mode & 0o777).toBe(0o600);
  });

  test("writes no file when Dispatch is off or refused, and never over one that is there", () => {
    const held = mkdtempSync(join(dir, "held."));
    const off = cli([join(held, "off")], { HOME: join(dir, "empty-home") });
    expect(off.exitCode).toBe(0);
    expect(JSON.parse(off.stdout.toString())).toMatchObject({ notice: expect.any(String) });
    const refused = cli([join(held, "refused")], {
      HOME: home,
      DISPATCH_URL: "https://other.internal.example",
    });
    expect(refused.exitCode).toBe(1);
    expect(refused.stdout.toString()).not.toContain("neutral-1");
    expect(readdirSync(held)).toEqual([]);

    const planted = join(held, "planted");
    writeFileSync(planted, "", { mode: 0o644 });
    const over = cli([planted], { HOME: home });
    expect(over.exitCode).toBe(1);
    expect(JSON.parse(over.stdout.toString()).error).toContain(planted);
    expect(readFileSync(planted, "utf-8")).toBe("");
  });

  test("refuses to run without the file's path", () => {
    const run = cli([], { HOME: home });
    expect(run.exitCode).toBe(2);
    expect(run.stdout.toString()).toBe("");
    expect(run.stderr.toString()).toContain("usage: bun scripts/e2e/lib/operator-dispatch.ts");
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

describe("operator_dispatch_hold_token and production_baseline", () => {
  test("the held bearer leaves the environment, and the baseline is the newest event id read with it", async () => {
    const requests: { path: string; auth: string | null }[] = [];
    const events: Record<string, number> = { "OPS-1": 7, "OPS-2": 12 };
    const server = Bun.serve({
      port: 0,
      fetch(request) {
        const url = new URL(request.url);
        requests.push({ path: url.pathname, auth: request.headers.get("authorization") });
        if (url.pathname === "/api/v1/issues")
          return Response.json([{ key: "OPS-1" }, { key: "OPS-2" }]);
        const key = /^\/api\/v1\/issues\/([^/]+)\/events$/.exec(url.pathname)?.[1];
        return key === undefined
          ? new Response("", { status: 404 })
          : Response.json([{ id: events[key] }]);
      },
    });
    const work = mkdtempSync(join(dir, "work."));
    const script = `fail() { printf 'FAIL %s\\n' "$*"; exit 1; }
note() { printf 'NOTE %s\\n' "$*"; }
work=$2
. "$1"
operator_dispatch_hold_token
printf 'held:%s\\n' "\${DISPATCH_TOKEN-unset}"
production_baseline
printf '%s|%s|%s\\n' "$prod_dispatch_url" "$prod_baseline" "$(ls -A "$work")"`;
    try {
      const shell = Bun.spawn(
        ["bash", "-c", script, "_", join(import.meta.dir, "operator-dispatch.sh"), work],
        {
          cwd,
          env: {
            HOME: join(dir, "empty-home"),
            PATH: process.env.PATH ?? "",
            DISPATCH_URL: `http://127.0.0.1:${server.port}`,
            DISPATCH_TOKEN: "operator-bearer",
          },
          stdout: "pipe",
          stderr: "pipe",
        }
      );
      const stdout = await new Response(shell.stdout).text();
      expect(await shell.exited).toBe(0);
      expect(stdout).toBe(`held:unset\nhttp://127.0.0.1:${server.port}|12|\n`);
      expect(requests.map((request) => request.path)).toEqual([
        "/api/v1/issues",
        "/api/v1/issues/OPS-1/events",
        "/api/v1/issues/OPS-2/events",
      ]);
      expect(new Set(requests.map((request) => request.auth))).toEqual(
        new Set(["Bearer operator-bearer"])
      );
    } finally {
      server.stop(true);
    }
  });
});
