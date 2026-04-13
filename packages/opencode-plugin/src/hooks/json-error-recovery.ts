import { isRecord } from "./utils";

/**
 * Attempt to repair common JSON malformations.
 *
 * Handles:
 * - Trailing commas: `{a: 1,}` → `{a: 1}`
 * - Single quotes: `{'key': 'val'}` → `{"key": "val"}`
 * - Unquoted keys: `{key: "val"}` → `{"key": "val"}`
 *
 * Returns the repaired JSON string if successful, or null if unrecoverable.
 */
export function repairJson(input: string): string | null {
  if (!input) return null;

  // Try parsing as-is first (fast path for valid JSON)
  try {
    JSON.parse(input);
    return input;
  } catch {
    // Continue to repair attempts
  }

  let repaired = input;

  // Step 1: Replace single-quoted strings with double-quoted strings.
  // This handles: {'key': 'val'} → {"key": "val"}
  // We walk character by character to handle escaping correctly.
  repaired = replaceSingleQuotes(repaired);

  // Step 2: Quote unquoted keys.
  // This handles: {key: "val"} → {"key": "val"}
  // Match word characters after { or , that are followed by :
  repaired = repaired.replace(/([{,]\s*)([a-zA-Z_$][a-zA-Z0-9_$]*)(\s*:)/g, '$1"$2"$3');

  // Step 3: Remove trailing commas before } or ]
  // This handles: {a: 1,} → {a: 1} and [1, 2,] → [1, 2]
  repaired = repaired.replace(/,\s*([}\]])/g, "$1");

  // Verify the repaired string is valid JSON
  try {
    JSON.parse(repaired);
    return repaired;
  } catch {
    return null;
  }
}

/**
 * Replace single-quoted strings with double-quoted strings in a JSON-like string.
 * Handles escaped quotes within strings.
 */
function replaceSingleQuotes(input: string): string {
  const chars: string[] = [];
  let i = 0;
  while (i < input.length) {
    if (input[i] === '"') {
      // Already in a double-quoted string — pass through until closing "
      chars.push('"');
      i++;
      while (i < input.length && input[i] !== '"') {
        if (input[i] === "\\" && i + 1 < input.length) {
          chars.push(input[i], input[i + 1]);
          i += 2;
        } else {
          chars.push(input[i]);
          i++;
        }
      }
      if (i < input.length) {
        chars.push('"');
        i++;
      }
    } else if (input[i] === "'") {
      // Single-quoted string — convert to double quotes
      chars.push('"');
      i++;
      while (i < input.length && input[i] !== "'") {
        if (input[i] === "\\" && i + 1 < input.length) {
          chars.push(input[i], input[i + 1]);
          i += 2;
        } else if (input[i] === '"') {
          // Escape double quotes that appear inside the now-double-quoted string
          chars.push('\\"');
          i++;
        } else {
          chars.push(input[i]);
          i++;
        }
      }
      chars.push('"');
      if (i < input.length) i++;
    } else {
      chars.push(input[i]);
      i++;
    }
  }
  return chars.join("");
}

/**
 * Check if a string looks like it could be JSON (starts with { or [).
 */
function looksLikeJson(value: string): boolean {
  const trimmed = value.trimStart();
  return trimmed.startsWith("{") || trimmed.startsWith("[");
}

/**
 * tool.execute.before hook that detects malformed JSON in tool arguments
 * and attempts auto-repair for common patterns.
 *
 * If repair succeeds, the repaired value replaces the original in output.args.
 * If repair fails, the original value is preserved (downstream sees original error).
 */
export function jsonErrorRecoveryHook(
  _input: { tool: string; sessionID: string; callID: string },
  output: { args: Record<string, unknown> }
): void {
  if (!isRecord(output) || !isRecord(output.args)) return;

  for (const key of Object.keys(output.args)) {
    const value = output.args[key];
    if (typeof value !== "string") continue;
    if (!looksLikeJson(value)) continue;

    const repaired = repairJson(value);
    if (repaired !== null && repaired !== value) {
      output.args[key] = repaired;
    }
  }
}
