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
  unknown(): E;
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
  /** zod v4 accepts an `error` callback; the OMP `pi.zod` facade ignores the second argument. */
  max(value: number, params?: { error: (issue: { input: unknown }) => string }): ZodNode;
  int(): ZodNode;
};

/** "is N characters over the M-character limit (L/M)"; the field name is prepended by the formatter. */
export function overCapMessage(length: number, max: number): string {
  return `is ${length - max} characters over the ${max}-character limit (${length}/${max})`;
}

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
  unknown(): ZodNode;
  object(shape: Record<string, ZodNode>): ZodObjectNode;
}

export function zodSchemaApi(zod: unknown): SchemaApi<z.ZodType> {
  const api = zod as ZodApi;

  return {
    string: (opts = {}) => {
      let schema = api.string();
      if (opts.min !== undefined) schema = schema.min(opts.min);
      if (opts.max !== undefined) {
        const max = opts.max;
        schema = schema.max(max, {
          error: (issue) =>
            overCapMessage(typeof issue.input === "string" ? issue.input.length : max + 1, max),
        });
      }
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
    unknown: () => api.unknown(),
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
