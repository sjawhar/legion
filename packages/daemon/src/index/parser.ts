import type { ExportedSymbol } from "./types";

const IMPORT_SOURCE_PATTERNS = [
  /\bimport\s+type\s+[^"']*?from\s*["']([^"']+)["']/g,
  /\bimport\s+[^"']*?from\s*["']([^"']+)["']/g,
  /\bimport\s*["']([^"']+)["']/g,
  /\bexport\s+\*\s+from\s*["']([^"']+)["']/g,
  /\bexport\s+\{[^}]*\}\s+from\s*["']([^"']+)["']/g,
];

const MAX_EXPORT_SIGNATURE_LENGTH = 200;
const EXPORT_FUNCTION_PATTERN = /^export\s+(?:async\s+)?function\s+([A-Za-z_$][\w$]*)\b/;
const EXPORT_CLASS_PATTERN = /^export\s+class\s+([A-Za-z_$][\w$]*)\b/;
const EXPORT_TYPE_PATTERN = /^export\s+type\s+([A-Za-z_$][\w$]*)\b/;
const EXPORT_INTERFACE_PATTERN = /^export\s+interface\s+([A-Za-z_$][\w$]*)\b/;
const EXPORT_CONST_PATTERN = /^export\s+const\s+([A-Za-z_$][\w$]*)\b/;
const EXPORT_LET_PATTERN = /^export\s+let\s+([A-Za-z_$][\w$]*)\b/;
const EXPORT_DEFAULT_PATTERN = /^export\s+default\b/;
const EXPORT_NAMED_REEXPORT_PATTERN = /^export\s+\{([^}]*)\}\s+from\b/;
const EXPORT_WILDCARD_REEXPORT_PATTERN = /^export\s+\*\s+from\b/;

export interface ParsedImports {
  relative: string[];
  externals: string[];
}

function isRelativeSpecifier(specifier: string): boolean {
  return specifier.startsWith("./") || specifier.startsWith("../");
}

export function parseImportSpecifiers(source: string): ParsedImports {
  const relative = new Set<string>();
  const externals = new Set<string>();

  for (const pattern of IMPORT_SOURCE_PATTERNS) {
    pattern.lastIndex = 0;
    for (const match of source.matchAll(pattern)) {
      const specifier = match[1]?.trim();
      if (!specifier) {
        continue;
      }

      if (isRelativeSpecifier(specifier)) {
        relative.add(specifier);
      } else {
        externals.add(specifier);
      }
    }
  }

  return {
    relative: [...relative],
    externals: [...externals],
  };
}

function normalizeSignature(line: string): string {
  return line.trim().slice(0, MAX_EXPORT_SIGNATURE_LENGTH);
}

function extractNamedReexportSymbols(specifiers: string): string[] {
  return specifiers
    .split(",")
    .map((value) => value.trim())
    .filter(Boolean)
    .map((value) => value.replace(/^type\s+/, ""))
    .map((value) => value.split(/\s+as\s+/i)[0]?.trim() ?? "")
    .filter(Boolean);
}

export function parseExportedSymbols(source: string): ExportedSymbol[] {
  const exportedSymbols: ExportedSymbol[] = [];

  for (const rawLine of source.split(/\r?\n/)) {
    const line = rawLine.trim();
    if (!line.startsWith("export ")) {
      continue;
    }

    if (EXPORT_WILDCARD_REEXPORT_PATTERN.test(line)) {
      exportedSymbols.push({
        name: "*",
        kind: "reexport",
        signature: normalizeSignature(line),
      });
      continue;
    }

    const namedReexportMatch = line.match(EXPORT_NAMED_REEXPORT_PATTERN);
    if (namedReexportMatch) {
      const names = extractNamedReexportSymbols(namedReexportMatch[1] ?? "");
      for (const name of names) {
        exportedSymbols.push({
          name,
          kind: "reexport",
          signature: normalizeSignature(line),
        });
      }
      continue;
    }

    if (EXPORT_DEFAULT_PATTERN.test(line)) {
      exportedSymbols.push({
        name: "default",
        kind: "default",
        signature: normalizeSignature(line),
      });
      continue;
    }

    const functionMatch = line.match(EXPORT_FUNCTION_PATTERN);
    if (functionMatch) {
      exportedSymbols.push({
        name: functionMatch[1],
        kind: "function",
        signature: normalizeSignature(line),
      });
      continue;
    }

    const classMatch = line.match(EXPORT_CLASS_PATTERN);
    if (classMatch) {
      exportedSymbols.push({
        name: classMatch[1],
        kind: "class",
        signature: normalizeSignature(line),
      });
      continue;
    }

    const typeMatch = line.match(EXPORT_TYPE_PATTERN);
    if (typeMatch) {
      exportedSymbols.push({
        name: typeMatch[1],
        kind: "type",
        signature: normalizeSignature(line),
      });
      continue;
    }

    const interfaceMatch = line.match(EXPORT_INTERFACE_PATTERN);
    if (interfaceMatch) {
      exportedSymbols.push({
        name: interfaceMatch[1],
        kind: "interface",
        signature: normalizeSignature(line),
      });
      continue;
    }

    const constMatch = line.match(EXPORT_CONST_PATTERN);
    if (constMatch) {
      exportedSymbols.push({
        name: constMatch[1],
        kind: "const",
        signature: normalizeSignature(line),
      });
      continue;
    }

    const letMatch = line.match(EXPORT_LET_PATTERN);
    if (letMatch) {
      exportedSymbols.push({
        name: letMatch[1],
        kind: "let",
        signature: normalizeSignature(line),
      });
    }
  }

  return exportedSymbols;
}
