import {
  accessSync,
  existsSync,
  mkdirSync,
  mkdtempSync,
  readFileSync,
  rmSync,
  symlinkSync,
  writeFileSync,
} from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";

// Shared fixtures for the multi-repository acceptance observer's tests:
// scripts/e2e/multi-repo-acceptance.sh reads only structured sources — the daemon's state JSON (a
// Bun.serve stub), Dispatch's own JSON API (a second stub, never `dispatch read`'s prose), GitHub's
// API (a fake `gh`), and `kubectl` (a fake binary that really execs what it is given, against a
// FAKE `/proc` tree built under this suite's own temp directory via MULTIREPO_PROC_ROOT, never the
// host's own `/proc`, whose live Legion panes would otherwise pollute the scan). Every fake, stub
// and fixture is isolated per test; the host's real dispatch/gh/kubectl/proc are never reachable.

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

// newTrustedRoot lays out the part of the image second-owner's prober runs by absolute path: a
// directory holding opt/legion/bin (where a test puts the fake `legion` and `gh`) and the
// interpreter and `env` the fakes' `#!/usr/bin/env bash` lines resolve through the prober's
// PATH, which names only directories under this root. It returns the root.
export function newTrustedRoot(): string {
  const root = join(newWork(), "trusted-root");
  mkdirSync(join(root, "opt", "legion", "bin"), { recursive: true });
  mkdirSync(join(root, "usr", "bin"), { recursive: true });
  symlinkSync(bashPath, join(root, "usr", "bin", "bash"));
  symlinkSync(join(realDir("env"), "env"), join(root, "usr", "bin", "env"));
  return root;
}

export const imageBin = (root: string) => join(root, "opt", "legion", "bin");

// Appends the environment and arguments of each call to `<base>.env` and `<base>.args`.
export const recordCall = (base: string) =>
  `{ env; echo --; } >>'${base}.env'\nprintf '%s\\n' "$*" >>'${base}.args'`;

// The `gh api repos/<T> --jq .full_name` read the prober makes as the role.
export function fakeRoleGh(bin: string, repo: string, answer: string, record: string): void {
  fakeBin(
    bin,
    "gh",
    `
${recordCall(record)}
case "$*" in
  "api repos/${repo} --jq .full_name") printf '${answer}' ;;
  *) echo "gh: unexpected call (role): $*" >&2; exit 1 ;;
esac`
  );
}

// The credential helper: for `git-credential get` with `path=<repo>` on stdin it prints that
// repository's own token, or nothing (exit 1) for a repository in `silent`.
export function fakeRoleLegion(bin: string, silent: string[], record: string): void {
  fakeBin(
    bin,
    "legion",
    `
${recordCall(record)}
if [ "$1 $2" != "git-credential get" ]; then
  echo "legion: unexpected call (role): $*" >&2
  exit 1
fi
repo=
while IFS= read -r line; do
  case "$line" in path=*) repo=\${line#path=} ;; esac
done
case "$repo" in
${silent.map((repo) => `  ${repo}) exit 1 ;;`).join("\n")}
esac
printf 'username=x-access-token\\npassword=TOKEN-%s\\n' "\${repo//\\//_}"`
  );
}

export const OWNERS_DIR = "/var/run/legion/gh";

export const IMAGE_PATH = [
  "/opt/legion/bin",
  "/opt/omp/bin",
  "/opt/codegraph/bin",
  "/usr/local/bin",
  "/usr/bin",
  "/bin",
];

// The environment the image's constants give `legion` and `gh` under a trusted root.
export const imageEnv = (root: string): Record<string, string> => ({
  HOME: "/nonexistent",
  PATH: IMAGE_PATH.map((directory) => root + directory).join(":"),
  LEGION_GH_OWNERS_DIR: `${root}${OWNERS_DIR}`,
  LEGION_GH_PATH: `${root}/usr/local/bin/gh`,
  GH_CONFIG_DIR: `${root}${OWNERS_DIR}/zeta`,
});

// What a recording fake saw: one environment per call (without the variables bash adds itself),
// and the arguments of each call.
export function recordedCalls(base: string): {
  envs: Array<Record<string, string>>;
  args: string[];
} {
  const read = (suffix: string) =>
    existsSync(base + suffix) ? readFileSync(base + suffix, "utf8") : "";
  const bashOwn = new Set(["PWD", "SHLVL", "_", "OLDPWD"]);
  const envs = read(".env")
    .split("--\n")
    .filter((block) => block !== "")
    .map((block) =>
      Object.fromEntries(
        block
          .split("\n")
          .filter((line) => line.includes("="))
          .map((line) => [line.slice(0, line.indexOf("=")), line.slice(line.indexOf("=") + 1)])
          .filter(([name]) => !bashOwn.has(name as string))
      )
    );
  return {
    envs,
    args: read(".args")
      .split("\n")
      .filter((line) => line !== ""),
  };
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
