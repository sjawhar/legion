import { afterAll, afterEach, describe, expect, test } from "bun:test";
import { accessSync, mkdirSync, mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";

// scripts/e2e/multi-repo-acceptance.sh is the Task 6.3 observer for the live Task 6.4 run. It
// reads only structured sources: the daemon's state JSON (a Bun.serve stub here), Dispatch's own
// JSON API (a second Bun.serve stub, never `dispatch read`'s prose), GitHub's API (a fake `gh`),
// and `kubectl` (a fake binary). Every fake and stub is isolated per test; the host's real
// dispatch/gh/kubectl are never on PATH, so this suite stays hermetic on CI.
const script = join(import.meta.dir, "..", "multi-repo-acceptance.sh");
const dir = mkdtempSync(join(tmpdir(), "multi-repo-acceptance-test."));
afterAll(() => rmSync(dir, { recursive: true, force: true }));

let runs = 0;
let daemonServer: Server | undefined;
let dispatchServer: Server | undefined;

afterEach(() => {
  daemonServer?.stop(true);
  dispatchServer?.stop(true);
  daemonServer = undefined;
  dispatchServer = undefined;
});

function fakeBin(binDir: string, name: string, body: string): void {
  writeFileSync(join(binDir, name), `#!/usr/bin/env bash\n${body}\n`, { mode: 0o755 });
}

const defaultFake = (name: string) => `echo "${name}: unexpected call: $*" >&2; exit 1`;

function realDir(name: string): string {
  for (const candidate of (process.env.PATH ?? "").split(":")) {
    try {
      accessSync(join(candidate, name));
      return candidate;
    } catch {
      /* keep looking */
    }
  }
  throw new Error(`${name} not found on PATH`);
}

function isolatedPath(bin: string): string {
  return [bin, realDir("jq"), realDir("curl"), realDir("base64"), "/usr/bin", "/bin"].join(":");
}

const bashPath = `${realDir("bash")}/bash`;

function newFakeDir(fakes: { gh?: string; kubectl?: string } = {}) {
  const work = join(dir, `run-${++runs}`);
  const bin = join(work, "bin");
  const evidence = join(work, "evidence");
  mkdirSync(bin, { recursive: true });
  mkdirSync(evidence, { recursive: true });
  fakeBin(bin, "gh", fakes.gh ?? defaultFake("gh"));
  fakeBin(bin, "kubectl", fakes.kubectl ?? defaultFake("kubectl"));
  return { bin, evidence };
}

async function spawnScript(
  args: string[],
  path: string,
  evidence: string,
  env: Record<string, string>
) {
  const proc = Bun.spawn([bashPath, script, ...args], {
    env: { PATH: path, MULTIREPO_EVIDENCE_DIR: evidence, ...env },
    stdout: "pipe",
    stderr: "pipe",
  });
  const [stdout, stderr] = await Promise.all([
    new Response(proc.stdout).text(),
    new Response(proc.stderr).text(),
  ]);
  const status = await proc.exited;
  return { status, stdout, stderr };
}

async function run(
  args: string[],
  env: Record<string, string> = {},
  fakes: { gh?: string; kubectl?: string } = {}
) {
  const { bin, evidence } = newFakeDir(fakes);
  const result = await spawnScript(args, isolatedPath(bin), evidence, env);
  return { ...result, evidence };
}

const tokenFile = join(dir, "dispatch-token");
writeFileSync(tokenFile, "dispatch-bearer\n");

const fullInputs = (daemonUrl: string, dispatchUrl: string): Record<string, string> => ({
  LEGION_E2E_REPO_T: "acme/widgets",
  LEGION_E2E_REPO_C: "acme/gadgets",
  LEGION_E2E_PIN_FILE: "toolchain-pin.txt",
  LEGION_E2E_REPO_X: "other/outside",
  LEGION_E2E_REPO_O: "zeta/tools",
  LEGION_E2E_DAEMON_URL: daemonUrl,
  DISPATCH_URL: dispatchUrl,
  DISPATCH_TOKEN_FILE: tokenFile,
});

// A fixed daemon /legion/v1/state body, overridable per test.
function makeStateBody(overrides: Record<string, unknown> = {}): string {
  return JSON.stringify({
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
    ...overrides,
  });
}
const stateBody = makeStateBody();

function daemonStub(body: string = stateBody): Server {
  return Bun.serve({
    port: 0,
    fetch(req) {
      const url = new URL(req.url);
      if (url.pathname === "/healthz") return Response.json({ status: "ok" });
      if (url.pathname === "/legion/v1/state") {
        return new Response(body, { headers: { "content-type": "application/json" } });
      }
      return new Response("not found", { status: 404 });
    },
  });
}

// mutableDaemonStub is daemonStub whose /legion/v1/state body can change after the server starts,
// without changing its URL: the baseline protocol binds a baseline to the daemon URL it captured
// against, so a test that must change the daemon's answer between `before` and `after` needs the
// same server, not a fresh one on a new port.
function mutableDaemonStub(initial: string): { server: Server; setBody: (body: string) => void } {
  let body = initial;
  const server = Bun.serve({
    port: 0,
    fetch(req) {
      const url = new URL(req.url);
      if (url.pathname === "/healthz") return Response.json({ status: "ok" });
      if (url.pathname === "/legion/v1/state") {
        return new Response(body, { headers: { "content-type": "application/json" } });
      }
      return new Response("not found", { status: 404 });
    },
  });
  return {
    server,
    setBody: (next: string) => {
      body = next;
    },
  };
}

// dispatchStub serves Dispatch's own GET /api/v1/issues/:key and /events, as issues/events map
// key -> response body (a JSON string). A key with no entry answers 404.
function dispatchStub(issues: Record<string, string>, events: Record<string, string> = {}): Server {
  return Bun.serve({
    port: 0,
    fetch(req) {
      const url = new URL(req.url);
      const eventsMatch = url.pathname.match(/^\/api\/v1\/issues\/([^/]+)\/events$/);
      if (eventsMatch) {
        const key = eventsMatch.at(1) ?? "";
        if (key in events)
          return new Response(events[key], { headers: { "content-type": "application/json" } });
        return new Response(JSON.stringify([]), {
          headers: { "content-type": "application/json" },
        });
      }
      const issueMatch = url.pathname.match(/^\/api\/v1\/issues\/([^/]+)$/);
      if (issueMatch) {
        const key = issueMatch.at(1) ?? "";
        if (key in issues)
          return new Response(issues[key], { headers: { "content-type": "application/json" } });
        return new Response(JSON.stringify({ error: "not found" }), { status: 404 });
      }
      return new Response("not found", { status: 404 });
    },
  });
}

function messageEvent(
  body: string,
  createdAt: string
): { type: string; created_at: string; payload: { body: string } } {
  return { type: "message.created", created_at: createdAt, payload: { body } };
}

describe("usage: an absent or invalid observation is refused before any env, tool, or network check", () => {
  test("no observation named: exit 2, with no env set and no PATH at all", async () => {
    const work = join(dir, `run-${++runs}`);
    mkdirSync(work, { recursive: true });
    const { status, stderr } = await spawnScript([], "", work, {});
    expect(status).toBe(2);
    expect(stderr).toContain("usage:");
  });

  test("negative pr-in-x without before|after is refused usage, exit 2", async () => {
    const work = join(dir, `run-${++runs}`);
    mkdirSync(work, { recursive: true });
    const { status } = await spawnScript(["negative", "pr-in-x", "ACME-2"], "", work, {});
    expect(status).toBe(2);
  });

  test("negative pr-in-x after with the wrong argument count is refused usage, exit 2", async () => {
    const work = join(dir, `run-${++runs}`);
    mkdirSync(work, { recursive: true });
    const { status } = await spawnScript(["negative", "pr-in-x", "after", "run-1"], "", work, {});
    expect(status).toBe(2);
  });
});

describe("required inputs", () => {
  test("every input unset: the refusal names the first missing one", async () => {
    const { status, stderr } = await run(["done", "X-1"], {});
    expect(status).not.toBe(0);
    expect(stderr).toContain("LEGION_E2E_REPO_T is unset");
  });

  test("each input missing in turn names itself", async () => {
    const all = fullInputs("http://127.0.0.1:1", "http://127.0.0.1:1");
    for (const name of Object.keys(all)) {
      const env = { ...all };
      delete (env as Record<string, string>)[name];
      const { status, stderr } = await run(["done", "X-1"], env);
      expect(status).not.toBe(0);
      expect(stderr).toContain(`${name} is unset`);
    }
  });
});

describe("repository shape and collisions", () => {
  test("a malformed repository value is refused before any network read", async () => {
    const env = fullInputs("http://127.0.0.1:1", "http://127.0.0.1:1");
    env.LEGION_E2E_REPO_T = "not-a-repo; $(touch /tmp/pwned)";
    const { status, stderr } = await run(["done", "X-1"], env);
    expect(status).not.toBe(0);
    expect(stderr).toContain("not an owner/name repository");
  });

  test("T and O sharing an owner in a different case is still refused (case-insensitive)", async () => {
    const env = fullInputs("http://127.0.0.1:1", "http://127.0.0.1:1");
    env.LEGION_E2E_REPO_T = "Acme/widgets";
    env.LEGION_E2E_REPO_O = "ACME/other-repo";
    const { status, stderr } = await run(["done", "X-1"], env);
    expect(status).not.toBe(0);
    expect(stderr).toContain("share an owner");
  });

  test("two repository variables naming the same repository in different case is still refused", async () => {
    const env = fullInputs("http://127.0.0.1:1", "http://127.0.0.1:1");
    env.LEGION_E2E_REPO_C = "ACME/Widgets";
    const { status, stderr } = await run(["done", "X-1"], env);
    expect(status).not.toBe(0);
    expect(stderr).toContain("both name");
  });
});

describe("the daemon health refusal", () => {
  test("a stub whose /healthz answers 404 is refused, naming the status", async () => {
    daemonServer = Bun.serve({ port: 0, fetch: () => new Response("not found", { status: 404 }) });
    const env = fullInputs(`http://127.0.0.1:${daemonServer.port}`, "http://127.0.0.1:1");
    const { status, stderr } = await run(["done", "X-1"], env);
    expect(status).not.toBe(0);
    expect(stderr).toContain(
      `LEGION_E2E_DAEMON_URL http://127.0.0.1:${daemonServer.port} is not a Legion daemon`
    );
  });
});

describe("children: structured Dispatch + state agreement (no CLI prose)", () => {
  test("one child resolves to T and one to C in both Dispatch and the daemon's state: PASS", async () => {
    daemonServer = daemonStub();
    dispatchServer = dispatchStub({
      "ACME-1": JSON.stringify({ key: "ACME-1", children: [{ key: "ACME-2" }, { key: "ACME-3" }] }),
      "ACME-2": JSON.stringify({
        key: "ACME-2",
        repo: "acme/widgets",
        resolved_repo: "acme/widgets",
      }),
      "ACME-3": JSON.stringify({
        key: "ACME-3",
        repo: "acme/gadgets",
        resolved_repo: "acme/gadgets",
      }),
    });
    const env = fullInputs(
      `http://127.0.0.1:${daemonServer.port}`,
      `http://127.0.0.1:${dispatchServer.port}`
    );
    const { status, stdout, evidence } = await run(["children", "ACME-1"], env);
    expect(status).toBe(0);
    expect(stdout).toContain("CHECK children: PASS");
    expect(stdout).toContain("ACME-2: dispatch=acme/widgets state=acme/widgets");
    expect(Bun.file(join(evidence, "children.txt")).size).toBeGreaterThan(0);
  });

  test("a child Dispatch and the daemon's state disagree on: FAIL", async () => {
    daemonServer = daemonStub();
    dispatchServer = dispatchStub({
      "ACME-1": JSON.stringify({ key: "ACME-1", children: [{ key: "ACME-2" }, { key: "ACME-3" }] }),
      "ACME-2": JSON.stringify({
        key: "ACME-2",
        repo: "acme/gadgets",
        resolved_repo: "acme/gadgets",
      }),
      "ACME-3": JSON.stringify({
        key: "ACME-3",
        repo: "acme/gadgets",
        resolved_repo: "acme/gadgets",
      }),
    });
    const env = fullInputs(
      `http://127.0.0.1:${daemonServer.port}`,
      `http://127.0.0.1:${dispatchServer.port}`
    );
    const { status, stderr } = await run(["children", "ACME-1"], env);
    expect(status).not.toBe(0);
    expect(stderr).toContain("CHECK children: FAIL");
  });

  test("Dispatch answering a non-2xx for a child fails the whole observation", async () => {
    daemonServer = daemonStub();
    dispatchServer = dispatchStub({
      "ACME-1": JSON.stringify({ key: "ACME-1", children: [{ key: "ACME-2" }] }),
      // ACME-2 deliberately absent: dispatchStub answers 404 for it.
    });
    const env = fullInputs(
      `http://127.0.0.1:${daemonServer.port}`,
      `http://127.0.0.1:${dispatchServer.port}`
    );
    const { status, stderr } = await run(["children", "ACME-1"], env);
    expect(status).not.toBe(0);
    expect(stderr).toContain("failed");
  });
});

describe("done: Dispatch's structured .status field", () => {
  test("every named issue reading status done is a PASS", async () => {
    daemonServer = daemonStub();
    dispatchServer = dispatchStub({
      "ACME-2": JSON.stringify({ key: "ACME-2", status: "done" }),
      "ACME-3": JSON.stringify({ key: "ACME-3", status: "done" }),
    });
    const env = fullInputs(
      `http://127.0.0.1:${daemonServer.port}`,
      `http://127.0.0.1:${dispatchServer.port}`
    );
    const { status, stdout } = await run(["done", "ACME-2", "ACME-3"], env);
    expect(status).toBe(0);
    expect(stdout).toContain("CHECK done: PASS");
  });

  test("one issue still todo is a FAIL", async () => {
    daemonServer = daemonStub();
    dispatchServer = dispatchStub({
      "ACME-2": JSON.stringify({ key: "ACME-2", status: "todo" }),
    });
    const env = fullInputs(
      `http://127.0.0.1:${daemonServer.port}`,
      `http://127.0.0.1:${dispatchServer.port}`
    );
    const { status, stderr } = await run(["done", "ACME-2"], env);
    expect(status).not.toBe(0);
    expect(stderr).toContain("CHECK done: FAIL");
  });
});

describe("record-head and pin: empty/invalid values never pass", () => {
  test("record-head refuses a repository argument that is not T", async () => {
    daemonServer = daemonStub();
    dispatchServer = dispatchStub({});
    const env = fullInputs(
      `http://127.0.0.1:${daemonServer.port}`,
      `http://127.0.0.1:${dispatchServer.port}`
    );
    const { status, stderr } = await run(["record-head", "acme/gadgets"], env, {});
    expect(status).not.toBe(0);
    expect(stderr).toContain("not LEGION_E2E_REPO_T");
  });

  test("an empty commit sha from GitHub is never recorded as the head: FAIL, not PASS", async () => {
    daemonServer = daemonStub();
    dispatchServer = dispatchStub({});
    const env = fullInputs(
      `http://127.0.0.1:${daemonServer.port}`,
      `http://127.0.0.1:${dispatchServer.port}`
    );
    const gh = `
case "$*" in
  "api repos/acme/widgets --jq .default_branch") printf 'main' ;;
  "api repos/acme/widgets/branches/main --jq .commit.sha") printf '' ;;
  *) echo "gh: unexpected call: $*" >&2; exit 1 ;;
esac`;
    const { status, stderr } = await run(["record-head", "acme/widgets"], env, { gh });
    expect(status).not.toBe(0);
    expect(stderr).toContain("CHECK record-head: FAIL");
  });

  test("a well-formed record-head is a PASS", async () => {
    daemonServer = daemonStub();
    dispatchServer = dispatchStub({});
    const env = fullInputs(
      `http://127.0.0.1:${daemonServer.port}`,
      `http://127.0.0.1:${dispatchServer.port}`
    );
    const sha = "a".repeat(40);
    const gh = `
case "$*" in
  "api repos/acme/widgets --jq .default_branch") printf 'main' ;;
  "api repos/acme/widgets/branches/main --jq .commit.sha") printf '${sha}' ;;
  *) echo "gh: unexpected call: $*" >&2; exit 1 ;;
esac`;
    const { status, stdout } = await run(["record-head", "acme/widgets"], env, { gh });
    expect(status).toBe(0);
    expect(stdout).toContain(sha);
    expect(stdout).toContain("CHECK record-head: PASS");
  });

  test("pin refuses an empty merge-sha argument", async () => {
    daemonServer = daemonStub();
    dispatchServer = dispatchStub({});
    const env = fullInputs(
      `http://127.0.0.1:${daemonServer.port}`,
      `http://127.0.0.1:${dispatchServer.port}`
    );
    const { status, stderr } = await run(["pin", "acme/gadgets", ""], env, {});
    expect(status).not.toBe(0);
    expect(stderr).toContain("not a full 40-character commit id");
  });
});

describe("second-owner: GitHub merge proof and in-pod credential checks (no prose matching)", () => {
  const stateWithPR = (phase: string, extra: Record<string, unknown> = {}) =>
    makeStateBody({
      issues: {
        "ACME-2": {
          key: "ACME-2",
          generation: 1,
          phase,
          status: "todo",
          repo: "zeta/tools",
          pullRequest: { repo: "zeta/tools", number: 7 },
          ...extra,
        },
      },
    });
  const goodKubectl = `
case "$*" in
  "exec the-pod -c implementer -- gh api repos/acme/widgets --jq .full_name") printf 'acme/widgets' ;;
  "get pod the-pod -o jsonpath={.metadata.labels.legion\\.dev/issue}") printf 'ACME-2' ;;
  *)
    if [[ "$*" == "exec the-pod -c implementer -- git push --dry-run"* ]]; then exit 1; fi
    echo "kubectl: unexpected call: $*" >&2
    exit 1
    ;;
esac`;
  const goodGh = `
case "$*" in
  "api repos/zeta/tools/pulls/7 --jq .merged") printf 'true' ;;
  *) echo "gh: unexpected call: $*" >&2; exit 1 ;;
esac`;

  test("a pre-merge phase (awaiting_merge) is a FAIL even with a merged PR and valid credentials", async () => {
    daemonServer = daemonStub(stateWithPR("awaiting_merge"));
    dispatchServer = dispatchStub({});
    const env = fullInputs(
      `http://127.0.0.1:${daemonServer.port}`,
      `http://127.0.0.1:${dispatchServer.port}`
    );
    const { status, stderr } = await run(["second-owner", "ACME-2", "the-pod"], env, {
      gh: goodGh,
      kubectl: goodKubectl,
    });
    expect(status).not.toBe(0);
    expect(stderr).toContain("CHECK second-owner: FAIL");
  });

  test("production_check but GitHub says the PR is not merged: FAIL", async () => {
    daemonServer = daemonStub(stateWithPR("production_check"));
    dispatchServer = dispatchStub({});
    const env = fullInputs(
      `http://127.0.0.1:${daemonServer.port}`,
      `http://127.0.0.1:${dispatchServer.port}`
    );
    const notMergedGh = `
case "$*" in
  "api repos/zeta/tools/pulls/7 --jq .merged") printf 'false' ;;
  *) echo "gh: unexpected call: $*" >&2; exit 1 ;;
esac`;
    const { status, stderr } = await run(["second-owner", "ACME-2", "the-pod"], env, {
      gh: notMergedGh,
      kubectl: goodKubectl,
    });
    expect(status).not.toBe(0);
    expect(stderr).toContain("CHECK second-owner: FAIL");
  });

  test("production_check, merged, but the in-pod dry-run push to T succeeds (no refusal): FAIL", async () => {
    daemonServer = daemonStub(stateWithPR("production_check"));
    dispatchServer = dispatchStub({});
    const env = fullInputs(
      `http://127.0.0.1:${daemonServer.port}`,
      `http://127.0.0.1:${dispatchServer.port}`
    );
    const pushSucceedsKubectl = `
case "$*" in
  "exec the-pod -c implementer -- gh api repos/acme/widgets --jq .full_name") printf 'acme/widgets' ;;
  "get pod the-pod -o jsonpath={.metadata.labels.legion\\.dev/issue}") printf 'ACME-2' ;;
  *)
    if [[ "$*" == "exec the-pod -c implementer -- git push --dry-run"* ]]; then exit 0; fi
    echo "kubectl: unexpected call: $*" >&2
    exit 1
    ;;
esac`;
    const { status, stderr } = await run(["second-owner", "ACME-2", "the-pod"], env, {
      gh: goodGh,
      kubectl: pushSucceedsKubectl,
    });
    expect(status).not.toBe(0);
    expect(stderr).toContain("CHECK second-owner: FAIL");
  });

  test("production_check, merged, read succeeds, push refused, pod matches the issue: PASS", async () => {
    daemonServer = daemonStub(stateWithPR("production_check"));
    dispatchServer = dispatchStub({});
    const env = fullInputs(
      `http://127.0.0.1:${daemonServer.port}`,
      `http://127.0.0.1:${dispatchServer.port}`
    );
    const { status, stdout } = await run(["second-owner", "ACME-2", "the-pod"], env, {
      gh: goodGh,
      kubectl: goodKubectl,
    });
    expect(status).toBe(0);
    expect(stdout).toContain("CHECK second-owner: PASS");
  });

  test("an unrelated pod (legion.dev/issue mismatch) is refused before any other check", async () => {
    daemonServer = daemonStub(stateWithPR("production_check"));
    dispatchServer = dispatchStub({});
    const env = fullInputs(
      `http://127.0.0.1:${daemonServer.port}`,
      `http://127.0.0.1:${dispatchServer.port}`
    );
    const otherPodKubectl = `
case "$*" in
  "get pod the-pod -o jsonpath={.metadata.labels.legion\\.dev/issue}") printf 'ACME-99' ;;
  *) echo "kubectl: unexpected call: $*" >&2; exit 1 ;;
esac`;
    const { status, stderr } = await run(["second-owner", "ACME-2", "the-pod"], env, {
      gh: goodGh,
      kubectl: otherPodKubectl,
    });
    expect(status).not.toBe(0);
    expect(stderr).toContain("is not that issue's pod");
  });
});

describe("cache: pod binding and the producer's exact success line", () => {
  const stateWithCache = makeStateBody({
    issues: {
      "ACME-2": {
        key: "ACME-2",
        generation: 1,
        phase: "implementing",
        status: "todo",
        repo: "acme/widgets",
      },
    },
    repositories: {
      owners: [],
      mapped: [
        {
          repo: "acme/widgets",
          default: true,
          cache: { repo: "acme/widgets", sandbox: "s", state: "ready", ready: true },
        },
      ],
      refused: [],
      caches: [],
    },
  });

  test("an unrelated pod is refused, not read as the issue's", async () => {
    daemonServer = daemonStub(stateWithCache);
    dispatchServer = dispatchStub({});
    const env = fullInputs(
      `http://127.0.0.1:${daemonServer.port}`,
      `http://127.0.0.1:${dispatchServer.port}`
    );
    const kubectl = `
case "$*" in
  "get pod other-pod -o jsonpath={.metadata.labels.legion\\.dev/issue}") printf 'ACME-99' ;;
  *) echo "kubectl: unexpected call: $*" >&2; exit 1 ;;
esac`;
    const { status, stderr } = await run(["cache", "ACME-2", "other-pod"], env, { kubectl });
    expect(status).not.toBe(0);
    expect(stderr).toContain("is not that issue's pod");
  });

  test("the producer's exact success line, on the right pod, with cache.ready true: PASS", async () => {
    daemonServer = daemonStub(stateWithCache);
    dispatchServer = dispatchStub({});
    const env = fullInputs(
      `http://127.0.0.1:${daemonServer.port}`,
      `http://127.0.0.1:${dispatchServer.port}`
    );
    const kubectl = `
case "$*" in
  "get pod the-pod -o jsonpath={.metadata.labels.legion\\.dev/issue}") printf 'ACME-2' ;;
  "logs the-pod -c workspace-fetch") printf 'workspace-init fetch: 6,612,345,678 bytes from the repository cache http://cache:8418 in 3s, its history verified in 1s; 128 bytes from GitHub for the delta (2 refs)\\n' ;;
  *) echo "kubectl: unexpected call: $*" >&2; exit 1 ;;
esac`;
    const { status, stdout } = await run(["cache", "ACME-2", "the-pod"], env, { kubectl });
    expect(status).toBe(0);
    expect(stdout).toContain("CHECK cache: PASS");
  });
});

describe("negative no-repo: Dispatch's structured absence plus the daemon's exact refusal", () => {
  test("an unrecorded issue with the exact no-repository admission refusal: PASS", async () => {
    daemonServer = daemonStub();
    const body =
      "Legion did not admit this issue: its repository resolves to none. Set its repository (the issue header's Repository picker, or dispatch issue-update --repo owner/name) and move it to todo again.";
    dispatchServer = dispatchStub(
      {},
      { "ACME-9": JSON.stringify([messageEvent(body, "2026-01-01T00:00:00Z")]) }
    );
    const env = fullInputs(
      `http://127.0.0.1:${daemonServer.port}`,
      `http://127.0.0.1:${dispatchServer.port}`
    );
    const { status, stdout } = await run(["negative", "no-repo", "ACME-9"], env);
    expect(status).toBe(0);
    expect(stdout).toContain("CHECK negative-no-repo-ACME-9: PASS");
  });

  test("an issue the state does record: FAIL", async () => {
    daemonServer = daemonStub();
    dispatchServer = dispatchStub({}, {});
    const env = fullInputs(
      `http://127.0.0.1:${daemonServer.port}`,
      `http://127.0.0.1:${dispatchServer.port}`
    );
    const { status, stderr } = await run(["negative", "no-repo", "ACME-2"], env);
    expect(status).not.toBe(0);
    expect(stderr).toContain("CHECK negative-no-repo-ACME-2: FAIL");
  });

  // A different admission failure (an App not installed) must never satisfy this control, even
  // though the issue is unrecorded: the exact resolves-to-none sentence is required, word for word.
  test("a different admission failure (an App not installed) is a FAIL", async () => {
    daemonServer = daemonStub();
    const body =
      "Legion did not admit this issue: the review App is not installed on acme/widgets. Install it on acme/widgets, or set another repository (the issue header's Repository picker, or dispatch issue-update --repo owner/name), and move the issue to todo again.";
    dispatchServer = dispatchStub(
      {},
      { "ACME-9": JSON.stringify([messageEvent(body, "2026-01-01T00:00:00Z")]) }
    );
    const env = fullInputs(
      `http://127.0.0.1:${daemonServer.port}`,
      `http://127.0.0.1:${dispatchServer.port}`
    );
    const { status, stderr } = await run(["negative", "no-repo", "ACME-9"], env);
    expect(status).not.toBe(0);
    expect(stderr).toContain("CHECK negative-no-repo-ACME-9: FAIL");
  });

  // Finding (round 2, #4): a failed read whose text happens to contain "token" or any other
  // loose substring must never satisfy this exact-sentence check.
  test("an unrelated message containing fragments of the refusal wording is a FAIL", async () => {
    daemonServer = daemonStub();
    const body =
      "the token expired; this issue did not admit of a simple fix, and its repository picker is broken";
    dispatchServer = dispatchStub(
      {},
      { "ACME-9": JSON.stringify([messageEvent(body, "2026-01-01T00:00:00Z")]) }
    );
    const env = fullInputs(
      `http://127.0.0.1:${daemonServer.port}`,
      `http://127.0.0.1:${dispatchServer.port}`
    );
    const { status, stderr } = await run(["negative", "no-repo", "ACME-9"], env);
    expect(status).not.toBe(0);
    expect(stderr).toContain("CHECK negative-no-repo-ACME-9: FAIL");
  });
});

describe("negative pr-in-x: the two-call baseline protocol", () => {
  test("before prints BASELINE, never PASS, for a recorded child", async () => {
    daemonServer = daemonStub();
    dispatchServer = dispatchStub({});
    const env = fullInputs(
      `http://127.0.0.1:${daemonServer.port}`,
      `http://127.0.0.1:${dispatchServer.port}`
    );
    const { status, stdout } = await run(["negative", "pr-in-x", "before", "ACME-2"], env);
    expect(status).toBe(0);
    expect(stdout).toContain("BASELINE pr-in-x-ACME-2: captured");
    expect(stdout).not.toContain("PASS");
    expect(stdout).toMatch(/RUN_ID: \S+/);
  });

  test("before on an unrecorded issue is refused: no recorded child, nothing to baseline", async () => {
    daemonServer = daemonStub();
    dispatchServer = dispatchStub({});
    const env = fullInputs(
      `http://127.0.0.1:${daemonServer.port}`,
      `http://127.0.0.1:${dispatchServer.port}`
    );
    const { status, stderr } = await run(["negative", "pr-in-x", "before", "ACME-404"], env);
    expect(status).not.toBe(0);
    expect(stderr).toContain("not the recorded child this control needs");
  });

  test("after with an unknown run id is refused", async () => {
    daemonServer = daemonStub();
    dispatchServer = dispatchStub({});
    const env = fullInputs(
      `http://127.0.0.1:${daemonServer.port}`,
      `http://127.0.0.1:${dispatchServer.port}`
    );
    const { status, stderr } = await run(
      ["negative", "pr-in-x", "after", "nonexistent-run", "ACME-2"],
      env
    );
    expect(status).not.toBe(0);
    expect(stderr).toContain("no baseline for run");
  });

  test("after with no closed pull request in X on legion/<child> is a FAIL (no operation happened)", async () => {
    daemonServer = daemonStub();
    dispatchServer = dispatchStub({});
    const env = fullInputs(
      `http://127.0.0.1:${daemonServer.port}`,
      `http://127.0.0.1:${dispatchServer.port}`
    );
    const { bin, evidence } = newFakeDir({
      gh: `case "$*" in "api repos/other/outside/pulls?state=all&per_page=100 --jq .") printf '[]' ;; *) echo "gh: unexpected call: $*" >&2; exit 1 ;; esac`,
    });
    const path = isolatedPath(bin);
    const before = await spawnScript(
      ["negative", "pr-in-x", "before", "ACME-2"],
      path,
      evidence,
      env
    );
    expect(before.status).toBe(0);
    const runId = before.stdout.match(/RUN_ID: (\S+)/)?.[1] ?? "";
    expect(runId).not.toBe("");
    const after = await spawnScript(
      ["negative", "pr-in-x", "after", runId, "ACME-2"],
      path,
      evidence,
      env
    );
    expect(after.status).not.toBe(0);
    expect(after.stderr).toContain("CHECK negative-pr-in-x-ACME-2: FAIL");
  });

  test("after with a closed PR on legion/<child> in X, opened and closed after the baseline: PASS", async () => {
    daemonServer = daemonStub();
    dispatchServer = dispatchStub({});
    const env = fullInputs(
      `http://127.0.0.1:${daemonServer.port}`,
      `http://127.0.0.1:${dispatchServer.port}`
    );
    const { bin, evidence } = newFakeDir();
    const path = isolatedPath(bin);
    const before = await spawnScript(
      ["negative", "pr-in-x", "before", "ACME-2"],
      path,
      evidence,
      env
    );
    expect(before.status).toBe(0);
    const runId = before.stdout.match(/RUN_ID: (\S+)/)?.[1] ?? "";
    expect(runId).not.toBe("");
    const future = new Date(Date.now() + 60_000).toISOString();
    const matchingGh = `case "$*" in "api repos/other/outside/pulls?state=all&per_page=100 --jq .") printf '[{"head":{"ref":"legion/ACME-2"},"state":"closed","created_at":"${future}","closed_at":"${future}"}]' ;; *) echo "gh: unexpected call: $*" >&2; exit 1 ;; esac`;
    fakeBin(bin, "gh", matchingGh);
    const after = await spawnScript(
      ["negative", "pr-in-x", "after", runId, "ACME-2"],
      path,
      evidence,
      env
    );
    expect(after.status).toBe(0);
    expect(after.stdout).toContain("CHECK negative-pr-in-x-ACME-2: PASS");
  });

  test("two overlapping before calls for the same issue never collide: each gets its own run id and baseline", async () => {
    daemonServer = daemonStub();
    dispatchServer = dispatchStub({});
    const env = fullInputs(
      `http://127.0.0.1:${daemonServer.port}`,
      `http://127.0.0.1:${dispatchServer.port}`
    );
    const { bin, evidence } = newFakeDir();
    const path = isolatedPath(bin);
    const [first, second] = await Promise.all([
      spawnScript(["negative", "pr-in-x", "before", "ACME-2"], path, evidence, env),
      spawnScript(["negative", "pr-in-x", "before", "ACME-2"], path, evidence, env),
    ]);
    expect(first.status).toBe(0);
    expect(second.status).toBe(0);
    const firstRun = first.stdout.match(/RUN_ID: (\S+)/)?.[1];
    const secondRun = second.stdout.match(/RUN_ID: (\S+)/)?.[1];
    expect(firstRun).toBeDefined();
    expect(secondRun).toBeDefined();
    expect(firstRun).not.toBe(secondRun);
  });
});

describe("negative repo-edit: the two-call baseline protocol", () => {
  test("before prints BASELINE, never PASS", async () => {
    daemonServer = daemonStub();
    dispatchServer = dispatchStub({});
    const env = fullInputs(
      `http://127.0.0.1:${daemonServer.port}`,
      `http://127.0.0.1:${dispatchServer.port}`
    );
    const { status, stdout } = await run(["negative", "repo-edit", "before", "ACME-2"], env);
    expect(status).toBe(0);
    expect(stdout).toContain("BASELINE repo-edit-ACME-2: captured");
    expect(stdout).not.toContain("PASS");
  });

  // Finding (round 2, #8): a repository that actually changed between before and after — with no
  // exact kept-repository message — is a FAIL, not a PASS on stale or partial evidence.
  test("the daemon's record changed between before and after: FAIL", async () => {
    const { server, setBody } = mutableDaemonStub(stateBody); // ACME-2 starts as acme/widgets
    daemonServer = server;
    dispatchServer = dispatchStub({
      "ACME-2": JSON.stringify({ key: "ACME-2", repo: "other/outside" }),
    });
    const env = fullInputs(
      `http://127.0.0.1:${server.port}`,
      `http://127.0.0.1:${dispatchServer.port}`
    );
    const { bin, evidence } = newFakeDir();
    const path = isolatedPath(bin);
    const before = await spawnScript(
      ["negative", "repo-edit", "before", "ACME-2"],
      path,
      evidence,
      env
    );
    expect(before.status).toBe(0);
    const runId = before.stdout.match(/RUN_ID: (\S+)/)?.[1];
    expect(runId).toBeDefined();
    // The daemon's own record now reads acme/gadgets instead of acme/widgets: a changed record,
    // same daemon URL as the baseline (so this is not merely the foreign-baseline refusal).
    setBody(
      makeStateBody({
        issues: {
          "ACME-2": {
            key: "ACME-2",
            generation: 1,
            phase: "implementing",
            status: "todo",
            repo: "acme/gadgets",
          },
        },
      })
    );
    const after = await spawnScript(
      ["negative", "repo-edit", "after", runId as string, "ACME-2"],
      path,
      evidence,
      env
    );
    expect(after.status).not.toBe(0);
    expect(after.stderr).toContain("CHECK negative-repo-edit-ACME-2: FAIL");
  });

  test("unchanged daemon record, Dispatch shows X, a postdating edit event, and the exact kept message: PASS", async () => {
    daemonServer = daemonStub(stateBody); // ACME-2 stays acme/widgets throughout
    dispatchServer = dispatchStub({});
    const env = fullInputs(
      `http://127.0.0.1:${daemonServer.port}`,
      `http://127.0.0.1:${dispatchServer.port}`
    );
    const { bin, evidence } = newFakeDir();
    const path = isolatedPath(bin);
    const before = await spawnScript(
      ["negative", "repo-edit", "before", "ACME-2"],
      path,
      evidence,
      env
    );
    expect(before.status).toBe(0);
    const runId = before.stdout.match(/RUN_ID: (\S+)/)?.[1] ?? "";
    expect(runId).not.toBe("");
    const future = new Date(Date.now() + 60_000).toISOString();
    dispatchServer.stop(true);
    dispatchServer = dispatchStub(
      { "ACME-2": JSON.stringify({ key: "ACME-2", repo: "other/outside" }) },
      {
        "ACME-2": JSON.stringify([
          { type: "issue.updated", created_at: future, payload: {} },
          messageEvent(
            "Legion keeps acme/widgets as this issue's repository; the change to other/outside in Dispatch is not applied — an issue's repository is fixed when Legion records it. Create a new issue for other/outside instead.",
            future
          ),
        ]),
      }
    );
    const env2 = { ...env, DISPATCH_URL: `http://127.0.0.1:${dispatchServer.port}` };
    const after = await spawnScript(
      ["negative", "repo-edit", "after", runId, "ACME-2"],
      path,
      evidence,
      env2
    );
    expect(after.status).toBe(0);
    expect(after.stdout).toContain("CHECK negative-repo-edit-ACME-2: PASS");
  });
});
