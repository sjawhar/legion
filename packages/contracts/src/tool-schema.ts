import type { z } from "zod";

/** One value a field may be declared to carry exactly, as JSON holds it. */
export type SchemaLiteral = string | number | boolean | null;

export interface SchemaNode<E> {
  optional(): E;
  nullable(): E;
  describe(text: string): E;
}

export interface SchemaApi<E extends SchemaNode<E>> {
  /** `maxHint` follows the over-cap message after "; ", saying what to send instead. */
  string(opts?: { min?: number; max?: number; maxHint?: string }): E;
  number(opts?: { int?: boolean; min?: number; max?: number }): E;
  boolean(): E;
  enum(values: readonly [string, ...string[]]): E;
  /**
   * The exact set of values a field may carry, one of which it must be, so a set that is not all
   * strings (`false | "optional" | "required"`) stays one field rather than a union of shapes.
   */
  literal(values: readonly [SchemaLiteral, ...SchemaLiteral[]]): E;
  array(item: E, opts?: { min?: number; max?: number }): E;
  unknown(): E;
  object(shape: Record<string, E>, opts?: { strict?: boolean }): E;
  /** `message` is the refusal, or builds it from the rejected value. */
  refineObject(
    shape: Record<string, E>,
    check: (value: unknown) => boolean,
    message: string | ((value: unknown) => string),
    opts?: { strict?: boolean }
  ): E;
}

export type ToolArgumentsShape = Readonly<Record<string, unknown>>;

/** zod v4's `error` callback, given the issue with the rejected input. */
interface ZodErrorParams {
  error(issue: { input: unknown }): string;
}

type ZodNode = z.ZodType & {
  min(value: number): ZodNode;
  /** zod v4 accepts an `error` callback; the OMP `pi.zod` facade ignores the second argument. */
  max(value: number, params?: ZodErrorParams): ZodNode;
  int(): ZodNode;
};

/** "is N characters over the M-character limit (L/M)"; the field name is prepended by the formatter. */
export function overCapMessage(length: number, max: number): string {
  return `is ${length - max} characters over the ${max}-character limit (${length}/${max})`;
}

interface ZodObjectNode {
  refine(check: (value: unknown) => unknown, params?: string | ZodErrorParams): ZodNode;
  strict(): ZodObjectNode;
}

interface ZodApi {
  string(): ZodNode;
  number(): ZodNode;
  boolean(): ZodNode;
  enum(values: readonly [string, ...string[]]): ZodNode;
  /**
   * zod v4's takes the whole set. Oh My Pi's `pi.zod` facade has a `literal` of its own that
   * takes one value and reads a set as a single literal equal to the array, so it is optional
   * here and `literalOfSet` only keeps a node that accepts every value of the set.
   */
  literal?(values: readonly SchemaLiteral[]): ZodNode;
  array(item: ZodNode): ZodNode;
  unknown(): ZodNode;
  object(shape: Record<string, ZodNode>): ZodObjectNode;
}

/**
 * The host's own literal node for the whole set, or undefined where the host cannot build one.
 *
 * zod v4's `literal` takes the set and refuses everything outside it, which is the refusal
 * `formatZodIssues` renders as `false|optional|required`. Oh My Pi's `pi.zod` facade has a
 * `literal` of its own that takes one value: measured against omp v18.16.0, it reads a set as a
 * single literal whose value is the array (`{"k":"lit","v":[false,"optional","required"]}`), which
 * would show a model a vocabulary no call can satisfy. So the node is kept only when it accepts
 * every value of the set; a host that cannot say this widens the field instead, and the real
 * refusal still comes from real zod where the arguments are parsed.
 */
function literalOfSet(api: ZodApi, values: readonly SchemaLiteral[]): ZodNode | undefined {
  const node = api.literal?.(values);
  if (node === undefined || typeof node.safeParse !== "function") return undefined;
  return values.every((value) => node.safeParse(value).success) ? node : undefined;
}

export function zodSchemaApi(zod: unknown): SchemaApi<z.ZodType> {
  const api = zod as ZodApi;

  return {
    string: (opts = {}) => {
      let schema = api.string();
      if (opts.min !== undefined) schema = schema.min(opts.min);
      if (opts.max !== undefined) {
        const max = opts.max;
        const hint = opts.maxHint === undefined ? "" : `; ${opts.maxHint}`;
        schema = schema.max(max, {
          error: (issue) =>
            overCapMessage(typeof issue.input === "string" ? issue.input.length : max + 1, max) +
            hint,
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
    literal: (values) => literalOfSet(api, values) ?? api.unknown(),
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
      return schema.refine(
        check,
        typeof message === "string" ? message : { error: (issue) => message(issue.input) }
      ) as z.ZodType;
    },
  };
}
