import { describe, expect, it } from "bun:test";
import { type ExecFn, parseGitHubRemoteUrl, resolveCwdRepo, resolveOrigin } from "../dispatch-cwd";

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
});

describe("resolveOrigin", () => {
  it("reports host omp from OMP_SESSION_ID", async () => {
    const { exec } = fakeExec({});
    const origin = await resolveOrigin({ OMP_SESSION_ID: "abc" }, exec, "/repo");
    expect(origin.host).toBe("omp");
    expect(origin.cwd).toBe("/repo");
  });

  it("reports host omp from OMPCODE", async () => {
    const { exec } = fakeExec({});
    const origin = await resolveOrigin({ OMPCODE: "1" }, exec, "/repo");
    expect(origin.host).toBe("omp");
  });

  it("reports host omp when both OMP_SESSION_ID and CLAUDECODE are set", async () => {
    // OMP sessions on this machine also carry CLAUDECODE; the more specific
    // host marker must win or every OMP thread is misattributed to Claude.
    const { exec } = fakeExec({});
    const origin = await resolveOrigin({ OMP_SESSION_ID: "abc", CLAUDECODE: "1" }, exec, "/repo");
    expect(origin.host).toBe("omp");
  });

  it("reports host claude from CLAUDECODE", async () => {
    const { exec } = fakeExec({});
    const origin = await resolveOrigin({ CLAUDECODE: "1" }, exec, "/repo");
    expect(origin.host).toBe("claude");
  });

  it("does not infer a host from OPENCODE_* variables (shell profiles export them)", async () => {
    const { exec } = fakeExec({});
    const origin = await resolveOrigin({ OPENCODE_GITHUB_REPO: "o/r" }, exec, "/repo");
    expect(origin.host).toBeUndefined();
  });

  it("omits host when no recognized host env is set", async () => {
    const { exec } = fakeExec({});
    const origin = await resolveOrigin({}, exec, "/repo");
    expect(origin.host).toBeUndefined();
  });

  it("includes the tmux pane target when TMUX_PANE is set", async () => {
    const { exec, calls } = fakeExec({
      "tmux display-message -p -t %3 #S:#I.#P #{pane_id}": "main:3.0 %3\n",
    });
    const origin = await resolveOrigin({ TMUX_PANE: "%3" }, exec, "/repo");
    expect(origin.tmux).toBe("main:3.0");
    expect(origin.pane).toBe("%3");
    expect(calls).toEqual(["tmux display-message -p -t %3 #S:#I.#P #{pane_id}"]);
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
});
