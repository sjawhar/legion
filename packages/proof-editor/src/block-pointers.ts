/**
 * A typed block's `block[]` attribute: the blocks it points at, as canonical block references.
 * Which values it may hold is decided by `@legion/contracts` (`parseBlockPointerValue`,
 * `parseCanonicalBlockReference`), the parsers the document service's twins are tested against
 * with the same table, so the editor and the service never disagree on a value. A pointer to a
 * block of the editor's own document is also refused when the block's type is not one the
 * attribute's `targets` allow; a block of another document is the service's to check, since only
 * it knows that document's blocks. Each refusal reads as the service's does (pmdoc
 * `ParseBlockPointerValue` and `CheckBlockPointers`).
 */

import { parseBlockPointerValue, parseCanonicalBlockReference } from "@legion/contracts";
import type { Node as ProseMirrorNode } from "@milkdown/kit/prose/model";
import type { BlockTypeSchema } from "./block-schema";
import { blockIdOf } from "./editor/schema/block-ids";

const NOT_CANONICAL =
  "is not a block reference as Dispatch writes one: dispatch://<ISSUE>/spec#b-<id>, or dispatch://<ISSUE or PROJECT>/artifact/<slug>#b-<id>, the id percent-encoded in upper-case hex except ASCII letters, digits and -, optionally pinned @v<N>";

/**
 * What is wrong with `values` as the pointers of a `block[]` attribute - one that is not a
 * canonical block reference, or one named twice - or undefined when nothing is.
 */
export function blockPointerProblem(values: readonly string[]): string | undefined {
  const seen = new Set<string>();
  for (const value of values) {
    if (parseCanonicalBlockReference(value) === undefined) {
      return `${JSON.stringify(value)} ${NOT_CANONICAL}`;
    }
    if (seen.has(value)) {
      return `names ${JSON.stringify(value)} twice`;
    }
    seen.add(value);
  }
  return undefined;
}

/**
 * The pointers the typed block `typeName`'s `block[]` attribute `name` holds when markdown or a
 * pasted block writes it as `value`: none for the empty string, else canonical block references
 * joined by exactly one space. Any other value is refused, never normalised.
 */
export function parseBlockPointers(typeName: string, name: string, value: string): string[] {
  const pointers = parseBlockPointerValue(value);
  if (pointers !== undefined) {
    return pointers;
  }
  const problem =
    value.startsWith(" ") || value.endsWith(" ") || value.includes("  ")
      ? `${JSON.stringify(value)} must be block references separated by exactly one space, with none leading or trailing`
      : (blockPointerProblem(value.split(" ")) ?? `${JSON.stringify(value)} ${NOT_CANONICAL}`);
  throw new Error(
    `typed block ${JSON.stringify(typeName)} attribute ${JSON.stringify(name)}: ${problem}`
  );
}

/**
 * Why a command may not leave `doc` with the typed block `type` (`block` its id, null for a block
 * not yet given one) holding the attribute values `attrs`, or undefined when it may: a `block[]`
 * value it introduces - one `held`, the block's attributes before the command, does not already
 * hold in that attribute - names a block of `doc` whose type the attribute's `targets` do not
 * allow. A pointer names a block of `doc` when its document is `documentRef`, the document's
 * unversioned reference; without one nothing is refused here. A pointer to a block `doc` lacks
 * is accepted, as the service accepts one to an absent block.
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
      let target: string | undefined;
      doc.descendants((node) => {
        if (target !== undefined) {
          return false;
        }
        if (blockIdOf(node) === reference.block) {
          target = node.type.name;
          return false;
        }
        return true;
      });
      if (target !== undefined && !targets.includes(target)) {
        const at = block === null ? "" : ` #${block}`;
        return `typed block ${JSON.stringify(type.name)}${at} attribute ${JSON.stringify(name)} points at ${value}, a ${JSON.stringify(target)} block; it points only at ${targets.join(", ")}`;
      }
    }
  }
  return undefined;
}
