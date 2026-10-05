import { afterAll, describe, expect, test } from "bun:test";
import { mkdtempSync, rmSync, truncateSync, writeFileSync } from "node:fs";
import * as os from "node:os";
import * as path from "node:path";
import { type DispatchToolResult, executeDispatchTool } from "../dispatch-execute";
import { pictureAddresses } from "../dispatch-pictures";
import { ToolInputError } from "../tool-input-errors";

const config = { enabled: true, url: "http://dispatch.test", token: "secret", error: null };
const session = "01a1058e-f14f-7684-87eb-3dc885955551";
const directory = mkdtempSync(path.join(os.tmpdir(), "dispatch-images-"));
afterAll(() => rmSync(directory, { recursive: true, force: true }));

const PNG = [0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a];
const JPEG = [0xff, 0xd8, 0xff, 0xe0];
const MiB = 1024 * 1024;

/** A file under the test directory holding `bytes`, padded with zeros to `size` when given. */
function file(name: string, bytes: readonly number[], size?: number): string {
  const location = path.join(directory, name);
  writeFileSync(location, new Uint8Array(bytes));
  if (size !== undefined) truncateSync(location, size);
  return location;
}

function json(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { "Content-Type": "application/json" },
  });
}

interface Upload {
  readonly path: string;
  readonly name: string;
  readonly type: string;
  readonly size: number;
}

/**
 * A Dispatch that records every request. An upload answers the slug the server gives `name` and the
 * next version of it; a write answers its own body back; anything `routes` does not answer fails.
 */
function dispatch(
  routes: (pathname: string, init: RequestInit) => Response | undefined = () => undefined
) {
  const requests: string[] = [];
  const uploads: Upload[] = [];
  const posted: Record<string, unknown>[] = [];
  const versions = new Map<string, number>();
  const fetchImpl = async (url: RequestInfo | URL, init: RequestInit = {}): Promise<Response> => {
    const target = new URL(String(url));
    const method = init.method ?? "GET";
    requests.push(`${method} ${target.pathname}`);
    const answered = routes(target.pathname, init);
    if (answered !== undefined) return answered;
    if (method === "POST" && target.pathname.endsWith("/artifacts")) {
      const form = init.body as FormData;
      const upload = form.get("file") as File;
      const name = String(form.get("name"));
      const slug = name.toLowerCase().replace(/[^a-z0-9]+/g, "-");
      const number = (versions.get(slug) ?? 0) + 1;
      versions.set(slug, number);
      uploads.push({ path: target.pathname, name, type: upload.type, size: upload.size });
      return json({ artifact: { slug, name }, version: { number } });
    }
    if (method === "POST") {
      const body = JSON.parse(String(init.body)) as Record<string, unknown>;
      posted.push(body);
      return json({ id: "written-1", issue_key: "DSP-41", in_reply_to: null, ...body });
    }
    throw new Error(`unexpected request: ${method} ${target.pathname}`);
  };
  return { requests, uploads, posted, fetchImpl: fetchImpl as typeof fetch };
}

function run(
  tool: string,
  args: Record<string, unknown>,
  fetchImpl: typeof fetch
): Promise<DispatchToolResult> {
  return executeDispatchTool({
    tool,
    args,
    cwd: directory,
    host: "omp",
    sessionId: session,
    config,
    env: {},
    exec: async () => {
      throw new Error("no repository");
    },
    fetchImpl,
  });
}

async function refusal(promise: Promise<unknown>): Promise<Error> {
  const outcome = await promise.then(
    () => undefined,
    (error: unknown) => error
  );
  if (!(outcome instanceof Error)) throw new Error("expected the call to be refused");
  return outcome;
}

describe("sending pictures", () => {
  test("a message uploads each picture to its issue in order and appends its lines after a blank line", async () => {
    const server = dispatch();
    const shot = file("shot.png", PNG);
    // Named .png, but JPEG bytes: the type is the bytes'. It is passed relative to the cwd.
    file("photo.png", JPEG);

    await run(
      "dispatch_message",
      { issue: "DSP-41", body: "Before and after.", images: [shot, "photo.png"] },
      server.fetchImpl
    );

    expect(server.uploads).toEqual([
      {
        path: "/api/v1/issues/DSP-41/artifacts",
        name: "shot.png",
        type: "image/png",
        size: PNG.length,
      },
      {
        path: "/api/v1/issues/DSP-41/artifacts",
        name: "photo.png",
        type: "image/jpeg",
        size: JPEG.length,
      },
    ]);
    expect(server.posted.map((body) => body.body)).toEqual([
      "Before and after.\n\n" +
        "![shot.png](dispatch://DSP-41/artifact/shot-png@v1)\n" +
        "![photo.png](dispatch://DSP-41/artifact/photo-png@v1)",
    ]);
    expect(server.requests.at(-1)).toBe("POST /api/v1/issues/DSP-41/messages");
  });

  test("a picture sent again is the next version of its upload, and its line names that version", async () => {
    const server = dispatch();
    const shot = file("again.png", PNG);
    await run(
      "dispatch_message",
      { issue: "DSP-41", body: "One.", images: [shot] },
      server.fetchImpl
    );
    await run(
      "dispatch_message",
      { issue: "DSP-41", body: "Two.", images: [shot] },
      server.fetchImpl
    );
    expect(server.posted.map((body) => body.body)).toEqual([
      "One.\n\n![again.png](dispatch://DSP-41/artifact/again-png@v1)",
      "Two.\n\n![again.png](dispatch://DSP-41/artifact/again-png@v2)",
    ]);
  });

  test("a body the picture lines carry over the cap is refused with the count before anything is uploaded", async () => {
    const server = dispatch();
    const shot = file("shot.png", PNG);
    const line = "![shot.png](dispatch://DSP-41/artifact/shot-png@v1)";
    const body = "x".repeat(2000 - 2 - line.length + 1);

    const failure = await refusal(
      run("dispatch_message", { issue: "DSP-41", body, images: [shot] }, server.fetchImpl)
    );

    expect(failure).toBeInstanceOf(ToolInputError);
    expect((failure as ToolInputError).problems).toEqual([
      "body plus 1 picture is 1 characters over the 2000-character limit (2001/2000); shorten the body or send fewer pictures",
    ]);
    expect(server.uploads).toEqual([]);
    expect(server.posted).toEqual([]);
  });

  test("an ask's question counts its ref and its pictures toward the 800-character cap", async () => {
    const server = dispatch();
    const shot = file("shot.png", PNG);
    const ref = "dispatch://DSP-41/message/0b6e0f4e-4e2c-4d55-9a7e-0e9b3a4c1d22";
    const line = "![shot.png](dispatch://DSP-41/artifact/shot-png@v1)";
    const question = "q".repeat(800 - `\n\nRef: ${ref}`.length - 2 - line.length + 1);

    const failure = await refusal(
      run("dispatch_ask", { issue: "DSP-41", question, ref, images: [shot] }, server.fetchImpl)
    );

    expect((failure as ToolInputError).problems).toEqual([
      "question plus ref plus 1 picture is 1 characters over the 800-character limit (801/800); shorten the question, drop the ref or send fewer pictures",
    ]);
    expect(server.uploads).toEqual([]);
  });

  test("a file that is no picture, a missing file and one over 25 MiB are each refused by path, with nothing sent", async () => {
    const server = dispatch();
    const notes = file("notes.png", [0x6e, 0x6f, 0x74, 0x65, 0x73]);
    const huge = file("huge.png", PNG, 25 * MiB + 1);

    const failure = await refusal(
      run(
        "dispatch_comment",
        { issue: "DSP-41", body: "See these.", images: [notes, "missing.png", huge] },
        server.fetchImpl
      )
    );

    expect(failure).toBeInstanceOf(ToolInputError);
    const problems = (failure as ToolInputError).problems;
    expect(problems).toHaveLength(3);
    expect(problems[0]).toBe(
      `images: ${notes} is not a PNG, JPEG, GIF or WebP picture (judged by its bytes, not its name)`
    );
    expect(problems[1]).toStartWith("images: missing.png cannot be read: ");
    expect(problems[2]).toBe(
      `images: ${huge} is 26,214,401 bytes, over the 25 MiB one Dispatch upload takes`
    );
    expect(server.requests).toEqual([]);
  });

  test("a GIF or a WebP is sent typed by its bytes, and another RIFF file is no picture", async () => {
    const server = dispatch();
    const RIFF = [0x52, 0x49, 0x46, 0x46, 0x24, 0x00, 0x00, 0x00];
    const gif = file("moving.gif", [0x47, 0x49, 0x46, 0x38, 0x39, 0x61, 0x01, 0x00]);
    const webp = file("photo.webp", [...RIFF, 0x57, 0x45, 0x42, 0x50]);
    const wave = file("sound.webp", [...RIFF, 0x57, 0x41, 0x56, 0x45]);

    await run(
      "dispatch_message",
      { issue: "DSP-41", body: "Both.", images: [gif, webp] },
      server.fetchImpl
    );
    const failure = await refusal(
      run("dispatch_message", { issue: "DSP-41", body: "Hear.", images: [wave] }, server.fetchImpl)
    );

    expect(server.uploads.map((upload) => upload.type)).toEqual(["image/gif", "image/webp"]);
    expect((failure as ToolInputError).problems).toEqual([
      `images: ${wave} is not a PNG, JPEG, GIF or WebP picture (judged by its bytes, not its name)`,
    ]);
  });

  // The cap is judged before the upload on the slug Dispatch will give the name, so a name holding
  // a letter outside a-z must be predicted as the server slugs it, or a body that fits is refused.
  test("a body that fits the cap exactly with a non-ASCII file name's picture is posted", async () => {
    const server = dispatch();
    const line = "![café.png](dispatch://DSP-41/artifact/caf-png@v1)";
    const body = "x".repeat(2000 - 2 - line.length);

    await run(
      "dispatch_message",
      { issue: "DSP-41", body, images: [file("café.png", PNG)] },
      server.fetchImpl
    );

    expect(server.posted.map((posted) => posted.body)).toEqual([`${body}\n\n${line}`]);
  });

  test("a picture of exactly 25 MiB, the most one Dispatch upload takes, is uploaded and posted", async () => {
    const server = dispatch();
    const largest = file("largest.png", PNG, 25 * MiB);

    await run(
      "dispatch_message",
      { issue: "DSP-41", body: "Full size.", images: [largest] },
      server.fetchImpl
    );

    expect(server.uploads.map((upload) => upload.size)).toEqual([25 * MiB]);
    expect(server.posted.map((body) => body.body)).toEqual([
      "Full size.\n\n![largest.png](dispatch://DSP-41/artifact/largest-png@v1)",
    ]);
  });

  test("a file name holding brackets or a backslash still reads back as its picture", async () => {
    const server = dispatch();
    const names = ["shot [1].png", "half].png", "back\\slash.png"];

    await run(
      "dispatch_message",
      { issue: "DSP-41", body: "Odd names.", images: names.map((name) => file(name, PNG)) },
      server.fetchImpl
    );

    expect(server.posted.flatMap((body) => pictureAddresses(String(body.body)))).toEqual([
      "dispatch://DSP-41/artifact/shot-1-png@v1",
      "dispatch://DSP-41/artifact/half-png@v1",
      "dispatch://DSP-41/artifact/back-slash-png@v1",
    ]);
  });

  test("a reply to a direct message uploads its pictures to this session's own conversation", async () => {
    const message = "6f1f4bb8-8d3c-4a35-9a0e-3b9e7c1d2f40";
    const server = dispatch((pathname, init) => {
      if (pathname !== `/api/v1/messages/${message}/reply`) return undefined;
      const body = JSON.parse(String(init.body)) as { body: string };
      return json({ id: "reply-1", body: body.body, in_reply_to: message });
    });
    const shot = file("screen.png", PNG);

    const result = await run(
      "dispatch_message",
      { in_reply_to: message, body: "Here it is.", images: [shot] },
      server.fetchImpl
    );

    expect(server.uploads.map((upload) => upload.path)).toEqual([
      `/api/v1/agents/${session}/artifacts`,
    ]);
    expect(server.requests.at(-1)).toBe(`POST /api/v1/messages/${message}/reply`);
    expect(result.details).toMatchObject({ message: "reply-1", posted: true });
    expect(result.text).toStartWith(`Replied to message ${message} with message reply-1.`);
  });

  test("a comment on a project document uploads its picture to that project", async () => {
    const document = {
      id: "doc-1",
      issue_key: null,
      project: "CORE",
      slug: "notes",
      name: "notes.md",
      kind: "doc",
      versions: [],
    };
    const server = dispatch((pathname, init) => {
      if (pathname === "/api/v1/projects/CORE/artifacts/notes") return json(document);
      if (pathname === "/api/v1/projects/CORE/artifacts" && init.method !== "POST") {
        return json([document]);
      }
      return undefined;
    });

    await run(
      "dispatch_comment",
      { project: "CORE", artifact: "notes", body: "This step.", images: [file("step.png", PNG)] },
      server.fetchImpl
    );

    expect(server.uploads.map((upload) => upload.path)).toEqual([
      "/api/v1/projects/CORE/artifacts",
    ]);
    expect(server.posted.map((body) => body.body)).toEqual([
      "This step.\n\n![step.png](dispatch://CORE/artifact/step-png@v1)",
    ]);
  });

  test("an upload Dispatch refuses fails the call naming its path, and nothing is posted", async () => {
    const server = dispatch((pathname, init) =>
      pathname === "/api/v1/issues/DSP-41/artifacts" && init.method === "POST"
        ? json({ code: "CAP_EXCEEDED", error: "upload too large" }, 413)
        : undefined
    );
    const shot = file("refused.png", PNG);

    const failure = await refusal(
      run("dispatch_message", { issue: "DSP-41", body: "Look.", images: [shot] }, server.fetchImpl)
    );

    expect(failure.message).toBe(
      `images: ${shot} was not uploaded: upload too large. Nothing was posted.`
    );
    expect(server.posted).toEqual([]);
  });

  test("a text the uploads' own addresses carry over the cap is refused with nothing posted", async () => {
    // The owner already holds a file of this name at version 12, so its line is longer than the
    // one judged before the upload.
    const server = dispatch((pathname, init) =>
      pathname === "/api/v1/issues/DSP-41/artifacts" && init.method === "POST"
        ? json({ artifact: { slug: "shot-png" }, version: { number: 12 } })
        : undefined
    );
    const line = "![shot.png](dispatch://DSP-41/artifact/shot-png@v1)";
    const body = "x".repeat(2000 - 2 - line.length);

    const failure = await refusal(
      run(
        "dispatch_message",
        { issue: "DSP-41", body, images: [file("shot.png", PNG)] },
        server.fetchImpl
      )
    );

    expect(failure.message).toBe(
      "body plus 1 picture is 1 characters over the 2000-character limit (2001/2000) with the " +
        "addresses Dispatch gave the uploads (dispatch://DSP-41/artifact/shot-png@v12), so " +
        "nothing was posted; shorten the body or send fewer pictures."
    );
    expect(server.posted).toEqual([]);
  });
});

/** An image upload a conversation owns, as Dispatch describes it, with one version. */
interface ImageArtifact {
  readonly id: string;
  readonly issue_key: null;
  readonly project: string;
  readonly session_id: string;
  readonly slug: string;
  readonly name: string;
  readonly kind: "image";
  readonly versions: readonly {
    readonly number: 1;
    readonly mime: string;
    readonly size: number;
  }[];
}

function image(id: string, name: string, size: number, mime = "image/png"): ImageArtifact {
  return {
    id,
    issue_key: null,
    project: "",
    session_id: session,
    slug: name.replace(".", "-"),
    name,
    kind: "image",
    versions: [{ number: 1, mime, size }],
  };
}

/** A Dispatch serving `artifacts` (a conversation's uploads, by slug) and their version bytes. */
function pictureServer(
  artifacts: readonly ImageArtifact[],
  bytes: Readonly<Record<string, Uint8Array<ArrayBuffer>>>,
  extra: (pathname: string) => Response | undefined = () => undefined
) {
  const requests: string[] = [];
  const fetchImpl = async (url: RequestInfo | URL): Promise<Response> => {
    const { pathname } = new URL(String(url));
    requests.push(pathname);
    const answered = extra(pathname);
    if (answered !== undefined) return answered;
    const slug = pathname.match(/^\/api\/v1\/agents\/[^/]+\/artifacts\/([^/]+)$/)?.[1];
    const artifact = artifacts.find((candidate) => candidate.slug === slug);
    if (artifact !== undefined) return json(artifact);
    const version = pathname.match(/^\/api\/v1\/artifacts\/([^/]+)\/versions\/1$/)?.[1];
    const served = version === undefined ? undefined : bytes[version];
    if (served !== undefined) {
      return new Response(served, { headers: { "Content-Type": "image/png" } });
    }
    throw new Error(`unexpected request: ${pathname}`);
  };
  return { requests, fetchImpl: fetchImpl as typeof fetch };
}

function png(size: number): Uint8Array<ArrayBuffer> {
  const bytes = new Uint8Array(size);
  bytes.set(PNG);
  return bytes;
}

const base64 = (bytes: Uint8Array) => Buffer.from(bytes).toString("base64");

describe("reading pictures", () => {
  test("dispatch_doc_read returns a conversation's picture as an image block with its name, type, size and version", async () => {
    const bytes = png(64);
    const server = pictureServer([image("pic-1", "shot.png", 64)], { "pic-1": bytes });

    const result = await run(
      "dispatch_doc_read",
      { ref: `dispatch://agent/${session}/artifact/shot-png@v1` },
      server.fetchImpl
    );

    expect(result).toEqual({
      text: "Picture shot.png: image/png, version 1, 64 bytes.",
      details: { session, artifact: "shot-png" },
      images: [{ data: base64(bytes), mimeType: "image/png" }],
    });
    expect(server.requests).toEqual([
      `/api/v1/agents/${session}/artifacts/shot-png`,
      "/api/v1/artifacts/pic-1/versions/1",
    ]);
  });

  test("dispatch_doc_read describes a picture over 5 MiB without fetching its bytes", async () => {
    const server = pictureServer([image("pic-2", "big.png", 5 * MiB + 1)], {});

    const result = await run(
      "dispatch_doc_read",
      { ref: `dispatch://agent/${session}/artifact/big-png@v1` },
      server.fetchImpl
    );

    expect(result.images).toBeUndefined();
    expect(result.text).toBe(
      "big.png is an uploaded image/png picture (version 1, 5,242,881 bytes), over the 5 MiB a " +
        "model is shown, so dispatch_doc_read cannot show it. GET /api/v1/artifacts/pic-2/versions/1 serves its bytes."
    );
    expect(server.requests).toEqual([`/api/v1/agents/${session}/artifacts/big-png`]);
  });

  test("dispatch_doc_read names no issue or project beside a conversation's picture", async () => {
    const failure = await refusal(
      run(
        "dispatch_doc_read",
        { issue: "DSP-41", ref: `dispatch://agent/${session}/artifact/shot-png@v1` },
        dispatch().fetchImpl
      )
    );
    expect((failure as ToolInputError).problems).toContain(
      "a dispatch://agent/... ref names its upload alone: name no issue, project, or artifact with it"
    );
  });

  const conversationId = "6f1f4bb8-8d3c-4a35-9a0e-3b9e7c1d2f40";

  /** A conversation, its replies one minute apart, the n-th message embedding `pictures[n]`. */
  function conversation(pictures: readonly (readonly string[])[]) {
    const at = (minute: number) => `2026-10-04T10:0${minute}:00Z`;
    const entry = (index: number) => ({
      id: `message-${index + 1}`,
      issue_key: null,
      author: { kind: "user", id: "sami" },
      body: `Note ${index + 1}.\n\n${(pictures[index] ?? []).map((slug) => `![${slug}](dispatch://agent/${session}/artifact/${slug}@v1)`).join("\n")}`,
      created_at: at(index),
    });
    return {
      message: entry(0),
      replies: pictures.slice(1).map((_, index) => entry(index + 1)),
    };
  }

  test("dispatch_read shows a conversation's pictures newest first, eight at most, and names the rest", async () => {
    const slugs = Array.from({ length: 10 }, (_, index) => `p${index}-png`);
    const artifacts = slugs.map((slug, index) => ({
      ...image(`pic-${index}`, `p${index}.png`, 16),
      slug,
    }));
    const bytes = Object.fromEntries(artifacts.map((artifact) => [artifact.id, png(16)]));
    // Message 1 embeds p0 and p1; each later reply one more, so p9 is the newest.
    const thread = conversation([["p0-png", "p1-png"], ...slugs.slice(2).map((slug) => [slug])]);
    const server = pictureServer(artifacts, bytes, (pathname) =>
      pathname === `/api/v1/messages/${conversationId}` ? json(thread) : undefined
    );

    const result = await run("dispatch_read", { message: conversationId }, server.fetchImpl);

    expect(result.images).toHaveLength(8);
    const pictureLines = result.text.split("\n").filter((line) => line.startsWith("- "));
    expect(pictureLines.slice(-10)).toEqual([
      ...[9, 8, 7, 6, 5, 4, 3, 2].map(
        (index, shown) =>
          `- image ${shown + 1}: dispatch://agent/${session}/artifact/p${index}-png@v1 (p${index}.png, image/png, 16 bytes)`
      ),
      `- not shown: dispatch://agent/${session}/artifact/p0-png@v1 (past this read's 8 pictures; dispatch_doc_read shows it)`,
      `- not shown: dispatch://agent/${session}/artifact/p1-png@v1 (past this read's 8 pictures; dispatch_doc_read shows it)`,
    ]);
    // The two past the limit are named without being read.
    expect(server.requests.filter((pathname) => pathname.includes("p0-png"))).toEqual([]);
  });

  test("dispatch_read keeps to 10 MiB of pictures and describes one over 5 MiB, fetching neither", async () => {
    const artifacts = [
      { ...image("a", "a.png", 4 * MiB), slug: "a-png" },
      { ...image("b", "b.png", 4 * MiB), slug: "b-png" },
      { ...image("c", "c.png", 4 * MiB), slug: "c-png" },
      { ...image("d", "d.png", 5 * MiB + 1), slug: "d-png" },
      { ...image("e", "e.svg", 900, "image/svg+xml"), slug: "e-svg" },
    ];
    const bytes = { a: png(4 * MiB), b: png(4 * MiB), c: png(4 * MiB) };
    const thread = conversation([["e-svg"], ["d-png"], ["c-png"], ["b-png"], ["a-png"]]);
    const server = pictureServer(artifacts, bytes, (pathname) =>
      pathname === `/api/v1/messages/${conversationId}` ? json(thread) : undefined
    );

    const result = await run("dispatch_read", { message: conversationId }, server.fetchImpl);

    expect(result.images?.map((shown) => shown.data.length)).toEqual([
      base64(bytes.a).length,
      base64(bytes.b).length,
    ]);
    expect(result.text).toContain(
      [
        "Pictures:",
        `- image 1: dispatch://agent/${session}/artifact/a-png@v1 (a.png, image/png, 4,194,304 bytes)`,
        `- image 2: dispatch://agent/${session}/artifact/b-png@v1 (b.png, image/png, 4,194,304 bytes)`,
        `- not shown: dispatch://agent/${session}/artifact/c-png@v1 (past this read's 10 MiB of pictures; dispatch_doc_read shows it)`,
        `- not shown: dispatch://agent/${session}/artifact/d-png@v1 (d.png is 5,242,881 bytes, over the 5 MiB a model is shown)`,
        `- not shown: dispatch://agent/${session}/artifact/e-svg@v1 (e.svg is image/svg+xml, not a PNG, JPEG, GIF or WebP a model is shown)`,
      ].join("\n")
    );
    expect(server.requests).not.toContain("/api/v1/artifacts/c/versions/1");
    expect(server.requests).not.toContain("/api/v1/artifacts/d/versions/1");
  });

  test("dispatch_read with no pictures has no Pictures section and no images", async () => {
    const thread = conversation([[]]);
    const server = pictureServer([], {}, (pathname) =>
      pathname === `/api/v1/messages/${conversationId}` ? json(thread) : undefined
    );
    const result = await run("dispatch_read", { message: conversationId }, server.fetchImpl);
    expect(result.images).toBeUndefined();
    expect(result.text).not.toContain("Pictures:");
  });

  test("dispatch_read of a comment shows the issue picture its body pins, at that version, before the graph", async () => {
    const comment = "0b6e0f4e-4e2c-4d55-9a7e-0e9b3a4c1d22";
    const bytes = png(32);
    const requests: string[] = [];
    const fetchImpl = async (url: RequestInfo | URL): Promise<Response> => {
      const { pathname } = new URL(String(url));
      requests.push(pathname);
      if (pathname === `/api/v1/comments/${comment}`) {
        return json({
          comment: {
            id: comment,
            issue_key: "DSP-42",
            author: { kind: "user", id: "sami" },
            body: "The second try:\n\n![chart.png](dispatch://DSP-42/artifact/chart-png@v2)",
            anchor: null,
            created_at: "2026-10-04T10:00:00Z",
          },
          replies: [],
        });
      }
      if (pathname === "/api/v1/issues/DSP-42/artifacts/chart-png") {
        return json({
          id: "chart-1",
          issue_key: "DSP-42",
          project: "DSP",
          slug: "chart-png",
          name: "chart.png",
          kind: "image",
          versions: [
            { number: 1, mime: "image/png", size: 99 },
            { number: 2, mime: "image/png", size: 32 },
          ],
        });
      }
      if (pathname === "/api/v1/artifacts/chart-1/versions/2") {
        return new Response(bytes, { headers: { "Content-Type": "image/png" } });
      }
      if (pathname === "/api/v1/references") return json({ code: "NOT_FOUND", error: "no" }, 404);
      throw new Error(`unexpected request: ${pathname}`);
    };

    const result = await run(
      "dispatch_read",
      { ref: `dispatch://DSP-42/comment/${comment}` },
      fetchImpl as typeof fetch
    );

    expect(result.images).toEqual([{ data: base64(bytes), mimeType: "image/png" }]);
    expect(result.text).toContain(
      "Pictures:\n- image 1: dispatch://DSP-42/artifact/chart-png@v2 (chart.png, image/png, 32 bytes)\nReferenced by:"
    );
    expect(requests).toContain("/api/v1/artifacts/chart-1/versions/2");
  });

  test("dispatch_read of an issue's log shows the pictures every kind of text in it embeds, newest first", async () => {
    const address = (n: number) => `dispatch://DSP-41/artifact/p${n}-png@v1`;
    const shows = (n: number) => `Number ${n}:\n\n![p${n}.png](${address(n)})`;
    const event = (seq: number, type: string, payload: Record<string, unknown>) => ({
      seq,
      type,
      actor: { kind: "user", id: "sami" },
      created_at: `2026-10-04T10:0${seq}:00Z`,
      payload,
    });
    const events = [
      event(1, "message.created", { body: shows(1) }),
      event(2, "comment.created", { body: shows(2) }),
      event(3, "comment.answered", { body: shows(3) }),
      event(4, "ask.opened", { question: shows(4) }),
      event(5, "ask.answered", { question: "Which?", answer: { selected: [], text: shows(5) } }),
      event(6, "ask.resolved", {
        question: "Which other?",
        resolution: { kind: "resolved", reason: shows(6) },
      }),
      event(7, "message.answered", { body: shows(7) }),
    ];
    const bytes = png(16);
    const fetchImpl = async (url: RequestInfo | URL): Promise<Response> => {
      const { pathname } = new URL(String(url));
      if (pathname === "/api/v1/issues/DSP-41") {
        return json({
          key: "DSP-41",
          title: "Pictures",
          status: "in_progress",
          route: null,
          open_asks: [],
          children: [],
          last_seq: events.length,
        });
      }
      if (pathname === "/api/v1/issues/DSP-41/events") return json(events);
      const slug = pathname.match(/^\/api\/v1\/issues\/DSP-41\/artifacts\/(p\d-png)$/)?.[1];
      if (slug !== undefined) {
        return json({
          id: slug,
          issue_key: "DSP-41",
          project: "DSP",
          slug,
          name: slug.replace("-png", ".png"),
          kind: "image",
          versions: [{ number: 1, mime: "image/png", size: bytes.length }],
        });
      }
      if (/^\/api\/v1\/artifacts\/p\d-png\/versions\/1$/.test(pathname)) {
        return new Response(bytes, { headers: { "Content-Type": "image/png" } });
      }
      throw new Error(`unexpected request: ${pathname}`);
    };

    const result = await run(
      "dispatch_read",
      { ref: "dispatch://DSP-41/log" },
      fetchImpl as typeof fetch
    );

    const shown = result.text
      .split("\n")
      .filter((line) => line.startsWith("- image "))
      .map((line) => line.split(" ")[3]);
    expect(shown).toEqual([7, 6, 5, 4, 3, 2, 1].map(address));
    expect(result.images).toHaveLength(7);
  });
});
