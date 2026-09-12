// Row selection in `panePid` over a `list-panes -F "#{pane_id} #{pane_pid}"` listing.
import { describe, expect, it } from "bun:test";
import { panePid, type TmuxServer } from "../tmux";

// One window, three panes: the architect's pane first, then two split-in workers.
const rows = "%1531 2363427\n%1533 3003090\n%1534 446716\n";

function server(
  reply: { stdout: string; exitCode: number },
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

describe("panePid", () => {
  it("returns the target pane's own pid from its window's listing, over the private server", async () => {
    const commands: string[][] = [];
    expect(await panePid(server({ stdout: rows, exitCode: 0 }, commands), "%1533")).toBe(3003090);
    expect(commands).toEqual([
      ["tmux", "-L", "legion-omp", "list-panes", "-t", "%1533", "-F", "#{pane_id} #{pane_pid}"],
    ]);
  });

  it("is undefined when tmux exits non-zero, whatever it printed", async () => {
    expect(await panePid(server({ stdout: rows, exitCode: 1 }), "%1533")).toBeUndefined();
  });

  it("is undefined for a pane id missing from the listing, never a sibling pane's pid", async () => {
    expect(await panePid(server({ stdout: rows, exitCode: 0 }), "%1535")).toBeUndefined();
  });

  it("matches the pane id exactly, never a longer or shorter id sharing its digits", async () => {
    // Longer ids first, so a prefix match in either direction would land on the wrong row.
    const prefixes = "%150 446716\n%1 3715931\n%15 4141285\n";
    expect(await panePid(server({ stdout: prefixes, exitCode: 0 }), "%15")).toBe(4141285);
    expect(await panePid(server({ stdout: prefixes, exitCode: 0 }), "%1")).toBe(3715931);
  });
});
