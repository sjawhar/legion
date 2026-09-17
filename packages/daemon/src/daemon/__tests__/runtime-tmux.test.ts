// `TmuxRuntime.scrubServerEnvironment`: what a boot against an already-running private server
// removes from that server's global and session environment tables before the first pane may open.
import { describe, expect, it } from "bun:test";
import { TmuxRuntime } from "../runtime-tmux";

interface Reply {
  stdout: string;
  stderr?: string;
  exitCode: number;
}

const OK: Reply = { stdout: "", stderr: "", exitCode: 0 };

/** A tmux environment table as `show-environment -s` prints it: set entries with their (already
 * escaped) values, and the names tmux marks unset for new panes. */
interface Table {
  entries: Map<string, string>;
  markers: string[];
}

function table(entries: Record<string, string>, markers: string[] = []): Table {
  return { entries: new Map(Object.entries(entries)), markers: [...markers] };
}

function shellDump(t: Table): string {
  const lines: string[] = [];
  const names = [...t.entries.keys(), ...t.markers].sort();
  for (const name of names) {
    const value = t.entries.get(name);
    lines.push(value === undefined ? `unset ${name};` : `${name}="${value}"; export ${name};`);
  }
  return lines.length === 0 ? "" : `${lines.join("\n")}\n`;
}

/** What tmux makes of a `set-environment -u <token>` argv token: a trailing `;` is its command
 * separator and is stripped, `\;` is the literal — so `HOME;` names `HOME` and `HOME\;` names
 * `HOME;`. */
function tmuxToken(token: string): string {
  if (token.endsWith("\\;")) return `${token.slice(0, -2)};`;
  if (token.endsWith(";")) return token.slice(0, -1);
  return token;
}

/** A fake server holding a mutable global and session table, answering `show-environment -s`
 * from them and applying `set-environment -u` to them exactly as tmux would (including the
 * re-tokenisation above); `set-option -t` answers like the session table does (a missing session
 * refuses both the same way). `options.unset` overrides the removal, for the invariant test. */
function runtimeOver(
  tables: { global: Table | Reply; session: Table | Reply },
  options: {
    unset?: (t: Table, name: string) => void;
    run?: (cmd: string[]) => Promise<Reply>;
  } = {}
) {
  const commands: string[][] = [];
  const isTable = (x: Table | Reply): x is Table => "entries" in x;
  const runtime = new TmuxRuntime({
    tmux: {
      socket: "legion-omp",
      run: async (cmd) => {
        commands.push(cmd);
        const which = cmd[4] === "-g" || cmd[5] === "-g" ? "global" : "session";
        const t = tables[which];
        if (cmd[3] === "show-environment") {
          if (!isTable(t)) return t;
          return { stdout: shellDump(t), stderr: "", exitCode: 0 };
        }
        if (cmd[3] === "set-environment") {
          if (!isTable(t)) return t;
          const name = tmuxToken(cmd.at(-1) ?? "");
          if (options.unset) options.unset(t, name);
          else t.entries.delete(name);
          return OK;
        }
        if (cmd[3] === "set-option") return isTable(tables.session) ? OK : tables.session;
        return OK;
      },
    },
    project: "omp",
    stateDir: "/unused",
    ompInvocation: "omp",
    ompLaunchPrefix: [],
    provisioningToken: async () => "token",
    run:
      options.run ??
      (async () => {
        throw new Error("not exercised by this test");
      }),
    repo: "acme/widgets",
    repoForIssue: (issue) =>
      issue.startsWith("AGENTC-") ? "trajectory-labs-pbc/agent-c" : "acme/widgets",
    credentialHelper: "!legion credential",
    slowCommandTimeoutMs: 1000,
    connectWorkerRpc: async () => {
      throw new Error("not exercised by this test");
    },
    workerRpcTimeoutMs: () => 5_000,
    now: () => 0,
    issueLocators: () => [],
  });
  return { runtime, commands };
}

const subcommands = (commands: string[][]) => commands.map((cmd) => cmd.slice(3));
const writes = (commands: string[][]) =>
  subcommands(commands).filter((cmd) => cmd[0] === "set-environment" || cmd[0] === "set-option");
const reads = (commands: string[][]) =>
  subcommands(commands).filter((cmd) => cmd[0] === "show-environment");

const FRESH_SESSION_MARKERS = [
  "DISPLAY",
  "KRB5CCNAME",
  "SSH_AGENT_PID",
  "SSH_ASKPASS",
  "SSH_AUTH_SOCK",
  "SSH_CONNECTION",
  "XAUTHORITY",
];

describe("TmuxRuntime.scrubServerEnvironment", () => {
  it("unsets every name the pane environment does not carry from both tables, and only those", async () => {
    const global = table(
      {
        GH_AGENT_APP_PRIVATE_KEY_B64: "leaked-agent-key",
        HOME: "/home/legion",
        PATH: "/full/bin:/usr/bin",
        DISPATCH_TOKEN: "leaked-bearer",
        PWD: "/home/legion",
        SHLVL: "0",
      },
      ["GH_REVIEW_APP_PRIVATE_KEY_B64"]
    );
    // The operator attached once: tmux's default update-environment copied the client's SSH
    // agent socket and connection in; HOME arrived through some earlier `set-environment -t`.
    const session = table(
      {
        HOME: "/home/legion",
        SSH_AUTH_SOCK: "/tmp/ssh-x/agent.1",
        SSH_CONNECTION: "100.100.92.97 57158 100.113.243.90 22",
      },
      ["DISPLAY", "XAUTHORITY"]
    );
    const { runtime, commands } = runtimeOver({ global, session });

    const removed = await runtime.scrubServerEnvironment({
      PATH: "/full/bin:/usr/bin",
      HOME: "/home/legion",
    });

    // Sorted and de-duplicated across both tables, so the boot log line is stable; the markers
    // are already unset and `PWD`/`SHLVL` are tmux's own.
    expect(removed).toEqual([
      "DISPATCH_TOKEN",
      "GH_AGENT_APP_PRIVATE_KEY_B64",
      "SSH_AUTH_SOCK",
      "SSH_CONNECTION",
    ]);
    expect(writes(commands)).toEqual([
      ["set-environment", "-g", "-u", "DISPATCH_TOKEN"],
      ["set-environment", "-g", "-u", "GH_AGENT_APP_PRIVATE_KEY_B64"],
      ["set-option", "-t", "legion-omp", "update-environment", ""],
      ["set-environment", "-t", "legion-omp", "-u", "SSH_AUTH_SOCK"],
      ["set-environment", "-t", "legion-omp", "-u", "SSH_CONNECTION"],
    ]);
    // Two reads per table: the listing, then the post-scrub verification. Never a per-name probe.
    expect(reads(commands)).toEqual([
      ["show-environment", "-s", "-g"],
      ["show-environment", "-s", "-g"],
      ["show-environment", "-s", "-t", "legion-omp"],
      ["show-environment", "-s", "-t", "legion-omp"],
    ]);
    expect([...global.entries.keys()].sort()).toEqual(["HOME", "PATH", "PWD", "SHLVL"]);
    expect([...session.entries.keys()]).toEqual(["HOME"]);
  });

  it("never reads a multi-line value's continuation line as a name: HOME;=x, key = value, and a PEM body", async () => {
    // The reviewer's three shapes, in the escaped form `show-environment -s` prints (a value's own
    // newlines stay in place). `HOME;=x` would, as a candidate, re-tokenise onto the real HOME;
    // `key = value` would probe as `key ` and throw; the PEM's padded line would be a phantom.
    const global = table({
      PATH: "/full/bin",
      HOME: "/home/legion",
      FOO_SECRET: "stale",
      TRICKY: "l1\nHOME;=x\nkey = value",
      RAW_FAKE_PEM:
        "-----BEGIN FAKE KEY-----\nQUJDREVGR0hJSktMTU5PUFFSU1RVVldYWVo=\nZmFrZS1rZXktYnl0ZXMtbm90LXJlYWw=\nSE9NRTs9eA==\n-----END FAKE KEY-----",
      "BASH_FUNC_git-fixup%%": "() {  git commit --fixup=HEAD\n}",
    });
    const { runtime, commands } = runtimeOver({
      global,
      session: table({}, FRESH_SESSION_MARKERS),
    });

    const removed = await runtime.scrubServerEnvironment({
      PATH: "/full/bin",
      HOME: "/home/legion",
    });

    expect(removed).toEqual(["BASH_FUNC_git-fixup%%", "FOO_SECRET", "RAW_FAKE_PEM", "TRICKY"]);
    const line = removed.join(", ");
    for (const fragment of ["HOME;", "key", "ZmFr", "QUJD", "SE9N"])
      expect(line).not.toContain(fragment);
    // tmux prints the table sorted by name, so the -u's follow that order.
    expect(writes(commands).filter((cmd) => cmd[0] === "set-environment")).toEqual([
      ["set-environment", "-g", "-u", "BASH_FUNC_git-fixup%%"],
      ["set-environment", "-g", "-u", "FOO_SECRET"],
      ["set-environment", "-g", "-u", "RAW_FAKE_PEM"],
      ["set-environment", "-g", "-u", "TRICKY"],
    ]);
    // HOME survived: the invariant check would have thrown otherwise, and the table says so.
    expect(global.entries.has("HOME")).toBe(true);
    expect([...global.entries.keys()].sort()).toEqual(["HOME", "PATH"]);
  });

  it("unsets a real entry whose name ends in `;` under its own name, never the entry before it", async () => {
    const global = table({ PATH: "/full/bin", HOME: "/home/legion", "HOME;": "semi" });
    const { runtime, commands } = runtimeOver({
      global,
      session: table({}, FRESH_SESSION_MARKERS),
    });

    expect(
      await runtime.scrubServerEnvironment({ PATH: "/full/bin", HOME: "/home/legion" })
    ).toEqual(["HOME;"]);
    expect(writes(commands)[0]).toEqual(["set-environment", "-g", "-u", "HOME\\;"]);
    expect([...global.entries.keys()].sort()).toEqual(["HOME", "PATH"]);
  });

  it("refuses the boot, naming only names, when an unset silently removed an allow-listed entry", async () => {
    // A tmux whose `-u HOME;` lands on HOME (the reviewer's re-tokenisation) — here simulated by a
    // server that removes HOME whenever anything is unset.
    const global = table({ PATH: "/full/bin", HOME: "/home/legion", FOO_SECRET: "leaked-value" });
    const { runtime } = runtimeOver(
      { global, session: table({}, FRESH_SESSION_MARKERS) },
      {
        unset: (t, name) => {
          t.entries.delete(name);
          t.entries.delete("HOME");
        },
      }
    );
    let message = "";
    await runtime
      .scrubServerEnvironment({ PATH: "/full/bin", HOME: "/home/legion" })
      .catch((error: Error) => {
        message = error.message;
      });
    expect(message).toBe(
      "tmux global environment scrub did not land as intended; allow-listed name(s) now missing: HOME"
    );
    expect(message).not.toContain("leaked-value");
  });

  it("refuses the boot when a removed name is still present after the scrub", async () => {
    const global = table({ PATH: "/full/bin", FOO_SECRET: "leaked-value" });
    const { runtime } = runtimeOver(
      { global, session: table({}, FRESH_SESSION_MARKERS) },
      { unset: () => {} }
    );
    await expect(runtime.scrubServerEnvironment({ PATH: "/full/bin" })).rejects.toThrow(
      "tmux global environment scrub did not land as intended; removed name(s) still present: FOO_SECRET"
    );
  });

  it("empties update-environment before it reads the session table, so an attach landing in between cannot slip a copy in behind the read", async () => {
    const { runtime, commands } = runtimeOver({
      global: table({ PATH: "/full/bin" }),
      session: table({ SSH_AUTH_SOCK: "/tmp/ssh-x/agent.1" }),
    });

    await runtime.scrubServerEnvironment({ PATH: "/full/bin" });

    const sequence = subcommands(commands);
    const disabledAt = sequence.findIndex((cmd) => cmd[0] === "set-option");
    const sessionReadAt = sequence.findIndex(
      (cmd) => cmd[0] === "show-environment" && cmd[2] === "-t"
    );
    expect(disabledAt).toBeGreaterThanOrEqual(0);
    expect(sessionReadAt).toBeGreaterThan(disabledAt);
  });

  it("unsets a table entry named like an Object.prototype member, exactly like any other", async () => {
    // `constructor`, `toString`, `hasOwnProperty`, `__proto__`: a prototype-chain lookup on the
    // exemption table would find these truthy and leave them in the server.
    // A Map, not an object literal: `__proto__` as a literal key would be swallowed by JS itself.
    const global: Table = {
      entries: new Map([
        ["PATH", "/full/bin"],
        ["constructor", "stale"],
        ["toString", "stale"],
        ["hasOwnProperty", "stale"],
        ["__proto__", "stale"],
        ["valueOf", "stale"],
      ]),
      markers: [],
    };
    const { runtime } = runtimeOver({ global, session: table({}, FRESH_SESSION_MARKERS) });

    expect(await runtime.scrubServerEnvironment({ PATH: "/full/bin" })).toEqual([
      "__proto__",
      "constructor",
      "hasOwnProperty",
      "toString",
      "valueOf",
    ]);
    expect([...global.entries.keys()]).toEqual(["PATH"]);
  });

  it("only empties update-environment on a server this daemon forked itself, whose tables hold nothing to remove", async () => {
    const { runtime, commands } = runtimeOver({
      global: table({
        HOME: "/home/legion",
        PATH: "/full/bin:/usr/bin",
        PWD: "/home/legion",
        SHLVL: "0",
      }),
      session: table({}, FRESH_SESSION_MARKERS),
    });

    const removed = await runtime.scrubServerEnvironment({
      PATH: "/full/bin:/usr/bin",
      HOME: "/home/legion",
    });

    expect(removed).toEqual([]);
    expect(writes(commands)).toEqual([
      ["set-option", "-t", "legion-omp", "update-environment", ""],
    ]);
    // Nothing was removed, so nothing to verify: one read per table.
    expect(reads(commands)).toHaveLength(2);
  });

  it("touches nothing when no server is running yet", async () => {
    const gone: Reply = {
      stdout: "",
      stderr: "no server running on /tmp/tmux-1000/legion-omp",
      exitCode: 1,
    };
    const { runtime, commands } = runtimeOver({ global: gone, session: gone });

    expect(await runtime.scrubServerEnvironment({ PATH: "/full/bin" })).toEqual([]);
    expect(writes(commands)).toEqual([]);
  });

  it("scrubs the global table and skips only the session step when the server is up without the daemon's session", async () => {
    const { runtime, commands } = runtimeOver({
      global: table({ PATH: "/full/bin", FOO_SECRET: "stale" }),
      session: { stdout: "", stderr: "no such session: legion-omp", exitCode: 1 },
    });

    expect(await runtime.scrubServerEnvironment({ PATH: "/full/bin" })).toEqual(["FOO_SECRET"]);
    expect(writes(commands)).toEqual([
      ["set-environment", "-g", "-u", "FOO_SECRET"],
      // The refused set-option is what told us the session is absent; nothing else was tried.
      ["set-option", "-t", "legion-omp", "update-environment", ""],
    ]);
    expect(reads(commands).some((cmd) => cmd[2] === "-t")).toBe(false);
  });
});

describe("TmuxRuntime project repositories", () => {
  it("uses the issue project repository when adopting its working copy", async () => {
    const workspaceCommands: string[][] = [];
    const { runtime } = runtimeOver(
      { global: table({}), session: table({}) },
      {
        run: async (command) => {
          workspaceCommands.push(command);
          return OK;
        },
      }
    );

    await runtime.adoptWorkingCopy(
      "AGENTC-9",
      "implementer",
      { jjUser: "legion-implement[bot]", jjEmail: "42+legion-implement[bot]@users.noreply.github.com" },
      1_000
    );

    expect(workspaceCommands[0]).toContain(
      "/unused/workspaces/trajectory-labs-pbc/agent-c/agentc-9"
    );
  });
});
