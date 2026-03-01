import type { OutputCompressionConfig } from "../config";
import { ContentStore } from "../store/content-store";

const DEFAULT_THRESHOLD_BYTES = 5000;
const DEFAULT_MAX_INDEX_SIZE_MB = 50;
const DEFAULT_EXCLUDED_TOOLS = new Set([
  "edit",
  "write",
  "slashcommand",
  "context_search",
  "todowrite",
  "switch_agent",
]);

export interface CompressionStats {
  compressed: number;
  bytesSaved: number;
  passedThrough: number;
}

function isErrorLikeOutput(text: string): boolean {
  const trimmed = text.trimStart();
  if (trimmed.startsWith("Error:") || trimmed.startsWith("error:")) {
    return true;
  }
  if (/(^|\n)\s*at\s+.+\(.+:\d+:\d+\)/m.test(text) || /(^|\n)\s*at\s+.+:\d+:\d+/m.test(text)) {
    return true;
  }
  const head = text.slice(0, 2000);
  if (/Traceback \(most recent call last\)/.test(head)) {
    return true;
  }
  if (/^goroutine \d+ \[/m.test(head)) {
    return true;
  }
  if (/^thread '.+' panicked at/m.test(head)) {
    return true;
  }
  return false;
}

export function createOutputCompressionHook(config: OutputCompressionConfig = {}) {
  const store = new ContentStore({ maxSizeMB: config.maxIndexSizeMB ?? DEFAULT_MAX_INDEX_SIZE_MB });
  const thresholdBytes = config.thresholdBytes ?? DEFAULT_THRESHOLD_BYTES;
  const excludedTools = new Set([...DEFAULT_EXCLUDED_TOOLS, ...(config.excludeTools ?? [])]);

  const stats: CompressionStats = {
    compressed: 0,
    bytesSaved: 0,
    passedThrough: 0,
  };

  const toolExecuteAfter = (
    input: { tool: string; sessionID: string; callID: string; args?: unknown },
    output: { title: string; output: string; metadata: unknown }
  ) => {
    if (config.enabled === false) {
      stats.passedThrough += 1;
      return;
    }

    if (excludedTools.has(input.tool)) {
      stats.passedThrough += 1;
      return;
    }

    if (typeof output.output !== "string") {
      stats.passedThrough++;
      return;
    }

    const rawOutput = output.output ?? "";
    const byteCount = Buffer.byteLength(rawOutput, "utf8");
    if (byteCount < thresholdBytes) {
      stats.passedThrough += 1;
      return;
    }

    if (isErrorLikeOutput(rawOutput)) {
      stats.passedThrough += 1;
      return;
    }

    const source = `${input.sessionID}:${input.tool}:${input.callID}`;
    try {
      const indexed = store.index({ content: rawOutput, source, session: input.sessionID });
      const topTerms = indexed.vocabulary.join(", ");
      const suggestedQueries = indexed.vocabulary.slice(0, 3).join(", ");
      output.output = [
        `[Compressed] ${byteCount} bytes from ${input.tool} indexed as "${source}".`,
        `${indexed.chunkCount} sections indexed. Top terms: ${topTerms}.`,
        `Use context_search tool with queries like [${suggestedQueries}] to retrieve specific sections.`,
      ].join("\n");
      stats.compressed += 1;
      stats.bytesSaved += Math.max(0, byteCount - Buffer.byteLength(output.output, "utf8"));
    } catch (error) {
      console.warn("[legion-plugin] Output compression failed:", error);
      stats.passedThrough += 1;
    }
  };

  // Clean up SQLite DB on process exit (DB is process-scoped)
  const cleanup = () => {
    try {
      store.close();
    } catch {
      // best-effort cleanup
    }
  };
  process.once("exit", cleanup);

  return {
    "tool.execute.after": toolExecuteAfter,
    getStore: () => store,
    getStats: (): CompressionStats => ({ ...stats }),
    cleanup,
  };
}
