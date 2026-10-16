import { afterAll, afterEach, describe, expect, test } from "bun:test";
import { mkdirSync, mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";

// scripts/e2e/multi-repo-acceptance.sh is the Task 6.3 observer for the live Task 6.4 run. These
// tests pin its refusals and the jq paths its state-reading observations use against a fixed
// /legion/v1/state body, standing in for a live daemon with a Bun.serve stub — the script never
// gets a real daemon, Dispatch, or GitHub in this suite.
const script = join(import.meta.dir, "..", "multi-repo-acceptance.sh");
const dir = mkdtempSync(join(tmpdir(), "multi-repo-acceptance-test."));
afterAll(() => rmSync(dir, { recursive: true, force: true }));

let runs = 0;
let server: Server | undefined;

afterEach(() => {
  server?.stop(true);
  server = undefined;
});

// fakeBin writes an executable `name` on a fresh PATH-only directory that prints `output` for
// every call and logs its arguments to `name.log` beside it, so a test can assert what the
// script asked for without a real dispatch or gh.
function fakeBin(binDir: string, name: string, script_: string): void {
  writeFileSync(join(binDir, name), `#!/usr/bin/env bash\n${script_}\n`, { mode: 0o755 });
}

async function run(
  args: string[],
  env: Record<string, string> = {},
  fakes: { dispatch?: string; gh?: string } = {}
) {
  const work = join(dir, `run-${++runs}`);
  const bin = join(work, "bin");
  const evidence = join(work, "evidence");
  mkdirSync(bin, { recursive: true });
  mkdirSync(evidence, { recursive: true });
  fakeBin(bin, "dispatch", fakes.dispatch ?? 'echo "dispatch: unexpected call: $*" >&2; exit 1');
  fakeBin(bin, "gh", fakes.gh ?? 'echo "gh: unexpected call: $*" >&2; exit 1');
  const base: Record<string, string> = {
    PATH: `${bin}:${process.env.PATH}`,
    MULTIREPO_EVIDENCE_DIR: evidence,
  };
  // Bun.spawn, not spawnSync: a synchronous spawn blocks this process's event loop, and the
  // Bun.serve stub daemon these tests run in the same process never gets to answer the child's
  // curl request.
  const proc = Bun.spawn(["bash", script, ...args], {
    env: { ...base, ...env },
    stdout: "pipe",
    stderr: "pipe",
  });
  const [stdout, stderr] = await Promise.all([
    new Response(proc.stdout).text(),
    new Response(proc.stderr).text(),
  ]);
  const status = await proc.exited;
  return { status, stdout, stderr, evidence };
}

const fullInputs = (daemonUrl: string): Record<string, string> => ({
  LEGION_E2E_REPO_T: "acme/widgets",
  LEGION_E2E_REPO_C: "acme/gadgets",
  LEGION_E2E_PIN_FILE: "toolchain-pin.txt",
  LEGION_E2E_REPO_X: "other/outside",
  LEGION_E2E_REPO_O: "zeta/tools",
  LEGION_E2E_DAEMON_URL: daemonUrl,
  DISPATCH_URL: "http://127.0.0.1:1",
  DISPATCH_TOKEN_FILE: "/dev/null",
});

describe("required inputs", () => {
  test("every input unset: the refusal names the first missing one", async () => {
    const { status, stderr } = await run(["done", "X-1"], {});
    expect(status).not.toBe(0);
    expect(stderr).toContain("LEGION_E2E_REPO_T is unset");
  });

  test("each input missing in turn names itself", async () => {
    const all = fullInputs("http://127.0.0.1:1");
    for (const name of Object.keys(all)) {
      const env = { ...all };
      delete (env as Record<string, string>)[name];
      const { status, stderr } = await run(["done", "X-1"], env);
      expect(status).not.toBe(0);
      expect(stderr).toContain(`${name} is unset`);
    }
  });
});

describe("repository collisions", () => {
  test("T and O sharing an owner is refused", async () => {
    const env = fullInputs("http://127.0.0.1:1");
    env.LEGION_E2E_REPO_O = `${env.LEGION_E2E_REPO_T.split("/")[0]}/other-repo`;
    const { status, stderr } = await run(["done", "X-1"], env);
    expect(status).not.toBe(0);
    expect(stderr).toContain("LEGION_E2E_REPO_T and LEGION_E2E_REPO_O share an owner");
  });

  test("two repository variables naming the same repository is refused", async () => {
    const env = fullInputs("http://127.0.0.1:1");
    env.LEGION_E2E_REPO_C = env.LEGION_E2E_REPO_T;
    const { status, stderr } = await run(["done", "X-1"], env);
    expect(status).not.toBe(0);
    expect(stderr).toContain("LEGION_E2E_REPO_T and LEGION_E2E_REPO_C both name");
  });
});

describe("the daemon health refusal", () => {
  test("a stub whose /healthz answers 404 is refused, naming the status", async () => {
    server = Bun.serve({ port: 0, fetch: () => new Response("not found", { status: 404 }) });
    const env = fullInputs(`http://127.0.0.1:${server.port}`);
    const { status, stderr } = await run(["done", "X-1"], env);
    expect(status).not.toBe(0);
    expect(stderr).toContain(
      `LEGION_E2E_DAEMON_URL http://127.0.0.1:${server.port} is not a Legion daemon`
    );
    expect(stderr).toContain("404");
  });
});

// A fixed /legion/v1/state body: two children of PARENT-1, one on T and one on C, matching the
// fake dispatch's own Repository: lines, and an issue with no record at all for negative no-repo.
const stateBody = JSON.stringify({
  daemon: { project: "ACME" },
  admission: { cap: 1, active: [], waiting: [] },
  issues: {
    "ACME-2": {
      key: "ACME-2",
      generation: 1,
      phase: "implementing",
      status: "todo",
      repo: "acme/widgets",
    },
    "ACME-3": {
      key: "ACME-3",
      generation: 1,
      phase: "implementing",
      status: "todo",
      repo: "acme/gadgets",
    },
  },
  pendingStatusWrites: [],
  strandedClaims: [],
  capabilities: [],
  repositories: { owners: [], mapped: [], refused: [], caches: [] },
});

function stateServer(): Server {
  return Bun.serve({
    port: 0,
    fetch(req) {
      const url = new URL(req.url);
      if (url.pathname === "/healthz") return Response.json({ status: "ok" });
      if (url.pathname === "/legion/v1/state")
        return new Response(stateBody, { headers: { "content-type": "application/json" } });
      return new Response("not found", { status: 404 });
    },
  });
}

describe("children: the state half against a fixed body", () => {
  test("one child resolves to T and one to C in both Dispatch and the daemon's state: PASS", async () => {
    server = stateServer();
    const env = fullInputs(`http://127.0.0.1:${server.port}`);
    const dispatchFake = `
case "$*" in
  "read --ref dispatch://ACME-1/children")
    printf 'Key: ACME-1\\nChildren:\\n- ACME-2: widgets child (todo)\\n- ACME-3: gadgets child (todo)\\n'
    ;;
  "read --issue ACME-2")
    printf 'Key: ACME-2\\nStatus: todo\\nRepository: acme/widgets (own)\\n'
    ;;
  "read --issue ACME-3")
    printf 'Key: ACME-3\\nStatus: todo\\nRepository: acme/gadgets (own)\\n'
    ;;
  *)
    echo "dispatch: unexpected call: $*" >&2
    exit 1
    ;;
esac`;
    const { status, stdout, evidence } = await run(["children", "ACME-1"], env, {
      dispatch: dispatchFake,
    });
    expect(status).toBe(0);
    expect(stdout).toContain("CHECK children: PASS");
    expect(stdout).toContain("ACME-2: dispatch=acme/widgets state=acme/widgets");
    expect(stdout).toContain("ACME-3: dispatch=acme/gadgets state=acme/gadgets");
    expect(Bun.file(join(evidence, "children.txt")).size).toBeGreaterThan(0);
  });

  test("a child Dispatch and the daemon's state disagree on: FAIL", async () => {
    server = stateServer();
    const env = fullInputs(`http://127.0.0.1:${server.port}`);
    const dispatchFake = `
case "$*" in
  "read --ref dispatch://ACME-1/children")
    printf 'Key: ACME-1\\nChildren:\\n- ACME-2: widgets child (todo)\\n- ACME-3: gadgets child (todo)\\n'
    ;;
  "read --issue ACME-2")
    printf 'Key: ACME-2\\nStatus: todo\\nRepository: acme/gadgets (own)\\n'
    ;;
  "read --issue ACME-3")
    printf 'Key: ACME-3\\nStatus: todo\\nRepository: acme/gadgets (own)\\n'
    ;;
  *)
    echo "dispatch: unexpected call: $*" >&2
    exit 1
    ;;
esac`;
    const { status, stderr } = await run(["children", "ACME-1"], env, { dispatch: dispatchFake });
    expect(status).not.toBe(0);
    expect(stderr).toContain("CHECK children: FAIL");
  });
});

describe("negative no-repo: the state half against a fixed body", () => {
  test("an unrecorded issue with the refusal message on Dispatch: PASS", async () => {
    server = stateServer();
    const env = fullInputs(`http://127.0.0.1:${server.port}`);
    const dispatchFake = `
case "$*" in
  "read --issue ACME-9")
    printf 'Key: ACME-9\\nStatus: todo\\nRepository: none\\nEvents:\\n- #1 message.created · session s · 2026-01-01T00:00:00Z · Legion did not admit this issue: its repository resolves to none\\n'
    ;;
  *)
    echo "dispatch: unexpected call: $*" >&2
    exit 1
    ;;
esac`;
    const { status, stdout } = await run(["negative", "no-repo", "ACME-9"], env, {
      dispatch: dispatchFake,
    });
    expect(status).toBe(0);
    expect(stdout).toContain("CHECK negative-no-repo-ACME-9: PASS");
  });

  test("an issue the state does record: FAIL", async () => {
    server = stateServer();
    const env = fullInputs(`http://127.0.0.1:${server.port}`);
    const dispatchFake = `
case "$*" in
  "read --issue ACME-2")
    printf 'Key: ACME-2\\nStatus: todo\\nRepository: acme/widgets (own)\\nEvents:\\n- none\\n'
    ;;
  *)
    echo "dispatch: unexpected call: $*" >&2
    exit 1
    ;;
esac`;
    const { status, stderr } = await run(["negative", "no-repo", "ACME-2"], env, {
      dispatch: dispatchFake,
    });
    expect(status).not.toBe(0);
    expect(stderr).toContain("CHECK negative-no-repo-ACME-2: FAIL");
  });
});

describe("usage", () => {
  test("no observation named: refused with usage, exit 2", async () => {
    server = stateServer();
    const env = fullInputs(`http://127.0.0.1:${server.port}`);
    const proc = Bun.spawn(["bash", script], {
      env: { PATH: process.env.PATH ?? "", MULTIREPO_EVIDENCE_DIR: dir, ...env },
      stdout: "pipe",
      stderr: "pipe",
    });
    const stderr = await new Response(proc.stderr).text();
    const status = await proc.exited;
    expect(status).toBe(2);
    expect(stderr).toContain("usage:");
  });
});
