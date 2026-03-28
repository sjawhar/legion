import { z } from "zod";

export const CODEBASE_INDEX_VERSION = 1;

export interface ModuleDependencyEntry {
  imports: string[];
  externals: string[];
}

export interface CodebaseIndexMetadata {
  generatedAt: string;
  rootDir: string;
  fileCount: number;
  mtimes: Record<string, number>;
}

export interface CodebaseIndex {
  version: number;
  dependencyGraph: Record<string, ModuleDependencyEntry>;
  metadata: CodebaseIndexMetadata;
}

export type CodebaseIndexResponse =
  | CodebaseIndex
  | {
      version: number;
      dependencyGraph: Record<string, ModuleDependencyEntry>;
      metadata: Record<string, never>;
    };

export const ModuleDependencyEntrySchema = z.object({
  imports: z.array(z.string()),
  externals: z.array(z.string()),
});

export const CodebaseIndexMetadataSchema = z.object({
  generatedAt: z.string(),
  rootDir: z.string(),
  fileCount: z.number(),
  mtimes: z.record(z.string(), z.number()),
});

export const CodebaseIndexSchema = z.object({
  version: z.number(),
  dependencyGraph: z.record(z.string(), ModuleDependencyEntrySchema),
  metadata: CodebaseIndexMetadataSchema,
});

export function createEmptyCodebaseIndexResponse(): CodebaseIndexResponse {
  return {
    version: CODEBASE_INDEX_VERSION,
    dependencyGraph: {},
    metadata: {},
  };
}
