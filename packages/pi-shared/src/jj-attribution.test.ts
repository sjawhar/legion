import { afterEach, describe, expect, it } from "bun:test";
import { chmod, mkdir, mkdtemp, readdir, readFile, realpath, rm, symlink } from "node:fs/promises";
import { tmpdir } from "node:os";
import path from "node:path";
import {
  exportJjSessionAttribution,
  JJ_CONFIG_PATH_TIMEOUT_MS,
  resolvedWorkspace,
  sessionIdFromTranscript,
  userConfigChain,
} from "./jj-attribution";

const ROOT_TRANSCRIPT =
  "/sessions/-legion-default/2026-08-30T03-47-40-329Z_01a050c7-b4a9-7000-a539-240e20271441.jsonl";
const ROOT_ID = "01a050c7-b4a9-7000-a539-240e20271441";

describe("sessionIdFromTranscript", () => {
  it("extracts the resumable id from a top-level transcript", () => {
    expect(sessionIdFromTranscript(ROOT_TRANSCRIPT)).toBe(ROOT_ID);
  });

  it("rejects subagent transcripts named after their agent", () => {
    expect(
      sessionIdFromTranscript("/sessions/-x/2026-08-30T03-47-40-329Z_abc/EnvProbe.jsonl")
    ).toBeUndefined();
  });

  it("rejects timestamped basenames whose id is not a UUID", () => {
    expect(
      sessionIdFromTranscript("/sessions/-x/2026-08-30T03-47-40-329Z_bad'id.jsonl")
    ).toBeUndefined();
  });
});

describe("exportJjSessionAttribution", () => {
  const dirs: string[] = [];
  async function stateDir(prefix = "jj-attribution-"): Promise<string> {
    const dir = await mkdtemp(path.join(tmpdir(), prefix));
    dirs.push(dir);
    return dir;
  }
  afterEach(async () => {
    await Promise.all(dirs.splice(0).map((dir) => rm(dir, { recursive: true, force: true })));
  });
  const sessionLine = `[templates]\ncommit_trailers = '"Omp-Session: ${ROOT_ID}"'\n`;
  /** A real directory to scope an overlay to, under a scratch directory the afterEach removes. */
  async function workspace(name = "ws"): Promise<string> {
    const dir = path.join(await stateDir("jj-attribution-ws-"), name);
    await mkdir(dir, { recursive: true });
    return await realpath(dir);
  }
  /** The overlay the writer composes: the unscoped session line, then the scope carrying both.
   * `inToml` is the workspace path as a TOML basic string's body, escapes included. */
  function scopedOverlay(inToml: string, issueKey: string): string {
    return (
      `${sessionLine}\n[[--scope]]\n--when.workspaces = ["${inToml}"]\n` +
      `[--scope.templates]\ncommit_trailers = '"Omp-Session: ${ROOT_ID}\\nDispatch: ${issueKey}"'\n`
    );
  }

  it("writes the trailer overlay and chains it after the resolved user config", async () => {
    const dir = await stateDir();
    const env: NodeJS.ProcessEnv = {};
    const chain = await exportJjSessionAttribution(ROOT_ID, dir, {
      env,
      resolveBase: async () => "/u/a.toml:/u/b.toml",
    });

    const overlay = path.join(dir, `omp-attribution-${ROOT_ID}.toml`);
    expect(chain).toBe(`/u/a.toml:/u/b.toml:${overlay}`);
    expect(env.JJ_CONFIG).toBe(chain);
    expect(await readFile(overlay, "utf8")).toBe(sessionLine);
  });

  it("scopes the Dispatch line to the pane's workspace for a key of the accepted shape", async () => {
    const dir = await stateDir();
    const ws = await workspace();
    const env: NodeJS.ProcessEnv = {};
    await exportJjSessionAttribution(ROOT_ID, dir, {
      issueKey: "ACME-7",
      workspace: ws,
      env,
      resolveBase: async () => "/u/base.toml",
    });
    expect(await readFile(path.join(dir, `omp-attribution-${ROOT_ID}.toml`), "utf8")).toBe(
      scopedOverlay(ws, "ACME-7")
    );
  });

  // Without a workspace there is nothing to scope the key to, and an unscoped key would stamp
  // this deployment's issue into every jj repository the pane's shell commits in.
  it("writes no scope for an accepted key with no workspace", async () => {
    const dir = await stateDir();
    const env: NodeJS.ProcessEnv = {};
    await exportJjSessionAttribution(ROOT_ID, dir, {
      issueKey: "ACME-7",
      env,
      resolveBase: async () => "/u/base.toml",
    });
    expect(await readFile(path.join(dir, `omp-attribution-${ROOT_ID}.toml`), "utf8")).toBe(
      sessionLine
    );
  });

  // jj resolves a workspace root before it matches a scope, so the link's own path would match
  // nothing — and say nothing about it.
  it("scopes to the real path when the workspace is reached through a symlink", async () => {
    const dir = await stateDir();
    const real = await workspace("real-ws");
    const link = path.join(path.dirname(real), "link-ws");
    await symlink(real, link);
    const env: NodeJS.ProcessEnv = {};
    await exportJjSessionAttribution(ROOT_ID, dir, {
      issueKey: "ACME-7",
      workspace: resolvedWorkspace(link),
      env,
      resolveBase: async () => "/u/base.toml",
    });
    expect(await readFile(path.join(dir, `omp-attribution-${ROOT_ID}.toml`), "utf8")).toBe(
      scopedOverlay(real, "ACME-7")
    );
  });

  it("writes no scope for a workspace path that does not exist", async () => {
    const dir = await stateDir();
    const env: NodeJS.ProcessEnv = {};
    await exportJjSessionAttribution(ROOT_ID, dir, {
      issueKey: "ACME-7",
      workspace: resolvedWorkspace(path.join(dir, "no-such-workspace")),
      env,
      resolveBase: async () => "/u/base.toml",
    });
    expect(await readFile(path.join(dir, `omp-attribution-${ROOT_ID}.toml`), "utf8")).toBe(
      sessionLine
    );
  });

  // The path goes into a TOML basic string inside an array; a raw quote would end that string
  // and a raw backslash would open an escape, leaving jj a file it refuses.
  it("escapes a workspace path holding a double quote and a backslash", async () => {
    const dir = await stateDir();
    const ws = await workspace('ws "quoted\\odd');
    const env: NodeJS.ProcessEnv = {};
    await exportJjSessionAttribution(ROOT_ID, dir, {
      issueKey: "ACME-7",
      workspace: ws,
      env,
      resolveBase: async () => "/u/base.toml",
    });
    expect(await readFile(path.join(dir, `omp-attribution-${ROOT_ID}.toml`), "utf8")).toBe(
      scopedOverlay(ws.replaceAll("\\", "\\\\").replaceAll('"', '\\"'), "ACME-7")
    );
  });

  // No escape of ours is worth guessing at here: a scope jj cannot parse breaks every `jj`
  // command in the pane, which is far worse than a missing trailer.
  it.each([
    ["a newline", "/ws/line\nbreak"],
    ["a tab", "/ws/tab\there"],
    ["a DEL", "/ws/del\u007fhere"],
  ])("writes no scope for a workspace path holding %s", async (_name, ws) => {
    const dir = await stateDir();
    const env: NodeJS.ProcessEnv = {};
    await exportJjSessionAttribution(ROOT_ID, dir, {
      issueKey: "ACME-7",
      workspace: ws,
      env,
      resolveBase: async () => "/u/base.toml",
    });
    expect(await readFile(path.join(dir, `omp-attribution-${ROOT_ID}.toml`), "utf8")).toBe(
      sessionLine
    );
  });

  // Containment as much as correctness: the value is interpolated into a jj template inside a
  // TOML literal, so anything but a whole key writes the session line alone.
  it.each([
    ["no key", undefined],
    ["an empty key", ""],
    ["a key with a prefix", "legion/ACME-7"],
    ["a key with a trailing space", "ACME-7 "],
    ["a value holding a single quote", "ACME-7'"],
    ["a value holding a backslash", "ACME\\-7"],
    ["a value holding a newline", "ACME-7\nDispatch: OTHER-1"],
    ["a project part over ten characters", "ABCDEFGHIJK-7"],
    ["a lowercase key", "acme-7"],
  ])("writes the session line alone for %s", async (_name, issueKey) => {
    const dir = await stateDir();
    const env: NodeJS.ProcessEnv = {};
    await exportJjSessionAttribution(ROOT_ID, dir, {
      issueKey,
      workspace: await workspace(),
      env,
      resolveBase: async () => "/u/base.toml",
    });
    expect(await readFile(path.join(dir, `omp-attribution-${ROOT_ID}.toml`), "utf8")).toBe(
      sessionLine
    );
  });

  it("carries a state directory holding a space and a single quote whole", async () => {
    const dir = path.join(await stateDir(), "state 'dir");
    const ws = await workspace();
    const env: NodeJS.ProcessEnv = {};
    const chain = await exportJjSessionAttribution(ROOT_ID, dir, {
      issueKey: "ACME-7",
      workspace: ws,
      env,
      resolveBase: async () => "/u/base.toml",
    });
    const overlay = path.join(dir, `omp-attribution-${ROOT_ID}.toml`);
    expect(chain).toBe(`/u/base.toml:${overlay}`);
    expect(await readFile(overlay, "utf8")).toBe(scopedOverlay(ws, "ACME-7"));
  });

  it("strips stale attribution overlays from an inherited JJ_CONFIG, keeping user layers", async () => {
    const dir = await stateDir();
    const env: NodeJS.ProcessEnv = {
      JJ_CONFIG: `/u/base.toml:/home/x/.cache/omp/jj/omp-attribution-${ROOT_ID.replace("b4a9", "dead")}.toml`,
    };
    await exportJjSessionAttribution(ROOT_ID, dir, {
      env,
      resolveBase: async () => {
        throw new Error("must not resolve");
      },
    });
    expect(env.JJ_CONFIG).toBe(`/u/base.toml:${path.join(dir, `omp-attribution-${ROOT_ID}.toml`)}`);
  });

  it("re-resolves the base when the inherited chain was only stale overlays", async () => {
    const dir = await stateDir();
    const env: NodeJS.ProcessEnv = { JJ_CONFIG: "/tmp/omp-attribution-old.toml" };
    await exportJjSessionAttribution(ROOT_ID, dir, {
      env,
      resolveBase: async () => "/u/base.toml",
    });
    expect(env.JJ_CONFIG).toBe(`/u/base.toml:${path.join(dir, `omp-attribution-${ROOT_ID}.toml`)}`);
  });

  it("returns the live chain and leaves the file alone when the overlay is already chained", async () => {
    const dir = await stateDir();
    const env: NodeJS.ProcessEnv = {};
    const first = await exportJjSessionAttribution(ROOT_ID, dir, {
      issueKey: "ACME-7",
      env,
      resolveBase: async () => "/u/base.toml",
    });
    const overlay = path.join(dir, `omp-attribution-${ROOT_ID}.toml`);
    const written = await readFile(overlay, "utf8");

    const again = await exportJjSessionAttribution(ROOT_ID, dir, {
      issueKey: "OTHER-9",
      env,
      resolveBase: async () => {
        throw new Error("must not resolve");
      },
    });
    expect(again).toBe(first);
    expect(env.JJ_CONFIG).toBe(first);
    expect(await readFile(overlay, "utf8")).toBe(written);
  });

  it("publishes the clean base and exports nothing when the overlay cannot be written", async () => {
    const dir = await stateDir();
    const blocked = path.join(dir, "not-a-directory");
    await Bun.write(blocked, "occupied");
    const env: NodeJS.ProcessEnv = { JJ_CONFIG: "/u/base.toml:/tmp/omp-attribution-old.toml" };
    const chain = await exportJjSessionAttribution(ROOT_ID, path.join(blocked, "nested"), {
      issueKey: "ACME-7",
      env,
    });
    expect(chain).toBeUndefined();
    expect(env.JJ_CONFIG).toBe("/u/base.toml"); // stale overlay gone, no new one
    expect(await readFile(blocked, "utf8")).toBe("occupied");
  });

  it("leaves the environment untouched when jj is unavailable", async () => {
    const dir = await stateDir();
    const env: NodeJS.ProcessEnv = {};
    expect(
      await exportJjSessionAttribution(ROOT_ID, dir, { env, resolveBase: async () => undefined })
    ).toBeUndefined();
    expect(env.JJ_CONFIG).toBeUndefined();
  });

  // The id reaches both the TOML literal and the overlay's file name, so an id that is not one
  // (`SESSION_ID_PATH_SEGMENT`) passes nothing on: a single quote would end the literal and break
  // every `jj` command in the pane, and a `/` would move the overlay out of the state directory.
  it.each([
    ["an empty session id", ""],
    ["a session id holding a single quote", "s'1"],
    ["a session id holding a slash", "../s-1"],
    ["a session id holding a backslash", "s\\1"],
    ["a session id over 128 characters", "s".repeat(129)],
  ])("leaves the environment untouched and writes nothing for %s", async (_name, sessionId) => {
    const dir = await stateDir();
    const env: NodeJS.ProcessEnv = {};
    expect(
      await exportJjSessionAttribution(sessionId, dir, {
        env,
        resolveBase: async () => "/u/base.toml",
      })
    ).toBeUndefined();
    expect(env.JJ_CONFIG).toBeUndefined();
    expect(await readdir(dir)).toEqual([]);
  });
});

describe("resolvedWorkspace", () => {
  const dirs: string[] = [];
  afterEach(async () => {
    await Promise.all(dirs.splice(0).map((dir) => rm(dir, { recursive: true, force: true })));
  });
  async function scratch(): Promise<string> {
    const dir = await mkdtemp(path.join(tmpdir(), "jj-attribution-resolve-"));
    dirs.push(dir);
    return await realpath(dir);
  }

  it("answers the real path of an existing directory", async () => {
    const dir = await scratch();
    expect(resolvedWorkspace(dir)).toBe(dir);
  });

  it("follows a symlink to the path jj would resolve the workspace to", async () => {
    const dir = await scratch();
    const real = path.join(dir, "real");
    await mkdir(real);
    const link = path.join(dir, "link");
    await symlink(real, link);
    expect(resolvedWorkspace(link)).toBe(real);
  });

  // A relative path matches no scope and says nothing about it, so the absolute one is written.
  it("answers an absolute path for a relative one", async () => {
    const dir = await scratch();
    const relative = path.relative(process.cwd(), dir);
    expect(path.isAbsolute(relative)).toBe(false);
    expect(resolvedWorkspace(relative)).toBe(dir);
  });

  it.each([
    ["an unset workspace", undefined],
    ["an empty workspace", ""],
  ])("answers undefined for %s", (_name, workspace) => {
    expect(resolvedWorkspace(workspace)).toBeUndefined();
  });

  it("answers undefined for a path that does not exist", async () => {
    expect(resolvedWorkspace(path.join(await scratch(), "gone"))).toBeUndefined();
  });
});

describe("userConfigChain", () => {
  const dirs: string[] = [];
  /**
   * A closed environment whose PATH finds a `jj` running `body` first: PATH and a home of the
   * test's own, so nothing the developer's environment holds reaches the stub.
   */
  async function withJj(body: string): Promise<NodeJS.ProcessEnv> {
    const dir = await mkdtemp(path.join(tmpdir(), "jj-attribution-bin-"));
    dirs.push(dir);
    await Bun.write(path.join(dir, "jj"), `#!/bin/sh\n${body}\n`);
    await chmod(path.join(dir, "jj"), 0o755);
    return { PATH: `${dir}:${process.env.PATH ?? ""}`, HOME: dir };
  }
  afterEach(async () => {
    await Promise.all(dirs.splice(0).map((dir) => rm(dir, { recursive: true, force: true })));
  });
  /** The wrapper's child pid, once it has written it. */
  async function pidFrom(pidFile: string): Promise<number> {
    const deadline = Date.now() + 3_000;
    while (Date.now() < deadline) {
      const pid = await readFile(pidFile, "utf8")
        .then((value) => Number(value.trim()))
        .catch(() => 0);
      if (Number.isInteger(pid) && pid > 0) return pid;
      await Bun.sleep(10);
    }
    throw new Error(`jj wrapper did not write ${pidFile}`);
  }

  // Real-time polling is required here: the wrapper's pidfile and the kernel reaping its child
  // are external OS state, which Bun's fake timers cannot advance.
  /** The kernel has reaped the child once signal 0 says it no longer exists. */
  async function stops(pid: number): Promise<boolean> {
    const deadline = Date.now() + 3_000;
    while (Date.now() < deadline) {
      try {
        process.kill(pid, 0);
      } catch {
        return true;
      }
      await Bun.sleep(10);
    }
    return false;
  }

  it("chains the user config files jj prints, in order", async () => {
    const env = await withJj("printf '/u/a.toml\\n/u/b.toml\\n'");
    expect(await userConfigChain(5_000, env)).toBe("/u/a.toml:/u/b.toml");
  });

  it("gives up on a jj that never answers, so attribution never holds bootstrap", async () => {
    const env = await withJj("exec sleep 30");
    const dir = await mkdtemp(path.join(tmpdir(), "jj-attribution-"));
    dirs.push(dir);
    const exported: NodeJS.ProcessEnv = {};
    const started = Date.now();
    await exportJjSessionAttribution(ROOT_ID, dir, {
      env: exported,
      resolveBase: () => userConfigChain(200, env),
    });
    expect(Date.now() - started).toBeLessThan(5_000);
    expect(exported.JJ_CONFIG).toBeUndefined();
  });

  it("kills a wrapper's forked child when jj times out", async () => {
    const pidFile = path.join(await mkdtemp(path.join(tmpdir(), "jj-attribution-child-")), "pid");
    dirs.push(path.dirname(pidFile));
    const env = await withJj(`sleep 30 & echo $! > ${JSON.stringify(pidFile)}; wait`);
    const started = Date.now();
    await userConfigChain(200, env);
    const pid = await pidFrom(pidFile);
    try {
      expect(Date.now() - started).toBeLessThan(5_000);
      expect(await stops(pid)).toBe(true);
    } finally {
      try {
        process.kill(pid, "SIGKILL");
      } catch {
        // The expected path: the process group killed it first.
      }
    }
  });

  it("kills a wrapper's detached child after jj returns a config path", async () => {
    const pidFile = path.join(await mkdtemp(path.join(tmpdir(), "jj-attribution-child-")), "pid");
    dirs.push(path.dirname(pidFile));
    const env = await withJj(
      `sleep 30 </dev/null >/dev/null 2>&1 & echo $! > ${JSON.stringify(pidFile)}; printf '/u/a.toml\\n'`
    );
    const paths = await userConfigChain(200, env);
    const pid = await pidFrom(pidFile);
    try {
      expect(paths).toBe("/u/a.toml");
      expect(await stops(pid)).toBe(true);
    } finally {
      try {
        process.kill(pid, "SIGKILL");
      } catch {
        // The expected path: the process group killed it first.
      }
    }
  });

  it("bounds jj at 10 s, inside the 30 s Oh My Pi gives the session_start handler", () => {
    expect(JJ_CONFIG_PATH_TIMEOUT_MS).toBe(10_000);
    expect(JJ_CONFIG_PATH_TIMEOUT_MS).toBeLessThan(30_000);
  });
});
