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

export interface TestMapping {
  sourceToTests: Record<string, string[]>;
  testToSources: Record<string, string[]>;
}

export interface ChangeHotspotEntry {
  filePath: string;
  changeCount: number;
  lastChanged: string;
}

export type ExportedSymbolKind =
  | "function"
  | "class"
  | "type"
  | "interface"
  | "const"
  | "let"
  | "default"
  | "reexport";

export interface ExportedSymbol {
  name: string;
  kind: ExportedSymbolKind;
  signature: string;
}

export interface CodebaseIndex {
  version: number;
  dependencyGraph: Record<string, ModuleDependencyEntry>;
  apiSurface: Record<string, ExportedSymbol[]>;
  testMapping: TestMapping;
  hotspots: ChangeHotspotEntry[];
  metadata: CodebaseIndexMetadata;
}

export type CodebaseIndexResponse =
  | CodebaseIndex
  | {
      version: number;
      dependencyGraph: Record<string, ModuleDependencyEntry>;
      apiSurface: Record<string, ExportedSymbol[]>;
      testMapping: TestMapping;
      hotspots: ChangeHotspotEntry[];
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

export const TestMappingSchema = z.object({
  sourceToTests: z.record(z.string(), z.array(z.string())),
  testToSources: z.record(z.string(), z.array(z.string())),
});

export const ChangeHotspotEntrySchema = z.object({
  filePath: z.string(),
  changeCount: z.number(),
  lastChanged: z.string(),
});

export const ExportedSymbolKindSchema = z.enum([
  "function",
  "class",
  "type",
  "interface",
  "const",
  "let",
  "default",
  "reexport",
]);

export const ExportedSymbolSchema = z.object({
  name: z.string(),
  kind: ExportedSymbolKindSchema,
  signature: z.string(),
});

export const CodebaseIndexSchema = z.object({
  version: z.number(),
  dependencyGraph: z.record(z.string(), ModuleDependencyEntrySchema),
  apiSurface: z.record(z.string(), z.array(ExportedSymbolSchema)),
  testMapping: TestMappingSchema,
  hotspots: z.array(ChangeHotspotEntrySchema).default([]),
  metadata: CodebaseIndexMetadataSchema,
});

export function createEmptyCodebaseIndexResponse(): CodebaseIndexResponse {
  return {
    version: CODEBASE_INDEX_VERSION,
    dependencyGraph: {},
    apiSurface: {},
    testMapping: {
      sourceToTests: {},
      testToSources: {},
    },
    hotspots: [],
    metadata: {},
  };
}
