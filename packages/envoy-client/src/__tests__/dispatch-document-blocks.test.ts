import { describe, expect, test } from "bun:test";
import type { ArtifactBlock } from "@legion/contracts";

import { readDocumentBlocks } from "../dispatch-execute";
import { DispatchClient } from "../dispatch-http";

const SERVER = "https://dispatch.example";

function json(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { "Content-Type": "application/json" },
  });
}

const artifact = (id: string, slug: string, name: string, primary = false) => ({
  id,
  issue_key: "DSP-1",
  project: "DSP",
  slug,
  name,
  kind: "doc",
  primary,
  versions: [{ number: 1 }],
});

/** DSP-1 holds its spec and three documents: plan.md's slug, `plan`, is plan's filename. */
const issue = {
  key: "DSP-1",
  primary_artifact_id: "doc-spec",
  artifacts: [
    artifact("doc-spec", "spec", "spec.md", true),
    artifact("doc-brief", "brief-md", "brief.md"),
    artifact("doc-plan", "plan", "plan.md"),
    artifact("doc-plan-file", "plan-2", "plan"),
  ],
  open_asks: [],
};

const block = (id: string, type: string, from: number, to: number) => ({
  id,
  type,
  from,
  to,
  token: `sha256:${id}`,
  references: { comments: 0, asks: 0 },
});

/** Each document's live text, and its blocks by UTF-8 byte range in that text. */
const documents: Record<string, { markdown: string; blocks: ArtifactBlock[] }> = {
  // "# Café" is 7 bytes (é takes 2), "Über den Fluss." 16 (Ü takes 2).
  "doc-spec": {
    markdown: "# Café\n\nÜber den Fluss.\n\n:::ask{#window}\nWhich window?\n:::\n",
    blocks: [
      block("h-1", "heading", 0, 7),
      block("p-1", "paragraph", 9, 25),
      block("window", "ask", 27, 60),
      block("p-2", "paragraph", 43, 56),
    ],
  },
  "doc-brief": { markdown: "The brief.\n", blocks: [block("b-1", "paragraph", 0, 10)] },
  "doc-plan": { markdown: "The plan.\n", blocks: [block("plan-1", "paragraph", 0, 9)] },
  "doc-plan-file": { markdown: "A file.\n", blocks: [block("file-1", "paragraph", 0, 7)] },
};

/** A fake Dispatch serving DSP-1 and its documents, recording each request's path. */
function dispatch(
  override: (path: string) => Response | Promise<Response> | undefined = () => undefined
) {
  const requests: string[] = [];
  const fetchImpl = async (input: RequestInfo | URL): Promise<Response> => {
    const url = new URL(input instanceof Request ? input.url : input.toString());
    const path = `${url.pathname}${url.search}`;
    requests.push(path);
    const answer = await override(path);
    if (answer !== undefined) return answer;
    if (path === "/api/v1/issues/DSP-1") return json(issue);
    const document = /^\/api\/v1\/artifacts\/([^/]+)\/(text|blocks)$/.exec(url.pathname);
    const served = document === null ? undefined : documents[document[1] ?? ""];
    if (served !== undefined && document?.[2] === "text") {
      return json({ markdown: served.markdown, version: 1, token: "sha256:doc" });
    }
    if (served !== undefined) return json(served.blocks);
    return json({ code: "NOT_FOUND", error: "not found" }, 404);
  };
  return { requests, fetchImpl: fetchImpl as typeof fetch };
}

const context = { cwd: "/", env: {}, serverUrls: [SERVER] };

describe("readDocumentBlocks", () => {
  test("each block's text is the UTF-8 byte range Dispatch reports, multi-byte text before it included", async () => {
    const { fetchImpl, requests } = dispatch();
    const client = new DispatchClient(SERVER, "token", fetchImpl);
    expect(await readDocumentBlocks(client, { issue: "DSP-1", artifact: "spec" }, context)).toEqual(
      {
        name: "spec.md",
        blocks: [
          { id: "h-1", text: "# Café" },
          { id: "p-1", text: "Über den Fluss." },
          { id: "window", text: ":::ask{#window}\nWhich window?\n:::" },
          { id: "p-2", text: "Which window?" },
        ],
      }
    );
    // The text, then the blocks, then the text again, so the offsets slice the text they were
    // computed over.
    expect(requests).toEqual([
      "/api/v1/issues/DSP-1",
      "/api/v1/artifacts/doc-spec/text",
      "/api/v1/artifacts/doc-spec/blocks",
      "/api/v1/artifacts/doc-spec/text",
    ]);
  });

  test("a block past the document's first 64 KiB holds its whole text", async () => {
    const markdown = `${"x".repeat(70_000)}\n\nThe far block, read whole.\n`;
    const { fetchImpl } = dispatch((path) => {
      if (path === "/api/v1/artifacts/doc-spec/text")
        return json({ markdown, version: 1, token: "t" });
      if (path === "/api/v1/artifacts/doc-spec/blocks") {
        // "The far block, read whole." is 26 bytes.
        return json([
          block("p-1", "paragraph", 0, 70_000),
          block("p-far", "paragraph", 70_002, 70_028),
        ]);
      }
    });
    const client = new DispatchClient(SERVER, "token", fetchImpl);
    const read = await readDocumentBlocks(client, { issue: "DSP-1", artifact: "spec" }, context);
    expect(read === "not-found" ? undefined : read.blocks[1]).toEqual({
      id: "p-far",
      text: "The far block, read whole.",
    });
  });

  test("a document that changes between its two text reads is refused", async () => {
    let reads = 0;
    const { fetchImpl } = dispatch((path) => {
      if (path !== "/api/v1/artifacts/doc-spec/text") return undefined;
      reads++;
      return json({
        markdown: reads === 1 ? "Before.\n" : "After.\n",
        version: reads,
        token: `t-${reads}`,
      });
    });
    const client = new DispatchClient(SERVER, "token", fetchImpl);
    await expect(
      readDocumentBlocks(client, { issue: "DSP-1", artifact: "spec" }, context)
    ).rejects.toThrow("spec.md changed while its blocks were read");
  });

  test("an issue with no document of that name has none: a first upload", async () => {
    const { fetchImpl, requests } = dispatch();
    const client = new DispatchClient(SERVER, "token", fetchImpl);
    expect(await readDocumentBlocks(client, { issue: "DSP-1", artifact: "new.md" }, context)).toBe(
      "not-found"
    );
    expect(requests).toEqual(["/api/v1/issues/DSP-1"]);
  });

  test("a project with no document of that name has none: a first upload", async () => {
    const { fetchImpl } = dispatch((path) =>
      path === "/api/v1/projects/CORE/artifacts?unlinked=true" ? json([]) : undefined
    );
    const client = new DispatchClient(SERVER, "token", fetchImpl);
    expect(await readDocumentBlocks(client, { project: "CORE", artifact: "new.md" }, context)).toBe(
      "not-found"
    );
  });

  test("a read that fails on a document that exists is no first upload", async () => {
    const { fetchImpl } = dispatch((path) =>
      path === "/api/v1/artifacts/doc-spec/blocks" ? json({ error: "boom" }, 500) : undefined
    );
    const client = new DispatchClient(SERVER, "token", fetchImpl);
    await expect(
      readDocumentBlocks(client, { issue: "DSP-1", artifact: "spec" }, context)
    ).rejects.toThrow();
  });

  test("a filename two documents answer to names no one document, and is no first upload", async () => {
    const { fetchImpl } = dispatch();
    const client = new DispatchClient(SERVER, "token", fetchImpl);
    await expect(
      readDocumentBlocks(client, { issue: "DSP-1", artifact: "plan" }, context)
    ).rejects.toThrow('"plan" names 2 documents on this issue; use the id');
  });

  // Each target, the document's own name as Dispatch stores it, and its first block.
  for (const [name, target, document, first] of [
    ["an id", { issue: "DSP-1", artifact: "doc-brief" }, "brief.md", "b-1"],
    ["a filename", { issue: "DSP-1", artifact: "brief.md" }, "brief.md", "b-1"],
    ["a slug", { issue: "DSP-1", artifact: "brief-md" }, "brief.md", "b-1"],
    ["a dispatch:// reference", { ref: "dispatch://DSP-1/artifact/brief-md" }, "brief.md", "b-1"],
    [
      "a dispatch:// reference, whose document is a slug where another's filename is the same",
      { ref: "dispatch://DSP-1/artifact/plan" },
      "plan.md",
      "plan-1",
    ],
    [
      "a dashboard URL on the session's Dispatch",
      { ref: `${SERVER}/issues/DSP-1/artifacts/brief-md` },
      "brief.md",
      "b-1",
    ],
    ["the issue's spec reference", { ref: "dispatch://DSP-1/spec" }, "spec.md", "h-1"],
  ] as const) {
    test(`the document ${name} names`, async () => {
      const { fetchImpl } = dispatch();
      const client = new DispatchClient(SERVER, "token", fetchImpl);
      const read = await readDocumentBlocks(client, target, context);
      expect(read === "not-found" ? read : [read.name, read.blocks[0]?.id]).toEqual([
        document,
        first,
      ]);
    });
  }

  // The server finds an upload's document by its exact name, and doc-edit reads `spec` as the
  // issue's primary document: the name tells the caller the two differ.
  test("an upload named spec reads the primary document, and names it by its own name", async () => {
    const { fetchImpl } = dispatch();
    const client = new DispatchClient(SERVER, "token", fetchImpl);
    const read = await readDocumentBlocks(client, { issue: "DSP-1", artifact: "spec" }, context);
    expect(read === "not-found" ? read : read.name).toBe("spec.md");
  });

  test("a project document, by the project route's slug", async () => {
    const notes = {
      ...artifact("doc-brief", "notes", "notes.md"),
      issue_key: null,
      project: "CORE",
    };
    const { fetchImpl, requests } = dispatch((path) =>
      path === "/api/v1/projects/CORE/artifacts/notes" ? json(notes) : undefined
    );
    const client = new DispatchClient(SERVER, "token", fetchImpl);
    expect(
      await readDocumentBlocks(client, { ref: "dispatch://CORE/artifact/notes" }, context)
    ).toEqual({ name: "notes.md", blocks: [{ id: "b-1", text: "The brief." }] });
    expect(requests[0]).toBe("/api/v1/projects/CORE/artifacts/notes");
  });

  test("the issue LEGION_ISSUE names, when the target names no owner", async () => {
    const { fetchImpl } = dispatch();
    const client = new DispatchClient(SERVER, "token", fetchImpl);
    const read = await readDocumentBlocks(
      client,
      { artifact: "brief.md" },
      { ...context, env: { LEGION_ISSUE: "DSP-1" } }
    );
    expect(read === "not-found" ? read : read.blocks.map((entry) => entry.id)).toEqual(["b-1"]);
  });

  test("a target that names no owner is refused before any request", async () => {
    const { fetchImpl, requests } = dispatch();
    const client = new DispatchClient(SERVER, "token", fetchImpl);
    await expect(readDocumentBlocks(client, { artifact: "spec" }, context)).rejects.toThrow(
      "issue is required"
    );
    expect(requests).toEqual([]);
  });

  test("a stop during the read ends it", async () => {
    const stop = new AbortController();
    const { fetchImpl, requests } = dispatch((path) => {
      if (path !== "/api/v1/artifacts/doc-spec/blocks") return undefined;
      stop.abort(new Error("the gate stopped"));
      return new Promise<Response>(() => {});
    });
    const waiting: typeof fetch = Object.assign(
      (input: RequestInfo | URL, init?: RequestInit) =>
        new Promise<Response>((resolve, reject) => {
          init?.signal?.addEventListener("abort", () => reject(init.signal?.reason), {
            once: true,
          });
          fetchImpl(input, init).then(resolve, reject);
        }),
      { preconnect: fetch.preconnect }
    );
    const client = new DispatchClient(SERVER, "token", waiting, stop.signal);
    await expect(
      readDocumentBlocks(client, { issue: "DSP-1", artifact: "spec" }, context)
    ).rejects.toThrow("the gate stopped");
    expect(requests).toEqual([
      "/api/v1/issues/DSP-1",
      "/api/v1/artifacts/doc-spec/text",
      "/api/v1/artifacts/doc-spec/blocks",
    ]);
  });
});
