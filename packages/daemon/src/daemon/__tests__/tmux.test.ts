// Row selection in `lookupPane` over a `list-panes -F "#{pane_id} #{pane_pid}"` listing, and the
// three-way verdict it returns: the pane's own pid, the pane provably absent, or a listing that
// failed for some other reason and therefore proves nothing either way — plus the environment
// builders the boot-time server scrub runs over.
import { describe, expect, it } from "bun:test";
import {
  disableEnvironmentUpdates,
  environmentNames,
  lookupPane,
  openWindow,
  parseShellEnvironment,
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

describe("parseShellEnvironment", () => {
  it("names every set entry of a show-environment -s dump and skips the unset markers", () => {
    // Real tmux 3.7c output shapes: a dashed bash function whose body spans lines, npm's
    // `//host/:_authToken`, a name with a space, a name with a `"`, a name ending in `;`, an empty
    // value, a value ending in a backslash, and `unset NAME;` markers.
    const dump = [
      '//registry.example/:_authToken="npm_abc"; export //registry.example/:_authToken;',
      'A B="spaced"; export A B;',
      'BASH_FUNC_git-fixup%%="() {  git commit --fixup=HEAD',
      '}"; export BASH_FUNC_git-fixup%%;',
      'EMPTY=""; export EMPTY;',
      'HOME="/home/legion"; export HOME;',
      "unset MARKED;",
      'PATH="/full/bin:/usr/bin"; export PATH;',
      'Q"N="q"; export Q"N;',
      'TRAILBS="ends\\\\"; export TRAILBS;',
      'X;="semi"; export X;;',
      "",
    ].join("\n");
    expect(parseShellEnvironment(dump, undefined)).toEqual([
      "//registry.example/:_authToken",
      "A B",
      "BASH_FUNC_git-fixup%%",
      "EMPTY",
      "HOME",
      "PATH",
      'Q"N',
      "TRAILBS",
      "X;",
    ]);
  });

  it("scans a multi-line value as one entry, whatever its continuation lines contain", () => {
    // The reviewer's shapes: a PEM whose `=`-padded last base64 line looks like `NAME=`, a value
    // whose lines read `HOME;=x` and `key = value`, a function body with an escaped `"` and `$`,
    // a value containing `"`, `\`, a backtick and `$` (all backslash-escaped by tmux), and a value
    // that itself contains the text `"; export HOME;` — escaped, so it never closes the entry.
    const dump = [
      'BASH_FUNC_x%%="() {  echo \\"hi \\$1\\";',
      " local a=1",
      '}"; export BASH_FUNC_x%%;',
      'EVIL="a\\"; export HOME;',
      'b"; export EVIL;',
      'QUOTEY="say \\"hi\\" \\\\ back \\`tick\\` \\$dollar"; export QUOTEY;',
      'RAW_FAKE_PEM="-----BEGIN FAKE KEY-----',
      "QUJDREVGR0hJSktMTU5PUFFSU1RVVldYWVo=",
      "ZmFrZS1rZXktYnl0ZXMtbm90LXJlYWw=",
      "SE9NRTs9eA==",
      '-----END FAKE KEY-----"; export RAW_FAKE_PEM;',
      'TRICKY="l1',
      "HOME;=x",
      'key = value"; export TRICKY;',
      "",
    ].join("\n");
    expect(parseShellEnvironment(dump, undefined)).toEqual([
      "BASH_FUNC_x%%",
      "EVIL",
      "QUOTEY",
      "RAW_FAKE_PEM",
      "TRICKY",
    ]);
  });

  it("parses the C-locale rendering identically (embedded newlines vis-encoded as `_`)", () => {
    const dump =
      'RAW_FAKE_PEM="-----BEGIN FAKE KEY-----_ZmFrZS1rZXktYnl0ZXMtbm90LXJlYWw=_-----END FAKE KEY-----"; export RAW_FAKE_PEM;\nTRICKY="l1_HOME;=x_key = value"; export TRICKY;\n';
    expect(parseShellEnvironment(dump, undefined)).toEqual(["RAW_FAKE_PEM", "TRICKY"]);
  });

  it("is empty for a fresh session table (markers only) and for no output", () => {
    expect(
      parseShellEnvironment("unset DISPLAY;\nunset SSH_AUTH_SOCK;\n", { session: "s" })
    ).toEqual([]);
    expect(parseShellEnvironment("", undefined)).toEqual([]);
  });

  it("decides marker or entry by structure, so an entry named `unset X` is an entry and a broken marker swallows nothing", () => {
    // Not creatable by a shell (`export 'unset X'=1` is not a valid identifier) but settable via
    // set-environment or a crafted envp: the `=` on the line makes it an entry, name and all.
    expect(parseShellEnvironment('unset X="v"; export unset X;\n', undefined)).toEqual(["unset X"]);
    expect(
      parseShellEnvironment(
        'unset GONE;\nunset X="a;\nb"; export unset X;\nA="1"; export A;\n',
        undefined
      )
    ).toEqual(["unset X", "A"]);
    // A marker missing its `;` used to skip to the next `;\n`, swallowing the entry after it.
    const swallowing = 'unset GONE\nA="leaked-value"; export A;\n';
    let message = "";
    try {
      parseShellEnvironment(swallowing, undefined);
    } catch (error) {
      message = (error as Error).message;
    }
    expect(message).toBe(
      "tmux show-environment -s (global): malformed unset marker at byte 0, cannot read the table"
    );
    expect(message).not.toContain("leaked");
    expect(message).not.toContain("GONE");
    for (const dump of ["unset X;", "unset ;\n", "unset X\n"]) {
      expect(() => parseShellEnvironment(dump, { session: "legion-omp" })).toThrow(
        "tmux show-environment -s (session legion-omp): malformed unset marker at byte 0, cannot read the table"
      );
    }
  });

  it("fails loudly on any other shape, naming the table and the byte offset — never the text there", () => {
    const cases: Array<[string, string]> = [
      ['A="leaked-value', "unterminated value at byte 15"],
      ['A="x\\n"; export A;\n', "unknown escape in value at byte 4"],
      ['A="leaked"; export B;\n', 'expected `"; export NAME;` closing the entry at byte 9'],
      ["A=leaked-value; export A;\n", 'expected `"` after NAME= at byte 2'],
      ["leaked-value\n", "expected NAME= at byte 0"],
    ];
    for (const [dump, detail] of cases) {
      let message = "";
      try {
        parseShellEnvironment(dump, undefined);
      } catch (error) {
        message = (error as Error).message;
      }
      expect(message).toBe(`tmux show-environment -s (global): ${detail}, cannot read the table`);
      expect(message).not.toContain("leaked");
    }
  });
});

describe("environmentNames", () => {
  it("reads a table with show-environment -s over the private server and returns its exact names", async () => {
    const commands: string[][] = [];
    const fake = server(
      { stdout: 'FOO_SECRET="stale"; export FOO_SECRET;\n', exitCode: 0 },
      commands
    );
    expect(await environmentNames(fake, undefined)).toEqual(["FOO_SECRET"]);
    expect(await environmentNames(fake, { session: "legion-omp" })).toEqual(["FOO_SECRET"]);
    expect(commands).toEqual([
      ["tmux", "-L", "legion-omp", "show-environment", "-s", "-g"],
      ["tmux", "-L", "legion-omp", "show-environment", "-s", "-t", "legion-omp"],
    ]);
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
    expect(await environmentNames(gone, undefined)).toBeUndefined();
    const neverCreated: TmuxServer = {
      socket: "legion-omp",
      run: async () => ({
        stdout: "",
        stderr: "error connecting to /tmp/tmux-1000/legion-omp (No such file or directory)",
        exitCode: 1,
      }),
    };
    expect(await environmentNames(neverCreated, undefined)).toBeUndefined();
    const noSession: TmuxServer = {
      socket: "legion-omp",
      run: async () => ({ stdout: "", stderr: "no such session: legion-omp", exitCode: 1 }),
    };
    expect(await environmentNames(noSession, { session: "legion-omp" })).toBeUndefined();
  });

  it("throws on any other failure, carrying tmux's stderr and never its stdout (the value dump)", async () => {
    const broken: TmuxServer = {
      socket: "legion-omp",
      run: async () => ({
        stdout:
          'GH_AGENT_APP_PRIVATE_KEY_B64="leaked-value"; export GH_AGENT_APP_PRIVATE_KEY_B64;\n',
        stderr: "server version is too old",
        exitCode: 1,
      }),
    };
    await expect(environmentNames(broken, undefined)).rejects.toThrow(
      "tmux show-environment -s -g failed (exit 1): server version is too old"
    );
    await expect(environmentNames(broken, { session: "legion-omp" })).rejects.toThrow(
      "tmux show-environment -s -t legion-omp failed (exit 1): server version is too old"
    );
    const silent: TmuxServer = {
      socket: "legion-omp",
      run: async () => ({
        stdout:
          'GH_AGENT_APP_PRIVATE_KEY_B64="leaked-value"; export GH_AGENT_APP_PRIVATE_KEY_B64;\n',
        stderr: "",
        exitCode: 1,
      }),
    };
    let message = "";
    await environmentNames(silent, undefined).catch((error: Error) => {
      message = error.message;
    });
    expect(message).toBe(
      "tmux show-environment -s -g failed (exit 1): tmux printed nothing on stderr"
    );
    expect(message).not.toContain("leaked-value");
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

  it("escapes a trailing `;` on the name as `\\;`, so tmux does not strip it and collapse the name onto another entry", async () => {
    const commands: string[][] = [];
    await unsetEnvironment(server({ stdout: "", exitCode: 0 }, commands), undefined, "HOME;");
    expect(commands).toEqual([
      ["tmux", "-L", "legion-omp", "set-environment", "-g", "-u", "HOME\\;"],
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

  it("says so when tmux printed nothing on stderr", async () => {
    const fake: TmuxServer = {
      socket: "legion-omp",
      run: async (cmd) => {
        if (cmd[3] === "new-window") return { stdout: "", stderr: "", exitCode: 1 };
        return { stdout: "", stderr: "", exitCode: 0 };
      },
    };
    let message = "";
    await openWindow(fake, "legion-omp", "legsmoke-1", ["sleep 1"], "legion-omp").catch(
      (error: Error) => {
        message = error.message;
      }
    );
    expect(message).toBe("tmux new-window failed (exit 1): tmux printed nothing on stderr");
  });
});
