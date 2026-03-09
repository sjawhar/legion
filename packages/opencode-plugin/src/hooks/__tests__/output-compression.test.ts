import { afterEach, describe, expect, it, spyOn } from "bun:test";
import { ContentStore } from "../../store/content-store";
import { createOutputCompressionHook } from "../output-compression";

const activeHooks: Array<ReturnType<typeof createOutputCompressionHook>> = [];

afterEach(() => {
  for (const hook of activeHooks) {
    hook.getStore()?.close();
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
    expect(hook.getStore()).toBeNull();
  });

  it("does not create store until first compression", async () => {
    const hook = makeHook({ thresholdBytes: 10 });
    expect(hook.getStore()).toBeNull();

    const output = { title: "bash", output: "large output\n".repeat(40), metadata: {} };
    await hook["tool.execute.after"]?.(
      { tool: "bash", sessionID: "s-1", callID: "c-lazy", args: {} },
      output
    );

    expect(hook.getStore()).toBeInstanceOf(ContentStore);
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

    const store = hook.getStore();
    expect(store).not.toBeNull();
    if (!store) {
      throw new Error("expected store to exist after compression");
    }
    const storeStats = store.getStats();
    expect(storeStats.totalChunks).toBeGreaterThan(1);
    expect(storeStats.sources).toContain("s-1:bash:c-9");
    expect(storeStats.sources).toContain("s-1:bash:c-10");
  });

  it("returns null before compression and same store instance after", async () => {
    const hook = makeHook({ thresholdBytes: 10 });
    expect(hook.getStore()).toBeNull();

    const output = { title: "bash", output: "alpha beta\n".repeat(40), metadata: {} };
    await hook["tool.execute.after"]?.(
      { tool: "bash", sessionID: "s-1", callID: "c-store", args: {} },
      output
    );

    const store = hook.getStore();
    expect(store).toBeInstanceOf(ContentStore);
    expect(hook.getStore()).toBe(store);
  });

  it("non-string output passes through without throwing", async () => {
    const hook = makeHook({ thresholdBytes: 10 });
    const output = {
      title: "bash",
      output: undefined as unknown as string,
      metadata: {},
    };

    await hook["tool.execute.after"]?.(
      { tool: "bash", sessionID: "s-1", callID: "c-ns", args: {} },
      output
    );

    expect(output.output).toBeUndefined();
    expect(hook.getStats().passedThrough).toBe(1);
  });

  it("compression failure logs warning and preserves raw output", async () => {
    const hook = makeHook({ thresholdBytes: 10 });
    const warmup = { title: "bash", output: "warmup data\n".repeat(20), metadata: {} };
    await hook["tool.execute.after"]?.(
      { tool: "bash", sessionID: "s-1", callID: "c-warmup", args: {} },
      warmup
    );

    const store = hook.getStore();
    expect(store).not.toBeNull();
    if (!store) {
      throw new Error("expected store to exist after warmup compression");
    }
    const originalIndex = store.index.bind(store);
    store.index = () => {
      throw new Error("simulated failure");
    };
    const raw = "some large output\n".repeat(50);
    const output = {
      title: "bash",
      output: raw,
      metadata: {},
    };

    const warnSpy = spyOn(console, "warn").mockImplementation(() => {});
    try {
      await hook["tool.execute.after"]?.(
        { tool: "bash", sessionID: "s-1", callID: "c-cf", args: {} },
        output
      );

      expect(output.output).toBe(raw);
      expect(hook.getStats().passedThrough).toBe(1);
      expect(hook.getStats().compressed).toBe(1);
      expect(warnSpy).toHaveBeenCalledTimes(1);
      expect(warnSpy.mock.calls[0][0]).toBe("[legion-plugin] Output compression failed:");
    } finally {
      warnSpy.mockRestore();
      store.index = originalIndex;
    }
  });

  it("session.deleted removes only deleted session content", async () => {
    const hook = makeHook({ thresholdBytes: 10 });
    const outputA = { title: "bash", output: "session one content\n".repeat(40), metadata: {} };
    const outputB = { title: "bash", output: "session two content\n".repeat(40), metadata: {} };

    await hook["tool.execute.after"]?.(
      { tool: "bash", sessionID: "s-1", callID: "c-s1", args: {} },
      outputA
    );
    await hook["tool.execute.after"]?.(
      { tool: "bash", sessionID: "s-2", callID: "c-s2", args: {} },
      outputB
    );

    const store = hook.getStore();
    expect(store).not.toBeNull();
    if (!store) {
      throw new Error("expected store to exist after compression");
    }

    expect(store.search({ queries: ["one content"], session: "s-1" }).length).toBeGreaterThan(0);
    expect(store.search({ queries: ["two content"], session: "s-2" }).length).toBeGreaterThan(0);

    await hook.event({
      event: { type: "session.deleted", properties: { sessionID: "s-1" } },
    });

    expect(store.search({ queries: ["one content"], session: "s-1" })).toHaveLength(0);
    expect(store.search({ queries: ["two content"], session: "s-2" }).length).toBeGreaterThan(0);
  });

  it("Python traceback output not compressed", async () => {
    const hook = makeHook({ thresholdBytes: 20 });
    const raw =
      'Traceback (most recent call last):\n  File "main.py", line 10\n    raise ValueError("boom")\nValueError: boom\n' +
      "x".repeat(200);
    const output = { title: "bash", output: raw, metadata: {} };

    await hook["tool.execute.after"]?.(
      { tool: "bash", sessionID: "s-1", callID: "c-py", args: {} },
      output
    );

    expect(output.output).toBe(raw);
    expect(output.output).not.toContain("[Compressed]");
    expect(hook.getStats().passedThrough).toBe(1);
  });

  it("Go panic output not compressed", async () => {
    const hook = makeHook({ thresholdBytes: 20 });
    const raw = `goroutine 1 [running]:\nmain.main()\n\t/tmp/main.go:10 +0x40\n${"x".repeat(200)}`;
    const output = { title: "bash", output: raw, metadata: {} };

    await hook["tool.execute.after"]?.(
      { tool: "bash", sessionID: "s-1", callID: "c-go", args: {} },
      output
    );

    expect(output.output).toBe(raw);
    expect(output.output).not.toContain("[Compressed]");
    expect(hook.getStats().passedThrough).toBe(1);
  });

  it("Rust panic output not compressed", async () => {
    const hook = makeHook({ thresholdBytes: 20 });
    const raw = `thread 'main' panicked at 'index out of bounds':\nsrc/main.rs:5:10\n${"x".repeat(200)}`;
    const output = { title: "bash", output: raw, metadata: {} };

    await hook["tool.execute.after"]?.(
      { tool: "bash", sessionID: "s-1", callID: "c-rs", args: {} },
      output
    );

    expect(output.output).toBe(raw);
    expect(output.output).not.toContain("[Compressed]");
    expect(hook.getStats().passedThrough).toBe(1);
  });

  it("false positive guard: Traceback past 2000 chars is compressed", async () => {
    const hook = makeHook({ thresholdBytes: 20 });
    const padding = "a".repeat(2100);
    const raw = `${padding}\nTraceback (most recent call last):\n  File "main.py"\nValueError: boom`;
    const output = { title: "bash", output: raw, metadata: {} };

    await hook["tool.execute.after"]?.(
      { tool: "bash", sessionID: "s-1", callID: "c-fp", args: {} },
      output
    );

    expect(output.output).toContain("[Compressed]");
    expect(hook.getStats().compressed).toBe(1);
  });
});
