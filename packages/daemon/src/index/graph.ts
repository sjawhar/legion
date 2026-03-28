import { readdir, readFile, stat } from "node:fs/promises";
import path from "node:path";
import { parseImportSpecifiers } from "./parser";
import type { CodebaseIndex, ModuleDependencyEntry } from "./types";
import { CODEBASE_INDEX_VERSION } from "./types";

const SOURCE_EXTENSIONS = [".ts", ".tsx", ".js", ".jsx"];
const RESOLUTION_SUFFIXES = [".ts", ".tsx", ".js", ".jsx", "/index.ts", "/index.js"];
const SKIP_DIRS = new Set(["node_modules", ".git", ".jj", "dist", "build"]);

function isSourceFile(filePath: string): boolean {
  return SOURCE_EXTENSIONS.includes(path.extname(filePath));
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
}

interface ParsedModuleEntry {
  relativeFilePath: string;
  dependencies: ModuleDependencyEntry;
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
    mtimeMs: stats.mtimeMs,
  };
}

export async function buildDependencyGraph(
  rootDir: string,
  options?: BuildDependencyGraphOptions
): Promise<CodebaseIndex> {
  const sourceFiles = await listSourceFiles(rootDir);
  const dependencyGraph: Record<string, ModuleDependencyEntry> = {};
  const mtimes: Record<string, number> = {};

  for (const absolutePath of sourceFiles) {
    const parsed = await parseModuleFile(rootDir, absolutePath, options);
    dependencyGraph[parsed.relativeFilePath] = parsed.dependencies;
    mtimes[parsed.relativeFilePath] = parsed.mtimeMs;
  }

  return {
    version: CODEBASE_INDEX_VERSION,
    dependencyGraph,
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
      if (existing) {
        nextGraph[relativePath] = existing;
        nextMtimes[relativePath] = previousMtime;
        continue;
      }
    }

    const parsed = await parseModuleFile(rootDir, absolutePath, options);
    nextGraph[relativePath] = parsed.dependencies;
    nextMtimes[relativePath] = parsed.mtimeMs;
  }

  return {
    version: CODEBASE_INDEX_VERSION,
    dependencyGraph: nextGraph,
    metadata: {
      generatedAt: new Date().toISOString(),
      rootDir,
      fileCount: sourceFiles.length,
      mtimes: nextMtimes,
    },
  };
}
