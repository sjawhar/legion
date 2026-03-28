import {
  type BuildDependencyGraphOptions,
  buildDependencyGraph,
  updateDependencyGraphIncremental,
} from "./graph";
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

  async initialize(): Promise<CodebaseIndex> {
    const existing = await readCodebaseIndex(this.filePath);
    if (existing) {
      this.currentIndex = existing;
      return existing;
    }

    const built = await buildDependencyGraph(this.rootDir, this.options);
    await writeCodebaseIndex(this.filePath, built);
    this.currentIndex = built;
    return built;
  }

  getResponse(): CodebaseIndexResponse {
    return this.currentIndex ?? createEmptyCodebaseIndexResponse();
  }

  async rebuild(): Promise<CodebaseIndex> {
    const rebuilt = await buildDependencyGraph(this.rootDir, this.options);
    await writeCodebaseIndex(this.filePath, rebuilt);
    this.currentIndex = rebuilt;
    return rebuilt;
  }

  async incrementalUpdate(): Promise<CodebaseIndex> {
    if (!this.currentIndex) {
      return this.rebuild();
    }

    const updated = await updateDependencyGraphIncremental(this.currentIndex, this.options);
    this.currentIndex = updated;
    await writeCodebaseIndex(this.filePath, updated);
    return updated;
  }
}
