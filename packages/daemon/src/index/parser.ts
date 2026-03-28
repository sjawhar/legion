const IMPORT_SOURCE_PATTERNS = [
  /\bimport\s+type\s+[^"']*?from\s*["']([^"']+)["']/g,
  /\bimport\s+[^"']*?from\s*["']([^"']+)["']/g,
  /\bimport\s*["']([^"']+)["']/g,
  /\bexport\s+\*\s+from\s*["']([^"']+)["']/g,
  /\bexport\s+\{[^}]*\}\s+from\s*["']([^"']+)["']/g,
];

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
