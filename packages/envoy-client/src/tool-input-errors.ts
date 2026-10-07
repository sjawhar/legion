import { dispatchToolSpecs } from "@legion/contracts";
import type { z } from "zod";
import { commandFlags, commandLine, commandName, fieldFlag } from "./dispatch-command";

/**
 * How a refusal names the call: `json` for a native tool (`envoy_send was not called`, field names
 * as written in its JSON arguments), `cli` for a `dispatch` command (`dispatch message was not
 * called`, each field by the flag that sets it, then the command's flags and an example).
 */
export type RefusalSyntax = "json" | "cli";

/**
 * A tool call refused before any request left the process. `problems` lists every
 * defect found in one pass so the caller fixes them all with a single retry.
 */
export class ToolInputError extends Error {
  readonly tool: string;
  readonly problems: readonly string[];

  constructor(
    tool: string,
    problems: readonly string[],
    options: { readonly syntax?: RefusalSyntax } = {}
  ) {
    const count = problems.length;
    const cli = options.syntax === "cli" && isDispatchTool(tool);
    const help = cli
      ? [
          `Allowed flags: ${commandFlags(tool).join(", ")}`,
          `Example: ${commandLine(tool, exampleFor(tool))}`,
        ]
      : [];
    super(
      [
        `${cli ? `dispatch ${commandName(tool)}` : tool} was not called: ${count} problem${count === 1 ? "" : "s"}`,
        ...problems.map((problem) => `- ${problem}`),
        ...help.map((line) => `- ${line}`),
      ].join("\n")
    );
    this.name = "ToolInputError";
    this.tool = tool;
    this.problems = problems;
  }
}

function isDispatchTool(tool: string): boolean {
  return dispatchToolSpecs.some((spec) => spec.name === tool);
}

function exampleFor(tool: string): Record<string, unknown> {
  const spec = dispatchToolSpecs.find((candidate) => candidate.name === tool);
  return { ...spec?.example };
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
 * How an issue path is written. As JSON it is the dotted path (`options.1.label`); as a command
 * line its first segment is the flag that sets the field and the rest follows it, so
 * `options.1.label` reads `--option[1] label` and `ops.0.find` reads `--ops-json[0] find`.
 */
function pathText(path: readonly PropertyKey[], cliTool: string | undefined): string {
  const [head, ...rest] = path.map(String);
  if (cliTool === undefined || head === undefined) return path.map(String).join(".");
  let flag = fieldFlag(cliTool, head);
  let index = 0;
  for (; index < rest.length && /^\d+$/.test(rest[index] as string); index++) {
    flag += `[${rest[index]}]`;
  }
  const tail = rest
    .slice(index)
    .map((segment) => (/^\d+$/.test(segment) ? `[${segment}]` : `.${segment}`))
    .join("")
    .replace(/^\./, "");
  return tail === "" ? flag : `${flag} ${tail}`;
}

/**
 * One line per zod issue, worded for the model that made the call: what is missing,
 * what is unknown (and what would be accepted), what the allowed values are, and how far
 * over a cap a value is. `schema` is the strict object schema that produced the issues;
 * it supplies the allowed top-level keys and nested object shapes. Parse with
 * `{ reportInput: true }` so string lengths and rejected values are available. With
 * `syntax: "cli"`, each field is named by the `dispatch` flag that sets it.
 */
export function formatZodIssues(
  issues: readonly z.core.$ZodIssue[],
  schema: z.ZodType,
  options: { readonly syntax?: "json" } | { readonly syntax: "cli"; readonly tool: string } = {}
): string[] {
  const cliTool = options.syntax === "cli" ? options.tool : undefined;
  const allowed = Object.keys(shapeOf(schema) ?? {}).join(", ");
  return issues.flatMap((issue) => {
    const path = pathText(issue.path, cliTool);
    switch (issue.code) {
      case "invalid_type":
        return issue.input === undefined
          ? [`${path} is required (${issue.expected})`]
          : [
              `${path} must be ${describeExpected(issue.expected, schemaAt(schema, issue.path))}, not ${describeInput(issue.input)}`,
            ];
      case "unrecognized_keys": {
        if (cliTool !== undefined && issue.path.length === 0) {
          return issue.keys.map((key) => `unknown flag ${fieldFlag(cliTool, key)}`);
        }
        // A nested object's allowed keys are its own: the tool's top-level keys would send a
        // model that mistyped a document-edit operation's key back with the same operation.
        const here = Object.keys(shapeOf(schemaAt(schema, issue.path)) ?? {}).join(", ") || allowed;
        const where = path === "" ? "" : ` in ${path}`;
        return issue.keys.map((key) => `unknown field "${key}"${where}; allowed: ${here}`);
      }
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
