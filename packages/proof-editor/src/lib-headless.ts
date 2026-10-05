/**
 * Proof Editor — headless (Node/Bun, no DOM) markdown <-> ProseMirror entry
 * point, published alongside ./lib.ts as @sjawhar/proof-editor/headless.
 *
 * Ported from server/milkdown-headless.ts's `buildHeadless()`: the same
 * Milkdown schema plugin list the browser editor uses (commonmark, gfm,
 * frontmatter, code-block-ext, proof marks) plus this library's own
 * dispatchAsk mark (./dispatch-marks.ts), so a document parsed/serialized
 * here uses exactly the same ProseMirror schema as the browser editor. The
 * fallback/warm-up machinery in that file (parseMarkdownWithHtmlFallback,
 * stripStandaloneHtmlLines, the warm/singleton cache) is intentionally not
 * ported — this is a plain factory a host calls once and reuses, mirroring
 * `createProofEditor`'s own shape.
 */

import { Editor, editorViewCtx, marksCtx, nodesCtx, remarkStringifyOptionsCtx } from '@milkdown/core';
import { schema as commonmarkSchema } from '@milkdown/preset-commonmark';
import { type BlockIdGenerator, blockIdSchemas, mintBlockId, withBlockIds } from './editor/schema/block-ids';
import {
  blockSchemaPlugins,
  remarkTypedBlocks,
  type BlockSchema,
} from './block-schema.js';
import { configureDispatchLinks } from './dispatch-links.js';
import { schema as gfmSchema } from '@milkdown/preset-gfm';
import { Schema, type Node as ProseMirrorNode } from '@milkdown/prose/model';
import { ParserState, SerializerState } from '@milkdown/transformer';
import remarkFrontmatter from 'remark-frontmatter';
import remarkGfm from 'remark-gfm';
import remarkParse from 'remark-parse';
import remarkStringify from 'remark-stringify';
import type { Options as RemarkStringifyOptions } from 'remark-stringify';
import { unified } from 'unified';

import { codeBlockExtPlugins } from 'proof-sdk-upstream/src/editor/schema/code-block-ext.js';
import { frontmatterSchema } from 'proof-sdk-upstream/src/editor/schema/frontmatter.js';
import { proofMarkPlugins } from 'proof-sdk-upstream/src/editor/schema/proof-marks.js';
import { remarkProofMarks, proofMarkHandler } from 'proof-sdk-upstream/src/formats/remark-proof-marks.js';
import { dispatchMarkPlugins, remarkDispatchMarks, dispatchMarkHandler } from './dispatch-marks.js';
import remarkInlineLinks from 'remark-inline-links';
import { remarkSoftBreakAsLine, remarkSoftBreakAsSpace } from './dispatch-soft-breaks.js';
import { remarkContainerDirectives } from './lib-remark-directive-plugin.js';

/**
 * What a single newline inside a paragraph (a CommonMark soft break) becomes. `'space'`, the
 * default, is what a document means by it: agents hard-wrap Markdown at a column, and the
 * lines are one paragraph (`remarkSoftBreakAsSpace`). `'line'` keeps it as a line break, for
 * text whose author meant the lines as lines: a model's streamed turn in the live view, where
 * `First, check the config.\nThen, verify the credentials.` is two lines, and joining them
 * reads as the one run-on line the document default would make of it.
 */
export type SoftBreaks = 'space' | 'line';

export interface HeadlessProofEditor {
  schema: Schema;
  /**
   * Parses markdown into a document whose every block carries a `blockId`, reading a soft
   * break as `softBreaks` says (`'space'` by default).
   *
   * Throws for a source the schema has no node for: a raw-HTML block or span (a bare
   * tag-shaped substring outside a code span or fence, e.g. `<img src=x>`) and a mark span of
   * a kind this schema does not carry. A throw leaves the parser's own stack mid-document,
   * which would make the next parses of unrelated text throw too, so the parser of that
   * policy is dropped and the next parse through it starts on a fresh one.
   */
  parseMarkdown(markdown: string, softBreaks?: SoftBreaks): ProseMirrorNode;
  serializeMarkdown(doc: ProseMirrorNode): string;
}

export interface HeadlessProofOptions {
  /** Mints block ids for parsed documents; defaults to random uuids. Fixture
   *  generators and tests pass a deterministic one. */
  blockId?: BlockIdGenerator;
  /** Typed block schema fetched from the document service before construction. */
  blockSchema?: BlockSchema;
}

export async function createHeadlessProof(options: HeadlessProofOptions = {}): Promise<HeadlessProofEditor> {
  const editor = Editor.make();
  const ctx = editor.ctx;

  // These slices are normally injected by Milkdown's init plugin. We only need enough
  // context for schema plugins to materialize their node/mark specs.
  ctx.inject(nodesCtx, []);
  ctx.inject(marksCtx, []);
  ctx.inject(remarkStringifyOptionsCtx, { handlers: {}, encode: [] } as RemarkStringifyOptions);

  // Some schema serializers reference the editor view (e.g. paragraph serialization
  // checks if the node is the last block). Provide a minimal stub updated per
  // serialization run to avoid ctx lookup errors in headless mode.
  let currentDoc: ProseMirrorNode | null = null;
  ctx.inject(editorViewCtx, {
    state: {
      get doc() {
        return currentDoc;
      },
    },
  } as never);

  const plugins = [
    ...commonmarkSchema,
    ...gfmSchema,
    // Frontmatter must be registered after commonmark so `---` parses as YAML.
    ...frontmatterSchema,
    ...codeBlockExtPlugins,
    ...(options.blockSchema ? blockSchemaPlugins(options.blockSchema) : []),
    ...blockIdSchemas,
    // Some schema nodes reference proof marks (e.g. code_block allows them).
    ...proofMarkPlugins,
    ...dispatchMarkPlugins,
  ].flat();

  for (const plugin of plugins) {
    const runner = plugin(ctx);
    if (typeof runner === 'function') {
      // Schema plugins are async but typically complete synchronously.
      await runner();
    }
  }

  configureDispatchLinks(ctx);
  const nodes = Object.fromEntries(ctx.get(nodesCtx) as never);
  const marks = Object.fromEntries(ctx.get(marksCtx) as never);
  const schema = new Schema({ nodes, marks });

  // Match the browser editor's GFM features (tables, task lists, strikethrough,
  // autolinks, ...), plus proof span and dispatchAsk span parsing. The schema and plugin list
  // above are policy-independent and built once; only the soft-break transform differs, so a
  // parser per policy is built the first time that policy is parsed with and reused.
  const buildParser = (softBreaks: SoftBreaks): ((markdown: string) => ProseMirrorNode) => {
    const parseProcessor = unified()
      .use(remarkParse)
      .use(remarkFrontmatter, ['yaml'])
      .use(remarkGfm)
      .use(remarkContainerDirectives)
      .use(remarkProofMarks)
      .use(remarkDispatchMarks)
      // Reference-style links and images (`[text][id]` with `[id]: url` elsewhere in the body)
      // become the inline link or image their definition describes, and the definitions render
      // as nothing, as CommonMark reads them; the schema has no node for an unresolved reference,
      // so without this the parse throws and the whole body falls back to its literal source.
      // The live editor's commonmark preset installs the same plugin on its own import parser.
      .use(remarkInlineLinks);
    // remark leaves a soft break as a "\n" inside its text node; the policy either joins the
    // lines with a space or splits them around a `break` node, which the commonmark preset's
    // hardbreak parser takes.
    parseProcessor.use(softBreaks === 'line' ? remarkSoftBreakAsLine : remarkSoftBreakAsSpace);
    if (options.blockSchema) parseProcessor.use(remarkTypedBlocks, options.blockSchema);
    return ParserState.create(schema as never, parseProcessor as never) as unknown as (
      markdown: string,
    ) => ProseMirrorNode;
  };

  const parsers = new Map<SoftBreaks, (markdown: string) => ProseMirrorNode>();
  const mint = options.blockId ?? mintBlockId;
  const parseMarkdown = (markdown: string, softBreaks: SoftBreaks = 'space'): ProseMirrorNode => {
    let parse = parsers.get(softBreaks);
    if (parse === undefined) {
      parse = buildParser(softBreaks);
      parsers.set(softBreaks, parse);
    }
    try {
      return withBlockIds(parse(markdown), mint);
    } catch (error) {
      // A throw leaves this ParserState's stack open, so its next parses would throw too. Drop
      // it; the next parse of this policy builds a fresh one (about 0.03 ms).
      parsers.delete(softBreaks);
      throw error;
    }
  };

  const serializeProcessor = unified()
    .use(remarkGfm)
    .use(remarkFrontmatter, ['yaml'])
    .use(remarkContainerDirectives)
    .use(remarkStringify, {
      handlers: {
        proofMark: proofMarkHandler,
        dispatchMark: dispatchMarkHandler,
      },
    } as RemarkStringifyOptions);
  const serializer = SerializerState.create(schema as never, serializeProcessor as never) as unknown as (
    doc: ProseMirrorNode,
  ) => string;
  const serializeMarkdown = (doc: ProseMirrorNode): string => {
    currentDoc = doc;
    return serializer(doc);
  };

  return { schema, parseMarkdown, serializeMarkdown };
}

export { BLOCK_ID_ATTR, blockIdOf, isIdentifiedBlock, withBlockIds } from './editor/schema/block-ids';
export type { BlockIdGenerator } from './editor/schema/block-ids';
export type {
  BlockAttributeKind,
  BlockAttributeSchema,
  BlockSchema,
  BlockTypeSchema,
} from './block-schema.js';

export default createHeadlessProof;
