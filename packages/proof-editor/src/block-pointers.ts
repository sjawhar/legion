/**
 * A typed block's `block[]` attribute: the blocks it points at, as canonical block references.
 * Which values it may hold, and why one is refused, is decided by `@legion/contracts`
 * (`parseBlockPointerValue`, `blockPointerValueProblem`, `parseCanonicalBlockReference`), the
 * parsers the document service's twins are tested against with the same table, so the editor and
 * the service never disagree on a value. A pointer to a block of the editor's own document is
 * also refused when the block's type is not one the attribute's `targets` allow; a block of
 * another document is the service's to check, since only it knows that document's blocks. The
 * refusals follow the service's wording (pmdoc `ParseBlockPointerValue` and `CheckBlockPointers`)
 * but are not byte for byte the same: a value is quoted with `JSON.stringify` where Go uses `%q`,
 * which escapes a control or non-printing character differently, and a type refusal carries no
 * `block pointer refused:` prefix.
 */

import {
  blockPointerValueProblem,
  parseBlockPointerValue,
  parseCanonicalBlockReference,
} from "@legion/contracts";
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
  const pointers = parseBlockPointerValue(value);
  if (pointers === undefined) {
    throw new Error(
      `typed block ${JSON.stringify(typeName)} attribute ${JSON.stringify(name)}: ${blockPointerValueProblem(value)}`
    );
  }
  return pointers;
}

/**
 * Why a command may not leave `doc` with a typed block of `type` holding the attribute values
 * `attrs`, or undefined when it may. `prior` is the block the command replaces or changes (null
 * for a new one); its id names the block in the refusal, and its attributes are the pointers
 * already held when it is of `type` too, since a retype from another type that declares the same
 * name may narrow its `targets`. A `block[]` value the command introduces - one the prior block
 * does not already hold in that attribute - is refused when it names a block of `doc` whose type
 * the attribute's `targets` do not allow. A pointer names a block of `doc` when its document is
 * `documentRef`, the document's unversioned reference; without one nothing is refused here. A
 * pointer to a block `doc` lacks is accepted, as the service accepts one to an absent block.
 */
export function blockPointerRefusal(
  doc: ProseMirrorNode,
  type: BlockTypeSchema,
  prior: ProseMirrorNode | null,
  attrs: Readonly<Record<string, unknown>>,
  documentRef: string | undefined
): string | undefined {
  if (documentRef === undefined) {
    return undefined;
  }
  const block = prior === null ? null : blockIdOf(prior);
  const held: Readonly<Record<string, unknown>> =
    prior !== null && prior.type.name === type.name ? prior.attrs : {};
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
    const heldValues: unknown = held[name];
    for (const value of values as readonly string[]) {
      const reference = parseCanonicalBlockReference(value);
      if (
        reference === undefined ||
        value.slice(0, value.indexOf("#")) !== documentRef ||
        (Array.isArray(heldValues) && heldValues.includes(value))
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
