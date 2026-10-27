/**
 * A typed block's `block[]` attribute: the blocks it points at, as canonical block references.
 * Which values it may hold, and why one is refused, is decided by `@legion/contracts`
 * (`blockPointerValueProblem`, `blockPointersProblem`, `parseCanonicalBlockReference`), the
 * parsers the document service's twins are tested against with the same table, in the service's
 * words, so the editor and the service never disagree on a value. A pointer to a block of the
 * editor's own document is also refused when the block's type is not one the attribute's
 * `targets` allow; a block of another document is the service's to check, since only it knows
 * that document's blocks. Each refusal reads as the service's does (pmdoc
 * `ParseBlockPointerValue` and `CheckBlockPointers`).
 */

import { blockPointerValueProblem, parseCanonicalBlockReference } from "@legion/contracts";
import type { Node as ProseMirrorNode } from "@milkdown/kit/prose/model";
import type { BlockTypeSchema } from "./block-schema";
import { blockIdOf } from "./editor/schema/block-ids";

/**
 * The first block of `doc` whose id is `id`, and its position, or null when no block has it. The
 * first holder of a repeated id is the block, as the document service reads it.
 */
export function findBlock(
  doc: ProseMirrorNode,
  id: string
): { node: ProseMirrorNode; pos: number } | null {
  let found: { node: ProseMirrorNode; pos: number } | null = null;
  doc.descendants((node, pos) => {
    if (found !== null) {
      return false;
    }
    if (blockIdOf(node) !== id) {
      return true;
    }
    found = { node, pos };
    return false;
  });
  return found;
}

/**
 * The pointers the typed block `typeName`'s `block[]` attribute `name` holds when markdown or a
 * pasted block writes it as `value`: none for the empty string, else canonical block references
 * joined by exactly one space. Any other value is refused, never normalised.
 */
export function parseBlockPointers(typeName: string, name: string, value: string): string[] {
  const problem = blockPointerValueProblem(value);
  if (problem !== undefined) {
    throw new Error(
      `typed block ${JSON.stringify(typeName)} attribute ${JSON.stringify(name)}: ${problem}`
    );
  }
  return value === "" ? [] : value.split(" ");
}

/**
 * Why a command may not leave `doc` with the typed block `type` (`block` its id, null for a block
 * not yet given one) holding the attribute values `attrs`, or undefined when it may: a `block[]`
 * value it introduces - one `held`, the block's attributes before the command as the same type,
 * does not already hold in that attribute - names a block of `doc` whose type the attribute's
 * `targets` do not allow. A pointer names a block of `doc` when its document is `documentRef`, the
 * document's unversioned reference; without one nothing is refused here. A pointer to a block
 * `doc` lacks is accepted, as the service accepts one to an absent block.
 */
export function blockPointerRefusal(
  doc: ProseMirrorNode,
  type: BlockTypeSchema,
  block: string | null,
  attrs: Readonly<Record<string, unknown>>,
  held: Readonly<Record<string, unknown>>,
  documentRef: string | undefined
): string | undefined {
  if (documentRef === undefined) {
    return undefined;
  }
  for (const name of Object.keys(attrs).sort()) {
    const targets = type.attributes[name]?.targets ?? [];
    const values = attrs[name];
    if (
      type.attributes[name]?.kind !== "block[]" ||
      targets.length === 0 ||
      !Array.isArray(values)
    ) {
      continue;
    }
    const prior: unknown = held[name];
    for (const value of values as readonly string[]) {
      const reference = parseCanonicalBlockReference(value);
      if (
        reference === undefined ||
        value.slice(0, value.indexOf("#")) !== documentRef ||
        (Array.isArray(prior) && prior.includes(value))
      ) {
        continue;
      }
      const target = findBlock(doc, reference.block)?.node.type.name;
      if (target !== undefined && !targets.includes(target)) {
        const at = block === null ? "" : ` #${block}`;
        return `typed block ${JSON.stringify(type.name)}${at} attribute ${JSON.stringify(name)} points at ${value}, a ${JSON.stringify(target)} block; it points only at ${targets.join(", ")}`;
      }
    }
  }
  return undefined;
}
