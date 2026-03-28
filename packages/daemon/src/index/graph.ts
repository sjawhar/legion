import { readdir, readFile, stat } from "node:fs/promises";
import path from "node:path";
import { parseExportedSymbols, parseImportSpecifiers } from "./parser";
import type { CodebaseIndex, ExportedSymbol, ModuleDependencyEntry, TestMapping } from "./types";
import { CODEBASE_INDEX_VERSION } from "./types";

const SOURCE_EXTENSIONS = [".ts", ".tsx", ".js", ".jsx"];
const RESOLUTION_SUFFIXES = [".ts", ".tsx", ".js", ".jsx", "/index.ts", "/index.js"];
const SKIP_DIRS = new Set(["node_modules", ".git", ".jj", "dist", "build"]);

function isSourceFile(filePath: string): boolean {
  return SOURCE_EXTENSIONS.includes(path.extname(filePath));
}

function isTestFile(relativeFilePath: string): boolean {
  if (/\.(test|spec)\.[jt]sx?$/.test(relativeFilePath)) {
    return true;
  }

  return /(^|\/)__tests__\/.*\.[jt]sx?$/.test(relativeFilePath);
}

function buildTestMapping(
  allRelativePaths: string[],
  dependencyGraph: Record<string, ModuleDependencyEntry>
): TestMapping {
  const sourceFiles = allRelativePaths.filter((relativePath) => !isTestFile(relativePath));
  const testFiles = allRelativePaths.filter((relativePath) => isTestFile(relativePath));
  const sourceFileSet = new Set(sourceFiles);
  const sourceToTests: Record<string, string[]> = {};
  const testToSources: Record<string, string[]> = {};

  for (const sourceFile of sourceFiles) {
    sourceToTests[sourceFile] = [];
  }

  for (const testFile of testFiles) {
    const directImports = dependencyGraph[testFile]?.imports ?? [];
    const mappedSources = new Set<string>();

    for (const importedFile of directImports) {
      if (!sourceFileSet.has(importedFile)) {
        continue;
      }
      mappedSources.add(importedFile);
    }

    const sources = [...mappedSources].sort((a, b) => a.localeCompare(b));
    testToSources[testFile] = sources;

    for (const sourceFile of sources) {
      sourceToTests[sourceFile]?.push(testFile);
    }
  }

  for (const tests of Object.values(sourceToTests)) {
    tests.sort((a, b) => a.localeCompare(b));
  }

  return {
    sourceToTests,
    testToSources,
  };
}

export async function listSourceFiles(rootDir: string): Promise<string[]> {
  const files: string[] = [];

  async function walk(currentDir: string): Promise<void> {
    const entries = await readdir(currentDir, { withFileTypes: true });
    await Promise.all(
      entries.map(async (entry) => {
        const absolutePath = path.join(currentDir, entry.name);
        if (entry.isDirectory()) {
          if (SKIP_DIRS.has(entry.name)) {
            return;
          }
          await walk(absolutePath);
          return;
        }

        if (!entry.isFile() || !isSourceFile(absolutePath)) {
          return;
        }
        files.push(absolutePath);
      })
    );
  }

  await walk(rootDir);
  files.sort((a, b) => a.localeCompare(b));
  return files;
}

function toPosixRelativePath(rootDir: string, absolutePath: string): string {
  return path.relative(rootDir, absolutePath).split(path.sep).join("/");
}

async function resolveRelativeImport(
  rootDir: string,
  importerAbsolutePath: string,
  importSpecifier: string
): Promise<string | null> {
  const importerDir = path.dirname(importerAbsolutePath);
  const basePath = path.resolve(importerDir, importSpecifier);

  for (const suffix of RESOLUTION_SUFFIXES) {
    const candidate = `${basePath}${suffix}`;
    try {
      const candidateStat = await stat(candidate);
      if (!candidateStat.isFile()) {
        continue;
      }
      return toPosixRelativePath(rootDir, candidate);
    } catch {}
  }

  return null;
}

export interface BuildDependencyGraphOptions {
  warn?: (message: string) => void;
  hotspotHistoryLimit?: number;
  hotspotCommandRunner?: (
    rootDir: string,
    args: string[]
  ) => Promise<{ stdout: string; stderr: string; exitCode: number }>;
}

interface ParsedModuleEntry {
  relativeFilePath: string;
  dependencies: ModuleDependencyEntry;
  apiSurface: ExportedSymbol[];
  mtimeMs: number;
}

async function parseModuleFile(
  rootDir: string,
  absolutePath: string,
  options?: BuildDependencyGraphOptions
): Promise<ParsedModuleEntry> {
  const source = await readFile(absolutePath, "utf-8");
  const stats = await stat(absolutePath);
  const parsed = parseImportSpecifiers(source);
  const relativeFilePath = toPosixRelativePath(rootDir, absolutePath);
  const imports: string[] = [];

  for (const specifier of parsed.relative) {
    const resolved = await resolveRelativeImport(rootDir, absolutePath, specifier);
    if (!resolved) {
      options?.warn?.(
        `[index] Unresolvable import "${specifier}" in ${toPosixRelativePath(rootDir, absolutePath)}`
      );
      continue;
    }
    imports.push(resolved);
  }

  return {
    relativeFilePath,
    dependencies: {
      imports,
      externals: parsed.externals,
    },
    apiSurface: parseExportedSymbols(source),
    mtimeMs: stats.mtimeMs,
  };
}

export async function buildDependencyGraph(
  rootDir: string,
  options?: BuildDependencyGraphOptions
): Promise<CodebaseIndex> {
  const sourceFiles = await listSourceFiles(rootDir);
  const dependencyGraph: Record<string, ModuleDependencyEntry> = {};
  const apiSurface: Record<string, ExportedSymbol[]> = {};
  const mtimes: Record<string, number> = {};
  const indexedRelativePaths: string[] = [];

  for (const absolutePath of sourceFiles) {
    const parsed = await parseModuleFile(rootDir, absolutePath, options);
    dependencyGraph[parsed.relativeFilePath] = parsed.dependencies;
    apiSurface[parsed.relativeFilePath] = parsed.apiSurface;
    mtimes[parsed.relativeFilePath] = parsed.mtimeMs;
    indexedRelativePaths.push(parsed.relativeFilePath);
  }

  const testMapping = buildTestMapping(indexedRelativePaths, dependencyGraph);

  return {
    version: CODEBASE_INDEX_VERSION,
    dependencyGraph,
    apiSurface,
    testMapping,
    hotspots: [],
    metadata: {
      generatedAt: new Date().toISOString(),
      rootDir,
      fileCount: sourceFiles.length,
      mtimes,
    },
  };
}

export async function updateDependencyGraphIncremental(
  current: CodebaseIndex,
  options?: BuildDependencyGraphOptions
): Promise<CodebaseIndex> {
  const rootDir = current.metadata.rootDir;
  const sourceFiles = await listSourceFiles(rootDir);
  const nextGraph: Record<string, ModuleDependencyEntry> = {};
  const nextApiSurface: Record<string, ExportedSymbol[]> = {};
  const nextMtimes: Record<string, number> = {};
  const sourceByRelativePath = new Map<string, string>();

  for (const absolutePath of sourceFiles) {
    const relativePath = toPosixRelativePath(rootDir, absolutePath);
    sourceByRelativePath.set(relativePath, absolutePath);
  }

  for (const [relativePath, absolutePath] of sourceByRelativePath.entries()) {
    let mtimeMs: number;
    try {
      mtimeMs = (await stat(absolutePath)).mtimeMs;
    } catch {
      continue;
    }

    const previousMtime = current.metadata.mtimes[relativePath];
    if (previousMtime !== undefined && previousMtime === mtimeMs) {
      const existing = current.dependencyGraph[relativePath];
      const existingApiSurface = current.apiSurface[relativePath];
      if (existing && existingApiSurface) {
        nextGraph[relativePath] = existing;
        nextApiSurface[relativePath] = existingApiSurface;
        nextMtimes[relativePath] = previousMtime;
        continue;
      }
    }

    const parsed = await parseModuleFile(rootDir, absolutePath, options);
    nextGraph[relativePath] = parsed.dependencies;
    nextApiSurface[relativePath] = parsed.apiSurface;
    nextMtimes[relativePath] = parsed.mtimeMs;
  }

  const testMapping = buildTestMapping([...sourceByRelativePath.keys()], nextGraph);

  return {
    version: CODEBASE_INDEX_VERSION,
    dependencyGraph: nextGraph,
    apiSurface: nextApiSurface,
    testMapping,
    hotspots: current.hotspots,
    metadata: {
      generatedAt: new Date().toISOString(),
      rootDir,
      fileCount: sourceFiles.length,
      mtimes: nextMtimes,
    },
  };
}
