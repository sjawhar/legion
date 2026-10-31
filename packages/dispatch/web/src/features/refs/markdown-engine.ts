import type { HeadlessProofEditor, SoftBreaks } from "@legion/proof-editor/headless";
import { DOMSerializer, type Node as ProseMirrorNode, type Schema } from "prosemirror-model";
import { useLayoutEffect, useState } from "react";

import type { BlockSchema } from "../../api/types";
import { loadBlockSchema } from "../doc/schema";
import { renderAskBlockShell, renderTypedBlock } from "../doc/typed-block-drawing";
import type { DocumentJSON, ParsedBody, ParseRequest, ParseSchema } from "./markdown-parse.worker";
import { markdownWorkerThreshold } from "./markdown-worker-threshold";
import { referenceSpans } from "./reference-spans";
import {
  buildDispatchReference,
  buildReferencePath,
  parseDispatchReference,
  pictureReference,
  shortForm,
} from "./routes";
import { rememberUpTo, type WorkerJob, WorkerQueue } from "./worker-queue";

/** What a single newline inside a paragraph becomes, chosen per parse: `"space"` (the document
 *  default, hard-wrapped Markdown is one paragraph) or `"line"` (text whose author meant its
 *  lines as lines, a model's streamed turn in the live view). The engine's own type. */
export type { SoftBreaks };

/**
 * The one headless Proof engine every Markdown-bearing surface parses with - `MarkdownBody` for a
 * whole body, `MarkdownPreview` for the clamped line of one - so a question, a comment, a hover
 * card and a search hit all read their source the way the document editor does. Its
 * `DOMSerializer` (`pictureSerializer`) is built once with it: the schema is fixed for the
 * engine's life, and every render on the page would otherwise rebuild the same serializer.
 */
export interface MarkdownEngine {
  /** The block schema it was built from, which the parse worker is built from too, so a long
   *  body reads the same above and below the threshold. Its version names a body's parse in the
   *  answer cache. */
  readonly blockSchema: BlockSchema;
  readonly serializer: DOMSerializer;
  /** The schema this engine parses with, which the parse worker is built from too: what turns
   *  the worker's answer, a document as `ProseMirrorNode.toJSON()` gives it, back into a document
   *  of this page's own schema, the one carrying its `renderBlock`. */
  readonly schema: Schema;
  /**
   * The parsed document, or `undefined` for a source the parser refuses, which the caller shows
   * as one plain-text node. The engine parses read-only: a container directive the block schema
   * refuses (`:::note` … `:::`), or a typed block whose content breaks its content rule (an ask
   * holding a heading), is drawn as its literal fence lines around its formatted content rather
   * than refused or silently dropped, a Pandoc `::: warning` fence or a `::leaf` / `:text{` line
   * reads as paragraph text, and so does a `:::` opener inside 32 open container directives, the
   * deepest the parser nests them (`directive-container.ts`), so none of them throws. Proof's
   * schema has no node for CommonMark's raw-HTML spans (a bare tag-shaped substring outside a
   * code span or fence, e.g. `<img src=x>`) or a mark span of a kind this schema does not carry,
   * so the parser still throws for them. HTML-like text inside a code span or fence parses
   * safely as literal text, so nothing is escaped beforehand: escaping the whole source
   * regardless of context would print literal backslashes around any
   * code span or fence containing something tag-shaped. Reference-style links and images
   * (`[text][id]` with their `[id]: url` definition) resolve to the link or image they name, so
   * they do not throw.
   *
   * A throw leaves the parser's own stack mid-document, so the headless editor drops that
   * parser and the next parse builds a fresh one (about 0.03 ms): one refused body answers
   * `undefined` here and falls back to literal text, and the next turn or card someone else is
   * reading still formats.
   *
   * The read-only parse (`remarkReadOnlyDirectives` in @legion/proof-editor) drops every
   * attribute the block schema marks `server` on a typed block, so it reads at its schema
   * default: outside a document no server wrote it, so a body's `:::ask{state="answered" …}`
   * reads as the open decision its author wrote, never as an answer nobody recorded.
   */
  parse(markdown: string, softBreaks?: SoftBreaks): ProseMirrorNode | undefined;
}

type HeadlessModule = typeof import("@legion/proof-editor/headless");

let headlessModule: Promise<HeadlessModule> | undefined;
const engines = new Map<number, Promise<MarkdownEngine>>();
/** The engine the newest resolved load produced: what a body renders with synchronously. The
 *  engine serves every soft-break policy (the policy is a parse argument), and a parse that
 *  throws no longer evicts it - the headless editor drops only its own refused parser. */
let readyEngine: MarkdownEngine | undefined;

/**
 * Proof's DOM serializer, except for a picture whose address is a Dispatch reference
 * (`pictureReference` in `routes.ts`, the reading `pictureView` shares): the browser cannot fetch
 * `dispatch://`, so an `image` node naming an artifact at a version
 * (`![shot.png](dispatch://KEY/artifact/shot-png@v1)`, the syntax a pasted picture is written in)
 * becomes an empty link to the artifact's page, marked `data-dispatch-picture` and captioned by
 * its `alt` or the artifact's slug, for `collectPictureAnchors` to hand to a `DispatchPicture`
 * (`useRenderedMarkdown`). One that pins no version, or names something other than an artifact,
 * is a reference like any other and becomes a link `collectReferenceAnchors` titles. A picture on
 * another website, or an address that is no reference, is serialized as Proof serializes it.
 */
function pictureSerializer(schema: Schema): DOMSerializer {
  const base = DOMSerializer.fromSchema(schema);
  const image = base.nodes.image;
  if (image === undefined) {
    return base;
  }
  return new DOMSerializer(
    {
      ...base.nodes,
      image: (node) => {
        const picture = pictureReference(node.attrs);
        if (picture === undefined) {
          return image(node);
        }
        const { named, route } = picture;
        if (named?.version === undefined) {
          return ["a", { href: picture.source }];
        }
        return [
          "a",
          {
            "data-dispatch-picture": buildDispatchReference(route),
            "data-picture-caption": picture.caption,
            href: buildReferencePath(route),
          },
        ];
      },
    },
    base.marks
  );
}

function buildEngine(proof: HeadlessProofEditor, blockSchema: BlockSchema): MarkdownEngine {
  return {
    blockSchema,
    schema: proof.schema,
    serializer: pictureSerializer(proof.schema),
    parse(markdown, softBreaks = "space") {
      try {
        return proof.parseMarkdown(markdown, softBreaks);
      } catch {
        return undefined;
      }
    },
  };
}

function loadEngine(blockSchema: BlockSchema): Promise<MarkdownEngine> {
  const cached = engines.get(blockSchema.version);
  if (cached !== undefined) {
    return cached;
  }
  headlessModule ??= import("@legion/proof-editor/headless");
  const created = headlessModule
    .then(({ createHeadlessProof }) =>
      createHeadlessProof({
        blockSchema,
        readOnly: true,
        renderBlock: (node, type) =>
          type.name === "ask" ? renderAskBlockShell(node) : renderTypedBlock(node, type),
      })
    )
    .then((proof) => {
      const engine = buildEngine(proof, blockSchema);
      readyEngine = engine;
      return engine;
    });
  engines.set(blockSchema.version, created);
  return created;
}

/** Loads the schema and headless Proof chunk before Markdown-bearing UI needs to render. The
 *  parse worker is not one of them: a page that draws no long body downloads none of it. */
export async function warmMarkdownRenderer(): Promise<void> {
  await loadEngine(await loadBlockSchema());
}

/** How long the worker may hold one body before the page parses it itself. It runs from the
 *  moment the worker can start on that body, never while its chunk is still downloading, as the
 *  diagram renderer's own limit does (`mermaid-diagrams.ts`). */
const PARSE_TIME_LIMIT_MS = 10_000;

/** How many parsed documents the answer cache keeps; the one read longest ago goes first. A long
 *  body's document is large, and the renders that read one again are a page's own repaints, so a
 *  few are enough: the bodies on screen, and the streaming turn growing among them. */
const ANSWER_LIMIT = 4;

/**
 * One rendering of a body: the engine it renders with, the document parsed from it, and whether
 * the parse is still owed. A body of at most `markdownWorkerThreshold` characters is rendered
 * once, parsed, in the caller's own commit. A longer one is rendered at once as pending, with no
 * document - its literal text is what the reader reads meanwhile - and again with the document
 * once the worker answers.
 */
export interface MarkdownRendering {
  /** `undefined` where neither the schema nor the engine could load, and while the parse is
   *  pending on a page where they are still loading. */
  readonly engine: MarkdownEngine | undefined;
  /** The parsed document. `undefined` while `pending`, and, once not pending, for a body the
   *  parser refused or an engine that could not load: a body's one final failure, which a
   *  surface shows as its literal text. */
  readonly parsed: ProseMirrorNode | undefined;
  /** The parse is still owed - by the worker, or by a schema and engine chunk still loading. */
  readonly pending: boolean;
  /** `mermaid-diagrams.ts`'s open-fence check, as the worker made it with the parse it answered;
   *  `undefined` where the page must make it itself. */
  readonly openMermaidFence: boolean | undefined;
}

/** Whether a body is parsed in the worker rather than on the page's own thread.
 *  `useRenderedMarkdown` reads it too: such a body's first paint is the one put into the page a
 *  batch at a time, since it is the one that lands after its own commit. */
export function parsesInTheWorker(markdown: string): boolean {
  return markdown.length > markdownWorkerThreshold;
}

/** Creates the parse worker; `setParseWorkerFactory` replaces it for a test. */
export type ParseWorkerFactory = () => Worker;

/** A body's parse: the worker's answer to it, or the page's own where no worker could give one. */
interface ParsedAnswer {
  /** The parsed document; `undefined` for a body the parser refused. */
  readonly parsed: ProseMirrorNode | undefined;
  /** The open-fence check made with the parse; `undefined` where the page must make it. */
  readonly openMermaidFence: boolean | undefined;
}

/** One body the parse worker is parsing, or is about to (`worker-queue.ts`). */
interface SentBody extends WorkerJob {
  /** The engine its answer becomes a document of, and that parses it here if the worker cannot. */
  readonly engine: MarkdownEngine;
  /** Its entry in the answer cache, and its place in the queue: the schema version, the
   *  soft-break policy and the source. */
  readonly key: string;
  readonly markdown: string;
  readonly softBreaks: SoftBreaks;
}

let createParseWorker: ParseWorkerFactory | undefined;
/** `PARSE_TIME_LIMIT_MS`, unless a test shortened it through `setParseDeadline`. */
let deadlineMs = PARSE_TIME_LIMIT_MS;
const answers = new Map<string, ParsedAnswer>();

/**
 * The document `json` describes, as a document of `schema`.
 *
 * `Schema.nodeFromJSON` is not what builds it: that checks every attribute against the type its
 * schema gives it, and this page's own parser writes values its schema does not - milkdown's
 * Markdown parser writes `title: null` for an image with no title, where `image.title` is a string
 * defaulting to `""` - so a body holding a picture could never be rebuilt, though the page's own
 * parse of it holds exactly the same attribute. That check is for JSON from somewhere else; this
 * JSON is the page's own worker's parse of the body, with the page's own schema.
 */
function documentFromJSON(schema: Schema, json: DocumentJSON): ProseMirrorNode {
  const marks = json.marks?.map(({ attrs, type }) => schema.marks[type].create(attrs));
  if (json.type === "text") {
    return schema.text(json.text ?? "", marks);
  }
  const content = json.content?.map((child) => documentFromJSON(schema, child));
  return schema.nodes[json.type].create(json.attrs, content, marks);
}

/** Parses `body` here, for a worker that could not: the reader waits for a parse on their own
 *  thread, and then reads the body formatted. */
function parseOnThePage(body: SentBody): ParsedAnswer {
  const answer: ParsedAnswer = {
    openMermaidFence: undefined,
    parsed: body.engine.parse(body.markdown, body.softBreaks),
  };
  rememberUpTo(answers, ANSWER_LIMIT, body.key, answer);
  return answer;
}

/**
 * The page's own worker: this module's sibling, which Vite bundles as a chunk of its own
 * (`markdown-parse.worker-<hash>.js`), fetched the first time a page draws a long body. The
 * `new Worker(new URL(…), { type: "module" })` below is written as one expression because that is
 * the shape a bundler recognises; split in two, the worker's module is copied out as an asset and
 * never bundled.
 */
function newParseWorker(): Worker {
  return new Worker(new URL("./markdown-parse.worker.ts", import.meta.url), { type: "module" });
}

/**
 * The bodies waiting on the parse worker, through the queue both workers of this folder share
 * (`worker-queue.ts`). A body the worker cannot answer - its deadline ran out, the worker
 * crashed, or none could be created at all - is parsed on the page instead, so a reader always
 * ends with a formatted body; only the parser refusing a body ends in its literal text.
 *
 * The worker is kept rather than terminated where the page moves on from the body it holds
 * (`stopAnAbandonedJob: false`): it carries a module of its own and builds a headless engine
 * before it can answer anything, which costs more than the one stale parse it would save. A turn
 * streaming into the live view therefore leaves the worker exactly one stale frame - the one it
 * was already parsing - and the frame the reader is waiting for is posted the moment that answer
 * lands, rather than queueing behind every frame the stream has written since. That stale frame
 * is the newest parse the reader can be given until the stream pauses, so the body that asked
 * for it is still shown it (`keepAPostedParse`), rather than watching an unchanging paint.
 */
const parseQueue = new WorkerQueue<SentBody, ParsedBody, ParsedAnswer>({
  stopAnAbandonedJob: false,
  answerOf(body, reply) {
    if (reply.doc === undefined) {
      // The parser refused the body: its one final failure, which reads as its literal text.
      const refused: ParsedAnswer = { openMermaidFence: undefined, parsed: undefined };
      rememberUpTo(answers, ANSWER_LIMIT, body.key, refused);
      return refused;
    }
    let answer: ParsedAnswer;
    try {
      answer = {
        openMermaidFence: reply.openMermaidFence,
        parsed: documentFromJSON(body.engine.schema, reply.doc),
      };
    } catch (error) {
      console.error(
        "Parsing a long Markdown body on the page, its answer does not fit the schema",
        error
      );
      return parseOnThePage(body);
    }
    rememberUpTo(answers, ANSWER_LIMIT, body.key, answer);
    return answer;
  },
  createWorker(first) {
    const worker = (createParseWorker ?? newParseWorker)();
    // The page loads one schema version per session (`doc/schema.ts`), so the worker is built
    // from the schema of the first long body it is sent and parses every later one the same way.
    worker.postMessage({ blockSchema: first.engine.blockSchema } satisfies ParseSchema);
    return worker;
  },
  limitMs() {
    return deadlineMs;
  },
  noWorkerFrom(error) {
    console.error(
      "Parsing long Markdown bodies on the page, the parse worker is unavailable",
      error
    );
  },
  post(worker, id, body) {
    worker.postMessage({
      id,
      markdown: body.markdown,
      softBreaks: body.softBreaks,
    } satisfies ParseRequest);
  },
  withoutAWorker: parseOnThePage,
  withoutTheWorker: parseOnThePage,
});

/**
 * Replaces the page's own `new Worker(…)` with `factory`, for the rows that drive a faked worker
 * (`parse-worker-fixture.ts`); `undefined` puts the page's own back. Installing one leaves the
 * engine holding no worker: the one it held is terminated and every body it held is parsed on
 * the page, so nothing a page asked the worker before reaches the next one.
 */
export function setParseWorkerFactory(factory: ParseWorkerFactory | undefined): void {
  parseQueue.recover();
  parseQueue.clear(parseOnThePage);
  createParseWorker = factory;
}

/** How long the worker may hold one body, in place of `PARSE_TIME_LIMIT_MS` until the next call,
 *  so a row that proves what the deadline ends in need not wait it out, as
 *  `resetMermaidDiagrams` does for the diagram renderer's limit. With no argument it puts the
 *  page's own deadline back. */
export function setParseDeadline(limitMs = PARSE_TIME_LIMIT_MS): void {
  deadlineMs = limitMs;
}

/** What a caller of `renderWithEngine` wants beyond the rendering itself. */
export interface RenderWithEngineOptions {
  /** Keeps the caller's place on a parse the worker has already been posted when the render is
   *  cancelled, so that answer still reaches `render` (`worker-queue.ts`, `keepIfPosted`). Every
   *  element `useRenderedMarkdown` draws asks for it, whatever surface it is on: the streaming
   *  turn is where it matters - its reader would otherwise watch a body that never changes -
   *  and no other surface is harmed by it, since an element decides for itself what an answer to
   *  a source it has moved on from is worth (sjawhar/legion#2519). A caller that asks must
   *  expect such an answer, and gets nothing in place of one: the deadline and a worker that
   *  never runs answer a kept place with no parse at all, since the render that asked has gone.
   *  `useMarkdownHeadline` is the caller that does not ask - a headline of a source it has moved
   *  on from would read as the wrong one - and off is the default: a cancelled render runs no
   *  more. */
  readonly keepAPostedParse?: boolean;
}

/** Renders `markdown` with a loaded engine: parsed here for a body up to the threshold; for a
 *  longer one the answer already cached for it, or a pending rendering now and the parsed
 *  document once the worker answers. */
function renderLoaded(
  engine: MarkdownEngine,
  markdown: string,
  softBreaks: SoftBreaks,
  render: (rendering: MarkdownRendering) => void,
  { keepAPostedParse = false }: RenderWithEngineOptions
): () => void {
  if (!parsesInTheWorker(markdown)) {
    render({
      engine,
      openMermaidFence: undefined,
      parsed: engine.parse(markdown, softBreaks),
      pending: false,
    });
    return () => undefined;
  }
  // The schema version, the soft-break policy and the source are all a parse reads, so one
  // answer serves every later render of the same three: a new decision card, paint or decorate
  // pass repaints the body it already has rather than asking for it again.
  const key = `${engine.blockSchema.version}\u0000${softBreaks}\u0000${markdown}`;
  const cached = answers.get(key);
  if (cached !== undefined) {
    // Reading an answer keeps it: what the cache drops is the body read longest ago, never one
    // the page is still repainting.
    answers.delete(key);
    answers.set(key, cached);
    render({
      engine,
      openMermaidFence: cached.openMermaidFence,
      parsed: cached.parsed,
      pending: false,
    });
    return () => undefined;
  }
  // What the reader has until the answer lands is the body's own text.
  render({ engine, openMermaidFence: undefined, parsed: undefined, pending: true });
  return parseQueue.wait(
    key,
    () => ({ engine, key, markdown, softBreaks }),
    ({ openMermaidFence, parsed }) => {
      render({ engine, openMermaidFence, parsed, pending: false });
    },
    { keepIfPosted: keepAPostedParse }
  );
}

/**
 * Renders `markdown` under the soft-break policy `softBreaks`, through the shared engine.
 *
 * A body of at most `markdownWorkerThreshold` characters is parsed on the page's own thread and
 * rendered synchronously, where a load has already resolved, so it renders in the layout phase of
 * the commit that mounts it (a list that inserts a turn sees the turn's full height in that same
 * commit, and `ViewportAnchor` compensates in one measurement); otherwise once the schema and the
 * engine chunk arrive. Only the very first render on a page, or a schema that failed to load,
 * takes that asynchronous path. Without the server schema (or Proof's headless engine) `render`
 * gets no engine and no document, and the surface shows the literal text instead: readable, never
 * lost. The cause is reported and the schema cache does not retain the failure, so the next render
 * tries the fetch again.
 *
 * A longer body is parsed in the parse worker (`markdown-parse.worker.ts`), created on the first
 * one a page draws. `render` is called at once with a pending rendering, so the reader has the
 * body's own text, and again with the parsed document when the worker answers. A worker that
 * cannot be created, whose chunk does not load, that crashes, or that holds a body past its
 * deadline leaves that body to the page's own thread, so a reader always ends with a formatted
 * body; only the parser refusing a body ends in its literal text. That is for a render still
 * waiting: a place kept on a posted parse belongs to a render already cancelled, so it takes
 * the worker's own answer or none, and the page parses nothing for it (sjawhar/legion#2519).
 *
 * Returns the cancel: a cancelled `render` never runs again - unless the caller asked to keep a
 * posted parse (`RenderWithEngineOptions`) and the worker is holding that body, whose answer
 * still reaches it - and the body it was waiting for is parsed for whoever else waits.
 */
export function renderWithEngine(
  markdown: string,
  softBreaks: SoftBreaks,
  render: (rendering: MarkdownRendering) => void,
  options: RenderWithEngineOptions = {}
): () => void {
  if (readyEngine !== undefined) {
    return renderLoaded(readyEngine, markdown, softBreaks, render, options);
  }
  let cancelled = false;
  let cancelAnswer: (() => void) | undefined;
  if (parsesInTheWorker(markdown)) {
    // The schema and the engine chunk are still loading, and the body is one the worker will
    // parse: the reader has its text now, as they would while the worker parsed it.
    render({ engine: undefined, openMermaidFence: undefined, parsed: undefined, pending: true });
  }
  void (async () => {
    let engine: MarkdownEngine | undefined;
    try {
      engine = await loadEngine(await loadBlockSchema());
    } catch (error) {
      console.error("Rendering literal Markdown, the block schema is unavailable", error);
    }
    if (cancelled) {
      return;
    }
    if (engine === undefined) {
      render({ engine: undefined, openMermaidFence: undefined, parsed: undefined, pending: false });
      return;
    }
    cancelAnswer = renderLoaded(engine, markdown, softBreaks, render, options);
  })();
  return () => {
    cancelled = true;
    cancelAnswer?.();
  };
}

/** `.dispatch-markdown` in styles.css overrides Tailwind Typography's fixed palette, heading
 * scale and font size, so a rendered body takes the surrounding text's size and colour. */
export const markdownClassName =
  "dispatch-markdown prose prose-sm prose-slate break-words dark:prose-invert";

/** Text outside code with every `dispatch://` reference written as its short form
 *  (`dispatch://CORE-1/ask/a1` reads `CORE-1 ask`), as a rendered body's link reads until its
 *  title resolves; a reference in code stays as its author wrote it, as it does in a body. */
function shortenReferences(text: string): string {
  let shortened = "";
  let cursor = 0;
  for (const { start, value } of referenceSpans(text, "dispatch://")) {
    const route = parseDispatchReference(value);
    if (route !== undefined) {
      shortened += `${text.slice(cursor, start)}${shortForm(route)}`;
      cursor = start + value.length;
    }
  }
  return shortened + text.slice(cursor);
}

/** The words a parsed document renders, on one line: text as it reads (references shortened),
 *  raw inline HTML as the literal characters a body shows for it, and every block boundary,
 *  hard break or other leaf a space. */
function plainWords(doc: ProseMirrorNode): string {
  const parts: string[] = [];
  doc.descendants((node, _position, parent) => {
    if (node.isText) {
      const code =
        parent?.type.spec.code === true || node.marks.some((mark) => mark.type.spec.code === true);
      parts.push(code ? (node.text ?? "") : shortenReferences(node.text ?? ""));
    } else if (node.isBlock || node.isLeaf) {
      parts.push(node.type.name === "html" ? String(node.attrs.value ?? "") : " ");
    }
    return true;
  });
  return parts.join("");
}

/** A headline and whether it had to be cut to fit. */
export interface MarkdownHeadline {
  readonly text: string;
  readonly cut: boolean;
}

/**
 * Markdown projected to one line of plain text and cut to `max` characters, for a place that can
 * hold only a string: a reference link's text, a card's title, a hover tooltip. Formatting goes
 * (a link's own styling is the formatting there) but no syntax stays: `**Blocking:** the deploy`
 * reads `Blocking: the deploy`, a code span keeps its words, a `dispatch://` reference reads as
 * its short form, and the cut happens on the projected text, so it never splits a mark (nor a
 * character outside the Basic Multilingual Plane). Blocks and hard breaks become single spaces.
 * `cut` says whether the text was cut, which an ellipsis the author typed cannot fake.
 * `undefined` until the engine has answered (one pre-paint commit once it is loaded, and for a
 * body the parse worker parses, until its answer lands) and for an `undefined` source; the
 * literal source, cut the same way, when the engine cannot load.
 */
export function useMarkdownHeadline(
  markdown: string | undefined,
  max: number
): MarkdownHeadline | undefined {
  const [headline, setHeadline] = useState<MarkdownHeadline>();
  useLayoutEffect(() => {
    if (markdown === undefined) {
      setHeadline(undefined);
      return;
    }
    return renderWithEngine(markdown, "space", ({ parsed, pending }) => {
      if (pending) {
        // A long body's headline waits for the worker rather than reading as its own source,
        // and the source before it goes with it: `undefined` is what this hook documents for a
        // body whose parse is still owed, where the headline left in place would be the one a
        // streaming turn's last frame produced.
        setHeadline(undefined);
        return;
      }
      const characters = Array.from(
        (parsed === undefined ? markdown : plainWords(parsed)).replace(/\s+/g, " ").trim()
      );
      const cut = characters.length > max;
      setHeadline({
        cut,
        text: cut ? `${characters.slice(0, max).join("")}…` : characters.join(""),
      });
    });
  }, [markdown, max]);
  return headline;
}
