import {
  type DispatchToolSpec,
  dispatchToolSchema,
  dispatchToolSpecs,
  zodSchemaApi,
} from "@legion/contracts";
import { z } from "zod";
import { messageFor } from "./errors";

/**
 * The `dispatch` command surface: one command per Dispatch tool spec and one flag per field, all
 * derived from each spec's JSON schema, so a new field or tool needs no edit here.
 *
 * A field's kind decides its flags:
 * - string or enum: `--<field> <value>`, and `--<field>-file <path>` (`-` reads stdin);
 * - integer or number: `--<field> <n>`;
 * - boolean: `--<field>` (true) and `--no-<field>` (false);
 * - nullable: `--clear-<field>` sends null;
 * - array of scalars: the singular flag, repeated (`--label a --label b`), and `--clear-<field>`
 *   sends an empty list; in an array of numbers that admits null, `none` is null;
 * - `options`: `--option "Label: description"`, repeated, split at the first ": "; and
 *   `--options-json <json>` for a list whose labels hold ": " themselves, which `commandLine`
 *   writes for such a list so every list round-trips;
 * - any other object or array: `--<field>-json <json>` and `--<field>-json-file <path>`.
 * Every command also takes `--help` and `--dry-run`.
 */

type ScalarArray = "string" | "number" | "number-or-null";
type Kind = "string" | "number" | "boolean" | "options" | "json" | { readonly array: ScalarArray };

interface FieldInfo {
  readonly kind: Kind;
  readonly nullable: boolean;
  readonly required: boolean;
  readonly description?: string;
  readonly values?: readonly string[];
}

type FlagAction =
  | "set"
  | "file"
  | "json"
  | "json-file"
  | "append"
  | "clear"
  | "true"
  | "false"
  | "help"
  | "dry-run";

interface FlagEntry {
  readonly field?: string;
  readonly action: FlagAction;
  readonly value?: string;
  readonly text: string;
}

interface CommandSurface {
  readonly spec: DispatchToolSpec;
  readonly command: string;
  readonly fields: ReadonlyMap<string, FieldInfo>;
  readonly flags: ReadonlyMap<string, FlagEntry>;
}

interface JsonSchemaNode {
  readonly type?: string;
  readonly enum?: readonly unknown[];
  readonly anyOf?: readonly JsonSchemaNode[];
  readonly items?: JsonSchemaNode;
  readonly properties?: Readonly<Record<string, JsonSchemaNode>>;
  readonly required?: readonly string[];
  readonly description?: string;
}

export type ParsedCommand =
  | {
      readonly kind: "call";
      readonly tool: string;
      readonly args: Record<string, unknown>;
      readonly dryRun: boolean;
    }
  | { readonly kind: "help"; readonly tool?: string }
  | { readonly kind: "refused"; readonly tool?: string; readonly problems: string[] };

export interface CommandFlagRow {
  /** The flag as written, with its value placeholder: `--issue <text>`. */
  readonly flag: string;
  readonly text: string;
}

const STDIN = "-";
const BARE_WORD = /^[A-Za-z0-9_./:@%+=,-]+$/;
const OPTION_SEPARATOR = ": ";

/** `dispatch_issue_update` → `issue-update`. */
export function commandName(tool: string): string {
  return tool.replace(/^dispatch_/, "").replaceAll("_", "-");
}

/** `reply_to_ask` → `reply-to-ask`. */
export function flagName(field: string): string {
  return field.replaceAll("_", "-");
}

const toolsByCommand = new Map(
  dispatchToolSpecs.map((spec) => [commandName(spec.name), spec.name])
);

/** The spec name a command runs, or undefined for a command no spec defines. */
export function toolForCommand(command: string): string | undefined {
  return toolsByCommand.get(command);
}

/** Every flag spelling the command accepts, `--help` and `--dry-run` included. */
export function commandFlags(tool: string): readonly string[] {
  return [...surfaceFor(tool).flags.keys()];
}

/** The flag that sets `field` on the command: `--option` for `options`, `--ops-json` for `ops`. */
export function fieldFlag(tool: string, field: string): string {
  for (const [flag, entry] of surfaceFor(tool).flags) {
    if (entry.field === field) return flag;
  }
  return `--${flagName(field)}`;
}

/** The command's flags with their value placeholders and what each sends, for help and docs. */
export function commandFlagTable(tool: string): readonly CommandFlagRow[] {
  return [...surfaceFor(tool).flags].map(([flag, entry]) => ({
    flag: entry.value === undefined ? flag : `${flag} ${entry.value}`,
    text: entry.text,
  }));
}

const surfaces = new Map<string, CommandSurface>();

function surfaceFor(tool: string): CommandSurface {
  const cached = surfaces.get(tool);
  if (cached !== undefined) return cached;
  const spec = dispatchToolSpecs.find((candidate) => candidate.name === tool);
  if (spec === undefined) throw new Error(`Unknown Dispatch tool: ${tool}`);
  const fields = fieldKinds(spec);
  const surface: CommandSurface = {
    spec,
    command: commandName(spec.name),
    fields,
    flags: flagTable(spec.name, fields),
  };
  surfaces.set(tool, surface);
  return surface;
}

function fieldKinds(spec: DispatchToolSpec): ReadonlyMap<string, FieldInfo> {
  const schema = z.toJSONSchema(dispatchToolSchema(spec, zodSchemaApi(z))) as JsonSchemaNode;
  const required = new Set(schema.required ?? []);
  const fields = new Map<string, FieldInfo>();
  for (const [field, node] of Object.entries(schema.properties ?? {})) {
    const nonNull = (node.anyOf ?? [node]).filter((member) => member.type !== "null");
    const nullable = node.anyOf !== undefined && nonNull.length < node.anyOf.length;
    const inner = nonNull.length === 1 ? nonNull[0] : undefined;
    const kind = inner === undefined ? "json" : kindOf(field, inner);
    const values = inner?.enum?.map(String);
    const description = node.description ?? inner?.description;
    fields.set(field, {
      kind,
      nullable,
      required: required.has(field),
      ...(description === undefined ? {} : { description }),
      ...(values === undefined ? {} : { values }),
    });
  }
  return fields;
}

function kindOf(field: string, node: JsonSchemaNode): Kind {
  switch (node.type) {
    case "string":
      return "string";
    case "integer":
    case "number":
      return "number";
    case "boolean":
      return "boolean";
    case "array": {
      const item = node.items;
      if (item === undefined) return "json";
      if (field === "options" && item.type === "object") return "options";
      if (item.type === "string") return { array: "string" };
      if (item.type === "integer" || item.type === "number") return { array: "number" };
      const members = item.anyOf ?? [];
      const numeric = members.filter((m) => m.type === "integer" || m.type === "number");
      if (
        members.length === 2 &&
        numeric.length === 1 &&
        members.some((member) => member.type === "null")
      ) {
        return { array: "number-or-null" };
      }
      return "json";
    }
    default:
      return "json";
  }
}

function singular(field: string): string {
  return field.endsWith("s") ? field.slice(0, -1) : field;
}

function placeholder(info: FieldInfo): string {
  if (info.values !== undefined) return `<${info.values.join("|")}>`;
  if (info.kind === "number") return "<n>";
  if (typeof info.kind === "object") return info.kind.array === "string" ? "<text>" : "<n>";
  return "<text>";
}

function flagTable(tool: string, fields: ReadonlyMap<string, FieldInfo>): Map<string, FlagEntry> {
  const flags = new Map<string, FlagEntry>();
  const add = (flag: string, entry: FlagEntry): void => {
    if (flags.has(flag)) throw new Error(`${tool}: two fields claim the flag ${flag}`);
    flags.set(flag, entry);
  };
  for (const [field, info] of fields) {
    const name = flagName(field);
    const about = [info.description, info.required ? "Required." : undefined]
      .filter((part) => part !== undefined)
      .join(" ");
    const { kind } = info;
    if (kind === "string") {
      add(`--${name}`, { field, action: "set", value: placeholder(info), text: about });
      add(`--${name}-file`, {
        field,
        action: "file",
        value: "<path>",
        text: `--${name} read from a file; - reads stdin.`,
      });
    } else if (kind === "number") {
      add(`--${name}`, { field, action: "set", value: "<n>", text: about });
    } else if (kind === "boolean") {
      add(`--${name}`, { field, action: "true", text: about });
      add(`--no-${name}`, { field, action: "false", text: `Sends ${field} as false.` });
    } else if (kind === "options") {
      add(`--${singular(name)}`, {
        field,
        action: "append",
        value: '"<label>: <description>"',
        text: `${about} One option per flag, repeated; split at the first ": " (without one, the whole value is the label). A label that holds ": " goes in --${name}-json.`.trim(),
      });
      add(`--${name}-json`, {
        field,
        action: "json",
        value: "<json>",
        text: `Every option as a JSON list of {label, description?}, in place of --${singular(name)}.`,
      });
    } else if (kind === "json") {
      add(`--${name}-json`, { field, action: "json", value: "<json>", text: about });
      add(`--${name}-json-file`, {
        field,
        action: "json-file",
        value: "<path>",
        text: `--${name}-json read from a file; - reads stdin.`,
      });
    } else {
      const none = kind.array === "number-or-null" ? " none is null." : "";
      add(`--${singular(name)}`, {
        field,
        action: "append",
        value: placeholder(info),
        text: `${about} One item per flag, repeated.${none}`.trim(),
      });
    }
    if (info.nullable || typeof kind === "object" || kind === "options") {
      add(`--clear-${name}`, {
        field,
        action: "clear",
        text: info.nullable ? `Sends ${field} as null.` : `Sends ${field} as an empty list.`,
      });
    }
  }
  add("--help", { action: "help", text: "Prints this help." });
  add("--dry-run", { action: "dry-run", text: "Prints the arguments as JSON and sends nothing." });
  return flags;
}

/** Parses `dispatch` argv (without the program name) into the arguments the tool takes. */
export function parseCommand(
  argv: readonly string[],
  io: { readText(path: string): string }
): ParsedCommand {
  const [command, ...rest] = argv;
  if (command === undefined || command === "--help") return { kind: "help" };
  const tool = toolForCommand(command);
  if (tool === undefined) {
    const commands = [...toolsByCommand.keys()].join(", ");
    return {
      kind: "refused",
      problems: [`unknown command ${quoteWord(command)}; the commands are: ${commands}`],
    };
  }
  const surface = surfaceFor(tool);

  const args: Record<string, unknown> = {};
  const setBy = new Map<string, string>();
  const problems: string[] = [];
  let help = false;
  let dryRun = false;
  let stdinFlag: string | undefined;

  const read = (flag: string, path: string): string | undefined => {
    if (path === STDIN) {
      if (stdinFlag !== undefined) {
        problems.push(`${stdinFlag} and ${flag} both read stdin (-); only one flag can`);
        return undefined;
      }
      stdinFlag = flag;
    }
    try {
      return io.readText(path);
    } catch (error) {
      problems.push(
        `${flag} could not read ${path === STDIN ? "stdin" : path}: ${messageFor(error)}`
      );
      return undefined;
    }
  };
  const claim = (field: string, flag: string, repeatable: boolean): boolean => {
    const earlier = setBy.get(field);
    if (earlier === undefined || (repeatable && earlier === flag)) {
      setBy.set(field, flag);
      return true;
    }
    problems.push(
      earlier === flag ? `${flag} is given twice` : `${earlier} and ${flag} both set ${field}`
    );
    return false;
  };
  const number = (flag: string, value: string): number | undefined => {
    const parsed = Number(value);
    if (value.trim() === "" || !Number.isFinite(parsed)) {
      problems.push(`${flag} must be a number, not ${JSON.stringify(value)}`);
      return undefined;
    }
    return parsed;
  };
  const json = (flag: string, text: string): { value: unknown } | undefined => {
    try {
      return { value: JSON.parse(text) };
    } catch (error) {
      problems.push(`${flag} is not JSON: ${messageFor(error)}`);
      return undefined;
    }
  };

  for (let index = 0; index < rest.length; index++) {
    const token = rest[index] as string;
    if (!token.startsWith("--")) {
      problems.push(`unexpected argument ${quoteWord(token)}: every value follows its flag`);
      continue;
    }
    const eq = token.indexOf("=");
    const flag = eq === -1 ? token : token.slice(0, eq);
    const inline = eq === -1 ? undefined : token.slice(eq + 1);
    const entry = surface.flags.get(flag);
    if (entry === undefined) {
      problems.push(`unknown flag ${flag}`);
      const next = rest[index + 1];
      if (inline === undefined && next !== undefined && !next.startsWith("--")) index++;
      continue;
    }
    const { field, action } = entry;
    if (action === "help" || action === "dry-run" || action === "true" || action === "false") {
      if (inline !== undefined) {
        problems.push(`${flag} takes no value`);
        continue;
      }
      if (action === "help") help = true;
      else if (action === "dry-run") dryRun = true;
      else if (field !== undefined && claim(field, flag, false)) args[field] = action === "true";
      continue;
    }
    if (field === undefined) continue;
    const info = surface.fields.get(field) as FieldInfo;
    if (action === "clear") {
      if (inline !== undefined) problems.push(`${flag} takes no value`);
      else if (claim(field, flag, false)) args[field] = info.nullable ? null : [];
      continue;
    }
    let value = inline;
    if (value === undefined) {
      const next = rest[index + 1];
      // A value that starts with `--` is written `--flag=--value`, so a typo'd next flag reads
      // as this flag's missing value rather than as its text.
      if (next === undefined || next.startsWith("--")) {
        problems.push(`${flag} needs a value`);
        continue;
      }
      value = next;
      index++;
    }
    switch (action) {
      case "set": {
        // Claimed whatever the value, so a repeat is named even after a value that did not parse.
        const claimed = claim(field, flag, false);
        const parsed = info.kind === "number" ? number(flag, value) : value;
        if (claimed && parsed !== undefined) args[field] = parsed;
        break;
      }
      case "file": {
        const text = read(flag, value);
        if (text !== undefined && claim(field, flag, false)) args[field] = text;
        break;
      }
      case "json":
      case "json-file": {
        const text = action === "json" ? value : read(flag, value);
        const parsed = text === undefined ? undefined : json(flag, text);
        if (parsed !== undefined && claim(field, flag, false)) args[field] = parsed.value;
        break;
      }
      case "append": {
        if (!claim(field, flag, true)) break;
        const item = appendedItem(info.kind, flag, value, number);
        if (item === undefined) break;
        const list = (args[field] as unknown[] | undefined) ?? [];
        list.push(item.value);
        args[field] = list;
        break;
      }
    }
  }

  if (help) return { kind: "help", tool };
  if (problems.length > 0) return { kind: "refused", tool, problems };
  return { kind: "call", tool, args, dryRun };
}

function appendedItem(
  kind: Kind,
  flag: string,
  value: string,
  number: (flag: string, value: string) => number | undefined
): { value: unknown } | undefined {
  if (kind === "options") {
    const at = value.indexOf(OPTION_SEPARATOR);
    return {
      value:
        at === -1
          ? { label: value }
          : { label: value.slice(0, at), description: value.slice(at + OPTION_SEPARATOR.length) },
    };
  }
  if (typeof kind !== "object" || kind.array === "string") return { value };
  if (kind.array === "number-or-null" && value === "none") return { value: null };
  const parsed = number(flag, value);
  return parsed === undefined ? undefined : { value: parsed };
}

/** A shell word: bare when it needs no quoting, otherwise POSIX single-quoted. */
function quoteWord(value: string): string {
  return BARE_WORD.test(value) ? value : `'${value.replaceAll("'", "'\\''")}'`;
}

/** `--flag value`, or `--flag=value` when the value itself starts with `--`. */
function flagWithValue(flag: string, value: string): string[] {
  return value.startsWith("--") ? [`${flag}=${quoteWord(value)}`] : [flag, quoteWord(value)];
}

function scalarText(value: unknown): string {
  return typeof value === "string" ? value : JSON.stringify(value);
}

function fieldWords(field: string, info: FieldInfo, value: unknown): string[] {
  const name = flagName(field);
  const { kind } = info;
  if (value === null && info.nullable) return [`--clear-${name}`];
  if (kind === "boolean" && typeof value === "boolean") {
    return [value ? `--${name}` : `--no-${name}`];
  }
  if (kind === "json") return flagWithValue(`--${name}-json`, JSON.stringify(value));
  if (Array.isArray(value) && (kind === "options" || typeof kind === "object")) {
    if (value.length === 0) return [`--clear-${name}`];
    if (kind === "options") {
      const texts = value.map(optionText);
      // A label holding the separator cannot be written as `--option`, whose value splits at it.
      if (texts.includes(undefined)) return flagWithValue(`--${name}-json`, JSON.stringify(value));
      return texts.flatMap((text) => flagWithValue(`--${singular(name)}`, text ?? ""));
    }
    const flag = `--${singular(name)}`;
    return value.flatMap((item) => flagWithValue(flag, item === null ? "none" : scalarText(item)));
  }
  return flagWithValue(`--${name}`, scalarText(value));
}

/** An option as `--option` writes it, or undefined when its label holds the separator. */
function optionText(option: unknown): string | undefined {
  if (typeof option !== "object" || option === null || !("label" in option)) {
    return scalarText(option);
  }
  const label = scalarText(option.label);
  if (label.includes(OPTION_SEPARATOR)) return undefined;
  return "description" in option && option.description !== undefined
    ? `${label}${OPTION_SEPARATOR}${scalarText(option.description)}`
    : label;
}

/** The command line that sends `args`: `dispatch <command> <flags>`, POSIX single-quoted. */
export function commandLine(tool: string, args: Record<string, unknown>): string {
  const surface = surfaceFor(tool);
  const words = ["dispatch", surface.command];
  for (const [field, info] of surface.fields) {
    const value = args[field];
    if (value !== undefined) words.push(...fieldWords(field, info, value));
  }
  for (const [field, value] of Object.entries(args)) {
    if (value !== undefined && !surface.fields.has(field)) {
      words.push(...flagWithValue(`--${flagName(field)}`, scalarText(value)));
    }
  }
  return words.join(" ");
}

/** One command's help: its description, each flag, and an example. */
export function commandHelp(tool: string): string {
  const surface = surfaceFor(tool);
  return [
    `Usage: dispatch ${surface.command} [flags]`,
    "",
    surface.spec.description,
    "",
    "Flags:",
    ...commandFlagTable(tool).flatMap((row) =>
      row.text === "" ? [`  ${row.flag}`] : [`  ${row.flag}`, `      ${row.text}`]
    ),
    "",
    `Example: ${commandLine(tool, surface.spec.example as Record<string, unknown>)}`,
    "",
  ].join("\n");
}

/** Every command, with the first sentence of what it does. */
export function commandsHelp(): string {
  const width = Math.max(...[...toolsByCommand.keys()].map((command) => command.length));
  return [
    "Usage: dispatch <command> [flags]",
    ...dispatchToolSpecs.map((spec) => {
      const sentence = /^.*?[.!?](?=\s|$)/s.exec(spec.description)?.[0] ?? spec.description;
      return `  ${commandName(spec.name).padEnd(width)}  ${sentence}`;
    }),
    "",
    "Run dispatch <command> --help for its flags and an example; --dry-run prints the arguments a command would send.",
    "",
  ].join("\n");
}
