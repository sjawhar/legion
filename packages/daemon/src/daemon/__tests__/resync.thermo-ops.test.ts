import { expect, it, vi } from "bun:test";
import { formatIssueKey } from "@legion/contracts";
import { newLegionState } from "../legion-state";
import { runResync } from "../resync";

const issue = formatIssueKey("sjawhar", "legion", 42);

it("warns once with the board-project configuration key when an open board item cannot heal", async () => {
  const state = newLegionState("omp", 1);
  let now = Date.parse("2026-08-26T00:00:00.000Z");
  const error = vi.spyOn(console, "error").mockImplementation(() => {});
  const deps = {
    state,
    config: {
      resyncIntervalMs: 600_000,
      boardProjectIds: [],
      appLogins: [],
      maxFixAttempts: 3,
    },
    fetchGitHubProjectItems: async () => ({
      items: [
        {
          content: {
            type: "Issue",
            number: 42,
            title: "Unhealable board issue",
            repository: "sjawhar/legion",
          },
          status: "Todo",
          labels: [],
        },
      ],
    }),
    applyEffects: async () => {},
    now: () => now,
  };

  try {
    const first = await runResync(deps);
    now += 600_000;
    await runResync(deps);

    expect(first.anomalies).toEqual([
      {
        kind: "missed-open",
        issue,
        detail: "open board issue is absent from Legion state",
      },
    ]);
    expect(error).toHaveBeenCalledTimes(1);
    expect(error).toHaveBeenCalledWith(expect.stringContaining("LEGION_BOARD_PROJECT_IDS"));
  } finally {
    error.mockRestore();
  }
});
