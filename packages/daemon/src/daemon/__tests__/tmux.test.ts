// Row selection in `lookupPane` over a `list-panes -F "#{pane_id} #{pane_pid}"` listing, and the
// three-way verdict it returns: the pane's own pid, the pane provably absent, or a listing that
// failed for some other reason and therefore proves nothing either way — plus the environment
// builders the boot-time server scrub runs over.
import { describe, expect, it } from "bun:test";
import {
  disableEnvironmentUpdates,
  environmentCandidates,
  environmentHas,
  lookupPane,
  openWindow,
  type TmuxServer,
  unsetEnvironment,
} from "../tmux";

// One window, three panes: the architect's pane first, then two split-in workers.
const rows = "%1531 2363427\n%1533 3003090\n%1534 446716\n";

function server(
  reply: { stdout: string; stderr?: string; exitCode: number },
  commands: string[][] = []
): TmuxServer {
  return {
    socket: "legion-omp",
    run: async (cmd) => {
      commands.push(cmd);
      return reply;
    },
  };
}

describe("lookupPane", () => {
  it("returns the target pane's own pid from its window's listing, over the private server", async () => {
    const commands: string[][] = [];
    expect(await lookupPane(server({ stdout: rows, exitCode: 0 }, commands), "%1533")).toEqual({
      status: "present",
      pid: 3003090,
    });
    expect(commands).toEqual([
      ["tmux", "-L", "legion-omp", "list-panes", "-t", "%1533", "-F", "#{pane_id} #{pane_pid}"],
    ]);
  });

  it("reports a pane id missing from a successful listing as absent, never a sibling pane's pid", async () => {
    expect(await lookupPane(server({ stdout: rows, exitCode: 0 }), "%1535")).toEqual({
      status: "absent",
    });
  });

  it.each([
    ["can't find pane: %1533"],
    ["no server running on /tmp/tmux-1000/legion-omp"],
    ["error connecting to /tmp/tmux-1000/legion-omp (No such file or directory)"],
  ])("reports a nonzero exit whose stderr says the pane or server is not there as absent: %s", async (stderr) => {
    expect(await lookupPane(server({ stdout: "", stderr, exitCode: 1 }), "%1533")).toEqual({
      status: "absent",
    });
  });

  it("reports any other nonzero exit as a failed listing that proves nothing, naming the exit and stderr", async () => {
    expect(
      await lookupPane(
        server({ stdout: "", stderr: "server not responding", exitCode: 1 }),
        "%1533"
      )
    ).toEqual({
      status: "failed",
      detail: "list-panes -t %1533 exited 1: server not responding",
    });
    // A client killed by the runner's own timeout: nonzero exit, nothing on stderr.
    expect(await lookupPane(server({ stdout: rows, exitCode: 143 }), "%1533")).toEqual({
      status: "failed",
      detail: "list-panes -t %1533 exited 143",
    });
  });

  it("reports a row whose pid is not a pid as a failed listing, never as absent", async () => {
    expect(await lookupPane(server({ stdout: "%1533 nope\n", exitCode: 0 }), "%1533")).toEqual({
      status: "failed",
      detail: "list-panes -t %1533 reported an unparseable pid for %1533: %1533 nope",
    });
  });

  it("matches the pane id exactly, never a longer or shorter id sharing its digits", async () => {
    // Longer ids first, so a prefix match in either direction would land on the wrong row.
    const prefixes = "%150 446716\n%1 3715931\n%15 4141285\n";
    expect(await lookupPane(server({ stdout: prefixes, exitCode: 0 }), "%15")).toEqual({
      status: "present",
      pid: 4141285,
    });
    expect(await lookupPane(server({ stdout: prefixes, exitCode: 0 }), "%1")).toEqual({
      status: "present",
      pid: 3715931,
    });
  });
});

describe("environmentCandidates", () => {
  it("lists each global entry's name over the private server, skipping unset markers and value continuation lines", async () => {
    // `BASH_FUNC__aws%%` is a bash exported function: its body continues over lines that start
    // with a space or `}` and may themselves contain `=` — never a new entry. A name is everything
    // up to the first `=`, whatever it contains: a dashed function, npm's `//host/:_authToken`.
    const stdout = [
      "-DISPATCH_TOKEN",
      'BASH_FUNC__aws%%=() {  local use_tty="";',
      ' [ -t 0 ] && use_tty="-it";',
      ' "$AWSCLI_DOCKER_BIN" run --rm $use_tty --env AWS_CLI_AUTO_PROMPT=$AWS_CLI_AUTO_PROMPT',
      "}",
      "BASH_FUNC_git-fixup%%=() {  git commit --fixup=HEAD",
      "}",
      "GH_AGENT_APP_PRIVATE_KEY_B64=abc",
      "HOME=/h",
      "PATH=/x:/y",
      "//registry.npmjs.org/:_authToken=npm_abc",
      "npm_config_user_agent=bun/1.3",
      "some.dotted:name=1",
      "",
    ].join("\n");
    const commands: string[][] = [];
    expect(
      await environmentCandidates(server({ stdout, exitCode: 0 }, commands), undefined)
    ).toEqual([
      "BASH_FUNC__aws%%",
      "BASH_FUNC_git-fixup%%",
      "GH_AGENT_APP_PRIVATE_KEY_B64",
      "HOME",
      "PATH",
      "//registry.npmjs.org/:_authToken",
      "npm_config_user_agent",
      "some.dotted:name",
    ]);
    expect(commands).toEqual([["tmux", "-L", "legion-omp", "show-environment", "-g"]]);
  });

  it("reads a session's table with -t, where a fresh session holds only update-environment's unset markers and an attach fills in real entries", async () => {
    // tmux 3.7: a new session's table is the `-NAME` marker for every default `update-environment`
    // name; a client attach then sets the ones it carries.
    const fresh =
      "-DISPLAY\n-KRB5CCNAME\n-MSYSTEM\n-SSH_AGENT_PID\n-SSH_ASKPASS\n-SSH_AUTH_SOCK\n-SSH_CONNECTION\n-WAYLAND_DISPLAY\n-WINDOWID\n-XAUTHORITY\n-XDG_CURRENT_DESKTOP\n-XDG_SESSION_DESKTOP\n-XDG_SESSION_TYPE\n";
    const commands: string[][] = [];
    expect(
      await environmentCandidates(server({ stdout: fresh, exitCode: 0 }, commands), {
        session: "legion-omp",
      })
    ).toEqual([]);
    expect(commands).toEqual([
      ["tmux", "-L", "legion-omp", "show-environment", "-t", "legion-omp"],
    ]);

    const attached =
      "-DISPLAY\nSSH_ASKPASS=/usr/bin/false\nSSH_AUTH_SOCK=/tmp/ssh-x/agent.1\nSSH_CONNECTION=100.100.92.97 57158 100.113.243.90 22\n-WAYLAND_DISPLAY\n";
    expect(
      await environmentCandidates(server({ stdout: attached, exitCode: 0 }), {
        session: "legion-omp",
      })
    ).toEqual(["SSH_ASKPASS", "SSH_AUTH_SOCK", "SSH_CONNECTION"]);
  });

  it("is undefined when no server is behind the socket (a first boot, or the server exited), or the session does not exist", async () => {
    const gone: TmuxServer = {
      socket: "legion-omp",
      run: async () => ({
        stdout: "",
        stderr: "no server running on /tmp/tmux-1000/legion-omp",
        exitCode: 1,
      }),
    };
    expect(await environmentCandidates(gone, undefined)).toBeUndefined();
    const neverCreated: TmuxServer = {
      socket: "legion-omp",
      run: async () => ({
        stdout: "",
        stderr: "error connecting to /tmp/tmux-1000/legion-omp (No such file or directory)",
        exitCode: 1,
      }),
    };
    expect(await environmentCandidates(neverCreated, undefined)).toBeUndefined();
    const noSession: TmuxServer = {
      socket: "legion-omp",
      run: async () => ({ stdout: "", stderr: "no such session: legion-omp", exitCode: 1 }),
    };
    expect(await environmentCandidates(noSession, { session: "legion-omp" })).toBeUndefined();
  });

  it("throws on any other failure, carrying tmux's stderr and never its stdout (the value dump)", async () => {
    const broken: TmuxServer = {
      socket: "legion-omp",
      run: async () => ({
        stdout: "GH_AGENT_APP_PRIVATE_KEY_B64=leaked-value\n",
        stderr: "server version is too old",
        exitCode: 1,
      }),
    };
    await expect(environmentCandidates(broken, undefined)).rejects.toThrow(
      "tmux show-environment -g failed (exit 1): server version is too old"
    );
    await expect(environmentCandidates(broken, { session: "legion-omp" })).rejects.toThrow(
      "tmux show-environment -t legion-omp failed (exit 1): server version is too old"
    );
    const silent: TmuxServer = {
      socket: "legion-omp",
      run: async () => ({
        stdout: "GH_AGENT_APP_PRIVATE_KEY_B64=leaked-value\n",
        stderr: "",
        exitCode: 1,
      }),
    };
    let message = "";
    await environmentCandidates(silent, undefined).catch((error: Error) => {
      message = error.message;
    });
    expect(message).toBe("tmux show-environment -g failed (exit 1)");
    expect(message).not.toContain("leaked-value");
  });
});

describe("environmentHas", () => {
  it("confirms a candidate against the table with show-environment <table> <name>, reading only the exit code", async () => {
    const commands: string[][] = [];
    // stdout is the value: present or not, it is never read.
    const present = server({ stdout: "FOO_SECRET=leaked-value\n", exitCode: 0 }, commands);
    expect(await environmentHas(present, undefined, "FOO_SECRET")).toBe(true);
    expect(await environmentHas(present, { session: "legion-omp" }, "SSH_AUTH_SOCK")).toBe(true);
    expect(commands).toEqual([
      ["tmux", "-L", "legion-omp", "show-environment", "-g", "FOO_SECRET"],
      ["tmux", "-L", "legion-omp", "show-environment", "-t", "legion-omp", "SSH_AUTH_SOCK"],
    ]);
  });

  it("classifies `unknown variable: <name>` as absent — a value fragment the dump parser mistook for a name", async () => {
    const absent: TmuxServer = {
      socket: "legion-omp",
      run: async (cmd) => ({
        stdout: "",
        stderr: `unknown variable: ${cmd.at(-1)}`,
        exitCode: 1,
      }),
    };
    expect(await environmentHas(absent, undefined, "ZmFrZS1rZXktYnl0ZXMtbm90LXJlYWw")).toBe(false);
    expect(await environmentHas(absent, { session: "legion-omp" }, "QUJD")).toBe(false);
  });

  it("throws on any other refusal, carrying stderr only", async () => {
    const broken: TmuxServer = {
      socket: "legion-omp",
      run: async () => ({
        stdout: "FOO=leaked-value\n",
        stderr: "server version is too old",
        exitCode: 1,
      }),
    };
    let message = "";
    await environmentHas(broken, undefined, "FOO").catch((error: Error) => {
      message = error.message;
    });
    expect(message).toBe("tmux show-environment -g FOO failed (exit 1): server version is too old");
    expect(message).not.toContain("leaked-value");
    // `unknown variable` for a *different* name is not this name's absence.
    const other: TmuxServer = {
      socket: "legion-omp",
      run: async () => ({ stdout: "", stderr: "unknown variable: BAR", exitCode: 1 }),
    };
    await expect(environmentHas(other, undefined, "FOO")).rejects.toThrow(
      "tmux show-environment -g FOO failed (exit 1): unknown variable: BAR"
    );
  });
});

describe("unsetEnvironment", () => {
  it("removes the name from the global or the session table with -u", async () => {
    const commands: string[][] = [];
    const fake = server({ stdout: "", exitCode: 0 }, commands);
    await unsetEnvironment(fake, undefined, "FOO_SECRET");
    await unsetEnvironment(fake, { session: "legion-omp" }, "SSH_AUTH_SOCK");
    expect(commands).toEqual([
      ["tmux", "-L", "legion-omp", "set-environment", "-g", "-u", "FOO_SECRET"],
      ["tmux", "-L", "legion-omp", "set-environment", "-t", "legion-omp", "-u", "SSH_AUTH_SOCK"],
    ]);
  });

  it("throws when tmux refuses", async () => {
    const refusing: TmuxServer = {
      socket: "legion-omp",
      run: async () => ({ stdout: "", stderr: "bad environment", exitCode: 1 }),
    };
    await expect(unsetEnvironment(refusing, undefined, "FOO_SECRET")).rejects.toThrow(
      "tmux set-environment -g -u FOO_SECRET failed (exit 1): bad environment"
    );
  });
});

describe("disableEnvironmentUpdates", () => {
  it("empties the session's update-environment option and reports the session as present", async () => {
    const commands: string[][] = [];
    expect(
      await disableEnvironmentUpdates(server({ stdout: "", exitCode: 0 }, commands), "legion-omp")
    ).toBe(true);
    expect(commands).toEqual([
      ["tmux", "-L", "legion-omp", "set-option", "-t", "legion-omp", "update-environment", ""],
    ]);
  });

  it("reports false, having changed nothing, when no server or no such session is there", async () => {
    const noSession: TmuxServer = {
      socket: "legion-omp",
      run: async () => ({ stdout: "", stderr: "no such session: legion-omp", exitCode: 1 }),
    };
    expect(await disableEnvironmentUpdates(noSession, "legion-omp")).toBe(false);
    const noServer: TmuxServer = {
      socket: "legion-omp",
      run: async () => ({
        stdout: "",
        stderr: "no server running on /tmp/tmux-1000/legion-omp",
        exitCode: 1,
      }),
    };
    expect(await disableEnvironmentUpdates(noServer, "legion-omp")).toBe(false);
  });

  it("throws on any other failure", async () => {
    const refusing: TmuxServer = {
      socket: "legion-omp",
      run: async () => ({ stdout: "", stderr: "bad option", exitCode: 1 }),
    };
    await expect(disableEnvironmentUpdates(refusing, "legion-omp")).rejects.toThrow(
      "tmux set-option -t legion-omp update-environment '' failed (exit 1): bad option"
    );
  });
});

describe("openWindow", () => {
  it("creates the session and empties its update-environment in one tmux invocation, so nothing can attach in between", async () => {
    const commands: string[][] = [];
    const fake: TmuxServer = {
      socket: "legion-omp",
      run: async (cmd) => {
        commands.push(cmd);
        if (cmd[3] === "has-session") return { stdout: "", stderr: "", exitCode: 1 };
        if (cmd[3] === "new-window") return { stdout: "@42 %1 4242\n", stderr: "", exitCode: 0 };
        return { stdout: "", stderr: "", exitCode: 0 };
      },
    };
    await openWindow(fake, "legion-omp", "legsmoke-1", ["sleep 1"], "legion-omp");
    // `;` as its own argv element is tmux's command separator (a shell would spell it `\;`).
    expect(commands[1]).toEqual([
      "tmux",
      "-L",
      "legion-omp",
      "new-session",
      "-d",
      "-s",
      "legion-omp",
      "-n",
      "__legion_bootstrap",
      "sleep 3600",
      ";",
      "set-option",
      "-t",
      "legion-omp",
      "update-environment",
      "",
    ]);
    expect(
      commands.filter((cmd) => cmd[3] === "set-option" && cmd[6] === "update-environment")
    ).toEqual([]);
  });

  it("leaves an existing session's options alone", async () => {
    const commands: string[][] = [];
    const fake: TmuxServer = {
      socket: "legion-omp",
      run: async (cmd) => {
        commands.push(cmd);
        if (cmd[3] === "new-window") return { stdout: "@42 %1 4242\n", stderr: "", exitCode: 0 };
        return { stdout: "", stderr: "", exitCode: 0 };
      },
    };
    await openWindow(fake, "legion-omp", "legsmoke-1", ["sleep 1"], "legion-omp");
    expect(commands.some((cmd) => cmd[3] === "set-option" && cmd[6] === "update-environment")).toBe(
      false
    );
  });

  it("names the command and carries stderr only when the -P -F report is malformed — never the report itself", async () => {
    const fake: TmuxServer = {
      socket: "legion-omp",
      run: async (cmd) => {
        if (cmd[3] === "new-window") {
          return { stdout: "GH_TOKEN=leaked-value\n", stderr: "unexpected output", exitCode: 0 };
        }
        return { stdout: "", stderr: "", exitCode: 0 };
      },
    };
    let message = "";
    await openWindow(fake, "legion-omp", "legsmoke-1", ["sleep 1"], "legion-omp").catch(
      (error: Error) => {
        message = error.message;
      }
    );
    expect(message).toBe("tmux new-window did not report a window id: unexpected output");
    expect(message).not.toContain("leaked-value");
  });
});
