import {
  type BuildDependencyGraphOptions,
  buildDependencyGraph,
  updateDependencyGraphIncremental,
} from "./graph";
import { buildChangeHotspots } from "./hotspots";
import { readCodebaseIndex, writeCodebaseIndex } from "./persistence";
import {
  type CodebaseIndex,
  type CodebaseIndexResponse,
  createEmptyCodebaseIndexResponse,
} from "./types";

export class CodebaseIndexManager {
  private currentIndex: CodebaseIndex | null = null;

  constructor(
    private readonly rootDir: string,
    private readonly filePath: string,
    private readonly options?: BuildDependencyGraphOptions
  ) {}

  private async buildFullIndex(): Promise<CodebaseIndex> {
    const [index, hotspots] = await Promise.all([
      buildDependencyGraph(this.rootDir, this.options),
      buildChangeHotspots(this.rootDir, this.options),
    ]);

    return {
      ...index,
      hotspots,
    };
  }

  private async incrementallyUpdateIndex(current: CodebaseIndex): Promise<CodebaseIndex> {
    const [index, hotspots] = await Promise.all([
      updateDependencyGraphIncremental(current, this.options),
      buildChangeHotspots(this.rootDir, this.options),
    ]);

    return {
      ...index,
      hotspots,
    };
  }

  async initialize(): Promise<CodebaseIndex> {
    const existing = await readCodebaseIndex(this.filePath);
    if (existing) {
      this.currentIndex = existing;
      return existing;
    }

    const built = await this.buildFullIndex();
    await writeCodebaseIndex(this.filePath, built);
    this.currentIndex = built;
    return built;
  }

  getResponse(): CodebaseIndexResponse {
    return this.currentIndex ?? createEmptyCodebaseIndexResponse();
  }

  async rebuild(): Promise<CodebaseIndex> {
    const rebuilt = await this.buildFullIndex();
    await writeCodebaseIndex(this.filePath, rebuilt);
    this.currentIndex = rebuilt;
    return rebuilt;
  }

  async incrementalUpdate(): Promise<CodebaseIndex> {
    if (!this.currentIndex) {
      return this.rebuild();
    }

    const updated = await this.incrementallyUpdateIndex(this.currentIndex);
    this.currentIndex = updated;
    await writeCodebaseIndex(this.filePath, updated);
    return updated;
  }
}
