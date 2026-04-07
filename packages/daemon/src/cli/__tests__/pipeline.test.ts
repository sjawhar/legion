import { describe, expect, it } from "bun:test";
import {
  buildPipelineJson,
  formatPipelineView,
  getStatusIndicator,
  type PipelineIssue,
  type PipelineState,
} from "../pipeline";

function makePipelineIssue(overrides: Partial<PipelineIssue> = {}): PipelineIssue {
  return {
    title: "Default issue title",
    status: "Todo",
    labels: [],
    hasLiveWorker: false,
    workerMode: null,
    workerStatus: null,
    suggestedAction: "skip",
    isBlocked: false,
    ...overrides,
  };
}

describe("getStatusIndicator", () => {
  it("returns BLOCKED when isBlocked is true", () => {
    const issue = makePipelineIssue({ isBlocked: true });
    expect(getStatusIndicator(issue)).toBe("BLOCKED");
  });

  it("returns NEEDS INPUT when user-input-needed label present", () => {
    const issue = makePipelineIssue({ labels: ["user-input-needed"] });
    expect(getStatusIndicator(issue)).toBe("NEEDS INPUT");
  });

  it("returns worker mode:status when live worker exists", () => {
    const issue = makePipelineIssue({
      hasLiveWorker: true,
      workerMode: "implement",
      workerStatus: "running",
    });
    expect(getStatusIndicator(issue)).toBe("implement:running");
  });

  it("returns IDLE when no live worker and non-skip action", () => {
    const issue = makePipelineIssue({ suggestedAction: "dispatch_planner" });
    expect(getStatusIndicator(issue)).toBe("IDLE");
  });

  it("returns empty string when skip action and no worker", () => {
    const issue = makePipelineIssue({ suggestedAction: "skip" });
    expect(getStatusIndicator(issue)).toBe("");
  });

  it("BLOCKED takes priority over NEEDS INPUT", () => {
    const issue = makePipelineIssue({
      isBlocked: true,
      labels: ["user-input-needed"],
    });
    expect(getStatusIndicator(issue)).toBe("BLOCKED");
  });

  it("NEEDS INPUT takes priority over worker status", () => {
    const issue = makePipelineIssue({
      labels: ["user-input-needed"],
      hasLiveWorker: true,
      workerMode: "implement",
      workerStatus: "running",
    });
    expect(getStatusIndicator(issue)).toBe("NEEDS INPUT");
  });
});

// Fixture: 10 issues across 5 phases for snapshot testing
function buildTenIssueFixture(): PipelineState {
  return {
    issues: {
      "sjawhar-legion-101": makePipelineIssue({
        title: "Set up authentication service with JWT tokens",
        status: "Backlog",
        suggestedAction: "dispatch_architect",
      }),
      "sjawhar-legion-102": makePipelineIssue({
        title: "Add rate limiting to API endpoints",
        status: "Backlog",
        isBlocked: true,
        suggestedAction: "skip",
      }),
      "sjawhar-legion-103": makePipelineIssue({
        title: "Design database schema for user profiles",
        status: "Todo",
        suggestedAction: "dispatch_planner",
      }),
      "sjawhar-legion-104": makePipelineIssue({
        title: "Implement webhook delivery system with retry logic",
        status: "In Progress",
        hasLiveWorker: true,
        workerMode: "implement",
        workerStatus: "running",
        labels: ["worker-active"],
        suggestedAction: "skip",
      }),
      "sjawhar-legion-105": makePipelineIssue({
        title: "Fix memory leak in event processor causing OOM crashes",
        status: "In Progress",
        labels: ["user-input-needed"],
        suggestedAction: "skip",
      }),
      "sjawhar-legion-106": makePipelineIssue({
        title: "Add OpenTelemetry tracing to all HTTP handlers",
        status: "Testing",
        hasLiveWorker: true,
        workerMode: "test",
        workerStatus: "running",
        labels: ["worker-active"],
        suggestedAction: "skip",
      }),
      "sjawhar-legion-107": makePipelineIssue({
        title: "Refactor config loading to support environment-specific overrides",
        status: "Testing",
        suggestedAction: "dispatch_tester",
      }),
      "sjawhar-legion-108": makePipelineIssue({
        title: "Implement graceful shutdown for all background workers",
        status: "Needs Review",
        labels: ["worker-done"],
        suggestedAction: "dispatch_reviewer",
      }),
      "sjawhar-legion-109": makePipelineIssue({
        title: "Add comprehensive error handling to CLI commands",
        status: "Needs Review",
        hasLiveWorker: true,
        workerMode: "review",
        workerStatus: "running",
        labels: ["worker-active"],
        suggestedAction: "skip",
      }),
      "sjawhar-legion-110": makePipelineIssue({
        title: "Update deployment scripts for multi-region support with automatic failover",
        status: "Done",
        suggestedAction: "skip",
      }),
    },
  };
}

describe("formatPipelineView", () => {
  it("formats 10 issues across 5 phases correctly", () => {
    const state = buildTenIssueFixture();
    const output = formatPipelineView(state);
    expect(output).toMatchSnapshot();
  });

  it("handles empty state", () => {
    const state: PipelineState = { issues: {} };
    const output = formatPipelineView(state);
    expect(output).toContain("Pipeline");
    expect(output).toContain("Total: 0 issues");
  });

  it("handles all issues in one phase", () => {
    const state: PipelineState = {
      issues: {
        "issue-1": makePipelineIssue({ status: "In Progress", title: "First" }),
        "issue-2": makePipelineIssue({ status: "In Progress", title: "Second" }),
        "issue-3": makePipelineIssue({ status: "In Progress", title: "Third" }),
      },
    };
    const output = formatPipelineView(state);
    expect(output).toContain("In Progress (3)");
    expect(output).not.toContain("Backlog");
    expect(output).not.toContain("Todo");
  });

  it("handles all blocked issues", () => {
    const state: PipelineState = {
      issues: {
        "issue-1": makePipelineIssue({
          status: "Todo",
          title: "Blocked 1",
          isBlocked: true,
        }),
        "issue-2": makePipelineIssue({
          status: "In Progress",
          title: "Blocked 2",
          isBlocked: true,
        }),
      },
    };
    const output = formatPipelineView(state);
    expect(output).toContain("[BLOCKED]");
    expect(output).toContain("Blocked: 2");
  });

  it("truncates titles longer than 50 characters", () => {
    const state: PipelineState = {
      issues: {
        "issue-1": makePipelineIssue({
          status: "Todo",
          title: "This is a very long title that exceeds fifty characters in length definitely",
        }),
      },
    };
    const output = formatPipelineView(state);
    // Truncated to 50 chars with ellipsis
    expect(output).toContain("…");
    expect(output).not.toContain("definitely");
  });

  it("handles no live workers", () => {
    const state: PipelineState = {
      issues: {
        "issue-1": makePipelineIssue({
          status: "Todo",
          title: "Idle issue",
          suggestedAction: "dispatch_planner",
        }),
      },
    };
    const output = formatPipelineView(state);
    expect(output).toContain("[IDLE]");
    expect(output).toContain("Active: 0 workers");
    expect(output).toContain("Idle: 1");
  });
});

describe("buildPipelineJson", () => {
  it("builds JSON for 10-issue fixture", () => {
    const state = buildTenIssueFixture();
    const json = buildPipelineJson(state);
    expect(json).toMatchSnapshot();
  });

  it("includes correct summary counts", () => {
    const state = buildTenIssueFixture();
    const json = buildPipelineJson(state);
    expect(json.summary.total).toBe(10);
    expect(json.summary.active).toBe(3); // 104, 106, 109 have live workers
    expect(json.summary.blocked).toBe(1); // 102
    expect(json.summary.needsInput).toBe(1); // 105
    expect(json.summary.idle).toBe(4); // 101, 103, 107, 108 have non-skip actions
  });

  it("groups issues by phase", () => {
    const state = buildTenIssueFixture();
    const json = buildPipelineJson(state);
    const phaseNames = json.phases.map((p) => p.name);
    expect(phaseNames).toContain("Backlog");
    expect(phaseNames).toContain("Todo");
    expect(phaseNames).toContain("In Progress");
    expect(phaseNames).toContain("Testing");
    expect(phaseNames).toContain("Needs Review");
    expect(phaseNames).toContain("Done");
    // Empty phases are excluded
    expect(phaseNames).not.toContain("Triage");
    expect(phaseNames).not.toContain("Retro");
  });

  it("handles empty state", () => {
    const state: PipelineState = { issues: {} };
    const json = buildPipelineJson(state);
    expect(json.phases).toEqual([]);
    expect(json.summary).toEqual({
      total: 0,
      active: 0,
      blocked: 0,
      needsInput: 0,
      idle: 0,
    });
  });
});
