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

/** The real entry names in a `show-environment` dump, the way tmux itself would answer
 * `show-environment <table> <name>`: a `-NAME` marker line is an entry too; a value's continuation
 * line is not, however much it looks like one. */
function entriesOf(dump: string): Set<string> {
  const names = new Set<string>();
  let inValue = false;
  for (const line of dump.split("\n")) {
    if (line === "") continue;
    if (inValue) {
      // A continuation line: the value continues until a line that closes it. The fixtures here
      // mark the end of a multi-line value with `-----END`; a bash function body ends with `}`.
      if (line.startsWith("-----END") || line === "}") inValue = false;
      continue;
    }
    const eq = line.indexOf("=");
    if (line.startsWith("-") && eq === -1) {
      names.add(line.slice(1));
      continue;
    }
    if (eq === -1) continue;
    names.add(line.slice(0, eq));
    const value = line.slice(eq + 1);
    if (value.startsWith("-----BEGIN") || value.endsWith("{")) inValue = true;
  }
  return names;
}

/** A fake server whose global and session tables answer `show-environment` (the whole dump, or one
 * name's presence exactly like tmux 3.7c: exit 0 when the name is an entry, exit 1 with
 * `unknown variable: <name>` otherwise); `set-option -t` answers like the session table does (a
 * missing session refuses both the same way). */
function runtimeOver(tables: { global: Reply; session: Reply }) {
  const commands: string[][] = [];
  const entries = {
    global: entriesOf(tables.global.stdout),
    session: entriesOf(tables.session.stdout),
  };
  const runtime = new TmuxRuntime({
    tmux: {
      socket: "legion-omp",
      run: async (cmd) => {
        commands.push(cmd);
        const table = cmd[4] === "-g" ? "global" : "session";
        const reply = tables[table];
        if (cmd[3] === "show-environment") {
          const name = table === "global" ? cmd[5] : cmd[6];
          if (name === undefined) return reply;
          if (reply.exitCode !== 0) return reply;
          return entries[table].has(name)
            ? { stdout: `${name}=<never read>`, stderr: "", exitCode: 0 }
            : { stdout: "", stderr: `unknown variable: ${name}`, exitCode: 1 };
        }
        if (cmd[3] === "set-option") return tables.session.exitCode === 0 ? OK : tables.session;
        return OK;
      },
    },
    project: "omp",
    stateDir: "/unused",
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
const probes = (commands: string[][]) =>
  subcommands(commands).filter(
    (cmd) => cmd[0] === "show-environment" && (cmd[1] === "-g" ? cmd[2] : cmd[3]) !== undefined
  );

const FRESH_SESSION_TABLE =
  "-DISPLAY\n-KRB5CCNAME\n-SSH_AGENT_PID\n-SSH_ASKPASS\n-SSH_AUTH_SOCK\n-SSH_CONNECTION\n-XAUTHORITY\n";

describe("TmuxRuntime.scrubServerEnvironment", () => {
  it("unsets every name the pane environment does not carry from both tables, and only those", async () => {
    const { runtime, commands } = runtimeOver({
      global: {
        stdout: [
          "GH_AGENT_APP_PRIVATE_KEY_B64=leaked-agent-key",
          "HOME=/home/legion",
          "PATH=/full/bin:/usr/bin",
          "DISPATCH_TOKEN=leaked-bearer",
          "-GH_REVIEW_APP_PRIVATE_KEY_B64",
          "PWD=/home/legion",
          "SHLVL=0",
          "",
        ].join("\n"),
        exitCode: 0,
      },
      // The operator attached once: tmux's default update-environment copied the client's SSH
      // agent socket and connection in; HOME arrived through some earlier `set-environment -t`.
      session: {
        stdout:
          "-DISPLAY\nHOME=/home/legion\nSSH_AUTH_SOCK=/tmp/ssh-x/agent.1\nSSH_CONNECTION=100.100.92.97 57158 100.113.243.90 22\n-XAUTHORITY\n",
        exitCode: 0,
      },
    });

    const removed = await runtime.scrubServerEnvironment({
      PATH: "/full/bin:/usr/bin",
      HOME: "/home/legion",
    });

    // Sorted and de-duplicated across both tables, so the boot log line is stable;
    // `-GH_REVIEW_…`/`-DISPLAY` are already unset and `PWD`/`SHLVL` are tmux's own.
    expect(removed).toEqual([
      "DISPATCH_TOKEN",
      "GH_AGENT_APP_PRIVATE_KEY_B64",
      "SSH_AUTH_SOCK",
      "SSH_CONNECTION",
    ]);
    expect(writes(commands)).toEqual([
      ["set-environment", "-g", "-u", "GH_AGENT_APP_PRIVATE_KEY_B64"],
      ["set-environment", "-g", "-u", "DISPATCH_TOKEN"],
      ["set-option", "-t", "legion-omp", "update-environment", ""],
      ["set-environment", "-t", "legion-omp", "-u", "SSH_AUTH_SOCK"],
      ["set-environment", "-t", "legion-omp", "-u", "SSH_CONNECTION"],
    ]);
    // Each candidate outside paneEnv was confirmed against its table before the -u; allow-listed
    // and tmux-owned names were never probed.
    expect(probes(commands)).toEqual([
      ["show-environment", "-g", "GH_AGENT_APP_PRIVATE_KEY_B64"],
      ["show-environment", "-g", "DISPATCH_TOKEN"],
      ["show-environment", "-t", "legion-omp", "SSH_AUTH_SOCK"],
      ["show-environment", "-t", "legion-omp", "SSH_CONNECTION"],
    ]);
  });

  it("never treats a continuation line of a multi-line value as a name: no phantom -u, no fragment in the result, exact count", async () => {
    // The reviewer's reproduction: a raw PEM exported into the daemon's launcher environment. Its
    // header and footer start with `-`; its `=`-padded last base64 line looks exactly like
    // `NAME=` in the dump. tmux answers `unknown variable` for it, so it is neither unset nor
    // logged, and the real entry is removed under its own name.
    const { runtime, commands } = runtimeOver({
      global: {
        stdout: [
          "PATH=/full/bin",
          "FOO_SECRET=stale",
          "RAW_FAKE_PEM=-----BEGIN FAKE KEY-----",
          "QUJDREVGR0hJSktMTU5PUFFSU1RVVldYWVo=",
          "ZmFrZS1rZXktYnl0ZXMtbm90LXJlYWw=",
          "-----END FAKE KEY-----",
          "BASH_FUNC_git-fixup%%=() {  git commit --fixup=HEAD",
          "}",
          "",
        ].join("\n"),
        exitCode: 0,
      },
      session: { stdout: FRESH_SESSION_TABLE, exitCode: 0 },
    });

    const removed = await runtime.scrubServerEnvironment({ PATH: "/full/bin" });

    expect(removed).toEqual(["BASH_FUNC_git-fixup%%", "FOO_SECRET", "RAW_FAKE_PEM"]);
    expect(removed.join(" ")).not.toContain("ZmFr");
    expect(removed.join(" ")).not.toContain("QUJD");
    expect(writes(commands).filter((cmd) => cmd[0] === "set-environment")).toEqual([
      ["set-environment", "-g", "-u", "FOO_SECRET"],
      ["set-environment", "-g", "-u", "RAW_FAKE_PEM"],
      ["set-environment", "-g", "-u", "BASH_FUNC_git-fixup%%"],
    ]);
    // The two fragments were probed (they cannot be told apart from an entry in the dump) and
    // answered `unknown variable`; that is where they stopped.
    expect(probes(commands).map((cmd) => cmd.at(-1))).toEqual([
      "FOO_SECRET",
      "RAW_FAKE_PEM",
      "QUJDREVGR0hJSktMTU5PUFFSU1RVVldYWVo",
      "ZmFrZS1rZXktYnl0ZXMtbm90LXJlYWw",
      "BASH_FUNC_git-fixup%%",
    ]);
  });

  it("empties update-environment before it reads the session table, so an attach landing in between cannot slip a copy in behind the read", async () => {
    const { runtime, commands } = runtimeOver({
      global: { stdout: "PATH=/full/bin\n", exitCode: 0 },
      session: { stdout: "SSH_AUTH_SOCK=/tmp/ssh-x/agent.1\n", exitCode: 0 },
    });

    await runtime.scrubServerEnvironment({ PATH: "/full/bin" });

    const sequence = subcommands(commands);
    const disabledAt = sequence.findIndex((cmd) => cmd[0] === "set-option");
    const sessionReadAt = sequence.findIndex(
      (cmd) => cmd[0] === "show-environment" && cmd[1] === "-t" && cmd[3] === undefined
    );
    expect(disabledAt).toBeGreaterThanOrEqual(0);
    expect(sessionReadAt).toBeGreaterThan(disabledAt);
  });

  it("unsets a table entry named like an Object.prototype member, exactly like any other", async () => {
    // `constructor`, `toString`, `hasOwnProperty`, `__proto__`: a prototype-chain lookup on the
    // exemption table would find these truthy and leave them in the server.
    const { runtime, commands } = runtimeOver({
      global: {
        stdout:
          "PATH=/full/bin\nconstructor=stale\ntoString=stale\nhasOwnProperty=stale\n__proto__=stale\nvalueOf=stale\n",
        exitCode: 0,
      },
      session: { stdout: FRESH_SESSION_TABLE, exitCode: 0 },
    });

    expect(await runtime.scrubServerEnvironment({ PATH: "/full/bin" })).toEqual([
      "__proto__",
      "constructor",
      "hasOwnProperty",
      "toString",
      "valueOf",
    ]);
    expect(writes(commands).filter((cmd) => cmd[1] === "-g")).toHaveLength(5);
  });

  it("only empties update-environment on a server this daemon forked itself, whose tables hold nothing to remove", async () => {
    const { runtime, commands } = runtimeOver({
      global: {
        stdout: "HOME=/home/legion\nPATH=/full/bin:/usr/bin\nPWD=/home/legion\nSHLVL=0\n",
        exitCode: 0,
      },
      session: { stdout: FRESH_SESSION_TABLE, exitCode: 0 },
    });

    const removed = await runtime.scrubServerEnvironment({
      PATH: "/full/bin:/usr/bin",
      HOME: "/home/legion",
    });

    expect(removed).toEqual([]);
    expect(writes(commands)).toEqual([
      ["set-option", "-t", "legion-omp", "update-environment", ""],
    ]);
    expect(probes(commands)).toEqual([]);
  });

  it("touches nothing when no server is running yet", async () => {
    const gone = {
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
      global: { stdout: "PATH=/full/bin\nFOO_SECRET=stale\n", exitCode: 0 },
      session: { stdout: "", stderr: "no such session: legion-omp", exitCode: 1 },
    });

    expect(await runtime.scrubServerEnvironment({ PATH: "/full/bin" })).toEqual(["FOO_SECRET"]);
    expect(writes(commands)).toEqual([
      ["set-environment", "-g", "-u", "FOO_SECRET"],
      // The refused set-option is what told us the session is absent; nothing else was tried.
      ["set-option", "-t", "legion-omp", "update-environment", ""],
    ]);
    expect(
      subcommands(commands).some((cmd) => cmd[0] === "show-environment" && cmd[1] === "-t")
    ).toBe(false);
  });
});
