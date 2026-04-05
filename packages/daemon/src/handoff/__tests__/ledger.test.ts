import { afterEach, describe, expect, it } from "bun:test";
import { mkdtemp, readdir, readFile, rm, writeFile } from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import {
  ensureLegionDir,
  getLegionDir,
  readAllHandoffs,
  readMessages,
  readPhaseHandoff,
  writeMessage,
  writePhaseHandoff,
} from "../ledger";

describe("handoff ledger", () => {
  let workspaceDir: string | null = null;

  afterEach(async () => {
    if (workspaceDir) {
      await rm(workspaceDir, { force: true, recursive: true });
      workspaceDir = null;
    }
  });

  it("writes and reads phase handoffs with auto-populated metadata", async () => {
    workspaceDir = await mkdtemp(path.join(os.tmpdir(), "legion-handoff-"));

    writePhaseHandoff(workspaceDir, "plan", {
      concerns: ["need test parallelism"],
      independentTasks: 2,
      routingHints: { complexity: "small", skipRetro: false },
      taskCount: 4,
      workflowRecommendation: "dispatch-parallel-workers",
    });

    const handoff = readPhaseHandoff(workspaceDir, "plan");
    expect(handoff).not.toBeNull();
    expect(handoff?.phase).toBe("plan");
    expect(handoff?.schemaVersion).toBe(1);
    expect(typeof handoff?.completed).toBe("string");
    expect(new Date(handoff?.completed ?? "").toString()).not.toBe("Invalid Date");

    const all = readAllHandoffs(workspaceDir);
    expect(all.plan?.phase).toBe("plan");
    expect(all.architect).toBeUndefined();
  });

  it("writes and reads plan handoffs with requiredSkills", async () => {
    workspaceDir = await mkdtemp(path.join(os.tmpdir(), "legion-handoff-"));

    writePhaseHandoff(workspaceDir, "plan", {
      taskCount: 5,
      independentTasks: 3,
      requiredSkills: {
        implement: ["reskin-environment", "task-workflow"],
        test: ["smoke-testing"],
        review: ["design-review"],
      },
    });

    const all = readAllHandoffs(workspaceDir);
    const plan = all.plan;
    expect(plan).toBeDefined();
    if (plan?.phase === "plan") {
      expect(plan.requiredSkills).toEqual({
        implement: ["reskin-environment", "task-workflow"],
        test: ["smoke-testing"],
        review: ["design-review"],
      });
    }
  });

  it("accepts plan handoffs with partial requiredSkills", async () => {
    workspaceDir = await mkdtemp(path.join(os.tmpdir(), "legion-handoff-"));

    writePhaseHandoff(workspaceDir, "plan", {
      taskCount: 2,
      requiredSkills: {
        implement: ["some-skill"],
      },
    });

    const all = readAllHandoffs(workspaceDir);
    const plan = all.plan;
    expect(plan).toBeDefined();
    if (plan?.phase === "plan") {
      expect(plan.requiredSkills).toEqual({
        implement: ["some-skill"],
      });
    }
  });

  it("accepts plan handoffs without requiredSkills (backward compat)", async () => {
    workspaceDir = await mkdtemp(path.join(os.tmpdir(), "legion-handoff-"));

    writePhaseHandoff(workspaceDir, "plan", {
      taskCount: 3,
      concerns: ["no skills needed"],
    });

    const all = readAllHandoffs(workspaceDir);
    const plan = all.plan;
    expect(plan).toBeDefined();
    if (plan?.phase === "plan") {
      expect(plan.requiredSkills).toBeUndefined();
    }
  });

  it("fails open on missing, corrupt, or schema-mismatched phase files", async () => {
    workspaceDir = await mkdtemp(path.join(os.tmpdir(), "legion-handoff-"));

    expect(readPhaseHandoff(workspaceDir, "implement")).toBeNull();

    ensureLegionDir(workspaceDir);
    const legionDir = getLegionDir(workspaceDir);

    await writeFile(path.join(legionDir, "implement.json"), "not-json", "utf-8");
    expect(readPhaseHandoff(workspaceDir, "implement")).toBeNull();

    await writeFile(
      path.join(legionDir, "implement.json"),
      JSON.stringify({
        completed: new Date().toISOString(),
        phase: "implement",
        schemaVersion: 2,
      }),
      "utf-8"
    );
    expect(readPhaseHandoff(workspaceDir, "implement")).toBeNull();
  });

  it("writes uniquely-named message files and reads messages sorted by name", async () => {
    workspaceDir = await mkdtemp(path.join(os.tmpdir(), "legion-handoff-"));

    writeMessage(workspaceDir, {
      body: "Initial architecture complete",
      from: "architect",
      to: "plan",
    });
    writeMessage(workspaceDir, {
      body: "Planning done, ready to implement",
      from: "plan",
      to: "implement",
    });

    const messagesDir = path.join(getLegionDir(workspaceDir), "messages");
    const entries = (await readdir(messagesDir)).sort();
    expect(entries).toHaveLength(2);
    // Filenames use timestamp+random — order within same timestamp is undefined
    const architectMsg = entries.find((e) => e.includes("-architect-to-plan.json"));
    const planMsg = entries.find((e) => e.includes("-plan-to-implement.json"));
    expect(architectMsg).toBeDefined();
    expect(planMsg).toBeDefined();

    const messages = readMessages(workspaceDir);
    expect(messages).toHaveLength(2);
    const fromArchitect = messages.find((m) => m.from === "architect");
    const toPlan = messages.find((m) => m.to === "implement");
    expect(fromArchitect).toBeDefined();
    expect(toPlan).toBeDefined();

    const firstPayload = JSON.parse(
      await readFile(path.join(messagesDir, entries[0] as string), "utf-8")
    ) as { timestamp?: unknown };
    expect(typeof firstPayload.timestamp).toBe("string");
  });

  it("continues when message files are missing or corrupt", async () => {
    workspaceDir = await mkdtemp(path.join(os.tmpdir(), "legion-handoff-"));

    expect(readMessages(workspaceDir)).toEqual([]);

    ensureLegionDir(workspaceDir);
    const messagesDir = path.join(getLegionDir(workspaceDir), "messages");

    await writeFile(path.join(messagesDir, "001-architect-to-plan.json"), "not-json", "utf-8");
    await writeFile(
      path.join(messagesDir, "002-plan-to-implement.json"),
      JSON.stringify({
        body: "done",
        from: "plan",
        timestamp: new Date().toISOString(),
        to: "implement",
      }),
      "utf-8"
    );

    const messages = readMessages(workspaceDir);
    expect(messages).toHaveLength(1);
    expect(messages[0]?.from).toBe("plan");
  });

  it("rejects handoff data with reserved fields", async () => {
    workspaceDir = await mkdtemp(path.join(os.tmpdir(), "legion-handoff-"));

    // Write with a reserved field smuggled in — schemaVersion should be overwritten by writePhaseHandoff
    writePhaseHandoff(workspaceDir, "plan", {
      schemaVersion: 99,
      taskCount: 3,
    } as Record<string, unknown>);

    // The write succeeds but schemaVersion is overwritten to 1
    const handoff = readPhaseHandoff(workspaceDir, "plan");
    expect(handoff).not.toBeNull();
    expect(handoff?.schemaVersion).toBe(1);
  });

  it("rejects phase handoffs with wrong field types via Zod", async () => {
    workspaceDir = await mkdtemp(path.join(os.tmpdir(), "legion-handoff-"));

    ensureLegionDir(workspaceDir);
    const legionDir = getLegionDir(workspaceDir);

    // trickyParts should be string[], not a string
    await writeFile(
      path.join(legionDir, "implement.json"),
      JSON.stringify({
        schemaVersion: 1,
        phase: "implement",
        completed: new Date().toISOString(),
        trickyParts: "not an array",
      }),
      "utf-8"
    );
    expect(readPhaseHandoff(workspaceDir, "implement")).toBeNull();

    // routingHints.complexity should be a valid enum, not freetext
    await writeFile(
      path.join(legionDir, "plan.json"),
      JSON.stringify({
        schemaVersion: 1,
        phase: "plan",
        completed: new Date().toISOString(),
        routingHints: { complexity: "enormous" },
      }),
      "utf-8"
    );
    expect(readPhaseHandoff(workspaceDir, "plan")).toBeNull();
  });

  it("rejects non-ISO timestamps", async () => {
    workspaceDir = await mkdtemp(path.join(os.tmpdir(), "legion-handoff-"));

    ensureLegionDir(workspaceDir);
    const legionDir = getLegionDir(workspaceDir);

    await writeFile(
      path.join(legionDir, "test.json"),
      JSON.stringify({
        schemaVersion: 1,
        phase: "test",
        completed: "Tuesday",
        passed: 5,
      }),
      "utf-8"
    );
    expect(readPhaseHandoff(workspaceDir, "test")).toBeNull();
  });

  it("migrates plan handoff learningsUsed to learningsInjected (backward compat)", async () => {
    workspaceDir = await mkdtemp(path.join(os.tmpdir(), "legion-handoff-"));

    ensureLegionDir(workspaceDir);
    const legionDir = getLegionDir(workspaceDir);

    // Write old-style plan handoff with learningsUsed
    await writeFile(
      path.join(legionDir, "plan.json"),
      JSON.stringify({
        schemaVersion: 1,
        phase: "plan",
        completed: new Date().toISOString(),
        taskCount: 3,
        learningsUsed: ["state/race-conditions.md", "daemon/controller-lifecycle-separation.md"],
      }),
      "utf-8"
    );

    const handoff = readPhaseHandoff(workspaceDir, "plan");
    expect(handoff).not.toBeNull();
    expect(handoff?.phase).toBe("plan");
    if (handoff?.phase === "plan") {
      // learningsUsed should be migrated to learningsInjected
      expect(handoff.learningsInjected).toEqual([
        "state/race-conditions.md",
        "daemon/controller-lifecycle-separation.md",
      ]);
      // learningsUsed should be removed from the output
      expect("learningsUsed" in handoff).toBe(false);
    }
  });

  it("preserves learningsInjected when both learningsUsed and learningsInjected exist in plan", async () => {
    workspaceDir = await mkdtemp(path.join(os.tmpdir(), "legion-handoff-"));

    ensureLegionDir(workspaceDir);
    const legionDir = getLegionDir(workspaceDir);

    // Write plan handoff with both fields — learningsInjected takes precedence
    await writeFile(
      path.join(legionDir, "plan.json"),
      JSON.stringify({
        schemaVersion: 1,
        phase: "plan",
        completed: new Date().toISOString(),
        taskCount: 2,
        learningsUsed: ["old/learning.md"],
        learningsInjected: ["new/learning.md"],
      }),
      "utf-8"
    );

    const handoff = readPhaseHandoff(workspaceDir, "plan");
    expect(handoff).not.toBeNull();
    if (handoff?.phase === "plan") {
      // Existing learningsInjected should be preserved, not overwritten by learningsUsed
      expect(handoff.learningsInjected).toEqual(["new/learning.md"]);
    }
  });

  it("validates handoff data with learningsInjected and learningsHelpful for each phase", async () => {
    workspaceDir = await mkdtemp(path.join(os.tmpdir(), "legion-handoff-"));

    const phases = [
      { phase: "architect" as const, extra: { scope: "small" } },
      { phase: "plan" as const, extra: { taskCount: 3 } },
      { phase: "implement" as const, extra: { filesChanged: ["a.ts"] } },
      { phase: "test" as const, extra: { passed: 5, failed: 0 } },
      { phase: "review" as const, extra: { verdict: "approved" } },
    ];

    for (const { phase, extra } of phases) {
      writePhaseHandoff(workspaceDir, phase, {
        ...extra,
        learningsInjected: [
          "daemon/controller-lifecycle-separation.md",
          "architecture-patterns/shared-state-file-ownership.md",
        ],
        learningsHelpful: ["daemon/controller-lifecycle-separation.md"],
      });

      const handoff = readPhaseHandoff(workspaceDir, phase);
      expect(handoff).not.toBeNull();
      expect(handoff?.learningsInjected).toEqual([
        "daemon/controller-lifecycle-separation.md",
        "architecture-patterns/shared-state-file-ownership.md",
      ]);
      expect(handoff?.learningsHelpful).toEqual(["daemon/controller-lifecycle-separation.md"]);
    }
  });

  it("validates handoff data without learnings fields (optional)", async () => {
    workspaceDir = await mkdtemp(path.join(os.tmpdir(), "legion-handoff-"));

    // All phases should validate without learnings fields
    writePhaseHandoff(workspaceDir, "implement", {
      filesChanged: ["b.ts"],
      trickyParts: ["none"],
    });

    const handoff = readPhaseHandoff(workspaceDir, "implement");
    expect(handoff).not.toBeNull();
    expect(handoff?.learningsInjected).toBeUndefined();
    expect(handoff?.learningsHelpful).toBeUndefined();
  });
});
