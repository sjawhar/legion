// Row selection in `lookupPane` over a `list-panes -F "#{pane_id} #{pane_pid}"` listing, and the
// three-way verdict it returns: the pane's own pid, the pane provably absent, or a listing that
// failed for some other reason and therefore proves nothing either way.
import { describe, expect, it } from "bun:test";
import { lookupPane, type TmuxServer } from "../tmux";

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
