import { afterEach, describe, expect, it } from "bun:test";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { ContentStore } from "../content-store";

const dbPaths: string[] = [];

function makeDbPath(): string {
  const dbPath = path.join(
    os.tmpdir(),
    `content-store-test-${process.pid}-${Date.now()}-${Math.random().toString(16).slice(2)}.db`
  );
  dbPaths.push(dbPath);
  return dbPath;
}

afterEach(() => {
  for (const dbPath of dbPaths) {
    if (fs.existsSync(dbPath)) {
      fs.rmSync(dbPath, { force: true });
    }
  }
  dbPaths.length = 0;
});

describe("ContentStore", () => {
  it("indexes plain text and retrieves via search", () => {
    const store = new ContentStore({ dbPath: makeDbPath() });

    const result = store.index({
      content:
        "Legion workers execute in isolated jj workspaces.\n\nWorkers run tests before review.",
      source: "plain-doc",
    });
    expect(result.chunkCount).toBeGreaterThan(0);
    expect(result.byteCount).toBeGreaterThan(0);

    const matches = store.search({ queries: ["isolated workspaces"] });
    expect(matches.length).toBeGreaterThan(0);
    expect(matches[0]?.source).toBe("plain-doc");
    expect(matches[0]?.content).toContain("isolated jj workspaces");

    store.close();
  });

  it("chunks markdown by headings and builds hierarchical titles", () => {
    const store = new ContentStore({ dbPath: makeDbPath() });
    const markdown = [
      "# Legion Guide",
      "",
      "## Setup",
      "Prepare workspace.",
      "",
      "### Install",
      "Run bun install.",
      "",
      "```bash",
      "bun test",
      "```",
      "",
      "## Operations",
      "Run legion status team-a.",
    ].join("\n");

    store.index({ content: markdown, source: "guide-md" });

    const matches = store.search({ queries: ["bun install"] });
    expect(matches.length).toBeGreaterThan(0);
    expect(matches[0]?.title).toContain("Setup");
    expect(matches[0]?.title).toContain("Install");
    expect(matches[0]?.content).toContain("```bash");

    store.close();
  });

  it("uses porter, trigram, and fuzzy fallbacks", () => {
    const store = new ContentStore({ dbPath: makeDbPath() });
    store.index({
      content: "Synchronization logic retries transient failures and retries stale locks.",
      source: "fallback-source",
    });

    const porterMatches = store.search({ queries: ["retries"] });
    expect(porterMatches.length).toBeGreaterThan(0);

    const trigramMatches = store.search({ queries: ["chronizat"] });
    expect(trigramMatches.length).toBeGreaterThan(0);

    const fuzzyMatches = store.search({ queries: ["syncronizashun"] });
    expect(fuzzyMatches.length).toBeGreaterThan(0);

    store.close();
  });

  it("normalizes stronger BM25 matches to higher scores and orders results by relevance", () => {
    const store = new ContentStore({ dbPath: makeDbPath() });
    const normalize = (
      store as unknown as { normalizeFtsScore: (raw: number, weight: number) => number }
    ).normalizeFtsScore;

    expect(normalize(-5.2, 1)).toBeGreaterThan(normalize(-0.2, 1));

    store.index({ content: "legion legion legion workers", source: "high" });
    store.index({ content: "legion workers", source: "low" });

    const matches = store.search({ queries: ["legion workers"] });
    expect(matches.length).toBeGreaterThan(1);
    expect(matches[0]?.source).toBe("high");

    store.close();
  });

  it("filters results by source", () => {
    const store = new ContentStore({ dbPath: makeDbPath() });
    store.index({ content: "Alpha source content", source: "alpha" });
    store.index({ content: "Beta source content", source: "beta" });

    const matches = store.search({ queries: ["source content"], source: "beta" });
    expect(matches.length).toBeGreaterThan(0);
    expect(matches.every((match) => match.source === "beta")).toBe(true);

    store.close();
  });

  it("extracts meaningful vocabulary terms", () => {
    const store = new ContentStore({ dbPath: makeDbPath() });
    const result = store.index({
      content:
        "Jujutsu bookmarks coordinate branchless workflows.\n\nJujutsu rebases cleanly and tracks change IDs.",
      source: "vocab-source",
    });

    expect(result.vocabulary.length).toBeGreaterThan(0);
    expect(result.vocabulary.join(" ")).toContain("jujutsu");

    store.close();
  });

  it("tracks aggregate stats", () => {
    const store = new ContentStore({ dbPath: makeDbPath() });
    store.index({ content: "first chunk", source: "source-a" });
    store.index({ content: "second chunk", source: "source-b" });
    store.search({ queries: ["chunk"] });

    const stats = store.getStats();
    expect(stats.totalChunks).toBeGreaterThanOrEqual(2);
    expect(stats.totalBytes).toBeGreaterThan(0);
    expect(stats.sources).toContain("source-a");
    expect(stats.sources).toContain("source-b");

    store.close();
  });

  it("applies max-size cap using replacement bytes when re-indexing same source", () => {
    const store = new ContentStore({ dbPath: makeDbPath(), maxSizeMB: 0.0012 });
    const first = "a".repeat(500);
    const replacement = "b".repeat(600);

    store.index({ content: first, source: "sess-a:bash:call-1", session: "sess-a" });
    expect(() => {
      store.index({ content: replacement, source: "sess-a:bash:call-1", session: "sess-a" });
    }).not.toThrow();

    const stats = store.getStats();
    expect(stats.totalBytes).toBe(Buffer.byteLength(replacement, "utf8"));
    expect(stats.totalChunks).toBeGreaterThan(0);

    store.close();
  });

  it("preserves existing content and stats when re-index transaction fails", () => {
    const store = new ContentStore({ dbPath: makeDbPath() });
    const source = "sess-x:bash:call-1";
    const content = "original content stays indexed";
    store.index({ content, source, session: "sess-x" });
    const before = store.getStats();

    (store as unknown as { db: { exec: (sql: string) => void } }).db.exec(
      "DROP TABLE trigram_index"
    );

    expect(() => {
      store.index({ content: "replacement should fail", source, session: "sess-x" });
    }).toThrow();

    const after = store.getStats();
    expect(after.totalBytes).toBe(before.totalBytes);
    expect(after.totalChunks).toBe(before.totalChunks);
    expect(store.search({ queries: ["original"] })[0]?.source).toBe(source);

    store.close();
  });

  it("removes DB and WAL sidecar files on close", () => {
    const dbPath = makeDbPath();
    const store = new ContentStore({ dbPath });
    store.index({ content: "cleanup content", source: "cleanup" });

    const walPath = `${dbPath}-wal`;
    const shmPath = `${dbPath}-shm`;
    if (!fs.existsSync(walPath)) {
      fs.writeFileSync(walPath, "");
    }
    if (!fs.existsSync(shmPath)) {
      fs.writeFileSync(shmPath, "");
    }

    expect(fs.existsSync(dbPath)).toBe(true);
    expect(fs.existsSync(walPath)).toBe(true);
    expect(fs.existsSync(shmPath)).toBe(true);
    store.close();
    expect(fs.existsSync(dbPath)).toBe(false);
    expect(fs.existsSync(walPath)).toBe(false);
    expect(fs.existsSync(shmPath)).toBe(false);
  });

  it("enforces max size cap", () => {
    const store = new ContentStore({ dbPath: makeDbPath(), maxSizeMB: 0.01 });
    const oversized = `${"x".repeat(1024)}\n`.repeat(256);

    expect(() => store.index({ content: oversized, source: "too-big" })).toThrow();

    store.close();
  });

  it("handles edge cases: empty content, very large content, special-character queries", () => {
    const store = new ContentStore({ dbPath: makeDbPath() });

    const empty = store.index({ content: "", source: "empty" });
    expect(empty.chunkCount).toBe(0);
    expect(store.search({ queries: ["anything"], source: "empty" })).toEqual([]);

    const largeContent = Array.from({ length: 1000 }, (_, i) => `line ${i} legion`).join("\n");
    const large = store.index({ content: largeContent, source: "large" });
    expect(large.chunkCount).toBeGreaterThan(1);

    const specialMatches = store.search({ queries: ['legion +()[]{} "quote"'] });
    expect(Array.isArray(specialMatches)).toBe(true);

    store.close();
  });

  it("isolates content by session", () => {
    const store = new ContentStore({ dbPath: makeDbPath() });

    store.index({
      content: "Session A private data about authentication",
      source: "s-a:bash:1",
      session: "session-a",
    });
    store.index({
      content: "Session B private data about deployment",
      source: "s-b:bash:2",
      session: "session-b",
    });

    // Session A can only see its own content
    const sessionAResults = store.search({ queries: ["data"], session: "session-a" });
    expect(sessionAResults.length).toBeGreaterThan(0);
    expect(sessionAResults.every((r) => r.source === "s-a:bash:1")).toBe(true);

    // Session B can only see its own content
    const sessionBResults = store.search({ queries: ["data"], session: "session-b" });
    expect(sessionBResults.length).toBeGreaterThan(0);
    expect(sessionBResults.every((r) => r.source === "s-b:bash:2")).toBe(true);

    // Without session filter, both are visible
    const allResults = store.search({ queries: ["data"] });
    expect(allResults.length).toBe(2);

    store.close();
  });

  it("prevents cross-session source key collisions", () => {
    const store = new ContentStore({ dbPath: makeDbPath() });

    // Same callID in different sessions should not collide
    store.index({
      content: "Alpha content from session one",
      source: "sess-1:bash:call-1",
      session: "sess-1",
    });
    store.index({
      content: "Beta content from session two",
      source: "sess-2:bash:call-1",
      session: "sess-2",
    });

    const stats = store.getStats();
    expect(stats.sources).toContain("sess-1:bash:call-1");
    expect(stats.sources).toContain("sess-2:bash:call-1");
    expect(stats.totalChunks).toBeGreaterThanOrEqual(2);

    // Each session sees only its own content
    const sess1 = store.search({ queries: ["content"], session: "sess-1" });
    expect(sess1.length).toBeGreaterThan(0);
    expect(sess1[0]?.content).toContain("Alpha");

    const sess2 = store.search({ queries: ["content"], session: "sess-2" });
    expect(sess2.length).toBeGreaterThan(0);
    expect(sess2[0]?.content).toContain("Beta");

    store.close();
  });

  it("deletes only one session's indexed content and updates stats", () => {
    const store = new ContentStore({ dbPath: makeDbPath() });

    store.index({
      content: "alpha session private payload",
      source: "session-a:bash:1",
      session: "session-a",
    });
    store.index({
      content: "beta session private payload",
      source: "session-b:bash:1",
      session: "session-b",
    });

    const before = store.getStats();
    expect(before.sources).toContain("session-a:bash:1");
    expect(before.sources).toContain("session-b:bash:1");

    store.deleteSession("session-a");

    expect(store.search({ queries: ["private"], session: "session-a" })).toEqual([]);
    expect(store.search({ queries: ["private"], session: "session-b" }).length).toBeGreaterThan(0);

    const after = store.getStats();
    expect(after.sources).not.toContain("session-a:bash:1");
    expect(after.sources).toContain("session-b:bash:1");
    expect(after.totalBytes).toBeLessThan(before.totalBytes);
    expect(after.totalChunks).toBeLessThan(before.totalChunks);

    store.close();
  });
});
