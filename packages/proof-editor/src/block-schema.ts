import { $nodeSchema } from '@milkdown/kit/utils';
import { DOMParser, type DOMOutputSpec, type Fragment, type Node as ProseMirrorNode, type Schema } from '@milkdown/kit/prose/model';
import type { MarkdownNode as ParsedMarkdownNode, NodeSchema } from '@milkdown/kit/transformer';

import { withBlockIdSpec } from './editor/schema/block-ids.js';
import { withDomAttributes } from './editor/schema/dom-attributes.js';
import { maxDirectiveNesting } from './directive-container.js';
import { markdownSource } from './typed-block-drawing.js';

export type BlockAttributeKind = 'string' | 'bool' | 'enum' | 'string[]' | 'actor' | 'timestamp';

/**
 * From which schema version an editor reads an element of the manifest (`since`) and from which
 * it may create one (`creatable_since`); each is 1 when the manifest names none. An editor built
 * from a schema reads every element the schema declares and creates the ones whose
 * `creatable_since` is at most the schema's `version` (`creatable`).
 */
export interface BlockElementVersions {
  since?: number;
  creatable_since?: number;
}

export interface BlockAttributeSchema extends BlockElementVersions {
  kind: BlockAttributeKind;
  choices?: readonly string[];
  default?: string | boolean | readonly string[];
  server?: boolean;
  /** The numbers of each choice that has some, by choice: the choices stay plain strings. */
  choice_versions?: Readonly<Record<string, BlockElementVersions>>;
}

export interface BlockTypeSchema extends BlockElementVersions {
  name: string;
  content: string;
  render: 'host';
  attributes: Readonly<Record<string, BlockAttributeSchema>>;
}

/** A node or a mark of the Proof schema, as the manifest names it. */
export interface BlockSchemaElement extends BlockElementVersions {
  name: string;
}

export interface BlockSchema {
  version: number;
  /** The Proof schema's nodes and marks; a document service older than the manifest names none. */
  nodes?: readonly BlockSchemaElement[];
  marks?: readonly BlockSchemaElement[];
  types: readonly BlockTypeSchema[];
}

/** Whether an editor of schema version `version` may create `element`. */
export function creatable(version: number, element: BlockElementVersions | undefined): boolean {
  return (element?.creatable_since ?? 1) <= version;
}

/** Whether an editor of schema version `version` may write `value` into `attribute`. */
export function creatableValue(version: number, attribute: BlockAttributeSchema, value: unknown): boolean {
  if (!creatable(version, attribute)) return false;
  return attribute.kind !== 'enum' || typeof value !== 'string' || creatable(version, attribute.choice_versions?.[value]);
}

/**
 * The manifest's numbers of the node or mark `name` of the Proof schema (a typed block is a
 * node), or undefined when the manifest names none.
 */
function elementVersions(schema: BlockSchema, kind: 'node' | 'mark', name: string): BlockElementVersions | undefined {
  if (kind === 'mark') return schema.marks?.find((mark) => mark.name === name);
  return schema.nodes?.find((node) => node.name === name) ?? schema.types.find((type) => type.name === name);
}

/** Why an editor built from `schema` may not create `node` itself, its marks or its attributes. */
function uncreatableNode(schema: BlockSchema, node: ProseMirrorNode): string | undefined {
  const name = node.type.name;
  const type = schema.types.find((candidate) => candidate.name === name);
  if (!creatable(schema.version, elementVersions(schema, 'node', name))) {
    return `this editor cannot create ${type ? 'typed block' : 'node'} ${JSON.stringify(name)} yet`;
  }
  for (const mark of node.marks) {
    if (!creatable(schema.version, elementVersions(schema, 'mark', mark.type.name))) {
      return `this editor cannot create mark ${JSON.stringify(mark.type.name)} yet`;
    }
  }
  for (const [attributeName, attribute] of Object.entries(type?.attributes ?? {})) {
    const value: unknown = node.attrs[attributeName];
    if (value !== undefined && value !== null && !creatableValue(schema.version, attribute, value)) {
      return `this editor cannot write ${JSON.stringify(value)} into typed block ${JSON.stringify(name)} attribute ${JSON.stringify(attributeName)} yet`;
    }
  }
  return undefined;
}

/**
 * Throws when `content`, which an editor built from `schema` is about to write, holds a node, a
 * mark, a typed block, an attribute or an enum choice that the editor may not create yet. A typed
 * block holds such an attribute only when its source names one (`schemaAttrs`).
 */
export function assertCreatable(schema: BlockSchema, content: Fragment): void {
  let refusal: string | undefined;
  content.descendants((node) => {
    refusal ??= uncreatableNode(schema, node);
    return refusal === undefined;
  });
  if (refusal !== undefined) throw new Error(refusal);
}

/**
 * `schema`'s DOM parser, which reads pasted and dropped HTML, without the rules of each node and
 * mark that an editor built from `blockSchema` may not create yet: such an element is not read,
 * and its content is kept without it, as a typed block's section with an attribute the editor
 * may not write is (`parsedDomAttrs`).
 */
export function creatableDomParser(schema: Schema, blockSchema: BlockSchema): DOMParser {
  const rules = DOMParser.fromSchema(schema).rules.filter((rule) => {
    if ('node' in rule && rule.node !== undefined) return creatable(blockSchema.version, elementVersions(blockSchema, 'node', rule.node));
    return rule.mark === undefined || creatable(blockSchema.version, elementVersions(blockSchema, 'mark', rule.mark));
  });
  return new DOMParser(schema, rules);
}

export type HostBlockRenderer = (node: ProseMirrorNode, type: BlockTypeSchema) => DOMOutputSpec;

type MarkdownNode = {
  type: string;
  name?: string;
  attributes?: Record<string, string>;
  children?: MarkdownNode[];
  position?: {
    start?: { line?: number; offset?: number };
    end?: { offset?: number };
  };
};

function directiveName(name: string): boolean {
  return /^[A-Za-z0-9_-]+$/.test(name);
}

function knownNames(schema: BlockSchema): string {
  return [...schema.types].map(({ name }) => name).sort().join(', ');
}

export function validateBlockSchema(schema: BlockSchema): void {
  if (!Number.isInteger(schema.version) || schema.version <= 0) {
    throw new Error('block schema version must be a positive integer');
  }
  if (schema.types.length === 0) throw new Error('block schema must declare at least one type');
  const names = new Set<string>();
  for (const type of schema.types) {
    if (!directiveName(type.name)) throw new Error(`typed block name ${JSON.stringify(type.name)} is invalid`);
    if (names.has(type.name)) throw new Error(`typed block ${JSON.stringify(type.name)} is declared twice`);
    names.add(type.name);
    if (typeof type.content !== 'string' || type.content.trim() === '') {
      throw new Error(`typed block ${JSON.stringify(type.name)} must declare a non-empty content rule`);
    }
    if (type.render !== 'host') throw new Error(`typed block ${JSON.stringify(type.name)} has unsupported render ${JSON.stringify(type.render)}`);
    for (const [name, attribute] of Object.entries(type.attributes)) {
      if (!directiveName(name)) throw new Error(`typed block ${JSON.stringify(type.name)} attribute ${JSON.stringify(name)} is invalid`);
      if (attribute.kind === 'enum' && (attribute.choices?.length ?? 0) === 0) {
        throw new Error(`typed block ${JSON.stringify(type.name)} attribute ${JSON.stringify(name)} enum has no choices`);
      }
    }
  }
}

function parseAttribute(typeName: string, name: string, attribute: BlockAttributeSchema, value: string): string | boolean | string[] {
  switch (attribute.kind) {
    case 'string':
    case 'actor':
    case 'timestamp':
      return value;
    case 'bool':
      if (value !== 'true' && value !== 'false') throw new Error(`typed block ${JSON.stringify(typeName)} attribute ${JSON.stringify(name)} must be true or false`);
      return value === 'true';
    case 'enum':
      if (!attribute.choices?.includes(value)) {
        throw new Error(`typed block ${JSON.stringify(typeName)} attribute ${JSON.stringify(name)} must be one of ${attribute.choices?.join(', ')}, got ${JSON.stringify(value)}`);
      }
      return value;
    case 'string[]': {
      let values: unknown;
      try {
        values = JSON.parse(value);
      } catch {
        throw new Error(`typed block ${JSON.stringify(typeName)} attribute ${JSON.stringify(name)} must be a JSON string array`);
      }
      if (!Array.isArray(values) || values.some((item) => typeof item !== 'string')) {
        throw new Error(`typed block ${JSON.stringify(typeName)} attribute ${JSON.stringify(name)} must be a JSON string array`);
      }
      return values;
    }
  }
}

/**
 * A block that names no value for an attribute holds its default, unless an editor of schema
 * version `version` may not create the attribute yet: that one is left out, so the editor never
 * writes it.
 */
function schemaAttrs(type: BlockTypeSchema, version: number): Record<string, { default?: string | boolean | readonly string[] }> {
  return Object.fromEntries(Object.entries(type.attributes).map(([name, attribute]) => [name, { default: creatable(version, attribute) ? attribute.default : undefined }]));
}

function directiveAttrs(type: BlockTypeSchema, attrs: Record<string, string> | undefined, version: number): Record<string, string | boolean | readonly string[] | undefined> {
  const parsed: Record<string, string | boolean | readonly string[] | undefined> = {};
  for (const [name, attribute] of Object.entries(type.attributes)) {
    const value = attrs?.[name];
    parsed[name] = value === undefined ? (creatable(version, attribute) ? attribute.default : undefined) : parseAttribute(type.name, name, attribute, value);
  }
  for (const name of Object.keys(attrs ?? {})) {
    if (name !== 'id' && name !== 'className' && !(name in type.attributes)) {
      throw new Error(`typed block ${JSON.stringify(type.name)} does not declare attribute ${JSON.stringify(name)}`);
    }
    if (name === 'className') {
      throw new Error(`typed block ${JSON.stringify(type.name)} does not declare attribute "class"`);
    }
  }
  if (attrs?.id) parsed.blockId = attrs.id;
  return parsed;
}

/** An attribute value as markdown and HTML carry it, the inverse of `parseAttribute`. */
function attributeText(attribute: BlockAttributeSchema, value: unknown): string {
  return attribute.kind === 'bool' ? String(value) : attribute.kind === 'string[]' ? JSON.stringify(value) : String(value);
}

function markdownAttrs(type: BlockTypeSchema, node: ProseMirrorNode): Record<string, string> {
  const attributes: Record<string, string> = {};
  const blockId = node.attrs.blockId;
  if (typeof blockId !== 'string' || blockId === '') throw new Error(`typed block ${JSON.stringify(type.name)} is missing blockId`);
  attributes.id = blockId;
  for (const [name, definition] of Object.entries(type.attributes)) {
    const value = node.attrs[name];
    if (value === undefined) continue;
    attributes[name] = attributeText(definition, value);
  }
  return attributes;
}

/** The DOM attribute that carries a typed block's attribute `name` through HTML. */
function domAttributeName(name: string): string {
  return `data-proof-block-attr-${name}`;
}

/**
 * A typed block's client-owned attributes as DOM attributes, so the block's HTML - a copy on the
 * clipboard - carries them back. A server-owned attribute is the server's to set, and a copy of a
 * block is a new block, so none is written.
 */
function domAttrs(type: BlockTypeSchema, node: ProseMirrorNode): Record<string, string> {
  const attributes: Record<string, string> = {};
  for (const [name, definition] of Object.entries(type.attributes)) {
    const value = node.attrs[name];
    if (definition.server || value === undefined || value === null) continue;
    attributes[domAttributeName(name)] = attributeText(definition, value);
  }
  return attributes;
}

/**
 * The client-owned attributes a typed block's section carries, parsed as its markdown would be;
 * the others keep their defaults. A section whose attribute the schema refuses, or holds a value an
 * editor of schema version `version` may not write yet, is not read as the typed block at all (the
 * rule does not match), since this editor never writes one.
 */
function parsedDomAttrs(type: BlockTypeSchema, dom: HTMLElement, version: number): Record<string, unknown> | false {
  const attributes: Record<string, unknown> = {};
  for (const [name, definition] of Object.entries(type.attributes)) {
    if (definition.server) continue;
    const value = dom.getAttribute(domAttributeName(name));
    if (value === null) continue;
    try {
      attributes[name] = parseAttribute(type.name, name, definition, value);
    } catch {
      return false;
    }
    if (!creatableValue(version, definition, attributes[name])) return false;
  }
  return attributes;
}

/**
 * The ProseMirror node spec of the typed block `type`, drawn by `renderBlock` when given, for an
 * editor of schema version `version`: a block it makes holds no attribute it may not create yet.
 */
export function typedBlockSpec(type: BlockTypeSchema, renderBlock?: HostBlockRenderer, version = Number.POSITIVE_INFINITY): NodeSchema {
  return withBlockIdSpec({
    attrs: schemaAttrs(type, version),
    content: type.content,
    group: 'block',
    defining: true,
    isolating: true,
    parseDOM: [{
      tag: `section[data-proof-block-type="${type.name}"]`,
      getAttrs: (dom) => parsedDomAttrs(type, dom, version),
      contentElement: (dom) => dom.querySelector('[data-proof-block-content]') ?? dom,
    }],
    toDOM: (node) => withDomAttributes(renderBlock?.(node, type) ?? [
      'section',
      {
        class: `proof-typed-block proof-typed-block-${type.name}`,
        'data-proof-block-type': type.name,
      },
      0,
    ], domAttrs(type, node)),
    parseMarkdown: {
      match: (node) => (node as MarkdownNode).type === 'containerDirective' && (node as MarkdownNode).name === type.name,
      runner: (state, node, nodeType) => {
        const directive = node as MarkdownNode;
        state.openNode(nodeType, directiveAttrs(type, directive.attributes, version));
        state.next(directive.children as unknown as ParsedMarkdownNode[]);
        state.closeNode();
      },
    },
    toMarkdown: {
      match: (node) => node.type.name === type.name,
      runner: (state, node) => {
        state.openNode('containerDirective', undefined, {
          name: type.name,
          attributes: markdownAttrs(type, node),
        });
        state.next(node.content);
        state.closeNode();
      },
    },
  });
}

export function blockSchemaPlugins(schema: BlockSchema, renderBlock?: HostBlockRenderer) {
  validateBlockSchema(schema);
  return schema.types.map((type) => $nodeSchema(type.name, () => typedBlockSpec(type, renderBlock, schema.version)));
}

/** Why the schema refuses the container directive `node`, as `visit` throws it, or undefined when it is accepted. */
export function typedBlockRefusal(schema: BlockSchema, node: { name?: string; attributes?: Record<string, string> }): string | undefined {
  const name = node.name ?? '';
  const type = schema.types.find((candidate) => candidate.name === name);
  if (!type) return `unknown typed block ${JSON.stringify(name)} (known types: ${knownNames(schema)})`;
  try {
    directiveAttrs(type, node.attributes, schema.version);
  } catch (error) {
    return (error as Error).message;
  }
  return undefined;
}

function visit(node: MarkdownNode, schema: BlockSchema): void {
  if (node.type === 'containerDirective') {
    const refusal = typedBlockRefusal(schema, node);
    if (refusal !== undefined) throw new Error(refusal);
  }
  for (const child of node.children ?? []) visit(child, schema);
}

/** The document service's reason for refusing a paragraph line that opens with `:`
 *  (pmdoc `unsupportedDirectiveReason`), or undefined when the line is ordinary text. The
 *  parser itself only knows the `:::name{...}` container, so these lines would otherwise
 *  parse here as text the server then refuses on settlement. `directives` is how many container
 *  directives stand around the line: a `:::name{...}` opener inside `maxDirectiveNesting` of them
 *  does not open (./directive-container) and is read as text, which the server refuses. */
function unsupportedDirectiveReason(line: string, directives: number): string | undefined {
  if (line.startsWith(':::')) {
    if (line === ':::') return undefined;
    if (/^:::[A-Za-z0-9_-]+\{.*\}$/.test(line)) {
      if (directives < maxDirectiveNesting) return undefined;
      return `a typed block opens inside ${maxDirectiveNesting} typed blocks; a document nests at most ${maxDirectiveNesting} typed blocks`;
    }
    return 'typed block directives use :::name{...}; Pandoc fenced divs and malformed directives are not supported';
  }
  if (/^::[A-Za-z0-9_-]/.test(line)) return 'leaf directives (::name) are not supported';
  if (/^:[A-Za-z0-9_-]/.test(line) && line.includes('{')) return 'text directives (:name{...}) are not supported';
  return undefined;
}

export function rejectUnsupportedDirectiveSyntax(tree: MarkdownNode, markdown: string): void {
  const reject = (node: MarkdownNode, directives: number): void => {
    if (node.type === 'paragraph') {
      const start = node.position?.start;
      const end = node.position?.end;
      if (start?.offset !== undefined && end?.offset !== undefined) {
        for (const [index, line] of markdown.slice(start.offset, end.offset).split('\n').entries()) {
          const reason = unsupportedDirectiveReason(line.trimStart(), directives);
          if (reason !== undefined) throw new Error(`line ${(start.line ?? 1) + index}: ${reason}`);
        }
      }
    }
    const inner = node.type === 'containerDirective' ? directives + 1 : directives;
    for (const child of node.children ?? []) reject(child, inner);
  };
  reject(tree, 0);
}

export function remarkTypedBlocks(schema: BlockSchema) {
  validateBlockSchema(schema);
  return (tree: MarkdownNode, file: { toString(): string }) => {
    rejectUnsupportedDirectiveSyntax(tree, markdownSource(file));
    for (const child of tree.children ?? []) visit(child, schema);
  };
}
