/**
 * Tests for state CLI entry point.
 *
 * The CLI reads issue JSON from stdin, accepts --team-id and --daemon-url args,
 * calls fetchAllIssueData then buildCollectedState, and outputs JSON to stdout.
 */

import { describe, expect, it } from "bun:test";
import { parseArgs, runPipeline } from "../cli";
import type { CommandRunner } from "../fetch";
import type { LinearIssueRaw } from "../types";

// =============================================================================
// Arg Parsing
// =============================================================================

describe("parseArgs", () => {
  it("parses --team-id and --daemon-url", () => {
    const args = parseArgs([
      "--team-id",
      "00000000-0000-0000-0000-000000000000",
      "--daemon-url",
      "http://localhost:3000",
    ]);
    expect(args.teamId).toBe("00000000-0000-0000-0000-000000000000");
    expect(args.daemonUrl).toBe("http://localhost:3000");
  });

  it("throws on missing --team-id", () => {
    expect(() => parseArgs(["--daemon-url", "http://localhost:3000"])).toThrow("--team-id");
  });

  it("throws on missing --daemon-url", () => {
    expect(() => parseArgs(["--team-id", "00000000-0000-0000-0000-000000000000"])).toThrow(
      "--daemon-url"
    );
  });

  it("throws on empty args", () => {
    expect(() => parseArgs([])).toThrow();
  });
});

// =============================================================================
// Pipeline (stdin → process → stdout)
// =============================================================================

describe("runPipeline", () => {
  it("processes issues and returns JSON state", async () => {
    const issues: LinearIssueRaw[] = [
      {
        identifier: "ENG-21",
        state: { name: "Todo" },
        labels: { nodes: [] },
      },
    ];

    // Mock fetch for getLiveWorkers
    const originalFetch = globalThis.fetch;
    globalThis.fetch = (async () =>
      new Response(JSON.stringify([]), { status: 200 })) as unknown as typeof fetch;

    try {
      const result = await runPipeline(
        issues,
        "00000000-0000-0000-0000-000000000000",
        "http://localhost:3000"
      );

      const parsed = JSON.parse(result);
      expect(parsed.issues).toBeDefined();
      expect(parsed.issues["ENG-21"]).toBeDefined();
      expect(parsed.issues["ENG-21"].suggestedAction).toBe("dispatch_planner");
    } finally {
      globalThis.fetch = originalFetch;
    }
  });

  it("handles empty issue list", async () => {
    const originalFetch = globalThis.fetch;
    globalThis.fetch = (async () =>
      new Response(JSON.stringify([]), { status: 200 })) as unknown as typeof fetch;

    try {
      const result = await runPipeline(
        [],
        "00000000-0000-0000-0000-000000000000",
        "http://localhost:3000"
      );

      const parsed = JSON.parse(result);
      expect(parsed.issues).toEqual({});
    } finally {
      globalThis.fetch = originalFetch;
    }
  });

  it("accepts optional runner for testing", async () => {
    const issues: LinearIssueRaw[] = [
      {
        identifier: "ENG-21",
        state: { name: "Needs Review" },
        labels: { nodes: [{ name: "worker-done" }] },
        attachments: [{ url: "https://github.com/owner/repo/pull/1" }],
      },
    ];

    const originalFetch = globalThis.fetch;
    globalThis.fetch = (async () =>
      new Response(JSON.stringify([]), { status: 200 })) as unknown as typeof fetch;

    const runner: CommandRunner = async (_cmd: string[]) => {
      const response = { data: { repo0: { pr0: { isDraft: false } } } };
      return { stdout: JSON.stringify(response), stderr: "", exitCode: 0 };
    };

    try {
      const result = await runPipeline(
        issues,
        "00000000-0000-0000-0000-000000000000",
        "http://localhost:3000",
        runner
      );

      const parsed = JSON.parse(result);
      expect(parsed.issues["ENG-21"].prIsDraft).toBe(false);
    } finally {
      globalThis.fetch = originalFetch;
    }
  });
});
