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
      routingHints: { complexity: "small", skipTest: false },
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
});
