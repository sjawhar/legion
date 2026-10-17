import { afterAll, afterEach, describe, expect, test } from "bun:test";
import { accessSync, mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";

// scripts/e2e/multi-repo-acceptance.sh is the Task 6.3 observer for the live Task 6.4 run. It
// reads only structured sources: the daemon's state JSON (a Bun.serve stub here), Dispatch's own
// JSON API (a second Bun.serve stub, never `dispatch read`'s prose), GitHub's API (a fake `gh`),
// and `kubectl` (a fake binary that really execs what it is given, so the observer's own in-pod
// mechanism — finding the role's process by its environment and re-executing under it — runs for
// real against a fixture process this suite spawns, never a canned answer). Every fake, stub and
// fixture is isolated per test; the host's real dispatch/gh/kubectl are never on PATH, so this
// suite stays hermetic on CI.
const script = join(import.meta.dir, "..", "multi-repo-acceptance.sh");
const dir = mkdtempSync(join(tmpdir(), "multi-repo-acceptance-test."));
afterAll(() => rmSync(dir, { recursive: true, force: true }));

let runs = 0;
let daemonServer: Server | undefined;
let dispatchServer: Server | undefined;
let roleFixtures: Array<ReturnType<typeof Bun.spawn>> = [];

afterEach(() => {
  daemonServer?.stop(true);
  dispatchServer?.stop(true);
  daemonServer = undefined;
  dispatchServer = undefined;
  for (const proc of roleFixtures) {
    try {
      proc.kill("SIGKILL");
    } catch {
      /* already gone */
    }
  }
  roleFixtures = [];
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

// realKubectl forwards `exec <pod> -c <container> -- <rest...>` to a real local exec of <rest>,
// standing in for a pod exec: this is what lets the observer's own as_role /proc scan and
// `env -i` re-exec run unmodified against a role fixture process (spawnRoleFixture), rather than
// a canned stand-in for the mechanism under test.
const realKubectl = `
if [ "$1" = exec ]; then
  shift; shift; shift; shift; shift
  exec "$@"
fi
echo "kubectl: unexpected call: $*" >&2
exit 1`;

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

// spawnRoleFixture starts a real background process whose exact environment is `env` (replaced,
// never inherited) so its /proc/<pid>/environ is exactly what the observer's as_role mechanism
// will find and re-exec under — the same contract a role's own process has in a pod, none of
// which a bare `kubectl exec` carries.
function spawnRoleFixture(env: Record<string, string>) {
  const proc = Bun.spawn(["sleep", "300"], { env, stdout: "ignore", stderr: "ignore" });
  roleFixtures.push(proc);
  return proc;
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

// daemonActorEvent is one event with the daemon's own exact actor identity
// (legion-daemon:<PROJECT>, internal/dispatch/client.go:287-289), a seq, and a message.created
// body.
function daemonActorEvent(
  project: string,
  seq: number,
  body: string,
  createdAt = "2026-01-01T00:00:00Z"
): {
  type: string;
  seq: number;
  actor: { kind: string; id: string };
  created_at: string;
  payload: { body: string };
} {
  return {
    type: "message.created",
    seq,
    actor: { kind: "session", id: `legion-daemon:${project}` },
    created_at: createdAt,
    payload: { body },
  };
}

// humanActorEvent is one message.created event from a human (or any non-daemon actor): a stale or
// quoted daemon sentence from a human never satisfies a daemon-message matcher (N5).
function humanActorEvent(body: string, createdAt = "2026-01-01T00:00:00Z") {
  return {
    type: "message.created",
    seq: 1,
    actor: { kind: "user", id: "u1" },
    created_at: createdAt,
    payload: { body },
  };
}

// issueUpdatedEvent is one issue.updated event, seq-numbered, carrying the issue's repo in its
// payload (the whole issue, repo included — model.go, issue_patch.go:407).
function issueUpdatedEvent(seq: number, repo: string | null, createdAt = "2026-01-01T00:00:00Z") {
  return {
    type: "issue.updated",
    seq,
    actor: { kind: "session", id: "legion-daemon:ACME" },
    created_at: createdAt,
    payload: { repo },
  };
}

const noneNote =
  "Legion did not admit this issue: its repository resolves to none. Set its repository (the issue header's Repository picker, or dispatch issue-update --repo owner/name) and move it to todo again.";

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

describe("children: structured Dispatch + state agreement, with raw evidence (N7)", () => {
  test("one child resolves to T and one to C in both Dispatch and the daemon's state: PASS, with raw reads kept", async () => {
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
    // N7: the raw Dispatch and state reads this observation judged are kept, not just a summary.
    expect(
      JSON.parse(readFileSync(join(evidence, "dispatch-issue-ACME-2.raw"), "utf8")).resolved_repo
    ).toBe("acme/widgets");
    expect(
      JSON.parse(readFileSync(join(evidence, "state.raw"), "utf8")).issues["ACME-2"].repo
    ).toBe("acme/widgets");
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
    dispatchServer = dispatchStub({ "ACME-2": JSON.stringify({ key: "ACME-2", status: "todo" }) });
    const env = fullInputs(
      `http://127.0.0.1:${daemonServer.port}`,
      `http://127.0.0.1:${dispatchServer.port}`
    );
    const { status, stderr } = await run(["done", "ACME-2"], env);
    expect(status).not.toBe(0);
    expect(stderr).toContain("CHECK done: FAIL");
  });
});

describe("record-head and pin: empty/invalid values never pass, trailing bytes kept (N7)", () => {
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

  // N7's own reproduction: a pin file ending in two newlines must keep both in evidence/pin.txt.
  test("the pin file's trailing bytes (two trailing newlines) are kept exactly in evidence", async () => {
    daemonServer = daemonStub();
    dispatchServer = dispatchStub({});
    const env = fullInputs(
      `http://127.0.0.1:${daemonServer.port}`,
      `http://127.0.0.1:${dispatchServer.port}`
    );
    const sha = "b".repeat(40);
    const pinContent = `pin: ${sha}\n\n`;
    const b64 = Buffer.from(pinContent).toString("base64");
    const gh = `
case "$*" in
  "api repos/acme/gadgets --jq .default_branch") printf 'main' ;;
  "api repos/acme/gadgets/contents/toolchain-pin.txt?ref=main --jq .content") printf '${b64}' ;;
  *) echo "gh: unexpected call: $*" >&2; exit 1 ;;
esac`;
    const { status, evidence } = await run(["pin", "acme/gadgets", sha], env, { gh });
    expect(status).toBe(0);
    const written = readFileSync(join(evidence, "pin.txt"));
    expect(written.length).toBe(Buffer.byteLength(pinContent));
    expect(written.toString()).toBe(pinContent);
  });
});

describe("second-owner: real in-pod exec as the role (N1), with a positive push control (N2)", () => {
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
  const goodGh = `
case "$*" in
  "api repos/zeta/tools/pulls/7 --jq .merged") printf 'true' ;;
  *) echo "gh: unexpected call: $*" >&2; exit 1 ;;
esac`;

  function roleBin(): string {
    const work = join(dir, `run-${++runs}`);
    const bin = join(work, "role-bin");
    mkdirSync(bin, { recursive: true });
    return bin;
  }

  // fakeRoleGh answers `gh api repos/acme/widgets --jq .full_name` only: the read probe T.
  function fakeRoleGh(bin: string): void {
    fakeBin(
      bin,
      "gh",
      `
case "$*" in
  "api repos/acme/widgets --jq .full_name") printf 'acme/widgets' ;;
  *) echo "gh: unexpected call (role): $*" >&2; exit 1 ;;
esac`
    );
  }

  // fakeRoleGit answers the dry-run push command for both O and T: O always succeeds (the
  // positive control); T's behavior is parameterized per test.
  function fakeRoleGit(bin: string, tBehavior: string): void {
    fakeBin(
      bin,
      "git",
      `
case "$*" in
  *"push --no-verify --dry-run https://github.com/zeta/tools"*)
    exit 0
    ;;
  *"push --no-verify --dry-run https://github.com/acme/widgets"*)
    ${tBehavior}
    ;;
  *)
    echo "git: unexpected call (role): $*" >&2
    exit 1
    ;;
esac`
    );
  }

  function fixtureEnv(bin: string): Record<string, string> {
    return {
      PATH: `${bin}:/usr/bin:/bin`,
      HOME: "/tmp",
      POD_UID: "test-pod-uid",
      LEGION_GH_PATH: "/opt/legion/bin/gh",
      LEGION_GH_OWNERS_DIR: "/gh",
      GH_CONFIG_DIR: "/gh/zeta-tools",
      LEGION_WORKSPACE: "/workspace",
    };
  }

  const podLabelKubectl = `
case "$*" in
  "get pod the-pod -o jsonpath={.metadata.labels.legion\\.dev/issue}")
    printf 'ACME-2'
    ;;
  *)
    ${realKubectl}
    ;;
esac`;

  test("a pre-merge phase (awaiting_merge) is a FAIL even with a merged PR and valid credentials", async () => {
    daemonServer = daemonStub(stateWithPR("awaiting_merge"));
    dispatchServer = dispatchStub({});
    const env = fullInputs(
      `http://127.0.0.1:${daemonServer.port}`,
      `http://127.0.0.1:${dispatchServer.port}`
    );
    const bin = roleBin();
    fakeRoleGh(bin);
    fakeRoleGit(
      bin,
      'echo "remote: Permission to acme/widgets.git denied. fatal: unable to access" >&2; exit 128'
    );
    spawnRoleFixture(fixtureEnv(bin));
    const { status, stderr } = await run(["second-owner", "ACME-2", "the-pod"], env, {
      gh: goodGh,
      kubectl: podLabelKubectl,
    });
    expect(status).not.toBe(0);
    expect(stderr).toContain("CHECK second-owner: FAIL");
  }, 30000);

  test("no process in the container carries LEGION_GH_PATH (the bare pod shell, N1's own reproduction): FAIL", async () => {
    daemonServer = daemonStub(stateWithPR("production_check"));
    dispatchServer = dispatchStub({});
    const env = fullInputs(
      `http://127.0.0.1:${daemonServer.port}`,
      `http://127.0.0.1:${dispatchServer.port}`
    );
    // No spawnRoleFixture at all: the /proc scan finds no LEGION_GH_PATH-carrying process.
    const { status, stderr } = await run(["second-owner", "ACME-2", "the-pod"], env, {
      gh: goodGh,
      kubectl: podLabelKubectl,
    });
    expect(status).not.toBe(0);
    expect(stderr).toContain("CHECK second-owner: FAIL");
  }, 30000);

  test("production_check, merged, read succeeds, O's own push succeeds, but T's push also succeeds (no refusal): FAIL", async () => {
    daemonServer = daemonStub(stateWithPR("production_check"));
    dispatchServer = dispatchStub({});
    const env = fullInputs(
      `http://127.0.0.1:${daemonServer.port}`,
      `http://127.0.0.1:${dispatchServer.port}`
    );
    const bin = roleBin();
    fakeRoleGh(bin);
    fakeRoleGit(bin, "exit 0"); // T's push succeeds too: never a refusal
    spawnRoleFixture(fixtureEnv(bin));
    const { status, stderr } = await run(["second-owner", "ACME-2", "the-pod"], env, {
      gh: goodGh,
      kubectl: podLabelKubectl,
    });
    expect(status).not.toBe(0);
    expect(stderr).toContain("CHECK second-owner: FAIL");
  }, 30000);

  // N2's own reproduction table: a push refused for a non-permission reason (bad ref, network,
  // not-a-repo, unanswered credential helper) is never counted as proof — only a 403 or an
  // explicit "Permission to ... denied" is.
  for (const [name, tBehavior] of [
    [
      "network unreachable",
      'echo "fatal: unable to access https://github.com/acme/widgets/: Failed to connect" >&2; exit 128',
    ],
    ["bad ref", 'echo "error: src refspec HEAD does not match any" >&2; exit 1'],
    ["cwd not a git repository", 'echo "fatal: not a git repository" >&2; exit 128'],
    [
      "credential helper answers nothing",
      `echo "fatal: could not read Username for 'https://github.com': No such device or address" >&2; exit 128`,
    ],
  ] as const) {
    test(`a dry-run push to T failing for a non-permission reason (${name}) is a FAIL, not a PASS`, async () => {
      daemonServer = daemonStub(stateWithPR("production_check"));
      dispatchServer = dispatchStub({});
      const env = fullInputs(
        `http://127.0.0.1:${daemonServer.port}`,
        `http://127.0.0.1:${dispatchServer.port}`
      );
      const bin = roleBin();
      fakeRoleGh(bin);
      fakeRoleGit(bin, tBehavior);
      spawnRoleFixture(fixtureEnv(bin));
      const { status, stderr } = await run(["second-owner", "ACME-2", "the-pod"], env, {
        gh: goodGh,
        kubectl: podLabelKubectl,
      });
      expect(status).not.toBe(0);
      expect(stderr).toContain("CHECK second-owner: FAIL");
    }, 30000);
  }

  test("GH_CONFIG_DIR not naming the issue's own owner directory is a FAIL", async () => {
    daemonServer = daemonStub(stateWithPR("production_check"));
    dispatchServer = dispatchStub({});
    const env = fullInputs(
      `http://127.0.0.1:${daemonServer.port}`,
      `http://127.0.0.1:${dispatchServer.port}`
    );
    const bin = roleBin();
    fakeRoleGh(bin);
    fakeRoleGit(
      bin,
      'echo "remote: Permission to acme/widgets.git denied. fatal: unable to access" >&2; exit 128'
    );
    spawnRoleFixture({ ...fixtureEnv(bin), GH_CONFIG_DIR: "/gh/other-owner" });
    const { status, stderr } = await run(["second-owner", "ACME-2", "the-pod"], env, {
      gh: goodGh,
      kubectl: podLabelKubectl,
    });
    expect(status).not.toBe(0);
    expect(stderr).toContain("CHECK second-owner: FAIL");
  }, 30000);

  test("production_check, merged, role read succeeds, O's push succeeds, T's push refused 403: PASS", async () => {
    daemonServer = daemonStub(stateWithPR("production_check"));
    dispatchServer = dispatchStub({});
    const env = fullInputs(
      `http://127.0.0.1:${daemonServer.port}`,
      `http://127.0.0.1:${dispatchServer.port}`
    );
    const bin = roleBin();
    fakeRoleGh(bin);
    fakeRoleGit(
      bin,
      `echo "remote: Permission to acme/widgets.git denied to zeta-tools-app. fatal: unable to access 'https://github.com/acme/widgets/': The requested URL returned error: 403" >&2; exit 128`
    );
    spawnRoleFixture(fixtureEnv(bin));
    const { status, stdout, evidence } = await run(["second-owner", "ACME-2", "the-pod"], env, {
      gh: goodGh,
      kubectl: podLabelKubectl,
    });
    expect(status).toBe(0);
    expect(stdout).toContain("CHECK second-owner: PASS");
    const written = readFileSync(join(evidence, "second-owner.txt"), "utf8");
    expect(written).toContain("403");
  }, 30000);

  test("an unrelated pod (legion.dev/issue mismatch) is refused before any exec as a role", async () => {
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

describe("cache: pod binding and the producer's exact, anchored success line", () => {
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

  // The anchored form rejects a near-miss line an unanchored `.*`-heavy regex previously accepted:
  // an error line that happens to contain the same two keywords (cache, GitHub) in another shape.
  test("an error line mentioning cache and github in another shape is a FAIL, not a PASS", async () => {
    daemonServer = daemonStub(stateWithCache);
    dispatchServer = dispatchStub({});
    const env = fullInputs(
      `http://127.0.0.1:${daemonServer.port}`,
      `http://127.0.0.1:${dispatchServer.port}`
    );
    const kubectl = `
case "$*" in
  "get pod the-pod -o jsonpath={.metadata.labels.legion\\.dev/issue}") printf 'ACME-2' ;;
  "logs the-pod -c workspace-fetch") printf 'workspace-init fetch: the repository cache failed; fell back to GitHub for everything\\n' ;;
  *) echo "kubectl: unexpected call: $*" >&2; exit 1 ;;
esac`;
    const { status, stderr } = await run(["cache", "ACME-2", "the-pod"], env, { kubectl });
    expect(status).not.toBe(0);
    expect(stderr).toContain("CHECK cache: FAIL");
  });

  test("the producer's exact anchored success line, on the right pod, with cache.ready true: PASS", async () => {
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
    const { status, stdout, evidence } = await run(["cache", "ACME-2", "the-pod"], env, {
      kubectl,
    });
    expect(status).toBe(0);
    expect(stdout).toContain("CHECK cache: PASS");
    expect(readFileSync(join(evidence, "cache-logs-ACME-2.raw"), "utf8")).toContain(
      "workspace-init fetch:"
    );
  });
});

describe("negative no-repo: exact daemon actor, outbox marker, resolved_repo, and the issues-type guard (N5)", () => {
  test("the exact none-note from the daemon's own actor, Dispatch still resolving to none: PASS", async () => {
    daemonServer = daemonStub();
    dispatchServer = dispatchStub(
      { "ACME-9": JSON.stringify({ key: "ACME-9", resolved_repo: null }) },
      {
        "ACME-9": JSON.stringify([
          daemonActorEvent("ACME", 5, `${noneNote}\n\n<!-- legion-outbox:41 -->`),
        ]),
      }
    );
    const env = fullInputs(
      `http://127.0.0.1:${daemonServer.port}`,
      `http://127.0.0.1:${dispatchServer.port}`
    );
    const { status, stdout } = await run(["negative", "no-repo", "ACME-9"], env);
    expect(status).toBe(0);
    expect(stdout).toContain("CHECK negative-no-repo-ACME-9: PASS");
  });

  test("a human message quoting the exact none-note is a FAIL: the actor must be the daemon's own session", async () => {
    daemonServer = daemonStub();
    dispatchServer = dispatchStub(
      { "ACME-9": JSON.stringify({ key: "ACME-9", resolved_repo: null }) },
      { "ACME-9": JSON.stringify([humanActorEvent(`FYI the daemon said: ${noneNote}`)]) }
    );
    const env = fullInputs(
      `http://127.0.0.1:${daemonServer.port}`,
      `http://127.0.0.1:${dispatchServer.port}`
    );
    const { status, stderr } = await run(["negative", "no-repo", "ACME-9"], env);
    expect(status).not.toBe(0);
    expect(stderr).toContain("CHECK negative-no-repo-ACME-9: FAIL");
  });

  test("Dispatch now resolves the issue to a real repository (a stale none-note from before): FAIL", async () => {
    daemonServer = daemonStub();
    dispatchServer = dispatchStub(
      { "ACME-9": JSON.stringify({ key: "ACME-9", resolved_repo: "acme/gadgets" }) },
      {
        "ACME-9": JSON.stringify([
          daemonActorEvent("ACME", 5, `${noneNote}\n\n<!-- legion-outbox:41 -->`),
        ]),
      }
    );
    const env = fullInputs(
      `http://127.0.0.1:${daemonServer.port}`,
      `http://127.0.0.1:${dispatchServer.port}`
    );
    const { status, stderr } = await run(["negative", "no-repo", "ACME-9"], env);
    expect(status).not.toBe(0);
    expect(stderr).toContain("CHECK negative-no-repo-ACME-9: FAIL");
  });

  test("the daemon's latest repository note is a different one (App not installed), not the none-note: FAIL", async () => {
    daemonServer = daemonStub();
    const laterNote =
      "Legion did not admit this issue: the review App is not installed on acme/gadgets. Install it on acme/gadgets, or set another repository (the issue header's Repository picker, or dispatch issue-update --repo owner/name), and move the issue to todo again.";
    dispatchServer = dispatchStub(
      { "ACME-9": JSON.stringify({ key: "ACME-9", resolved_repo: null }) },
      {
        "ACME-9": JSON.stringify([
          daemonActorEvent("ACME", 6, `${laterNote}\n\n<!-- legion-outbox:42 -->`),
          daemonActorEvent("ACME", 5, `${noneNote}\n\n<!-- legion-outbox:41 -->`),
        ]),
      }
    );
    const env = fullInputs(
      `http://127.0.0.1:${daemonServer.port}`,
      `http://127.0.0.1:${dispatchServer.port}`
    );
    const { status, stderr } = await run(["negative", "no-repo", "ACME-9"], env);
    expect(status).not.toBe(0);
    expect(stderr).toContain("CHECK negative-no-repo-ACME-9: FAIL");
  });

  test("a state body of {} or null never satisfies `.issues has(key)` vacuously: FAIL", async () => {
    daemonServer = daemonStub("{}");
    dispatchServer = dispatchStub(
      { "ACME-9": JSON.stringify({ key: "ACME-9", resolved_repo: null }) },
      {
        "ACME-9": JSON.stringify([
          daemonActorEvent("ACME", 5, `${noneNote}\n\n<!-- legion-outbox:41 -->`),
        ]),
      }
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

// prsGhFake is a fake `gh` answering both calls negative_pr_in_x makes against X's pull-request
// list: the `--jq [.[].number] | max // 0` cursor read at `before`, and the raw list read at
// `after` (raw_fetch, no --jq) — both must answer the same prsJson consistently within one test.
function prsGhFake(prsJson: string): string {
  const highest = Math.max(
    0,
    ...(JSON.parse(prsJson) as Array<{ number: number }>).map((pr) => pr.number)
  );
  return `
case "$*" in
  "api repos/other/outside/pulls?state=all&per_page=100 --jq [.[].number] | max // 0") printf '${highest}' ;;
  "api repos/other/outside/pulls?state=all&per_page=100") printf '${prsJson}' ;;
  *) echo "gh: unexpected: $*" >&2; exit 1 ;;
esac`;
}

// baselineRunId pulls the minted run id out of a `before` call's stdout.
function baselineRunId(stdout: string): string {
  const runId = stdout.match(/RUN_ID: (\S+)/)?.[1] ?? "";
  expect(runId).not.toBe("");
  return runId;
}

describe("negative pr-in-x: PR-number cursor and the daemon-side check (N3)", () => {
  test("before prints BASELINE, never PASS, for a recorded child", async () => {
    daemonServer = daemonStub();
    dispatchServer = dispatchStub({});
    const env = fullInputs(
      `http://127.0.0.1:${daemonServer.port}`,
      `http://127.0.0.1:${dispatchServer.port}`
    );
    const gh = prsGhFake("[]");
    const { status, stdout } = await run(["negative", "pr-in-x", "before", "ACME-2"], env, { gh });
    expect(status).toBe(0);
    expect(stdout).toContain("BASELINE pr-in-x-ACME-2: captured");
    expect(stdout).not.toContain("PASS");
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

  // Hardening: a run id with a path-traversal shape is refused outright, never used to open a
  // baseline file outside this run's own evidence directory.
  test("a run id containing '..' is refused as unsafe, never read as a path", async () => {
    daemonServer = daemonStub();
    dispatchServer = dispatchStub({});
    const env = fullInputs(
      `http://127.0.0.1:${daemonServer.port}`,
      `http://127.0.0.1:${dispatchServer.port}`
    );
    const { status, stderr } = await run(
      ["negative", "pr-in-x", "after", "../../etc/passwd", "ACME-2"],
      env
    );
    expect(status).not.toBe(0);
    expect(stderr).toContain("not a bare id");
  });

  test("after immediately (no PR opened yet) with highest-PR 0 in X: FAIL", async () => {
    daemonServer = daemonStub();
    dispatchServer = dispatchStub({});
    const env = fullInputs(
      `http://127.0.0.1:${daemonServer.port}`,
      `http://127.0.0.1:${dispatchServer.port}`
    );
    const { bin, evidence } = newFakeDir({
      gh: prsGhFake("[]"),
    });
    const path = isolatedPath(bin);
    const before = await spawnScript(
      ["negative", "pr-in-x", "before", "ACME-2"],
      path,
      evidence,
      env
    );
    expect(before.status).toBe(0);
    const runId = baselineRunId(before.stdout);
    const after = await spawnScript(
      ["negative", "pr-in-x", "after", runId, "ACME-2"],
      path,
      evidence,
      env
    );
    expect(after.status).not.toBe(0);
    expect(after.stderr).toContain("CHECK negative-pr-in-x-ACME-2: FAIL");
  });

  // N3's own reproduction: a PR opened and closed in X, with the daemon's own record also showing
  // the change (pullRequest now names X), must never PASS — the operation proof alone is not
  // enough; the daemon side must be unaffected too.
  test("a closed PR exists in X, but the daemon's own pullRequest now names X too: FAIL", async () => {
    const { server, setBody } = mutableDaemonStub(stateBody); // ACME-2 starts as acme/widgets, no PR
    daemonServer = server;
    dispatchServer = dispatchStub({});
    const env = fullInputs(
      `http://127.0.0.1:${server.port}`,
      `http://127.0.0.1:${dispatchServer.port}`
    );
    const { bin, evidence } = newFakeDir({
      gh: prsGhFake("[]"),
    });
    const path = isolatedPath(bin);
    const before = await spawnScript(
      ["negative", "pr-in-x", "before", "ACME-2"],
      path,
      evidence,
      env
    );
    expect(before.status).toBe(0);
    const runId = baselineRunId(before.stdout);
    setBody(
      makeStateBody({
        issues: {
          "ACME-2": {
            key: "ACME-2",
            generation: 1,
            phase: "needs_review",
            status: "todo",
            repo: "acme/widgets",
            pullRequest: { repo: "other/outside", number: 5 },
          },
        },
      })
    );
    fakeBin(
      bin,
      "gh",
      prsGhFake(
        '[{"number":5,"head":{"ref":"legion/ACME-2"},"state":"closed","created_at":"2026-01-02T00:00:00Z","closed_at":"2026-01-02T01:00:00Z"}]'
      )
    );
    const after = await spawnScript(
      ["negative", "pr-in-x", "after", runId, "ACME-2"],
      path,
      evidence,
      env
    );
    expect(after.status).not.toBe(0);
    expect(after.stderr).toContain("CHECK negative-pr-in-x-ACME-2: FAIL");
  });

  test("a closed PR numbered above the baseline's cursor, daemon side unaffected: PASS", async () => {
    daemonServer = daemonStub(stateBody);
    dispatchServer = dispatchStub({});
    const env = fullInputs(
      `http://127.0.0.1:${daemonServer.port}`,
      `http://127.0.0.1:${dispatchServer.port}`
    );
    const { bin, evidence } = newFakeDir({
      gh: prsGhFake('[{"number":4,"head":{"ref":"legion/OTHER-1"},"state":"open"}]'),
    });
    const path = isolatedPath(bin);
    const before = await spawnScript(
      ["negative", "pr-in-x", "before", "ACME-2"],
      path,
      evidence,
      env
    );
    expect(before.status).toBe(0);
    const runId = baselineRunId(before.stdout);
    fakeBin(
      bin,
      "gh",
      prsGhFake(
        '[{"number":4,"head":{"ref":"legion/OTHER-1"},"state":"open"},{"number":5,"head":{"ref":"legion/ACME-2"},"state":"closed","created_at":"2026-01-02T00:00:00Z","closed_at":"2026-01-02T01:00:00Z"}]'
      )
    );
    const after = await spawnScript(
      ["negative", "pr-in-x", "after", runId, "ACME-2"],
      path,
      evidence,
      env
    );
    expect(after.status).toBe(0);
    expect(after.stdout).toContain("CHECK negative-pr-in-x-ACME-2: PASS");
  });

  // Hardening: a generation bump between before and after (a hand-over or re-admission) is a
  // foreign baseline, refused outright rather than compared across two different issue lifetimes.
  test("the issue's generation moved since the baseline: refused as foreign", async () => {
    const { server, setBody } = mutableDaemonStub(stateBody);
    daemonServer = server;
    dispatchServer = dispatchStub({});
    const env = fullInputs(
      `http://127.0.0.1:${server.port}`,
      `http://127.0.0.1:${dispatchServer.port}`
    );
    const { bin, evidence } = newFakeDir({
      gh: prsGhFake("[]"),
    });
    const path = isolatedPath(bin);
    const before = await spawnScript(
      ["negative", "pr-in-x", "before", "ACME-2"],
      path,
      evidence,
      env
    );
    expect(before.status).toBe(0);
    const runId = baselineRunId(before.stdout);
    setBody(
      makeStateBody({
        issues: {
          "ACME-2": {
            key: "ACME-2",
            generation: 2,
            phase: "implementing",
            status: "todo",
            repo: "acme/widgets",
          },
        },
      })
    );
    const after = await spawnScript(
      ["negative", "pr-in-x", "after", runId, "ACME-2"],
      path,
      evidence,
      env
    );
    expect(after.status).not.toBe(0);
    expect(after.stderr).toContain("a hand-over or re-admission since");
  });

  test("two overlapping before calls for the same issue never collide: each gets its own run id and baseline", async () => {
    daemonServer = daemonStub();
    dispatchServer = dispatchStub({});
    const env = fullInputs(
      `http://127.0.0.1:${daemonServer.port}`,
      `http://127.0.0.1:${dispatchServer.port}`
    );
    const gh = prsGhFake("[]");
    const { bin, evidence } = newFakeDir({ gh });
    const path = isolatedPath(bin);
    const [first, second] = await Promise.all([
      spawnScript(["negative", "pr-in-x", "before", "ACME-2"], path, evidence, env),
      spawnScript(["negative", "pr-in-x", "before", "ACME-2"], path, evidence, env),
    ]);
    expect(first.status).toBe(0);
    expect(second.status).toBe(0);
    expect(baselineRunId(first.stdout)).not.toBe(baselineRunId(second.stdout));
  });
});

describe("negative repo-edit: Dispatch's seq cursor and the exact kept-repository message (N4, N5)", () => {
  test("before refuses when Dispatch already resolves to X", async () => {
    daemonServer = daemonStub();
    dispatchServer = dispatchStub({
      "ACME-2": JSON.stringify({ key: "ACME-2", resolved_repo: "other/outside" }),
    });
    const env = fullInputs(
      `http://127.0.0.1:${daemonServer.port}`,
      `http://127.0.0.1:${dispatchServer.port}`
    );
    const { status, stderr } = await run(["negative", "repo-edit", "before", "ACME-2"], env);
    expect(status).not.toBe(0);
    expect(stderr).toContain("already resolves to X");
  });

  test("before on an unrecorded issue is refused", async () => {
    daemonServer = daemonStub();
    dispatchServer = dispatchStub({ "ACME-404": JSON.stringify({ key: "ACME-404" }) });
    const env = fullInputs(
      `http://127.0.0.1:${daemonServer.port}`,
      `http://127.0.0.1:${dispatchServer.port}`
    );
    const { status, stderr } = await run(["negative", "repo-edit", "before", "ACME-404"], env);
    expect(status).not.toBe(0);
    expect(stderr).toContain("not the recorded child this control needs");
  });

  const xTargetedNote =
    "Legion keeps acme/widgets as this issue's repository; the change to other/outside in Dispatch is not applied — an issue's repository is fixed when Legion records it. Create a new issue for other/outside instead.";

  // N4's own reproduction: the repository and the message both already reflect the edit, but the
  // only post-baseline event is the daemon's own routine status write (also issue.updated) — this
  // must never satisfy the seq-cursor check, which requires payload.repo == X on that event.
  test("the only issue.updated after the baseline is a routine status write (payload.repo unset): FAIL", async () => {
    daemonServer = daemonStub(stateBody); // ACME-2: acme/widgets
    dispatchServer = dispatchStub({
      "ACME-2": JSON.stringify({ key: "ACME-2", resolved_repo: "acme/widgets" }),
    });
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
    const runId = baselineRunId(before.stdout);
    dispatchServer.stop(true);
    dispatchServer = dispatchStub(
      { "ACME-2": JSON.stringify({ key: "ACME-2", repo: "other/outside" }) },
      {
        "ACME-2": JSON.stringify([
          issueUpdatedEvent(20, null), // a routine status write: no repo in its payload
          daemonActorEvent("ACME", 19, `${xTargetedNote}\n\n<!-- legion-outbox:9 -->`),
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
    expect(after.status).not.toBe(0);
    expect(after.stderr).toContain("CHECK negative-repo-edit-ACME-2: FAIL");
  });

  test("an issue.updated after the baseline naming X, with the exact kept-repository message from the daemon: PASS", async () => {
    daemonServer = daemonStub(stateBody);
    dispatchServer = dispatchStub({
      "ACME-2": JSON.stringify({ key: "ACME-2", resolved_repo: "acme/widgets" }),
    });
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
    const runId = baselineRunId(before.stdout);
    dispatchServer.stop(true);
    dispatchServer = dispatchStub(
      { "ACME-2": JSON.stringify({ key: "ACME-2", repo: "other/outside" }) },
      {
        "ACME-2": JSON.stringify([
          issueUpdatedEvent(20, "other/outside"),
          daemonActorEvent("ACME", 19, `${xTargetedNote}\n\n<!-- legion-outbox:9 -->`),
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

  // noteOnce posts the kept-repository note only once per resolution: a rerun that has no fresh
  // message must still PASS as long as the seq-bound issue.updated and the daemon's repo are
  // right, since the message check only verifies the format, never requiring a brand-new one.
  test("no fresh message after the baseline, but the seq-bound issue.updated and an earlier matching note both exist: PASS", async () => {
    daemonServer = daemonStub(stateBody);
    dispatchServer = dispatchStub({
      "ACME-2": JSON.stringify({ key: "ACME-2", resolved_repo: "acme/widgets" }),
    });
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
    const runId = baselineRunId(before.stdout);
    dispatchServer.stop(true);
    dispatchServer = dispatchStub(
      { "ACME-2": JSON.stringify({ key: "ACME-2", repo: "other/outside" }) },
      {
        "ACME-2": JSON.stringify([
          issueUpdatedEvent(20, "other/outside"),
          daemonActorEvent("ACME", 3, `${xTargetedNote}\n\n<!-- legion-outbox:1 -->`), // an older message, before the baseline's own last_seq
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
