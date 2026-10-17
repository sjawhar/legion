import { afterAll, afterEach, describe, expect, test } from "bun:test";
import { accessSync, mkdirSync, mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";

// scripts/e2e/multi-repo-acceptance.sh is the Task 6.3 observer for the live Task 6.4 run. These
// tests pin its refusals and the jq paths its state-reading observations use against a fixed
// /legion/v1/state body, standing in for a live daemon with a Bun.serve stub, and a fake
// dispatch/gh/kubectl on an isolated PATH — the script never gets a real daemon, Dispatch, or
// GitHub in this suite, and never reads the host's own dispatch/gh/kubectl (finding 13: the host
// PATH must never leak in, or this suite is not hermetic on CI).
const script = join(import.meta.dir, "..", "multi-repo-acceptance.sh");
const dir = mkdtempSync(join(tmpdir(), "multi-repo-acceptance-test."));
afterAll(() => rmSync(dir, { recursive: true, force: true }));

let runs = 0;
let server: Server | undefined;

afterEach(() => {
  server?.stop(true);
  server = undefined;
});

// fakeBin writes an executable `name` on a fresh PATH-only directory running `body`.
function fakeBin(binDir: string, name: string, body: string): void {
  writeFileSync(join(binDir, name), `#!/usr/bin/env bash\n${body}\n`, { mode: 0o755 });
}

const defaultFake = (name: string) => `echo "${name}: unexpected call: $*" >&2; exit 1`;

// realDir is the first directory on the host's own PATH holding name, for the few real binaries
// (jq, curl, base64) this suite deliberately keeps instead of faking.
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

// isolatedPath is a PATH naming only bin (the test's fake dispatch/gh/kubectl directory) and the
// host's real jq, curl, and base64: no other host directory, so a stray dependency on anything
// else fails loudly instead of silently reading the host's copy.
function isolatedPath(bin: string): string {
  return [bin, realDir("jq"), realDir("curl"), realDir("base64"), "/usr/bin", "/bin"].join(":");
}

// newFakeDir makes a fresh bin directory under a fresh work directory, with dispatch, gh and
// kubectl faked (defaultFake unless fakes overrides one), and returns both the bin directory and
// a ready-made evidence directory beside it.
function newFakeDir(fakes: { dispatch?: string; gh?: string; kubectl?: string } = {}) {
  const work = join(dir, `run-${++runs}`);
  const bin = join(work, "bin");
  const evidence = join(work, "evidence");
  mkdirSync(bin, { recursive: true });
  mkdirSync(evidence, { recursive: true });
  fakeBin(bin, "dispatch", fakes.dispatch ?? defaultFake("dispatch"));
  fakeBin(bin, "gh", fakes.gh ?? defaultFake("gh"));
  fakeBin(bin, "kubectl", fakes.kubectl ?? defaultFake("kubectl"));
  return { bin, evidence };
}

// spawnScript runs the script once against path/evidence/env and collects its outcome. bash is
// resolved to an absolute path once, from this process's own PATH: a test that gives the child an
// empty PATH must still be able to exec bash itself (PATH resolution happens for the child's own
// argv[0] lookup too, not only for commands the script later runs).
const bashPath = `${realDir("bash")}/bash`;
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

// run is spawnScript over a fresh fake dir, for the common case of one call per test.
async function run(
  args: string[],
  env: Record<string, string> = {},
  fakes: { dispatch?: string; gh?: string; kubectl?: string } = {}
) {
  const { bin, evidence } = newFakeDir(fakes);
  const result = await spawnScript(args, isolatedPath(bin), evidence, env);
  return { ...result, evidence };
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

describe("usage: an absent or invalid observation is refused before any env, tool, or network check", () => {
  test("no observation named: exit 2, with no env set and no PATH at all", async () => {
    const work = join(dir, `run-${++runs}`);
    mkdirSync(work, { recursive: true });
    const { status, stderr } = await spawnScript([], "", work, {});
    expect(status).toBe(2);
    expect(stderr).toContain("usage:");
  });

  test("an unknown observation name is refused usage, exit 2, with no env set", async () => {
    const work = join(dir, `run-${++runs}`);
    mkdirSync(work, { recursive: true });
    const { status } = await spawnScript(["not-a-real-observation"], "", work, {});
    expect(status).toBe(2);
  });

  test("the wrong argument count for a known observation is refused usage, exit 2", async () => {
    const work = join(dir, `run-${++runs}`);
    mkdirSync(work, { recursive: true });
    const { status } = await spawnScript(["pin", "only-one-arg"], "", work, {});
    expect(status).toBe(2);
  });

  test("negative pr-in-x without before|after is refused usage, exit 2", async () => {
    const work = join(dir, `run-${++runs}`);
    mkdirSync(work, { recursive: true });
    const { status } = await spawnScript(["negative", "pr-in-x", "ACME-2"], "", work, {});
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

describe("repository shape and collisions", () => {
  test("a malformed repository value is refused before any network read", async () => {
    const env = fullInputs("http://127.0.0.1:1");
    env.LEGION_E2E_REPO_T = "not-a-repo; $(touch /tmp/pwned)";
    const { status, stderr } = await run(["done", "X-1"], env);
    expect(status).not.toBe(0);
    expect(stderr).toContain("LEGION_E2E_REPO_T");
    expect(stderr).toContain("not an owner/name repository");
  });

  test("T and O sharing an owner is refused", async () => {
    const env = fullInputs("http://127.0.0.1:1");
    env.LEGION_E2E_REPO_O = `${env.LEGION_E2E_REPO_T.split("/")[0]}/other-repo`;
    const { status, stderr } = await run(["done", "X-1"], env);
    expect(status).not.toBe(0);
    expect(stderr).toContain("LEGION_E2E_REPO_T and LEGION_E2E_REPO_O share an owner");
  });

  test("T and O sharing an owner in a different case is still refused (case-insensitive)", async () => {
    const env = fullInputs("http://127.0.0.1:1");
    env.LEGION_E2E_REPO_T = "Acme/widgets";
    env.LEGION_E2E_REPO_O = "ACME/other-repo";
    const { status, stderr } = await run(["done", "X-1"], env);
    expect(status).not.toBe(0);
    expect(stderr).toContain("share an owner");
  });

  test("two repository variables naming the same repository is refused", async () => {
    const env = fullInputs("http://127.0.0.1:1");
    env.LEGION_E2E_REPO_C = env.LEGION_E2E_REPO_T;
    const { status, stderr } = await run(["done", "X-1"], env);
    expect(status).not.toBe(0);
    expect(stderr).toContain("LEGION_E2E_REPO_T and LEGION_E2E_REPO_C both name");
  });

  test("two repository variables naming the same repository in different case is still refused", async () => {
    const env = fullInputs("http://127.0.0.1:1");
    env.LEGION_E2E_REPO_C = "ACME/Widgets"; // same as T's acme/widgets, folded
    const { status, stderr } = await run(["done", "X-1"], env);
    expect(status).not.toBe(0);
    expect(stderr).toContain("both name");
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

function stateServer(body: string = stateBody): Server {
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

describe("children: the state half against a fixed body", () => {
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

  test("one child resolves to T and one to C in both Dispatch and the daemon's state: PASS", async () => {
    server = stateServer();
    const env = fullInputs(`http://127.0.0.1:${server.port}`);
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
    const disagree = dispatchFake.replace(
      "Repository: acme/widgets (own)",
      "Repository: acme/gadgets (own)"
    );
    const { status, stderr } = await run(["children", "ACME-1"], env, { dispatch: disagree });
    expect(status).not.toBe(0);
    expect(stderr).toContain("CHECK children: FAIL");
  });

  // Finding 11: a nonzero dispatch exit with stdout that happens to match must never PASS — the
  // exit status is checked before the output is parsed, for every dispatch call this observation
  // makes.
  test("a dispatch read that exits nonzero but prints a matching Repository line is never a PASS", async () => {
    server = stateServer();
    const env = fullInputs(`http://127.0.0.1:${server.port}`);
    const failingButMatching = dispatchFake.replace(
      "\"read --issue ACME-2\")\n    printf 'Key: ACME-2\\nStatus: todo\\nRepository: acme/widgets (own)\\n'\n    ;;",
      "\"read --issue ACME-2\")\n    printf 'Key: ACME-2\\nStatus: todo\\nRepository: acme/widgets (own)\\n'\n    exit 1\n    ;;"
    );
    const { status, stderr } = await run(["children", "ACME-1"], env, {
      dispatch: failingButMatching,
    });
    expect(status).not.toBe(0);
    expect(stderr).toContain("exited");
  });
});

describe("done: exit-status discipline (finding 11)", () => {
  test("a dispatch read that exits nonzero with a matching Status line is never a PASS", async () => {
    server = stateServer();
    const env = fullInputs(`http://127.0.0.1:${server.port}`);
    const dispatchFake = `
case "$*" in
  "read --issue ACME-2")
    printf 'Key: ACME-2\\nStatus: done\\n'
    exit 1
    ;;
  *) echo "dispatch: unexpected call: $*" >&2; exit 1 ;;
esac`;
    const { status, stderr } = await run(["done", "ACME-2"], env, { dispatch: dispatchFake });
    expect(status).not.toBe(0);
    expect(stderr).toContain("exited");
  });

  test("every named issue reading Status: done is a PASS", async () => {
    server = stateServer();
    const env = fullInputs(`http://127.0.0.1:${server.port}`);
    const dispatchFake = `
case "$*" in
  "read --issue ACME-2") printf 'Key: ACME-2\\nStatus: done\\n' ;;
  "read --issue ACME-3") printf 'Key: ACME-3\\nStatus: done\\n' ;;
  *) echo "dispatch: unexpected call: $*" >&2; exit 1 ;;
esac`;
    const { status, stdout } = await run(["done", "ACME-2", "ACME-3"], env, {
      dispatch: dispatchFake,
    });
    expect(status).toBe(0);
    expect(stdout).toContain("CHECK done: PASS");
  });
});

describe("negative no-repo: the state half against a fixed body, and the exact refusal (finding 9)", () => {
  test("an unrecorded issue with Repository: none and the no-repository admission refusal: PASS", async () => {
    server = stateServer();
    const env = fullInputs(`http://127.0.0.1:${server.port}`);
    const dispatchFake = `
case "$*" in
  "read --issue ACME-9")
    printf 'Key: ACME-9\\nStatus: todo\\nRepository: none\\nEvents:\\n- #1 message.created · session s · 2026-01-01T00:00:00Z · Legion did not admit this issue: its repository resolves to none\\n'
    ;;
  *) echo "dispatch: unexpected call: $*" >&2; exit 1 ;;
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
  *) echo "dispatch: unexpected call: $*" >&2; exit 1 ;;
esac`;
    const { status, stderr } = await run(["negative", "no-repo", "ACME-2"], env, {
      dispatch: dispatchFake,
    });
    expect(status).not.toBe(0);
    expect(stderr).toContain("CHECK negative-no-repo-ACME-2: FAIL");
  });

  // Finding 9: a different admission failure (the Apps not installed) must never satisfy this
  // control, even when the issue is unrecorded and the repository line happens to name a real one.
  test("a different admission failure (an App not installed) on a recorded repository: FAIL", async () => {
    server = stateServer();
    const env = fullInputs(`http://127.0.0.1:${server.port}`);
    const dispatchFake = `
case "$*" in
  "read --issue ACME-9")
    printf 'Key: ACME-9\\nStatus: todo\\nRepository: acme/widgets (own)\\nEvents:\\n- #1 message.created · session s · 2026-01-01T00:00:00Z · Legion did not admit this issue: the Apps are not installed\\n'
    ;;
  *) echo "dispatch: unexpected call: $*" >&2; exit 1 ;;
esac`;
    const { status, stderr } = await run(["negative", "no-repo", "ACME-9"], env, {
      dispatch: dispatchFake,
    });
    expect(status).not.toBe(0);
    expect(stderr).toContain("CHECK negative-no-repo-ACME-9: FAIL");
  });

  test("Repository: none but no admission-refusal message at all: FAIL", async () => {
    server = stateServer();
    const env = fullInputs(`http://127.0.0.1:${server.port}`);
    const dispatchFake = `
case "$*" in
  "read --issue ACME-9")
    printf 'Key: ACME-9\\nStatus: todo\\nRepository: none\\nEvents:\\n- none\\n'
    ;;
  *) echo "dispatch: unexpected call: $*" >&2; exit 1 ;;
esac`;
    const { status, stderr } = await run(["negative", "no-repo", "ACME-9"], env, {
      dispatch: dispatchFake,
    });
    expect(status).not.toBe(0);
    expect(stderr).toContain("CHECK negative-no-repo-ACME-9: FAIL");
  });
});

describe("record-head and pin: empty/invalid values never pass (finding 10)", () => {
  test("record-head refuses a repository argument that is not T", async () => {
    server = stateServer();
    const env = fullInputs(`http://127.0.0.1:${server.port}`);
    const { status, stderr } = await run(["record-head", "acme/gadgets"], env, {});
    expect(status).not.toBe(0);
    expect(stderr).toContain("not LEGION_E2E_REPO_T");
  });

  test("an empty default_branch is never recorded as a head", async () => {
    server = stateServer();
    const env = fullInputs(`http://127.0.0.1:${server.port}`);
    const gh = `
case "$*" in
  "api repos/acme/widgets --jq .default_branch") printf '' ;;
  *) echo "gh: unexpected call: $*" >&2; exit 1 ;;
esac`;
    const { status, stderr } = await run(["record-head", "acme/widgets"], env, { gh });
    expect(status).not.toBe(0);
    expect(stderr).toContain("empty default_branch");
  });

  test("an empty commit sha from GitHub is never recorded as the head: FAIL, not PASS", async () => {
    server = stateServer();
    const env = fullInputs(`http://127.0.0.1:${server.port}`);
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
    server = stateServer();
    const env = fullInputs(`http://127.0.0.1:${server.port}`);
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
    server = stateServer();
    const env = fullInputs(`http://127.0.0.1:${server.port}`);
    const { status, stderr } = await run(["pin", "acme/gadgets", ""], env, {});
    expect(status).not.toBe(0);
    expect(stderr).toContain("not a full 40-character commit id");
  });

  test("pin's repository argument must be C", async () => {
    server = stateServer();
    const env = fullInputs(`http://127.0.0.1:${server.port}`);
    const { status, stderr } = await run(["pin", "acme/widgets", "a".repeat(40)], env, {});
    expect(status).not.toBe(0);
    expect(stderr).toContain("not LEGION_E2E_REPO_C");
  });
});

describe("second-owner: phase and distinct success/refusal evidence (finding 5)", () => {
  const stateWithPR = (phase: string) =>
    makeStateBody({
      issues: {
        "ACME-2": {
          key: "ACME-2",
          generation: 1,
          phase,
          status: "todo",
          repo: "zeta/tools",
          pullRequest: { repo: "zeta/tools", number: 1, head: "a".repeat(40) },
        },
      },
    });
  const goodDispatch = `
case "$*" in
  "read --issue ACME-2")
    printf 'Key: ACME-2\\nStatus: todo\\nEvents:\\n- #1 message.created · session s · 2026-01-01T00:00:00Z · gh api repos/acme/widgets succeeded; git push to acme/widgets refused\\n'
    ;;
  *) echo "dispatch: unexpected call: $*" >&2; exit 1 ;;
esac`;

  test("a pre-merge phase (awaiting_merge) with full message evidence is still a FAIL", async () => {
    server = stateServer(stateWithPR("awaiting_merge"));
    const env = fullInputs(`http://127.0.0.1:${server.port}`);
    const { status, stderr } = await run(["second-owner", "ACME-2"], env, {
      dispatch: goodDispatch,
    });
    expect(status).not.toBe(0);
    expect(stderr).toContain("CHECK second-owner: FAIL");
  });

  test("production_check with only a refusal phrase, no successful-read evidence, is a FAIL", async () => {
    server = stateServer(stateWithPR("production_check"));
    const env = fullInputs(`http://127.0.0.1:${server.port}`);
    const dispatchFake = `
case "$*" in
  "read --issue ACME-2")
    printf 'Key: ACME-2\\nStatus: todo\\nEvents:\\n- #1 message.created · session s · 2026-01-01T00:00:00Z · push to acme/widgets refused; read failed\\n'
    ;;
  *) echo "dispatch: unexpected call: $*" >&2; exit 1 ;;
esac`;
    const { status, stderr } = await run(["second-owner", "ACME-2"], env, {
      dispatch: dispatchFake,
    });
    expect(status).not.toBe(0);
    expect(stderr).toContain("CHECK second-owner: FAIL");
  });

  test("production_check with both distinct evidence lines is a PASS", async () => {
    server = stateServer(stateWithPR("production_check"));
    const env = fullInputs(`http://127.0.0.1:${server.port}`);
    const { status, stdout } = await run(["second-owner", "ACME-2"], env, {
      dispatch: goodDispatch,
    });
    expect(status).toBe(0);
    expect(stdout).toContain("CHECK second-owner: PASS");
  });

  test("a pull request in another repository than O is a FAIL even post-merge", async () => {
    server = stateServer(
      makeStateBody({
        issues: {
          "ACME-2": {
            key: "ACME-2",
            generation: 1,
            phase: "production_check",
            status: "todo",
            repo: "zeta/tools",
            pullRequest: { repo: "acme/widgets", number: 1, head: "a".repeat(40) },
          },
        },
      })
    );
    const env = fullInputs(`http://127.0.0.1:${server.port}`);
    const { status, stderr } = await run(["second-owner", "ACME-2"], env, {
      dispatch: goodDispatch,
    });
    expect(status).not.toBe(0);
    expect(stderr).toContain("CHECK second-owner: FAIL");
  });
});

describe("negative pr-in-x: the two-call protocol (finding 7)", () => {
  test("before on an unrecorded issue is refused: no recorded child, nothing to snapshot", async () => {
    server = stateServer();
    const env = fullInputs(`http://127.0.0.1:${server.port}`);
    const { status, stderr } = await run(["negative", "pr-in-x", "before", "ACME-404"], env, {});
    expect(status).not.toBe(0);
    expect(stderr).toContain("not the recorded child this control needs");
  });

  test("after without a prior before call is refused", async () => {
    server = stateServer();
    const env = fullInputs(`http://127.0.0.1:${server.port}`);
    const { status, stderr } = await run(["negative", "pr-in-x", "after", "ACME-2"], env, {});
    expect(status).not.toBe(0);
    expect(stderr).toContain("no before snapshot");
  });

  test("before, unchanged, after: PASS (byte-identical snapshots of a recorded child)", async () => {
    server = stateServer();
    const env = fullInputs(`http://127.0.0.1:${server.port}`);
    const { bin, evidence } = newFakeDir();
    const path = isolatedPath(bin);
    const before = await spawnScript(
      ["negative", "pr-in-x", "before", "ACME-2"],
      path,
      evidence,
      env
    );
    expect(before.status).toBe(0);
    expect(before.stdout).toContain("CHECK negative-pr-in-x-ACME-2: PASS");
    const after = await spawnScript(
      ["negative", "pr-in-x", "after", "ACME-2"],
      path,
      evidence,
      env
    );
    expect(after.status).toBe(0);
    expect(after.stdout).toContain("CHECK negative-pr-in-x-ACME-2: PASS");
  });
});

describe("negative repo-edit: the two-call protocol preserves the original (finding 8)", () => {
  test("before on an unrecorded issue is refused", async () => {
    server = stateServer();
    const env = fullInputs(`http://127.0.0.1:${server.port}`);
    const { status, stderr } = await run(["negative", "repo-edit", "before", "ACME-404"], env, {});
    expect(status).not.toBe(0);
    expect(stderr).toContain("not the recorded child this control needs");
  });

  test("after without a prior before call is refused", async () => {
    server = stateServer();
    const env = fullInputs(`http://127.0.0.1:${server.port}`);
    const { status, stderr } = await run(["negative", "repo-edit", "after", "ACME-2"], env, {});
    expect(status).not.toBe(0);
    expect(stderr).toContain("no before snapshot");
  });

  const xTargetedRefusal = `
case "$*" in
  "read --issue ACME-2")
    printf 'Key: ACME-2\\nStatus: todo\\nEvents:\\n- #1 message.created · session s · 2026-01-01T00:00:00Z · repo edit to other/outside is not applied\\n'
    ;;
  *) echo "dispatch: unexpected call: $*" >&2; exit 1 ;;
esac`;

  // Finding 8: the record could have changed T -> C between the two calls, and without the
  // original saved by `before`, that change would be invisible to `after`'s single read.
  test("a repository changed between before and after is a FAIL, not a PASS", async () => {
    const { bin, evidence } = newFakeDir();
    const path = isolatedPath(bin);

    server = stateServer(stateBody); // ACME-2 is acme/widgets
    const before = await spawnScript(
      ["negative", "repo-edit", "before", "ACME-2"],
      path,
      evidence,
      fullInputs(`http://127.0.0.1:${server.port}`)
    );
    expect(before.status).toBe(0);
    server.stop(true);

    // ACME-2 now reads acme/gadgets (C) instead of acme/widgets (T): a changed record.
    server = stateServer(
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
    fakeBin(bin, "dispatch", xTargetedRefusal);
    const after = await spawnScript(
      ["negative", "repo-edit", "after", "ACME-2"],
      path,
      evidence,
      fullInputs(`http://127.0.0.1:${server.port}`)
    );
    expect(after.status).not.toBe(0);
    expect(after.stderr).toContain("CHECK negative-repo-edit-ACME-2: FAIL");
  });

  test("an unchanged repository with the X-targeted refusal message is a PASS", async () => {
    const { bin, evidence } = newFakeDir();
    const path = isolatedPath(bin);
    server = stateServer(stateBody);
    const env = fullInputs(`http://127.0.0.1:${server.port}`);
    const before = await spawnScript(
      ["negative", "repo-edit", "before", "ACME-2"],
      path,
      evidence,
      env
    );
    expect(before.status).toBe(0);
    fakeBin(bin, "dispatch", xTargetedRefusal);
    const after = await spawnScript(
      ["negative", "repo-edit", "after", "ACME-2"],
      path,
      evidence,
      env
    );
    expect(after.status).toBe(0);
    expect(after.stdout).toContain("CHECK negative-repo-edit-ACME-2: PASS");
  });
});

describe("cache: pod/issue binding and the producer's exact success line (finding 6)", () => {
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

  test("an unrelated pod (legion.dev/issue mismatch) is refused, not read as the issue's", async () => {
    server = stateServer(stateWithCache);
    const env = fullInputs(`http://127.0.0.1:${server.port}`);
    const kubectl = `
case "$*" in
  "get pod other-pod -o jsonpath={.metadata.labels.legion\\.dev/issue}") printf 'ACME-99' ;;
  *) echo "kubectl: unexpected call: $*" >&2; exit 1 ;;
esac`;
    const { status, stderr } = await run(["cache", "ACME-2", "other-pod"], env, { kubectl });
    expect(status).not.toBe(0);
    expect(stderr).toContain("is not that issue's pod");
  });

  test("an error line mentioning cache and github, on the right pod, is a FAIL not a PASS", async () => {
    server = stateServer(stateWithCache);
    const env = fullInputs(`http://127.0.0.1:${server.port}`);
    const kubectl = `
case "$*" in
  "get pod the-pod -o jsonpath={.metadata.labels.legion\\.dev/issue}") printf 'ACME-2' ;;
  "logs the-pod -c workspace-fetch") printf 'ERROR: could not read bytes from the repository cache; fetching full history from GitHub\\n' ;;
  *) echo "kubectl: unexpected call: $*" >&2; exit 1 ;;
esac`;
    const { status, stderr } = await run(["cache", "ACME-2", "the-pod"], env, { kubectl });
    expect(status).not.toBe(0);
    expect(stderr).toContain("CHECK cache: FAIL");
  });

  test("the producer's exact success line, on the right pod, with cache.ready true: PASS", async () => {
    server = stateServer(stateWithCache);
    const env = fullInputs(`http://127.0.0.1:${server.port}`);
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
