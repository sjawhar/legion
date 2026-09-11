// Unit tests for `panePid`'s row selection. `list-panes -t <target>` always lists the target's
// whole window (a pane id resolves to its window), so the pane-id column — not row position —
// must pick the row for a pane target; a window target keeps the first row (its first pane, the
// same pane `firstPaneId` backfills into a pane-id-less locator).
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

  it("returns the first pane's pid for a window id", async () => {
    expect(await panePid(server({ stdout: rows, exitCode: 0 }), "@1464")).toBe(2363427);
  });

  it("is undefined when tmux exits non-zero, whatever it printed", async () => {
    expect(await panePid(server({ stdout: rows, exitCode: 1 }), "%1533")).toBeUndefined();
  });

  it("is undefined for a pane id missing from the listing, never a sibling pane's pid", async () => {
    expect(await panePid(server({ stdout: rows, exitCode: 0 }), "%1535")).toBeUndefined();
  });
});
