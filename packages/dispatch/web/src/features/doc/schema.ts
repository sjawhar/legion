import manifest from "../../../../../envoy/internal/dispatch/pmdoc/schema/blocks.json";
import type { BlockElementVersions, BlockSchema } from "../../api/types";

/**
 * The document schema manifest this page was built with (`pmdoc/schema/blocks.json`, the file the
 * server of the same build embeds): the nodes, marks and typed blocks its editors build, and its
 * version. During a release two builds serve one deployment, so the server that answers the page
 * can be of the other build; the page reads no schema from it. The proof-editor checks the shape
 * when it builds an editor or the headless engine from it.
 */
export const compiledBlockSchema = manifest as unknown as BlockSchema;

/**
 * R for `schema`: the highest `since` among the elements an editor of `schema.version` may create
 * (`creatable_since` at most that version), each number 1 where the manifest names none. A build
 * whose version is R or more reads everything this editor writes. It is Go's
 * `pmdoc.SchemaCreatable()` for the same manifest.
 */
export function schemaCreatable(schema: BlockSchema): number {
  let creatable = 0;
  const count = (element: BlockElementVersions | undefined) => {
    if ((element?.creatable_since ?? 1) <= schema.version) {
      creatable = Math.max(creatable, element?.since ?? 1);
    }
  };
  for (const element of [...(schema.nodes ?? []), ...(schema.marks ?? [])]) {
    count(element);
  }
  for (const type of schema.types) {
    count(type);
    for (const attribute of Object.values(type.attributes)) {
      count(attribute);
      for (const choice of attribute.choices ?? []) {
        count(attribute.choice_versions?.[choice]);
      }
    }
  }
  return creatable;
}

/** R for the manifest this page was built with, which its document connections present. */
export const compiledSchemaCreatable = schemaCreatable(compiledBlockSchema);
