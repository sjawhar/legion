// @ts-nocheck — verbatim proof-sdk source. The fork emits this tree's declarations with
// `noCheck` (its tsconfig.lib.json), so it has never type-checked; see AGENTS.md.
import type { Node as ProseMirrorNode, NodeType } from '@milkdown/kit/prose/model';
import type { EditorState, Transaction } from '@milkdown/kit/prose/state';

import type { BlockAttributeSchema, BlockSchema, BlockTypeSchema } from './block-schema.js';
import { BLOCK_ID_ATTR, blockIdOf } from './editor/schema/block-ids.js';

export type TypedBlockAttributeValue = string | boolean | readonly string[];
export type TypedBlockAttributes = Readonly<Record<string, TypedBlockAttributeValue>>;

export interface TypedBlockCommandContext {
  blockSchema?: BlockSchema;
  getState(): EditorState;
  dispatch(transaction: Transaction): void;
}

export interface TypedBlockMenuItem {
  label: string;
  run(): boolean;
}

export interface TypedBlockCommands {
  /** Inserts a typed block containing an empty paragraph at the selection. */
  insertTypedBlock(typeName: string, attrs?: TypedBlockAttributes): boolean;
  /** Replaces one block with a typed block while preserving its stable block id. */
  retypeBlock(blockId: string, typeName: string, attrs?: TypedBlockAttributes): boolean;
  /** Updates client-owned attributes of a typed block. */
  setBlockAttributes(blockId: string, attrs: TypedBlockAttributes): boolean;
  /** Items a host can render in its block menu for the current selection. */
  blockMenuItems(): readonly TypedBlockMenuItem[];
}

interface LocatedBlock {
  node: ProseMirrorNode;
  pos: number;
}


function findType(schema: BlockSchema, typeName: string): BlockTypeSchema {
  const type = schema.types.find((candidate) => candidate.name === typeName);
  if (!type) throw new Error(`typed block schema does not declare ${JSON.stringify(typeName)}`);
  return type;
}

function nodeType(state: EditorState, type: BlockTypeSchema): NodeType {
  const result = state.schema.nodes[type.name];
  if (!result) throw new Error(`editor schema does not declare typed block ${JSON.stringify(type.name)}`);
  return result;
}

function validateValue(typeName: string, name: string, attribute: BlockAttributeSchema, value: TypedBlockAttributeValue): void {
  switch (attribute.kind) {
    case 'string':
    case 'actor':
    case 'timestamp':
      if (typeof value !== 'string') throw new Error(`typed block ${JSON.stringify(typeName)} attribute ${JSON.stringify(name)} must be a string`);
      return;
    case 'bool':
      if (typeof value !== 'boolean') throw new Error(`typed block ${JSON.stringify(typeName)} attribute ${JSON.stringify(name)} must be a boolean`);
      return;
    case 'enum':
      if (typeof value !== 'string' || !attribute.choices?.includes(value)) {
        throw new Error(`typed block ${JSON.stringify(typeName)} attribute ${JSON.stringify(name)} must be one of ${attribute.choices?.join(', ')}`);
      }
      return;
    case 'string[]':
      if (!Array.isArray(value) || value.some((item) => typeof item !== 'string')) {
        throw new Error(`typed block ${JSON.stringify(typeName)} attribute ${JSON.stringify(name)} must be a string array`);
      }
  }
}

function clientAttributes(type: BlockTypeSchema, attrs: TypedBlockAttributes | undefined): Record<string, TypedBlockAttributeValue> {
  const result: Record<string, TypedBlockAttributeValue> = {};
  for (const [name, value] of Object.entries(attrs ?? {})) {
    const attribute = type.attributes[name];
    if (!attribute) throw new Error(`typed block ${JSON.stringify(type.name)} does not declare attribute ${JSON.stringify(name)}`);
    if (attribute.server) throw new Error(`typed block ${JSON.stringify(type.name)} attribute ${JSON.stringify(name)} is server-owned`);
    validateValue(type.name, name, attribute, value);
    result[name] = value;
  }
  return result;
}

function findBlock(doc: ProseMirrorNode, id: string): LocatedBlock | null {
  let found: LocatedBlock | null = null;
  doc.descendants((node, pos) => {
    if (blockIdOf(node) !== id) return;
    found = { node, pos };
    return false;
  });
  return found;
}

function selectedParagraph(state: EditorState): LocatedBlock | null {
  for (let depth = state.selection.$head.depth; depth > 0; depth -= 1) {
    const node = state.selection.$head.node(depth);
    if (node.type.name !== 'paragraph') continue;
    const id = blockIdOf(node);
    if (id === null) return null;
    return { node, pos: state.selection.$head.before(depth) };
  }
  return null;
}

function insert(context: TypedBlockCommandContext, schema: BlockSchema, typeName: string, attrs?: TypedBlockAttributes): boolean {
  const state = context.getState();
  const type = findType(schema, typeName);
  const typedNode = nodeType(state, type);
  const paragraph = state.schema.nodes.paragraph.createAndFill();
  if (paragraph === null) throw new Error('editor schema cannot create an empty paragraph');
  const block = typedNode.createAndFill(clientAttributes(type, attrs), paragraph);
  if (block === null) throw new Error(`typed block ${JSON.stringify(typeName)} cannot contain a paragraph body`);
  context.dispatch(state.tr.replaceSelectionWith(block));
  return true;
}

function retype(context: TypedBlockCommandContext, schema: BlockSchema, blockId: string, typeName: string, attrs?: TypedBlockAttributes): boolean {
  const state = context.getState();
  const source = findBlock(state.doc, blockId);
  if (source === null) return false;
  const type = findType(schema, typeName);
  const typedNode = nodeType(state, type);
  const body = source.node.type.create({ ...source.node.attrs, [BLOCK_ID_ATTR]: null }, source.node.content, source.node.marks);
  const replacement = typedNode.createChecked({
    ...clientAttributes(type, attrs),
    [BLOCK_ID_ATTR]: blockId,
  }, body);
  context.dispatch(state.tr.replaceWith(source.pos, source.pos + source.node.nodeSize, replacement));
  return true;
}

function setAttributes(context: TypedBlockCommandContext, schema: BlockSchema, blockId: string, attrs: TypedBlockAttributes): boolean {
  const state = context.getState();
  const located = findBlock(state.doc, blockId);
  if (located === null) return false;
  const type = findType(schema, located.node.type.name);
  const changes = clientAttributes(type, attrs);
  if (Object.keys(changes).length === 0) return true;
  context.dispatch(state.tr.setNodeMarkup(located.pos, undefined, { ...located.node.attrs, ...changes }, located.node.marks));
  return true;
}

function canRetype(state: EditorState, schema: BlockSchema, source: LocatedBlock, type: BlockTypeSchema): boolean {
  try {
    const typedNode = nodeType(state, type);
    const body = source.node.type.create({ ...source.node.attrs, [BLOCK_ID_ATTR]: null }, source.node.content, source.node.marks);
    typedNode.createChecked({ [BLOCK_ID_ATTR]: blockIdOf(source.node) }, body);
    return true;
  } catch {
    return false;
  }
}

/**
 * Creates the command set exposed by a browser editor handle. Keeping the commands
 * independent of the editor view also lets hosts wrap dispatch for their own state
 * management and makes their document-level behavior testable without a DOM.
 */
export function createTypedBlockCommands(context: TypedBlockCommandContext): TypedBlockCommands {
  const schema = context.blockSchema;
  if (!schema) {
    return {
      insertTypedBlock: () => false,
      retypeBlock: () => false,
      setBlockAttributes: () => false,
      blockMenuItems: () => [],
    };
  }

  return {
    insertTypedBlock(typeName, attrs) {
      return insert(context, schema, typeName, attrs);
    },
    retypeBlock(blockId, typeName, attrs) {
      return retype(context, schema, blockId, typeName, attrs);
    },
    setBlockAttributes(blockId, attrs) {
      return setAttributes(context, schema, blockId, attrs);
    },
    blockMenuItems() {
      const state = context.getState();
      const items: TypedBlockMenuItem[] = schema.types.map((type) => ({
        label: `Insert ${type.name.slice(0, 1).toUpperCase()}${type.name.slice(1)}`,
        run: () => insert(context, schema, type.name),
      }));
      const paragraph = selectedParagraph(state);
      if (paragraph === null) return items;
      for (const type of schema.types) {
        if (!canRetype(state, schema, paragraph, type)) continue;
        const blockId = blockIdOf(paragraph.node);
        if (blockId === null) continue;
        items.push({
          label: `Turn into ${type.name.slice(0, 1).toUpperCase()}${type.name.slice(1)}`,
          run: () => retype(context, schema, blockId, type.name),
        });
      }
      return items;
    },
  };
}
