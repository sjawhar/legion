import { dispatchToolSchema, dispatchToolSpecs, zodSchemaApi } from "@legion/contracts";
import { z } from "zod";

/**
 * A tool call refused before any request left the process. `problems` lists every
 * defect found in one pass so the caller fixes them all with a single retry.
 */
export class ToolInputError extends Error {
  readonly tool: string;
  readonly problems: readonly string[];

  constructor(tool: string, problems: readonly string[]) {
    const count = problems.length;
    const help = dispatchInputHelp(tool);
    super(
      [
        `${tool} was not called: ${count} problem${count === 1 ? "" : "s"}`,
        ...problems.map((problem) => `- ${problem}`),
        ...(help === undefined ? [] : help.map((line) => `- ${line}`)),
      ].join("\n")
    );
    this.name = "ToolInputError";
    this.tool = tool;
    this.problems = problems;
  }
}

/** The keys and a schema-valid invocation shown after a Dispatch input refusal. */
function dispatchInputHelp(tool: string): readonly string[] | undefined {
  const spec = dispatchToolSpecs.find((candidate) => candidate.name === tool);
  if (spec === undefined) return undefined;
  const schema = dispatchToolSchema(spec, zodSchemaApi(z), { strict: true });
  const allowed = Object.keys(shapeOf(schema) ?? {}).join(", ") || "none";
  return [`Allowed keys: ${allowed}`, `Example: ${tool}(${JSON.stringify(spec.example)})`];
}

/** Strips optional/nullable/default wrappers so the node's own `def.type` is visible. */
function unwrap(schema: z.ZodType | undefined): z.ZodType | undefined {
  let current = schema;
  while (current !== undefined) {
    const def: z.core.$ZodTypeDef = current.def;
    if (
      !("innerType" in def) ||
      !(def.type === "optional" || def.type === "nullable" || def.type === "default")
    )
      break;
    current = def.innerType as z.ZodType;
  }
  return current;
}

/** The object shape a schema declares, or undefined for anything that is not an object. */
function shapeOf(schema: z.ZodType | undefined): Readonly<Record<string, z.ZodType>> | undefined {
  const unwrapped = unwrap(schema);
  if (unwrapped === undefined) return undefined;
  const def: z.core.$ZodTypeDef = unwrapped.def;
  return def.type === "object" && "shape" in def
    ? (def.shape as Readonly<Record<string, z.ZodType>>)
    : undefined;
}

/** The schema node a zod issue path addresses. */
function schemaAt(schema: z.ZodType, path: readonly PropertyKey[]): z.ZodType | undefined {
  let current: z.ZodType | undefined = schema;
  for (const key of path) {
    const unwrapped = unwrap(current);
    if (unwrapped === undefined) return undefined;
    const def: z.core.$ZodTypeDef = unwrapped.def;
    if (def.type === "object" && "shape" in def) {
      current = shapeOf(unwrapped)?.[String(key)];
    } else if (def.type === "array" && "element" in def) {
      current = def.element as z.ZodType;
    } else {
      return undefined;
    }
  }
  return unwrap(current);
}

function describeInput(value: unknown): string {
  if (value === null) return "null";
  if (Array.isArray(value)) return "an array";
  switch (typeof value) {
    case "number":
    case "boolean":
      return String(value);
    case "string":
      return "a string";
    case "object":
      return "an object";
    default:
      return typeof value;
  }
}

function describeExpected(expected: string, schema: z.ZodType | undefined): string {
  switch (expected) {
    case "int":
      return "an integer";
    case "object": {
      const shape = shapeOf(schema);
      const keys =
        shape === undefined
          ? []
          : Object.entries(shape).map(
              ([key, field]) => `${key}${field.def.type === "optional" ? "?" : ""}`
            );
      return keys.length === 0 ? "an object" : `an object {${keys.join(", ")}}`;
    }
    case "array":
      return "an array";
    default:
      return `a ${expected}`;
  }
}

/**
 * One line per zod issue, worded for the model that made the call: what is missing,
 * what is unknown (and what would be accepted), what the allowed values are, and how far
 * over a cap a value is. `schema` is the strict object schema that produced the issues;
 * it supplies the allowed top-level keys and nested object shapes. Parse with
 * `{ reportInput: true }` so string lengths and rejected values are available.
 */
export function formatZodIssues(issues: readonly z.core.$ZodIssue[], schema: z.ZodType): string[] {
  const allowed = Object.keys(shapeOf(schema) ?? {}).join(", ");
  return issues.flatMap((issue) => {
    const path = issue.path.map(String).join(".");
    switch (issue.code) {
      case "invalid_type":
        return issue.input === undefined
          ? [`${path} is required (${issue.expected})`]
          : [
              `${path} must be ${describeExpected(issue.expected, schemaAt(schema, issue.path))}, not ${describeInput(issue.input)}`,
            ];
      case "unrecognized_keys":
        return issue.keys.map((key) => `unknown field "${key}"; allowed: ${allowed}`);
      case "invalid_value":
        return [
          `${path} must be one of ${issue.values.map(String).join("|")}; got ${JSON.stringify(issue.input)}`,
        ];
      case "too_big":
        if (issue.origin === "array" && Array.isArray(issue.input)) {
          return [`${path} has ${issue.input.length} items; the limit is ${issue.maximum}`];
        }
        return [`${path} ${issue.message}`];
      case "custom":
        return [issue.message];
      default:
        return [path === "" ? issue.message : `${path}: ${issue.message}`];
    }
  });
}
