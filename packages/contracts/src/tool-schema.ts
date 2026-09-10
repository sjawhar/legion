import type { z } from "zod";

export interface SchemaNode<E> {
  optional(): E;
  describe(text: string): E;
}

export interface SchemaApi<E extends SchemaNode<E>> {
  string(opts?: { min?: number; max?: number }): E;
  number(opts?: { int?: boolean; min?: number; max?: number }): E;
  boolean(): E;
  enum(values: readonly [string, ...string[]]): E;
  array(item: E, opts?: { min?: number; max?: number }): E;
  object(shape: Record<string, E>, opts?: { strict?: boolean }): E;
  refineObject(
    shape: Record<string, E>,
    check: (value: unknown) => boolean,
    message: string,
    opts?: { strict?: boolean }
  ): E;
}

export type ToolArgumentsShape = Readonly<Record<string, unknown>>;

type ZodNode = z.ZodType & {
  min(value: number): ZodNode;
  max(value: number): ZodNode;
  int(): ZodNode;
};

interface ZodObjectNode {
  refine(check: (value: unknown) => unknown, params?: unknown): ZodNode;
  strict(): ZodObjectNode;
}

interface ZodApi {
  string(): ZodNode;
  number(): ZodNode;
  boolean(): ZodNode;
  enum(values: readonly [string, ...string[]]): ZodNode;
  array(item: ZodNode): ZodNode;
  object(shape: Record<string, ZodNode>): ZodObjectNode;
}

export function zodSchemaApi(zod: unknown): SchemaApi<z.ZodType> {
  const api = zod as ZodApi;

  return {
    string: (opts = {}) => {
      let schema = api.string();
      if (opts.min !== undefined) schema = schema.min(opts.min);
      if (opts.max !== undefined) schema = schema.max(opts.max);
      return schema;
    },
    number: (opts = {}) => {
      let schema = api.number();
      if (opts.int) schema = schema.int();
      if (opts.min !== undefined) schema = schema.min(opts.min);
      if (opts.max !== undefined) schema = schema.max(opts.max);
      return schema;
    },
    boolean: () => api.boolean(),
    enum: (values) => api.enum(values),
    array: (item, opts = {}) => {
      let schema = api.array(item as ZodNode);
      if (opts.min !== undefined) schema = schema.min(opts.min);
      if (opts.max !== undefined) schema = schema.max(opts.max);
      return schema;
    },
    object: (shape, opts = {}) => {
      let schema = api.object(shape as unknown as Record<string, ZodNode>);
      if (opts.strict) schema = schema.strict();
      return schema as unknown as z.ZodType;
    },
    refineObject: (shape, check, message, opts = {}) => {
      let schema = api.object(shape as unknown as Record<string, ZodNode>);
      if (opts.strict) schema = schema.strict();
      return schema.refine(check, message) as z.ZodType;
    },
  };
}
