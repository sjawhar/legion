import { afterEach, describe, expect, it, jest } from "bun:test";
import { execFileSync } from "node:child_process";
import { mkdtempSync, rmSync } from "node:fs";
import { readFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { canonicalRepo } from "@legion/contracts/repo";
import {
  defaultExec,
  type ExecFn,
  parseGitHubRemoteUrl,
  resolveCwdRepo,
  resolveOrigin,
} from "../dispatch-cwd";

const TMUX_CALL = "tmux display-message -p -t %3 #S:#I.#P #{pane_id}";

// The bounds the changelogs promise (dispatch-cwd.ts's TMUX_PANE_LOOKUP_MS and REMOTE_LOOKUP_MS).
const PANE_BOUND_MS = 1_000;
const REMOTE_BOUND_MS = 5_000;

// The bounded tests drive time with fake timers and check that a lookup has settled before they
// await it: Bun's test timeout does not fire while fake timers are on, so awaiting a lookup that
// never settles would hang the run instead of failing it.
afterEach(() => {
  jest.useRealTimers();
});

/** Run every job already queued: a settled race reaches its caller's `.finally` within these. */
async function settle(): Promise<void> {
  for (let turn = 0; turn < 50; turn += 1) await Promise.resolve();
}

/** A tmux stand-in, run under fake timers, that answers after `answerAfterMs` or never (null).
 *  It never heeds its abort signal, as a tmux client waiting to connect to a busy server catches
 *  the SIGTERM a plain execFile timeout sends and keeps waiting. Calls are recorded, not asserted
 *  here, since the lookup swallows whatever its exec throws. */
function slowTmux(answerAfterMs: number | null) {
  const seen: { calls: string[]; signal?: AbortSignal; answer?: Promise<{ stdout: string }> } = {
    calls: [],
  };
  const exec: ExecFn = (file, args, options) => {
    seen.calls.push([file, ...args].join(" "));
    seen.signal = options.signal;
    const answer = Promise.withResolvers<{ stdout: string }>();
    seen.answer = answer.promise;
    if (answerAfterMs !== null) {
      setTimeout(() => answer.resolve({ stdout: "main:3.0 %3\n" }), answerAfterMs);
    }
    return answer.promise;
  };
  return { exec, seen };
}

/** Route each call by `${file} ${args.join(" ")}` to a canned stdout or a thrown error. */
function fakeExec(script: Record<string, string>): { exec: ExecFn; calls: string[] } {
  const calls: string[] = [];
  const exec: ExecFn = async (file, args) => {
    const key = [file, ...args].join(" ");
    calls.push(key);
    if (!(key in script)) throw new Error(`unscripted command: ${key}`);
    return { stdout: script[key] };
  };
  return { exec, calls };
}

describe("parseGitHubRemoteUrl", () => {
  it("parses an https remote, with and without .git and a trailing slash", () => {
    expect(parseGitHubRemoteUrl("https://github.com/acme/widgets")).toBe("acme/widgets");
    expect(parseGitHubRemoteUrl("https://github.com/acme/widgets.git")).toBe("acme/widgets");
    expect(parseGitHubRemoteUrl("https://github.com/acme/widgets/")).toBe("acme/widgets");
  });

  it("parses an scp-like ssh remote, with and without .git", () => {
    expect(parseGitHubRemoteUrl("git@github.com:acme/widgets.git")).toBe("acme/widgets");
    expect(parseGitHubRemoteUrl("git@github.com:acme/widgets")).toBe("acme/widgets");
  });

  it("parses an ssh:// remote, with and without .git", () => {
    expect(parseGitHubRemoteUrl("ssh://git@github.com/acme/widgets.git")).toBe("acme/widgets");
    expect(parseGitHubRemoteUrl("ssh://git@github.com/acme/widgets")).toBe("acme/widgets");
  });

  it("parses an https remote carrying userinfo ahead of the host", () => {
    expect(parseGitHubRemoteUrl("https://sjawhar@github.com/acme/widgets.git")).toBe(
      "acme/widgets"
    );
    expect(parseGitHubRemoteUrl("https://x-access-token:ghp_abc@github.com/acme/widgets")).toBe(
      "acme/widgets"
    );
  });

  it("matches the GitHub host case-insensitively", () => {
    expect(parseGitHubRemoteUrl("https://GitHub.com/acme/widgets")).toBe("acme/widgets");
    expect(parseGitHubRemoteUrl("git@GITHUB.COM:acme/widgets.git")).toBe("acme/widgets");
  });

  it("canonicalizes repository owner and name", () => {
    expect(parseGitHubRemoteUrl("https://github.com/Owner/Repo.git")).toBe("owner/repo");
  });

  it("trims repository components", () => {
    expect(canonicalRepo(" Owner ", " Repo.git ")).toBe("owner/repo");
  });

  it("returns null for a non-GitHub host", () => {
    expect(parseGitHubRemoteUrl("https://gitlab.com/acme/widgets.git")).toBeNull();
    expect(parseGitHubRemoteUrl("git@gitlab.com:acme/widgets.git")).toBeNull();
  });

  it("returns null for garbage input", () => {
    expect(parseGitHubRemoteUrl("not a url")).toBeNull();
    expect(parseGitHubRemoteUrl("")).toBeNull();
  });
});

describe("resolveCwdRepo", () => {
  it("prefers origin over upstream from jj git remote list", async () => {
    const { exec } = fakeExec({
      "jj git remote list":
        "origin https://github.com/acme/widgets.git\nupstream https://github.com/other/widgets.git",
    });
    expect(await resolveCwdRepo("/repo", exec)).toBe("acme/widgets");
  });

  it("falls back to the sole remote when there is no origin", async () => {
    const { exec } = fakeExec({
      "jj git remote list": "fork https://github.com/acme/widgets.git",
    });
    expect(await resolveCwdRepo("/repo", exec)).toBe("acme/widgets");
  });

  it("never selects upstream alone, even as the sole remote", async () => {
    const { exec } = fakeExec({
      "jj git remote list": "upstream https://github.com/acme/widgets.git",
    });
    expect(await resolveCwdRepo("/repo", exec)).toBeNull();
  });

  it("gives up when there is no origin and more than one candidate remote", async () => {
    const { exec } = fakeExec({
      "jj git remote list":
        "fork-a https://github.com/acme/widgets.git\nfork-b https://github.com/other/widgets.git",
    });
    expect(await resolveCwdRepo("/repo", exec)).toBeNull();
  });

  it("falls back to git remote get-url origin when jj fails", async () => {
    const { exec, calls } = fakeExec({
      "git remote get-url origin": "https://github.com/acme/widgets.git\n",
    });
    expect(await resolveCwdRepo("/repo", exec)).toBe("acme/widgets");
    expect(calls).toEqual(["jj git remote list", "git remote get-url origin"]);
  });

  it("returns null when neither jj nor git resolve a remote", async () => {
    const { exec } = fakeExec({});
    expect(await resolveCwdRepo("/repo", exec)).toBeNull();
  });

  it("does not fall back to git when jj succeeds with a non-GitHub origin", async () => {
    const { exec, calls } = fakeExec({
      "jj git remote list": "origin https://gitlab.com/acme/widgets.git",
      "git remote get-url origin": "https://github.com/should-not/be-used.git",
    });
    expect(await resolveCwdRepo("/repo", exec)).toBeNull();
    expect(calls).toEqual(["jj git remote list"]);
  });

  it("falls back to git once jj has not answered within 5 s, aborting jj and not git", async () => {
    jest.useFakeTimers();
    const calls: string[] = [];
    const signals = new Map<string, AbortSignal>();
    const exec: ExecFn = (file, args, options) => {
      calls.push([file, ...args].join(" "));
      signals.set(file, options.signal);
      return file === "jj"
        ? Promise.withResolvers<{ stdout: string }>().promise
        : Promise.resolve({ stdout: "https://github.com/acme/widgets\n" });
    };
    let settled = false;
    const pending = resolveCwdRepo("/repo", exec).finally(() => {
      settled = true;
    });
    jest.advanceTimersByTime(REMOTE_BOUND_MS - 1);
    await settle();
    expect({ settled, aborted: signals.get("jj")?.aborted }).toEqual({
      settled: false,
      aborted: false,
    });
    jest.advanceTimersByTime(1);
    await settle();
    expect({ settled, aborted: signals.get("jj")?.aborted }).toEqual({
      settled: true,
      aborted: true,
    });
    expect(await pending).toBe("acme/widgets");
    expect(calls).toEqual(["jj git remote list", "git remote get-url origin"]);
    expect(signals.get("git")?.aborted).toBe(false);
  });

  it("gives up on a git that has not answered within 5 s after jj failed", async () => {
    jest.useFakeTimers();
    let gitSignal: AbortSignal | undefined;
    const exec: ExecFn = (file, _args, options) => {
      if (file === "jj") return Promise.reject(new Error("not a jj repository"));
      gitSignal = options.signal;
      return Promise.withResolvers<{ stdout: string }>().promise;
    };
    let settled = false;
    const pending = resolveCwdRepo("/repo", exec).finally(() => {
      settled = true;
    });
    await settle();
    jest.advanceTimersByTime(REMOTE_BOUND_MS - 1);
    await settle();
    expect({ settled, aborted: gitSignal?.aborted }).toEqual({ settled: false, aborted: false });
    jest.advanceTimersByTime(1);
    await settle();
    expect({ settled, aborted: gitSignal?.aborted }).toEqual({ settled: true, aborted: true });
    expect(await pending).toBeNull();
  });
});

describe("resolveOrigin", () => {
  it("never sets host: the plugin asserts it", async () => {
    const { exec } = fakeExec({});
    const origin = await resolveOrigin(
      { OMP_SESSION_ID: "abc", OMPCODE: "1", CLAUDECODE: "1" },
      exec,
      "/repo"
    );
    expect(origin.host).toBeUndefined();
    expect(origin.cwd).toBe("/repo");
    expect(typeof origin.machine).toBe("string");
  });

  it("never sets session identity: the plugin supplies it from the host", async () => {
    const { exec } = fakeExec({});
    const origin = await resolveOrigin({ OMP_SESSION_ID: "abc" }, exec, "/repo");
    expect(origin.sessionId).toBeUndefined();
    expect(origin.sessionTitle).toBeUndefined();
  });

  it("includes the tmux pane target when TMUX_PANE is set", async () => {
    const { exec, calls } = fakeExec({ [TMUX_CALL]: "main:3.0 %3\n" });
    const origin = await resolveOrigin({ TMUX_PANE: "%3" }, exec, "/repo");
    expect(origin.tmux).toBe("main:3.0");
    expect(origin.pane).toBe("%3");
    expect(calls).toEqual([TMUX_CALL]);
  });

  it("omits tmux when TMUX_PANE is unset", async () => {
    const { exec, calls } = fakeExec({});
    const origin = await resolveOrigin({}, exec, "/repo");
    expect(origin.tmux).toBeUndefined();
    expect(calls).toEqual([]);
  });

  it("omits tmux when the tmux call fails", async () => {
    const { exec } = fakeExec({});
    const origin = await resolveOrigin({ TMUX_PANE: "%3" }, exec, "/repo");
    expect(origin.tmux).toBeUndefined();
  });

  it("gives up on a tmux that never answers once 1 s has passed, and aborts it", async () => {
    jest.useFakeTimers();
    const { exec, seen } = slowTmux(null);
    let settled = false;
    const pending = resolveOrigin({ TMUX_PANE: "%3" }, exec, "/repo").finally(() => {
      settled = true;
    });
    jest.advanceTimersByTime(PANE_BOUND_MS - 1);
    await settle();
    expect({ settled, aborted: seen.signal?.aborted }).toEqual({ settled: false, aborted: false });
    jest.advanceTimersByTime(1);
    await settle();
    expect({ settled, aborted: seen.signal?.aborted }).toEqual({ settled: true, aborted: true });
    const origin = await pending;
    expect(origin.cwd).toBe("/repo");
    expect(origin.tmux).toBeUndefined();
    expect(origin.pane).toBeUndefined();
    expect(seen.calls).toEqual([TMUX_CALL]);
  });

  it("ignores a tmux answer that lands after 1 s, though it lands before the lookup is awaited", async () => {
    jest.useFakeTimers();
    const { exec, seen } = slowTmux(PANE_BOUND_MS + 500);
    let settled = false;
    const pending = resolveOrigin({ TMUX_PANE: "%3" }, exec, "/repo").finally(() => {
      settled = true;
    });
    // One advance of the clock fires both the bound and the late answer before the test awaits.
    jest.advanceTimersByTime(PANE_BOUND_MS + 500);
    expect(await seen.answer).toEqual({ stdout: "main:3.0 %3\n" });
    await settle();
    expect({ settled, aborted: seen.signal?.aborted }).toEqual({ settled: true, aborted: true });
    const origin = await pending;
    expect(origin.tmux).toBeUndefined();
    expect(origin.pane).toBeUndefined();
    expect(seen.calls).toEqual([TMUX_CALL]);
  });

  it("records the pane when tmux answers inside 1 s, and never aborts it", async () => {
    jest.useFakeTimers();
    const { exec, seen } = slowTmux(PANE_BOUND_MS - 1);
    let settled = false;
    const pending = resolveOrigin({ TMUX_PANE: "%3" }, exec, "/repo").finally(() => {
      settled = true;
    });
    jest.advanceTimersByTime(PANE_BOUND_MS - 1);
    await settle();
    expect(settled).toBe(true);
    jest.advanceTimersByTime(PANE_BOUND_MS);
    expect(seen.signal?.aborted).toBe(false);
    const origin = await pending;
    expect(origin.tmux).toBe("main:3.0");
    expect(origin.pane).toBe("%3");
    expect(seen.calls).toEqual([TMUX_CALL]);
  });
});

describe("defaultExec", () => {
  it("returns a child's stdout", async () => {
    const call = defaultExec("bash", ["-c", "echo ok"], {
      cwd: tmpdir(),
      signal: new AbortController().signal,
    });
    expect(await call).toMatchObject({ stdout: "ok\n" });
  });

  it("kills a child that ignores SIGTERM when its signal aborts", async () => {
    const dir = mkdtempSync(join(tmpdir(), "dispatch-cwd-"));
    try {
      // The child writes `ready` once its trap is set and holds `alive` open until it dies, so
      // reading `alive` to its end waits for exactly its death, with no clock in the test.
      const ready = join(dir, "ready");
      const alive = join(dir, "alive");
      execFileSync("mkfifo", [ready, alive]);
      const died = readFile(alive, "utf8");
      const controller = new AbortController();
      const call = defaultExec(
        "bash",
        ["-c", `trap '' TERM; exec 3>"$1"; echo ready > "$0"; exec sleep 30`, ready, alive],
        { cwd: dir, signal: controller.signal }
      );
      expect(await readFile(ready, "utf8")).toBe("ready\n");
      controller.abort();
      // The call rejects as soon as it is aborted, before the child is gone; `died` is the proof.
      await expect(call).rejects.toThrow();
      expect(await died).toBe("");
    } finally {
      rmSync(dir, { recursive: true, force: true });
    }
  });
});
