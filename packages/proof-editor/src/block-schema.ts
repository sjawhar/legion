// @ts-nocheck — verbatim proof-sdk source. The fork emits this tree's declarations with
// `noCheck` (its tsconfig.lib.json), so it has never type-checked; see AGENTS.md.
import { $nodeSchema } from '@milkdown/kit/utils';
import type { DOMOutputSpec, Node as ProseMirrorNode, NodeSpec } from '@milkdown/kit/prose/model';

import { withBlockIdSpec } from './editor/schema/block-ids.js';
import { withDomAttributes } from './editor/schema/dom-attributes.js';

export type BlockAttributeKind = 'string' | 'bool' | 'enum' | 'string[]' | 'actor' | 'timestamp';

export interface BlockAttributeSchema {
  kind: BlockAttributeKind;
  choices?: readonly string[];
  default?: string | boolean | readonly string[];
  server?: boolean;
}

export interface BlockTypeSchema {
  name: string;
  content: string;
  render: 'host';
  attributes: Readonly<Record<string, BlockAttributeSchema>>;
}

export interface BlockSchema {
  version: number;
  types: readonly BlockTypeSchema[];
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

function schemaAttrs(type: BlockTypeSchema): Record<string, { default?: string | boolean | readonly string[] }> {
  return Object.fromEntries(Object.entries(type.attributes).map(([name, attribute]) => [name, { default: attribute.default }]));
}

function directiveAttrs(type: BlockTypeSchema, attrs: Record<string, string> | undefined): Record<string, string | boolean | string[] | undefined> {
  const parsed: Record<string, string | boolean | string[] | undefined> = {};
  for (const [name, attribute] of Object.entries(type.attributes)) {
    const value = attrs?.[name];
    parsed[name] = value === undefined ? attribute.default : parseAttribute(type.name, name, attribute, value);
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
 * the others keep their defaults. A section whose attribute the schema refuses is not read as the
 * typed block at all (the rule does not match), since this editor never writes one.
 */
function parsedDomAttrs(type: BlockTypeSchema, dom: HTMLElement): Record<string, unknown> | false {
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
  }
  return attributes;
}

/** The ProseMirror node spec of the typed block `type`, drawn by `renderBlock` when given. */
export function typedBlockSpec(type: BlockTypeSchema, renderBlock?: HostBlockRenderer): NodeSpec {
  return withBlockIdSpec({
    attrs: schemaAttrs(type),
    content: type.content,
    group: 'block',
    defining: true,
    isolating: true,
    parseDOM: [{
      tag: `section[data-proof-block-type="${type.name}"]`,
      getAttrs: (dom) => parsedDomAttrs(type, dom),
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
        state.openNode(nodeType, directiveAttrs(type, directive.attributes));
        state.next(directive.children);
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
  return schema.types.map((type) => $nodeSchema(type.name, () => typedBlockSpec(type, renderBlock)));
}

function visit(node: MarkdownNode, schema: BlockSchema): void {
  if (node.type === 'containerDirective') {
    const name = node.name ?? '';
    const type = schema.types.find((candidate) => candidate.name === name);
    if (!type) throw new Error(`unknown typed block ${JSON.stringify(name)} (known types: ${knownNames(schema)})`);
    directiveAttrs(type, node.attributes);
  }
  for (const child of node.children ?? []) visit(child, schema);
}

/** The document service's reason for refusing a paragraph line that opens with `:`
 *  (pmdoc `unsupportedDirectiveReason`), or undefined when the line is ordinary text. The
 *  parser itself only knows the `:::name{...}` container, so these lines would otherwise
 *  parse here as text the server then refuses on settlement. */
function unsupportedDirectiveReason(line: string): string | undefined {
  if (line.startsWith(':::')) {
    if (line === ':::' || /^:::[A-Za-z0-9_-]+\{.*\}$/.test(line)) return undefined;
    return 'typed block directives use :::name{...}; Pandoc fenced divs and malformed directives are not supported';
  }
  if (/^::[A-Za-z0-9_-]/.test(line)) return 'leaf directives (::name) are not supported';
  if (/^:[A-Za-z0-9_-]/.test(line) && line.includes('{')) return 'text directives (:name{...}) are not supported';
  return undefined;
}

export function rejectUnsupportedDirectiveSyntax(tree: MarkdownNode, markdown: string): void {
  const reject = (node: MarkdownNode): void => {
    if (node.type === 'paragraph') {
      const start = node.position?.start;
      const end = node.position?.end;
      if (start?.offset !== undefined && end?.offset !== undefined) {
        for (const [index, line] of markdown.slice(start.offset, end.offset).split('\n').entries()) {
          const reason = unsupportedDirectiveReason(line.trimStart());
          if (reason !== undefined) throw new Error(`line ${(start.line ?? 1) + index}: ${reason}`);
        }
      }
    }
    for (const child of node.children ?? []) reject(child);
  };
  reject(tree);
}

export function remarkTypedBlocks(schema: BlockSchema) {
  validateBlockSchema(schema);
  return (tree: MarkdownNode, file: { toString(): string }) => {
    rejectUnsupportedDirectiveSyntax(tree, file.toString());
    for (const child of tree.children ?? []) visit(child, schema);
  };
}
