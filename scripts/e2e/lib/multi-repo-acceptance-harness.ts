import { accessSync, mkdirSync, mkdtempSync, rmSync, symlinkSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";

// Shared fixtures for the multi-repository acceptance observer's tests:
// scripts/e2e/multi-repo-acceptance.sh reads only structured sources — the daemon's state JSON (a
// Bun.serve stub), Dispatch's own JSON API (a second stub, never `dispatch read`'s prose), GitHub's
// API (a fake `gh`), and `kubectl` (a fake binary that really execs what it is given, against a
// FAKE `/proc` tree built under this suite's own temp directory via MULTIREPO_PROC_ROOT, never the
// host's own `/proc`, whose live Legion panes would otherwise pollute the scan). Every fake, stub
// and fixture is isolated per test; the host's real dispatch/gh/kubectl/proc are never reachable.

// The one path the prober accepts as a worker shim's executable: the image's own legion binary.
export const LEGION_BINARY = "/opt/legion/bin/legion";
export const script = join(import.meta.dir, "..", "multi-repo-acceptance.sh");
export const dir = mkdtempSync(join(tmpdir(), "multi-repo-acceptance-test."));
process.on("exit", () => rmSync(dir, { recursive: true, force: true }));

let runs = 0;

// nextRun numbers a fresh scratch path under dir; every fixture of every test file draws from it.
export function nextRun(): number {
  return ++runs;
}

// closeServers stops every stub a test started through the stub builders below. Each test file
// calls it from its own afterEach.
const closers: Array<() => void> = [];
export const servers = {
  track<T extends { stop: (force?: boolean) => unknown }>(server: T): T {
    closers.push(() => server.stop(true));
    return server;
  },
  trackCloser(close: () => void): void {
    closers.push(close);
  },
};
export function closeServers(): void {
  for (const close of closers.splice(0)) close();
}

export function fakeBin(binDir: string, name: string, body: string): void {
  writeFileSync(join(binDir, name), `#!/usr/bin/env bash\n${body}\n`, { mode: 0o755 });
}

export const defaultFake = (name: string) => `echo "${name}: unexpected call: $*" >&2; exit 1`;

export function realDir(name: string): string {
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

export function isolatedPath(bin: string): string {
  return [bin, realDir("jq"), realDir("curl"), realDir("base64"), "/usr/bin", "/bin"].join(":");
}

export const bashPath = `${realDir("bash")}/bash`;

// realKubectl forwards `exec [-i] <pod> -c <container> -- <rest...>` to a real local exec of
// <rest>, with the caller's stdin: this is what lets the observer's own in-pod prober run
// unmodified, scanning MULTIREPO_PROC_ROOT (a fake tree this suite builds) instead of a real pod's
// `/proc`.
export const realKubectl = `
if [ "$1" = exec ]; then
  shift
  if [ "$1" = -i ]; then shift; fi
  shift; shift; shift; shift
  exec "$@"
fi
echo "kubectl: unexpected call: $*" >&2
exit 1`;

// newWork numbers a fresh scratch directory under dir.
export function newWork(): string {
  return join(dir, `run-${nextRun()}`);
}

export function newFakeDir(fakes: { gh?: string; kubectl?: string } = {}) {
  const work = newWork();
  const bin = join(work, "bin");
  const evidence = join(work, "evidence");
  mkdirSync(bin, { recursive: true });
  mkdirSync(evidence, { recursive: true });
  fakeBin(bin, "gh", fakes.gh ?? defaultFake("gh"));
  fakeBin(bin, "kubectl", fakes.kubectl ?? defaultFake("kubectl"));
  return { bin, evidence, work };
}

export async function spawnScript(
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

export async function run(
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
// (NUL-separated argv, trailing NUL), environ (NUL-separated VAR=value pairs, trailing NUL when
// non-empty) and, with `exe`, the /proc/<pid>/exe symlink to that path — exactly what
// scripts/e2e/lib/as-role.py reads.
export function writeProcEntry(
  root: string,
  pid: string,
  opts: {
    ppid: string;
    cmdline: string[];
    environ?: Record<string, string>;
    unreadableEnviron?: boolean;
    exe?: string;
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
  if (opts.exe !== undefined) {
    symlinkSync(opts.exe, join(procDir, "exe"));
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
// every shim and agent's environment (they share one in a real pod). Every worker-shim entry gets
// an `exe` symlink to the image's legion binary, as the prober requires; `shimExe` points it
// elsewhere, or null leaves it out.
export interface FakeProcTreeOptions {
  shimPids?: string[];
  skipAgent?: boolean;
  agentArgv?: string[];
  extraShimChildren?: boolean;
  duplicateAgent?: boolean;
  shimParent?: string;
  shimExe?: string | null;
}

export function fakeProcTree(
  roleEnviron: Record<string, string>,
  opts: FakeProcTreeOptions = {}
): string {
  const root = join(dir, `proc-${nextRun()}`);
  mkdirSync(root, { recursive: true });
  writeProcEntry(root, "1", { ppid: "0", cmdline: ["launcher"], environ: { PATH: "/usr/bin" } });
  const shimPids = opts.shimPids ?? ["7"];
  const agentArgv = opts.agentArgv ?? ["omp", "--mode", "rpc"];
  const shimParent = opts.shimParent ?? "1";
  const shimExe = opts.shimExe === undefined ? LEGION_BINARY : opts.shimExe;
  for (const shimPid of shimPids) {
    writeProcEntry(root, shimPid, {
      ppid: shimParent,
      cmdline: ["legion", "worker-shim", "--connect", "tcp://x:1", "--", ...agentArgv],
      environ: roleEnviron,
      ...(shimExe === null ? {} : { exe: shimExe }),
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

export const tokenFile = join(dir, "dispatch-token");
writeFileSync(tokenFile, "dispatch-bearer\n");

export const fullInputs = (daemonUrl: string, dispatchUrl: string): Record<string, string> => ({
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
export function makeStateBody(overrides: Record<string, unknown> = {}): string {
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
export const stateBody = makeStateBody();

export function daemonStub(body: string = stateBody): Server {
  return servers.track(
    Bun.serve({
      port: 0,
      fetch(req) {
        const url = new URL(req.url);
        if (url.pathname === "/healthz") return Response.json({ status: "ok" });
        if (url.pathname === "/legion/v1/state") {
          return new Response(body, { headers: { "content-type": "application/json" } });
        }
        return new Response("not found", { status: 404 });
      },
    })
  );
}

// mutableDaemonStub is daemonStub whose /legion/v1/state body can change after the server starts,
// without changing its URL: the baseline protocol binds a baseline to the daemon URL it captured
// against, so a test that must change the daemon's answer between `before` and `after` needs the
// same server, not a fresh one on a new port.
export function mutableDaemonStub(initial: string): {
  server: Server;
  setBody: (body: string) => void;
} {
  let body = initial;
  const server = servers.track(
    Bun.serve({
      port: 0,
      fetch(req) {
        const url = new URL(req.url);
        if (url.pathname === "/healthz") return Response.json({ status: "ok" });
        if (url.pathname === "/legion/v1/state") {
          return new Response(body, { headers: { "content-type": "application/json" } });
        }
        return new Response("not found", { status: 404 });
      },
    })
  );
  return {
    server,
    setBody: (next: string) => {
      body = next;
    },
  };
}

// dispatchStub serves Dispatch's own GET /api/v1/issues/:key and /events, as issues/events map
// key -> response body (a JSON string). A key with no entry answers 404.
export function dispatchStub(
  issues: Record<string, string>,
  events: Record<string, string> = {}
): Server {
  return servers.track(
    Bun.serve({
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
    })
  );
}

// daemonActorEvent is one event with the daemon's own exact actor identity
// (legion-daemon:<PROJECT>, internal/dispatch/client.go:287-289), a seq, and a message.created
// body.
export function daemonActorEvent(
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
export function humanActorEvent(body: string, createdAt = "2026-01-01T00:00:00Z") {
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
export function issueUpdatedEvent(
  seq: number,
  repo: string | null,
  createdAt = "2026-01-01T00:00:00Z"
) {
  return {
    type: "issue.updated",
    seq,
    actor: { kind: "session", id: "legion-daemon:ACME" },
    created_at: createdAt,
    payload: { repo },
  };
}

export const noneNote =
  "Legion did not admit this issue: its repository resolves to none. Set its repository (the issue header's Repository picker, or dispatch issue-update --repo owner/name) and move it to todo again.";
