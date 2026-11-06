import { queryOptions } from "@tanstack/react-query";

import { api } from "../../api/client";
import type { BlockSchema } from "../../api/types";

/**
 * Loads one server schema per session while retaining parsed schemas by their version. A
 * failed fetch is not retained: the next `load` fetches again, so a transient error on page
 * load does not leave every document surface without a schema until reload.
 */
export class BlockSchemaCache {
  private readonly schemas = new Map<number, BlockSchema>();
  private loading: Promise<BlockSchema> | undefined;

  load(fetchSchema: () => Promise<BlockSchema>): Promise<BlockSchema> {
    if (this.loading !== undefined) {
      return this.loading;
    }
    const loading = fetchSchema().then(
      (schema) => {
        this.schemas.set(schema.version, schema);
        return schema;
      },
      (error: unknown) => {
        if (this.loading === loading) {
          this.loading = undefined;
        }
        throw error;
      }
    );
    this.loading = loading;
    return loading;
  }

  forVersion(version: number): BlockSchema | undefined {
    return this.schemas.get(version);
  }
}

export const blockSchemaCache = new BlockSchemaCache();

/** Fetches the current schema once, then retains it by version for every document surface. */
export function loadBlockSchema(): Promise<BlockSchema> {
  return blockSchemaCache.load(() => api.getBlockSchema());
}

/** The schema under the key every document surface reads it by. A document address reads it in
 *  the boot task (`open-document-early.ts`), so the surface that draws the document and the
 *  admission that opens its socket - which the schema's version is part of (`connection.ts`) -
 *  find it read. */
export const blockSchemaQueryOptions = () =>
  queryOptions({
    queryFn: loadBlockSchema,
    queryKey: ["block-schema"],
    staleTime: Number.POSITIVE_INFINITY,
  });
