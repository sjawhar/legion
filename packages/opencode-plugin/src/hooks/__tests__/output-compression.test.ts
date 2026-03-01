import { afterEach, describe, expect, it } from "bun:test";
import { ContentStore } from "../../store/content-store";
import { createOutputCompressionHook } from "../output-compression";

const activeHooks: Array<ReturnType<typeof createOutputCompressionHook>> = [];

afterEach(() => {
  for (const hook of activeHooks) {
    hook.getStore().close();
  }
  activeHooks.length = 0;
});

function makeHook(config?: {
  enabled?: boolean;
  thresholdBytes?: number;
  excludeTools?: string[];
  maxIndexSizeMB?: number;
}) {
  const hook = createOutputCompressionHook(config ?? {});
  activeHooks.push(hook);
  return hook;
}

describe("createOutputCompressionHook", () => {
  it("passes through small outputs unchanged", async () => {
    const hook = makeHook({ thresholdBytes: 200 });
    const output = {
      title: "bash",
      output: "small output",
      metadata: {},
    };

    await hook["tool.execute.after"]?.(
      { tool: "bash", sessionID: "s-1", callID: "c-1", args: {} },
      output
    );

    expect(output.output).toBe("small output");
    expect(hook.getStats()).toEqual({ compressed: 0, bytesSaved: 0, passedThrough: 1 });
  });

  it("compresses large outputs and replaces output with summary", async () => {
    const hook = makeHook({ thresholdBytes: 40 });
    const output = {
      title: "grep",
      output: "alpha beta gamma\n".repeat(30),
      metadata: {},
    };

    await hook["tool.execute.after"]?.(
      { tool: "grep", sessionID: "s-1", callID: "c-2", args: {} },
      output
    );

    expect(output.output).toContain("[Compressed]");
    expect(output.output).toContain('indexed as "s-1:grep:c-2"');
    expect(output.output).toContain("sections indexed");
    expect(output.output).toContain("Top terms:");
    expect(output.output).toContain("Use context_search tool");
    expect(hook.getStats().compressed).toBe(1);
    expect(hook.getStats().bytesSaved).toBeGreaterThan(0);
  });

  it("always passes through excluded tools", async () => {
    const hook = makeHook({ thresholdBytes: 20, excludeTools: ["bash"] });
    const raw = "huge output\n".repeat(100);
    const output = {
      title: "bash",
      output: raw,
      metadata: {},
    };

    await hook["tool.execute.after"]?.(
      { tool: "bash", sessionID: "s-1", callID: "c-3", args: {} },
      output
    );

    expect(output.output).toBe(raw);
    expect(hook.getStats()).toEqual({ compressed: 0, bytesSaved: 0, passedThrough: 1 });
  });

  it("always passes through error outputs", async () => {
    const hook = makeHook({ thresholdBytes: 20 });
    const errorOutput = {
      title: "error",
      output: `Error: crash\n at run (/tmp/file.ts:10:5)\n${"x".repeat(200)}`,
      metadata: {},
    };

    await hook["tool.execute.after"]?.(
      { tool: "bash", sessionID: "s-1", callID: "c-4", args: {} },
      errorOutput
    );

    expect(errorOutput.output.startsWith("Error:")).toBe(true);
    expect(errorOutput.output).not.toContain("[Compressed]");
    expect(hook.getStats()).toEqual({ compressed: 0, bytesSaved: 0, passedThrough: 1 });
  });

  it("disables compression when enabled=false", async () => {
    const hook = makeHook({ enabled: false, thresholdBytes: 1 });
    const raw = "compress me maybe".repeat(50);
    const output = {
      title: "bash",
      output: raw,
      metadata: {},
    };

    await hook["tool.execute.after"]?.(
      { tool: "bash", sessionID: "s-1", callID: "c-5", args: {} },
      output
    );

    expect(output.output).toBe(raw);
    expect(hook.getStats()).toEqual({ compressed: 0, bytesSaved: 0, passedThrough: 1 });
  });

  it("includes byte count, section count, and vocabulary in summary", async () => {
    const hook = makeHook({ thresholdBytes: 20 });
    const output = {
      title: "grep",
      output: "jujutsu workers bookmarks rebase change-id\n".repeat(60),
      metadata: {},
    };

    await hook["tool.execute.after"]?.(
      { tool: "grep", sessionID: "s-1", callID: "c-6", args: {} },
      output
    );

    expect(output.output).toMatch(
      /^\[Compressed\] \d+ bytes from grep indexed as "s-1:grep:c-6"\./
    );
    expect(output.output).toMatch(/\n\d+ sections indexed\. Top terms: /);
    expect(output.output).toContain("queries like [");
  });

  it("tracks compressed count and bytes saved across multiple calls", async () => {
    const hook = makeHook({ thresholdBytes: 10 });
    const first = { title: "bash", output: "alpha beta\n".repeat(40), metadata: {} };
    const second = { title: "bash", output: "gamma delta\n".repeat(40), metadata: {} };

    await hook["tool.execute.after"]?.(
      { tool: "bash", sessionID: "s-1", callID: "c-7", args: {} },
      first
    );
    await hook["tool.execute.after"]?.(
      { tool: "bash", sessionID: "s-1", callID: "c-8", args: {} },
      second
    );

    const stats = hook.getStats();
    expect(stats.compressed).toBe(2);
    expect(stats.bytesSaved).toBeGreaterThan(0);
    expect(stats.passedThrough).toBe(0);
  });

  it("supports repeated compression while indexing each source", async () => {
    const hook = makeHook({ thresholdBytes: 10 });
    const first = { title: "bash", output: "first chunk\n".repeat(50), metadata: {} };
    const second = { title: "bash", output: "second chunk\n".repeat(50), metadata: {} };

    await hook["tool.execute.after"]?.(
      { tool: "bash", sessionID: "s-1", callID: "c-9", args: {} },
      first
    );
    await hook["tool.execute.after"]?.(
      { tool: "bash", sessionID: "s-1", callID: "c-10", args: {} },
      second
    );

    const storeStats = hook.getStore().getStats();
    expect(storeStats.totalChunks).toBeGreaterThan(1);
    expect(storeStats.sources).toContain("s-1:bash:c-9");
    expect(storeStats.sources).toContain("s-1:bash:c-10");
  });

  it("returns the underlying ContentStore instance", () => {
    const hook = makeHook();
    const store = hook.getStore();

    expect(store).toBeInstanceOf(ContentStore);
    expect(hook.getStore()).toBe(store);
  });
});
